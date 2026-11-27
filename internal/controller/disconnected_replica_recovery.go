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
	"fmt"
	"strings"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const disconnectedReplicaRecoveryPodUIDAnnotation = "valkey.io/disconnected-replica-recovery-pod-uid"

var errReplicaSetReadForbidden = errors.New("replica recovery cannot verify Deployment ownership: operator needs get permission for apps/replicasets")

type disconnectedReplicaCandidate struct {
	live     *valkey.NodeState
	resource *valkeyiov1alpha1.ValkeyNode
}

// recoverDisconnectedReplica replaces one empty, ephemeral replica that joined
// a membership view disconnected from its intended slot-owning primary. The
// ValkeyNode annotation permits retries against the same Pod UID, but prevents
// a second replacement until the first one has joined and synced.
func (r *ValkeyClusterReconciler) recoverDisconnectedReplica(
	ctx context.Context,
	cluster *valkeyiov1alpha1.ValkeyCluster,
	nodes *valkeyiov1alpha1.ValkeyNodeList,
	state *valkey.ClusterState,
	dial valkey.DialFunc,
) (requeue bool, blocked string, err error) {
	if cluster.Spec.Persistence != nil || !replicaRecoveryMayApply(nodes, state) {
		return false, "", nil
	}

	freshCluster := &valkeyiov1alpha1.ValkeyCluster{}
	if err := r.uncachedReader().Get(ctx, client.ObjectKeyFromObject(cluster), freshCluster); err != nil {
		return false, "", err
	}
	if freshCluster.UID != cluster.UID || !freshCluster.DeletionTimestamp.IsZero() ||
		freshCluster.Spec.Persistence != nil || reconciliationPaused(freshCluster) {
		return false, "", nil
	}

	freshNodes := &valkeyiov1alpha1.ValkeyNodeList{}
	if err := r.uncachedReader().List(ctx, freshNodes,
		client.InNamespace(cluster.Namespace), client.MatchingLabels(map[string]string{LabelCluster: cluster.Name})); err != nil {
		return false, "", err
	}
	owned := ownedValkeyNodes(freshCluster, freshNodes)
	expected := int(freshCluster.Spec.Shards) * (1 + int(freshCluster.GetReplicasPerShard()))
	if len(owned) != expected {
		return false, "", nil
	}
	ownedNodes := &valkeyiov1alpha1.ValkeyNodeList{Items: make([]valkeyiov1alpha1.ValkeyNode, 0, len(owned))}
	for _, node := range owned {
		ownedNodes.Items = append(ownedNodes.Items, *node)
	}
	freshNodes = ownedNodes
	if !recoveryNodeAddressesComplete(freshNodes) {
		return false, "", nil
	}

	freshState := r.scrapeClusterState(ctx, freshNodes, dial)
	if !completeRecoveryState(freshState, freshNodes) {
		return false, "", nil
	}
	attempt, attemptErr := recoveryAttempt(freshNodes)
	if attemptErr != nil {
		return false, attemptErr.Error(), nil
	}
	if attempt != nil && replicaRecoveryComplete(freshState, freshNodes, attempt) {
		pod, err := r.disconnectedReplicaPod(ctx, freshCluster, attempt)
		if err != nil {
			if errors.Is(err, errReplicaSetReadForbidden) {
				return false, err.Error(), nil
			}
			return false, "", err
		}
		if pod == nil {
			return false, "", nil
		}
		if err := r.clearReplicaRecoveryAttempt(ctx, attempt); err != nil {
			return false, "", err
		}
		return true, "", nil
	}

	return r.replaceDisconnectedReplica(ctx, freshCluster, freshNodes, freshState, attempt)
}

func (r *ValkeyClusterReconciler) replaceDisconnectedReplica(
	ctx context.Context,
	cluster *valkeyiov1alpha1.ValkeyCluster,
	nodes *valkeyiov1alpha1.ValkeyNodeList,
	state *valkey.ClusterState,
	attempt *valkeyiov1alpha1.ValkeyNode,
) (requeue bool, blocked string, err error) {
	for _, candidate := range findDisconnectedReplicas(state, nodes, attempt) {
		pod, err := r.disconnectedReplicaPod(ctx, cluster, candidate.resource)
		if err != nil {
			if errors.Is(err, errReplicaSetReadForbidden) {
				return false, err.Error(), nil
			}
			return false, "", err
		}
		if pod == nil {
			continue
		}
		// Confirm the candidate from the fresh scrape before recording the attempt.
		safe, err := disconnectedReplicaStillDisposable(ctx, candidate.live, state)
		if err != nil {
			return false, "", err
		}
		if !safe {
			if candidate.resource.Annotations[disconnectedReplicaRecoveryPodUIDAnnotation] == string(pod.UID) {
				if err := r.clearReplicaRecoveryAttempt(ctx, candidate.resource); err != nil {
					return false, "", err
				}
			}
			continue
		}
		if previous := candidate.resource.Annotations[disconnectedReplicaRecoveryPodUIDAnnotation]; previous != "" && previous != string(pod.UID) {
			return false, fmt.Sprintf("replica recovery for ValkeyNode %s already replaced Pod UID %s; refusing to replace Pod UID %s",
				candidate.resource.Name, previous, pod.UID), nil
		}
		if err := r.markReplicaRecoveryAttempt(ctx, candidate.resource, pod.UID); err != nil {
			return false, "", err
		}

		// A later reconcile sees the marker and stays on this ValkeyNode. Re-read
		// Valkey after writing it because membership or data may have changed.
		safe, err = disconnectedReplicaStillDisposable(ctx, candidate.live, state)
		if err != nil {
			return true, "", err
		}
		if !safe {
			if err := r.clearReplicaRecoveryAttempt(ctx, candidate.resource); err != nil {
				return false, "", err
			}
			return false, "", nil
		}
		if err := r.Delete(ctx, pod, client.Preconditions{UID: &pod.UID}); err != nil {
			if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
				return true, "", nil
			}
			return true, "", err
		}
		r.Recorder.Eventf(cluster, pod, corev1.EventTypeNormal, "DisconnectedReplicaReplaced", "RecoverReplica",
			"Replaced empty Pod %s after its Valkey node joined a membership view disconnected from shard %s", pod.Name, candidate.resource.Labels[LabelShardIndex])
		return true, "", nil
	}
	return false, "", nil
}

func replicaRecoveryMayApply(nodes *valkeyiov1alpha1.ValkeyNodeList, state *valkey.ClusterState) bool {
	for i := range nodes.Items {
		if nodes.Items[i].Annotations[disconnectedReplicaRecoveryPodUIDAnnotation] != "" {
			return true
		}
	}
	return findDisconnectedReplica(state, nodes) != nil
}

func recoveryNodeAddressesComplete(nodes *valkeyiov1alpha1.ValkeyNodeList) bool {
	seen := make(map[string]struct{}, len(nodes.Items))
	for i := range nodes.Items {
		address := nodes.Items[i].Status.PodIP
		if address == "" {
			return false
		}
		if _, found := seen[address]; found {
			return false
		}
		seen[address] = struct{}{}
	}
	return true
}

func completeRecoveryState(state *valkey.ClusterState, nodes *valkeyiov1alpha1.ValkeyNodeList) bool {
	if state == nil || len(state.GetUnassignedSlots()) != 0 || len(state.AllNodes()) != len(nodes.Items) {
		return false
	}
	ids := make(map[string]struct{}, len(nodes.Items))
	for i := range nodes.Items {
		address := nodes.Items[i].Status.PodIP
		live := state.FindNodeForAddress(address)
		if live == nil || live.Id == "" || live.Myself() == nil {
			return false
		}
		if _, found := ids[live.Id]; found {
			return false
		}
		ids[live.Id] = struct{}{}
	}
	return true
}

func recoveryAttempt(nodes *valkeyiov1alpha1.ValkeyNodeList) (*valkeyiov1alpha1.ValkeyNode, error) {
	var attempt *valkeyiov1alpha1.ValkeyNode
	for i := range nodes.Items {
		if nodes.Items[i].Annotations[disconnectedReplicaRecoveryPodUIDAnnotation] == "" {
			continue
		}
		if attempt != nil {
			return nil, errors.New("more than one ValkeyNode has a disconnected replica recovery in progress")
		}
		attempt = &nodes.Items[i]
	}
	return attempt, nil
}

func findDisconnectedReplica(state *valkey.ClusterState, nodes *valkeyiov1alpha1.ValkeyNodeList) *disconnectedReplicaCandidate {
	candidates := findDisconnectedReplicas(state, nodes, nil)
	if len(candidates) == 0 {
		return nil
	}
	return candidates[0]
}

func findDisconnectedReplicas(state *valkey.ClusterState, nodes *valkeyiov1alpha1.ValkeyNodeList, attempt *valkeyiov1alpha1.ValkeyNode) []*disconnectedReplicaCandidate {
	if state == nil || len(state.PendingNodes) == 0 || len(state.GetUnassignedSlots()) != 0 {
		return nil
	}
	active := activeNodeIDs(state)
	var candidates []*disconnectedReplicaCandidate
	for _, live := range state.PendingNodes {
		if live.IsIsolated() || live.Myself() == nil || !live.IsPrimary() || live.Myself().HasSlotAssignment() ||
			live.Info["loading"] != "0" || live.Info["connected_slaves"] != "0" || live.HasKnownReplica() || state.HasReplicaOf(live.Id) {
			continue
		}
		resource := nodeForAddress(nodes, live.Address)
		if resource == nil || resource.Spec.Persistence != nil || resource.Status.ObservedGeneration != resource.Generation ||
			(attempt != nil && resource.Name != attempt.Name) {
			continue
		}
		_, shardIndex := nodeRoleAndShard(live.Address, nodes)
		primaryID, _ := findShardPrimary(state, shardIndex, nodes)
		if primaryID == "" || live.KnowsNode(primaryID) {
			continue
		}
		knownByActive := false
		for id := range active {
			if live.KnowsNode(id) {
				knownByActive = true
				break
			}
		}
		for _, shard := range state.Shards {
			for _, member := range shard.Nodes {
				if member.KnowsNode(live.Id) {
					knownByActive = true
					break
				}
			}
			if knownByActive {
				break
			}
		}
		if knownByActive {
			continue
		}
		if membershipPathToActive(state, live.Id, nil, active) {
			continue
		}
		if !activeMembershipConnected(state) {
			return nil
		}
		candidates = append(candidates, &disconnectedReplicaCandidate{live: live, resource: resource})
	}
	return candidates
}

func activeMembershipConnected(state *valkey.ClusterState) bool {
	active := activeNodeIDs(state)
	owners := make(map[string][]valkey.SlotsRange, len(state.Shards))
	for _, shard := range state.Shards {
		if shard.PrimaryId == "" || len(shard.Slots) == 0 {
			return false
		}
		owners[shard.PrimaryId] = shard.Slots
	}
	for _, shard := range state.Shards {
		for _, node := range shard.Nodes {
			// Check each node, not only the union of scraped slots. This rejects
			// split slot-owner views that look complete when combined.
			if node.ClusterInfo["cluster_state"] != "ok" ||
				node.ClusterInfo["cluster_slots_ok"] != fmt.Sprint(valkey.TotalSlots) ||
				node.ClusterInfo["cluster_slots_pfail"] != "0" || node.ClusterInfo["cluster_slots_fail"] != "0" ||
				!node.HasExactSlotOwnerView(owners) {
				return false
			}
			for id := range active {
				if id != node.Id && !node.HasHealthyPeer(id) {
					return false
				}
			}
		}
	}
	return len(active) > 0
}

// membershipPathToActive treats a healthy peer reference as an undirected
// gossip path. A pending bridge means normal convergence may still connect the
// candidate to the intended cluster, so recovery must not delete it.
func membershipPathToActive(
	state *valkey.ClusterState,
	startID string,
	freshPeers map[string]struct{},
	active map[string]struct{},
) bool {
	adjacency := make(map[string]map[string]struct{})
	addEdge := func(a, b string) {
		if a == "" || b == "" || a == b {
			return
		}
		if adjacency[a] == nil {
			adjacency[a] = make(map[string]struct{})
		}
		if adjacency[b] == nil {
			adjacency[b] = make(map[string]struct{})
		}
		adjacency[a][b] = struct{}{}
		adjacency[b][a] = struct{}{}
	}
	for _, node := range state.AllNodes() {
		if node.Id == startID && freshPeers != nil {
			continue
		}
		for _, peerID := range node.HealthyPeerIDs() {
			addEdge(node.Id, peerID)
		}
	}
	for peerID := range freshPeers {
		addEdge(startID, peerID)
	}

	seen := map[string]struct{}{startID: {}}
	queue := []string{startID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for peerID := range adjacency[id] {
			if _, found := seen[peerID]; found {
				continue
			}
			if _, found := active[peerID]; found {
				return true
			}
			seen[peerID] = struct{}{}
			queue = append(queue, peerID)
		}
	}
	return false
}

func activeNodeIDs(state *valkey.ClusterState) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, shard := range state.Shards {
		for _, node := range shard.Nodes {
			ids[node.Id] = struct{}{}
		}
	}
	return ids
}

func nodeForAddress(nodes *valkeyiov1alpha1.ValkeyNodeList, address string) *valkeyiov1alpha1.ValkeyNode {
	for i := range nodes.Items {
		if nodes.Items[i].Status.PodIP == address {
			return &nodes.Items[i]
		}
	}
	return nil
}

func replicaRecoveryComplete(state *valkey.ClusterState, nodes *valkeyiov1alpha1.ValkeyNodeList, resource *valkeyiov1alpha1.ValkeyNode) bool {
	live := state.FindNodeForAddress(resource.Status.PodIP)
	if live == nil || live.Info["loading"] == "1" {
		return false
	}
	_, shardIndex := nodeRoleAndShard(resource.Status.PodIP, nodes)
	primaryID, _ := findShardPrimary(state, shardIndex, nodes)
	if primaryID == "" {
		return false
	}
	if live.IsPrimary() {
		return live.Id == primaryID && live.Myself() != nil && live.Myself().HasSlotAssignment() && activeMembershipConnected(state)
	}
	return live.Info["master_sync_in_progress"] != "1" && live.Info["master_link_status"] == "up" &&
		live.PrimaryIdFromSelf() == primaryID
}

func (r *ValkeyClusterReconciler) markReplicaRecoveryAttempt(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode, uid types.UID) error {
	if node.Annotations[disconnectedReplicaRecoveryPodUIDAnnotation] == string(uid) {
		return nil
	}
	base := node.DeepCopy()
	updated := node.DeepCopy()
	upsertAnnotation(updated, disconnectedReplicaRecoveryPodUIDAnnotation, string(uid))
	if err := r.Patch(ctx, updated, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	node.Annotations = updated.Annotations
	node.ResourceVersion = updated.ResourceVersion
	return nil
}

func (r *ValkeyClusterReconciler) clearReplicaRecoveryAttempt(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) error {
	base := node.DeepCopy()
	updated := node.DeepCopy()
	delete(updated.Annotations, disconnectedReplicaRecoveryPodUIDAnnotation)
	return r.Patch(ctx, updated, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func (r *ValkeyClusterReconciler) disconnectedReplicaPod(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, node *valkeyiov1alpha1.ValkeyNode) (*corev1.Pod, error) {
	if !metav1.IsControlledBy(node, cluster) || node.Spec.Persistence != nil ||
		node.Status.ObservedGeneration != node.Generation || node.Status.PodName == "" || node.Status.PodIP == "" {
		return nil, nil
	}
	pod := &corev1.Pod{}
	if err := r.uncachedReader().Get(ctx, client.ObjectKey{Namespace: node.Namespace, Name: node.Status.PodName}, pod); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	server := serverContainerStatus(pod)
	if pod.DeletionTimestamp != nil || pod.Status.PodIP != node.Status.PodIP ||
		server == nil || server.State.Running == nil || !server.Ready || !podHasEphemeralData(pod) {
		return nil, nil
	}

	switch effectiveWorkloadType(node.Spec.WorkloadType) {
	case valkeyiov1alpha1.WorkloadTypeStatefulSet:
		workload := &appsv1.StatefulSet{}
		if err := r.uncachedReader().Get(ctx, client.ObjectKey{Namespace: node.Namespace, Name: valkeyNodeResourceName(node)}, workload); err != nil {
			return nil, client.IgnoreNotFound(err)
		}
		if !metav1.IsControlledBy(workload, node) || !metav1.IsControlledBy(pod, workload) {
			return nil, nil
		}
	case valkeyiov1alpha1.WorkloadTypeDeployment:
		workload := &appsv1.Deployment{}
		if err := r.uncachedReader().Get(ctx, client.ObjectKey{Namespace: node.Namespace, Name: valkeyNodeResourceName(node)}, workload); err != nil {
			return nil, client.IgnoreNotFound(err)
		}
		owner := metav1.GetControllerOf(pod)
		if !metav1.IsControlledBy(workload, node) || owner == nil || owner.Kind != "ReplicaSet" {
			return nil, nil
		}
		replicaSet := &appsv1.ReplicaSet{}
		if err := r.uncachedReader().Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: owner.Name}, replicaSet); err != nil {
			if apierrors.IsForbidden(err) {
				return nil, fmt.Errorf("%w: %v", errReplicaSetReadForbidden, err)
			}
			return nil, client.IgnoreNotFound(err)
		}
		if !metav1.IsControlledBy(replicaSet, workload) || !metav1.IsControlledBy(pod, replicaSet) {
			return nil, nil
		}
	default:
		return nil, nil
	}
	return pod, nil
}

func podHasEphemeralData(pod *corev1.Pod) bool {
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == dataVolumeName {
			return pod.Spec.Volumes[i].EmptyDir != nil
		}
	}
	return false
}

func disconnectedReplicaStillDisposable(ctx context.Context, candidate *valkey.NodeState, state *valkey.ClusterState) (bool, error) {
	results := candidate.Client.DoMulti(ctx,
		candidate.Client.B().ClusterMyid().Build(),
		candidate.Client.B().ClusterNodes().Build(),
		candidate.Client.B().Info().Section("persistence", "replication", "keyspace").Build(),
	)
	if len(results) != 3 {
		return false, fmt.Errorf("expected 3 final replica recovery checks, got %d", len(results))
	}
	id, err := results[0].ToString()
	if err != nil {
		return false, err
	}
	rawNodes, err := results[1].ToString()
	if err != nil {
		return false, err
	}
	info, err := results[2].ToString()
	if err != nil {
		return false, err
	}
	fields := infoFields(info)
	if id != candidate.Id || !emptyKeyspace(info) || fields["loading"] != "0" ||
		fields["master_sync_in_progress"] == "1" || fields["connected_slaves"] != "0" {
		return false, nil
	}
	parsed := valkey.ParseClusterNodes(strings.TrimPrefix(rawNodes, "txt:"))
	myself := valkey.FindMyself(parsed)
	if myself == nil || myself.Id != candidate.Id || !myself.IsPrimary() || myself.HasSlotAssignment() {
		return false, nil
	}
	establishedPeer := false
	freshPeers := make(map[string]struct{})
	active := activeNodeIDs(state)
	for i := range parsed {
		if parsed[i].HasFlag(valkey.FlagHandshake) {
			return false, nil
		}
		if parsed[i].Id != candidate.Id {
			establishedPeer = true
		}
		if parsed[i].PrimaryId == candidate.Id {
			return false, nil
		}
		if _, found := active[parsed[i].Id]; found {
			return false, nil
		}
		if parsed[i].Id != candidate.Id && parsed[i].IsConnected() && !parsed[i].IsFailing() && !parsed[i].HasNoAddress() {
			freshPeers[parsed[i].Id] = struct{}{}
		}
	}
	for _, shard := range state.Shards {
		for _, member := range shard.Nodes {
			if member.KnowsNode(candidate.Id) {
				return false, nil
			}
		}
	}
	if membershipPathToActive(state, candidate.Id, freshPeers, active) {
		return false, nil
	}
	return establishedPeer, nil
}

func emptyKeyspace(info string) bool {
	inKeyspace := false
	for line := range strings.SplitSeq(info, "\n") {
		line = strings.TrimSpace(line)
		if line == "# Keyspace" {
			inKeyspace = true
			continue
		}
		if !inKeyspace {
			continue
		}
		if strings.HasPrefix(line, "# ") {
			return true
		}
		if strings.HasPrefix(line, "db") {
			return false
		}
	}
	return inKeyspace
}

func infoFields(info string) map[string]string {
	fields := make(map[string]string)
	for line := range strings.SplitSeq(info, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && key != "" {
			fields[key] = value
		}
	}
	return fields
}
