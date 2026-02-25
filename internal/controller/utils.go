/*
Copyright 2025 Valkey Contributors.

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
	"maps"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	valkeyv1 "valkey.io/valkey-operator/api/v1alpha1"
	"valkey.io/valkey-operator/internal/valkey"
)

const appName = "valkey"

// Naming and labelling scheme
//
// Every Deployment (and therefore every Pod) encodes the node's position in
// the Valkey cluster in its *name*:
//
//	<cluster>-<N>-<M>    e.g. "mycluster-0-0", "mycluster-1-2"
//
// where N is the shard index and M is the node index within the shard. By
// convention, node 0 is the initial primary and nodes 1, 2, … are replicas.
// The name deliberately avoids "primary"/"replica" because failover can swap
// roles at any time.
//
// Two labels are set for selector uniqueness and kubectl convenience:
//
//	valkey.io/shard-index  – which shard ("0", "1", …)
//	valkey.io/node-index   – node within shard ("0", "1", …)
//
// The reconciler reads pod labels (via podRoleAndShard) to decide whether
// to assign slots (node 0 = initial primary) or issue CLUSTER REPLICATE
// (node 1+ = initial replica), and for which shard. After a failover,
// Valkey may promote a replica to primary, making node 0 a replica. The
// reconciler detects this via shardExistsInTopology: if the shard already
// has members in the cluster topology, the replacement node-index=0 pod
// joins as a replica instead of trying to claim slots. The labels themselves are not
// updated — the live role is always read from CLUSTER NODES.
//
// Names are set by deploymentName, labels by createClusterDeployment.
const (
	// LabelShardIndex identifies which shard a pod belongs to (e.g. "0", "1", "2").
	LabelShardIndex = "valkey.io/shard-index"
	// LabelNodeIndex identifies the node within a shard (e.g. "0", "1", "2").
	// Node 0 is the initial primary; nodes 1+ are replicas. Together with
	// LabelShardIndex this forms a unique selector per Deployment.
	LabelNodeIndex = "valkey.io/node-index"
)

// Role label values.
const (
	RolePrimary = "primary"
	RoleReplica = "replica"
)

// Labels returns a copy of user defined labels including recommended:
// https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/
func labels(cluster *valkeyv1.ValkeyCluster) map[string]string {
	if cluster.Labels == nil {
		cluster.Labels = make(map[string]string)
	}
	l := maps.Clone(cluster.Labels)
	l["app.kubernetes.io/name"] = appName
	l["app.kubernetes.io/instance"] = cluster.Name
	l["app.kubernetes.io/component"] = "valkey-cluster"
	l["app.kubernetes.io/part-of"] = appName
	l["app.kubernetes.io/managed-by"] = "valkey-operator"
	return l
}

// Annotations returns a copy of user defined annotations.
func annotations(cluster *valkeyv1.ValkeyCluster) map[string]string {
	return maps.Clone(cluster.Annotations)
}

// This function takes a K8S object reference (eg: pod, secret, configmap, etc),
// and a map of annotations to add to, or replace existing, within the object.
// Returns true if the annotation was added, or updated
func upsertAnnotation(o metav1.Object, key string, val string) bool {

	// Get current annotations
	annotations := o.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}

	// If found, and equal, then no update
	if annotations[key] == val {
		return false
	}

	annotations[key] = val
	o.SetAnnotations(annotations)

	return true
}

// podRoleAndShard finds the pod matching the given IP address and reads its
// labels to determine the intended role and shard index.
//
// The role is derived from valkey.io/node-index: node 0 is the initial
// primary, nodes 1+ are replicas. Returns ("", -1) if the pod is not
// found or the labels are missing.
func podRoleAndShard(address string, pods *corev1.PodList) (string, int) {
	idx := slices.IndexFunc(pods.Items, func(p corev1.Pod) bool { return p.Status.PodIP == address })
	if idx == -1 {
		return "", -1
	}
	pod := &pods.Items[idx]
	shardIndex, err := strconv.Atoi(pod.Labels[LabelShardIndex])
	if err != nil {
		return "", -1
	}
	nodeIndex, err := strconv.Atoi(pod.Labels[LabelNodeIndex])
	if err != nil {
		return "", -1
	}
	if nodeIndex == 0 {
		return RolePrimary, shardIndex
	}
	return RoleReplica, shardIndex
}

// shardExistsInTopology reports whether another pod in the same shard (same
// shard-index label) already exists as a member of any shard in the Valkey
// cluster topology. This covers two cases:
//
//  1. Post-failover (completed): a promoted replica is the primary.
//  2. Mid-failover (in progress): the replica exists but hasn't been promoted
//     yet — the operator must wait rather than trying to assign new slots.
//
// In both cases the replacement node-index=0 pod must NOT call
// assignSlotsToNewPrimary. Instead it should fall through to
// replicateToShardPrimary, which will either succeed (case 1) or return an
// error and retry on the next reconcile (case 2).
func shardExistsInTopology(state *valkey.ClusterState, shardIndex int, pods *corev1.PodList) bool {
	si := strconv.Itoa(shardIndex)
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Labels[LabelShardIndex] != si || p.Status.PodIP == "" {
			continue
		}
		for _, shard := range state.Shards {
			for _, node := range shard.Nodes {
				if node.Address == p.Status.PodIP {
					return true
				}
			}
		}
	}
	return false
}

// findShardPrimary scans all pods with the given shard-index label and returns
// the Valkey node ID + IP of whichever pod is currently the slot-bearing
// primary, regardless of its node-index label. This handles the post-failover
// case where node-index=1 (or higher) was promoted by Valkey.
// Returns ("", "") if no primary is found.
func findShardPrimary(state *valkey.ClusterState, shardIndex int, pods *corev1.PodList) (nodeID, ip string) {
	si := strconv.Itoa(shardIndex)
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Labels[LabelShardIndex] != si || p.Status.PodIP == "" {
			continue
		}
		for _, shard := range state.Shards {
			if len(shard.Slots) == 0 {
				continue
			}
			primary := shard.GetPrimaryNode()
			if primary != nil && primary.Address == p.Status.PodIP {
				return primary.Id, p.Status.PodIP
			}
		}
	}
	return "", ""
}

func countSlots(ranges []valkey.SlotsRange) int {
	count := 0
	for _, slot := range ranges {
		count += slot.End - slot.Start + 1
	}
	return count
}
