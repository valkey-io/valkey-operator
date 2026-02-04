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
	"context"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
	"valkey.io/valkey-operator/internal/valkey"
)

func (r *ValkeyClusterReconciler) attachReplica(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState) (bool, error) {
	if state == nil {
		return false, nil
	}

	replicasRequired := int(cluster.Spec.Replicas)
	if replicasRequired == 0 {
		return false, nil
	}

	needsReplica := []*valkey.NodeState{}
	emptyMasters := []*valkey.NodeState{}
	for _, shard := range state.Shards {
		primary := shard.GetPrimaryNode()
		if primary == nil {
			continue
		}
		if countSlots(shard.Slots) == 0 {
			emptyMasters = append(emptyMasters, primary)
			continue
		}
		// Ask the primary itself how many replicas it sees.
		// This is the most authoritative view because CLUSTER REPLICATE
		// notifies the primary directly, so it converges fastest.
		replicaCount := countReplicasForPrimary(primary.ClusterNodes, primary.Id)
		if replicaCount < replicasRequired {
			needsReplica = append(needsReplica, primary)
		}
	}

	if len(needsReplica) == 0 {
		return false, nil
	}

	pendingCandidates := []*valkey.NodeState{}
	for _, node := range state.PendingNodes {
		if node.IsPrimary() && len(node.GetSlots()) == 0 {
			pendingCandidates = append(pendingCandidates, node)
		}
	}

	if len(emptyMasters) == 0 && len(pendingCandidates) == 0 {
		return false, nil
	}

	sort.Slice(needsReplica, func(i, j int) bool {
		return needsReplica[i].Address < needsReplica[j].Address
	})
	sort.Slice(emptyMasters, func(i, j int) bool {
		return emptyMasters[i].Address < emptyMasters[j].Address
	})
	sort.Slice(pendingCandidates, func(i, j int) bool {
		return pendingCandidates[i].Address < pendingCandidates[j].Address
	})

	log := logf.FromContext(ctx)
	candidates := append(emptyMasters, pendingCandidates...)
	for _, candidate := range candidates {
		for _, target := range needsReplica {
			if candidate.Id == target.Id {
				continue
			}
			knowsPrimary := false
			for line := range strings.SplitSeq(candidate.ClusterNodes, "\n") {
				fields := strings.Fields(line)
				if len(fields) < 2 {
					continue
				}
				if fields[0] == target.Id {
					knowsPrimary = true
					break
				}
			}
			if !knowsPrimary {
				log.V(1).Info("meeting primary before replicate", "primary address", target.Address, "replica address", candidate.Address)
				if err := candidate.Client.Do(ctx, candidate.Client.B().ClusterMeet().Ip(target.Address).Port(int64(target.Port)).Build()).Error(); err != nil {
					log.Error(err, "command failed: CLUSTER MEET", "from", candidate.Address, "to", target.Address)
					r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "ClusterMeetFailed", "ClusterMeet", "CLUSTER MEET failed: %v", err)
					return false, err
				}
				r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "ClusterMeet", "ClusterMeet", "Node %v met node %v", candidate.Address, target.Address)
				return true, nil
			}
			log.V(1).Info("add a new replica", "primary address", target.Address, "primary Id", target.Id, "replica address", candidate.Address)
			if err := candidate.Client.Do(ctx, candidate.Client.B().ClusterReplicate().NodeId(target.Id).Build()).Error(); err != nil {
				log.Error(err, "command failed: CLUSTER REPLICATE", "nodeId", target.Id)
				r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "ReplicaCreationFailed", "CreateReplica", "Failed to create replica: %v", err)
				return false, err
			}
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "ReplicaCreated", "CreateReplica", "Created replica for primary %v", target.Id)
			return true, nil
		}
	}

	return false, nil
}

// countReplicasForPrimary counts how many non-failed replicas the primary
// sees for itself in its own CLUSTER NODES output.
func countReplicasForPrimary(clusterNodes string, primaryId string) int {
	count := 0
	for line := range strings.SplitSeq(clusterNodes, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		flags := strings.Split(fields[2], ",")
		if slices.Contains(flags, "slave") && !slices.Contains(flags, "fail") && !slices.Contains(flags, "noaddr") {
			if fields[3] == primaryId {
				count++
			}
		}
	}
	return count
}

func countSlots(ranges []valkey.SlotsRange) int {
	count := 0
	for _, slot := range ranges {
		count += slot.End - slot.Start + 1
	}
	return count
}
