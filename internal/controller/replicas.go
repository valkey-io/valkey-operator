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
	"sort"

	corev1 "k8s.io/api/core/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
	"valkey.io/valkey-operator/internal/valkey"
)

func (r *ValkeyClusterReconciler) attachReplicaFromEmptyShard(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState) (bool, error) {
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
		if len(shard.Nodes) < (1 + replicasRequired) {
			needsReplica = append(needsReplica, primary)
		}
	}

	if len(needsReplica) == 0 || len(emptyMasters) == 0 {
		return false, nil
	}

	sort.Slice(needsReplica, func(i, j int) bool {
		return needsReplica[i].Address < needsReplica[j].Address
	})
	sort.Slice(emptyMasters, func(i, j int) bool {
		return emptyMasters[i].Address < emptyMasters[j].Address
	})

	log := logf.FromContext(ctx)
	for _, candidate := range emptyMasters {
		for _, target := range needsReplica {
			if candidate.Id == target.Id {
				continue
			}
			log.V(1).Info("add a new replica", "primary address", target.Address, "primary Id", target.Id, "replica address", candidate.Address)
			if err := candidate.Client.Do(ctx, candidate.Client.B().ClusterReplicate().NodeId(target.Id).Build()).Error(); err != nil {
				log.Error(err, "command failed: CLUSTER REPLICATE", "nodeId", target.Id)
				r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "ReplicaCreationFailed", "Failed to create replica: %v", err)
				return false, err
			}
			r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "ReplicaCreated", "Created replica for primary %v", target.Id)
			return true, nil
		}
	}

	return false, nil
}

func countSlots(ranges []valkey.SlotsRange) int {
	count := 0
	for _, slot := range ranges {
		count += slot.End - slot.Start + 1
	}
	return count
}
