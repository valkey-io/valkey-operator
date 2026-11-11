/*
Copyright 2026 Valkey Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

// initialSyncGate is the readiness gate on a cluster member's pod. The cluster
// controller sets it once the pod holds its shard's data: it owns slots, or it
// is a replica whose full sync has finished. Pod Ready includes the gate, so
// the PodDisruptionBudget does not count a replacement replica before it has
// the data, and a drain cannot evict the primary it is still copying from.
// The operator itself reads ContainersReady (podContainersReady), so the walk
// never waits on the gate it is responsible for.
const initialSyncGate corev1.PodConditionType = "valkey.io/initial-sync-complete"

// initialSyncGateRecheck is how soon the cluster reconciles again while a
// member's gate is still false.
const initialSyncGateRecheck = 5 * time.Second

// usesInitialSyncGate reports whether the node's pod carries initialSyncGate:
// a member the cluster controller built, which has all three cluster labels.
// A standalone ValkeyNode, even one with the cluster label, has no controller
// to set the gate. Membership is read from labels rather than the owner
// reference, as in usesPrimaryHandoffScript, so both controllers build the
// same pod template.
func usesInitialSyncGate(node *valkeyiov1alpha1.ValkeyNode) bool {
	return node.Labels[LabelCluster] != "" && node.Labels[LabelShardIndex] != "" && node.Labels[LabelNodeIndex] != ""
}

// gateVerdict is what one scrape says about a node's initialSyncGate.
type gateVerdict int

const (
	// gateKeep: no evidence either way, so the pod keeps its current value.
	gateKeep gateVerdict = iota
	// gateSet: the node holds its shard's data.
	gateSet
	// gateClear: the node is loading a dataset or receiving a full sync.
	gateClear
)

// initialSyncVerdict decides a node's gate from this pass's scrape, with the
// condition reason and message to record. A replica whose link drops keeps a
// gate it already has, since it still holds the data. A load or a full sync
// clears it, since the node is replacing its dataset, for example after a
// restart with an empty one.
func initialSyncVerdict(n *valkey.NodeState, newShardPrimary bool) (gateVerdict, string, string) {
	if n == nil {
		return gateKeep, "", ""
	}
	if n.Info["loading"] == "1" {
		return gateClear, "Loading", "The server is loading its dataset"
	}
	if n.Info["master_sync_in_progress"] == "1" {
		return gateClear, "FullSyncInProgress", "The replica is receiving a full sync from its primary"
	}
	if n.IsPrimary() {
		if len(n.GetSlots()) > 0 {
			return gateSet, "OwnsSlots", "The primary owns slots"
		}
		if newShardPrimary {
			return gateSet, "NewShardPrimary", "The primary of a shard that is not in the cluster yet has no data to copy"
		}
		return gateKeep, "", ""
	}
	if n.Info["master_link_status"] == "up" {
		return gateSet, "ReplicaSynced", "The replica finished its full sync from its primary"
	}
	return gateKeep, "", ""
}

// newShardPrimaries returns the pod IPs of node-index 0 members whose shard is
// not in the topology yet. Such a primary has nothing to copy, so its gate is
// set before it receives slots, which covers both cluster creation and
// scale-out. It returns nothing unless the scrape reached every member with a
// pod IP: a member the scrape missed may be what puts the shard in the
// topology.
func newShardPrimaries(state *valkey.ClusterState, nodes *valkeyiov1alpha1.ValkeyNodeList) map[string]bool {
	if state == nil {
		return nil
	}
	for i := range nodes.Items {
		if ip := nodes.Items[i].Status.PodIP; ip != "" && !state.HasAddress(ip) {
			return nil
		}
	}
	primaries := map[string]bool{}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Labels[LabelNodeIndex] != "0" || n.Status.PodIP == "" {
			continue
		}
		shardIndex, err := strconv.Atoi(n.Labels[LabelShardIndex])
		if err != nil || shardExistsInTopology(state, shardIndex, nodes) {
			continue
		}
		primaries[n.Status.PodIP] = true
	}
	return primaries
}

// syncInitialSyncGates sets initialSyncGate on each member pod from this
// pass's scrape. A node the scrape missed keeps its value. It reports whether
// any member pod is still gated, so the caller can look again soon.
func (r *ValkeyClusterReconciler) syncInitialSyncGates(ctx context.Context, nodes *valkeyiov1alpha1.ValkeyNodeList, state *valkey.ClusterState) (bool, error) {
	scraped := map[string]*valkey.NodeState{}
	if state != nil {
		for _, n := range state.AllNodes() {
			scraped[n.Address] = n
		}
	}
	newPrimaries := newShardPrimaries(state, nodes)
	gated := false
	var errs []error
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if !usesInitialSyncGate(node) || node.Status.PodName == "" {
			continue
		}
		pod := &corev1.Pod{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: node.Namespace, Name: node.Status.PodName}, pod); err != nil {
			if !apierrors.IsNotFound(err) {
				errs = append(errs, err)
			}
			continue
		}
		// A pod from before the gate existed has no gate to set until it rolls.
		if pod.DeletionTimestamp != nil || !podHasReadinessGate(pod, initialSyncGate) {
			continue
		}
		// The scrape is keyed by the node's recorded IP. A pod under the same
		// name with another IP is a replacement the scrape does not describe.
		var scrape *valkey.NodeState
		if ip := node.Status.PodIP; ip != "" && ip == pod.Status.PodIP {
			scrape = scraped[ip]
		}
		want, write, reason, message := initialSyncGateChange(pod, scrape, newPrimaries[node.Status.PodIP])
		if write {
			if err := r.setInitialSyncGate(ctx, pod, want, reason, message); err != nil {
				errs = append(errs, err)
				gated = true
				continue
			}
			logf.FromContext(ctx).V(1).Info("initial sync gate updated", "pod", pod.Name, "ready", want, "reason", reason)
		}
		if !want {
			gated = true
		}
	}
	return gated, errors.Join(errs...)
}

// initialSyncGateChange decides the pod's gate and whether to write it. A true
// gate belongs to the server container that earned it, whose ID the condition
// message records. Once that container is gone the server may have come back
// empty, so the gate is cleared, and only a ready server container can earn it
// again: a container that has just restarted is not ready yet, so a scrape
// taken before the restart cannot approve it.
func initialSyncGateChange(pod *corev1.Pod, scrape *valkey.NodeState, newShardPrimary bool) (bool, bool, string, string) {
	condition := podCondition(pod, initialSyncGate)
	current := condition != nil && condition.Status == corev1.ConditionTrue
	server := serverContainerStatus(pod)
	running := server != nil && server.State.Running != nil
	if current && (!running || server.ContainerID != gateContainerID(condition.Message)) {
		return false, true, "ServerRestarted", "The server container that earned the gate is gone; it is set again once the new one shows its data"
	}
	verdict, reason, message := initialSyncVerdict(scrape, newShardPrimary)
	switch verdict {
	case gateSet:
		if current || !running || !server.Ready {
			return current, false, "", ""
		}
		return true, true, reason, gateMessage(message, server.ContainerID)
	case gateClear:
		return false, current || condition == nil || condition.Reason != reason, reason, message
	}
	return current, false, "", ""
}

// gateContainerMarker introduces the server container ID at the end of a true
// gate's message.
const gateContainerMarker = " (server container "

// gateMessage appends the ID of the server container that earned the gate.
func gateMessage(message, containerID string) string {
	return message + gateContainerMarker + containerID + ")"
}

// gateContainerID returns the server container ID a gate message records, or
// "" when it records none.
func gateContainerID(message string) string {
	i := strings.LastIndex(message, gateContainerMarker)
	if i < 0 || !strings.HasSuffix(message, ")") {
		return ""
	}
	return strings.TrimSuffix(message[i+len(gateContainerMarker):], ")")
}

// serverContainerStatus returns the kubelet's status of the server container,
// or nil.
func serverContainerStatus(pod *corev1.Pod) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == serverContainerName {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	return nil
}

// setInitialSyncGate writes initialSyncGate on the pod with a strategic merge
// patch, which replaces only that condition, so the kubelet's conditions are
// left alone. The patch carries the pod's resourceVersion: a replacement pod
// under the same name gets a conflict instead of a verdict about its
// predecessor.
func (r *ValkeyClusterReconciler) setInitialSyncGate(ctx context.Context, pod *corev1.Pod, ready bool, reason, message string) error {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	patched := pod.DeepCopy()
	condition := corev1.PodCondition{
		Type:               initialSyncGate,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}
	replaced := false
	for i := range patched.Status.Conditions {
		if patched.Status.Conditions[i].Type == initialSyncGate {
			patched.Status.Conditions[i] = condition
			replaced = true
		}
	}
	if !replaced {
		patched.Status.Conditions = append(patched.Status.Conditions, condition)
	}
	return r.Status().Patch(ctx, patched, client.StrategicMergeFrom(pod, client.MergeFromWithOptimisticLock{}))
}

// podHasReadinessGate reports whether the pod spec lists the condition as a
// readiness gate.
func podHasReadinessGate(pod *corev1.Pod, condition corev1.PodConditionType) bool {
	for _, gate := range pod.Spec.ReadinessGates {
		if gate.ConditionType == condition {
			return true
		}
	}
	return false
}

// podCondition returns the pod's condition of the given type, or nil.
func podCondition(pod *corev1.Pod, condition corev1.PodConditionType) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == condition {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}
