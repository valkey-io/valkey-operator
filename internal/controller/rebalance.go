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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
	"valkey.io/valkey-operator/internal/valkey"
)

const (
	rebalanceSlotBatchSize    = 20
	rebalanceKeysBatchSize    = 100
	rebalanceMigrateTimeoutMs = 5000
)

func (r *ValkeyClusterReconciler) rebalanceSlots(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState) (bool, error) {
	if state == nil {
		return false, nil
	}

	move, err := valkey.BuildRebalanceMove(state, int(cluster.Spec.Shards), rebalanceSlotBatchSize)
	if err != nil {
		return false, err
	}
	if move == nil || len(move.Slots) == 0 {
		return false, nil
	}

	log := logf.FromContext(ctx)
	log.V(1).Info("rebalancing slots", "src", move.Src.Address, "dst", move.Dst.Address, "slots", len(move.Slots))
	r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "SlotsRebalancing", "Moving %d slots from %s to %s", len(move.Slots), move.Src.Address, move.Dst.Address)

	for _, slot := range move.Slots {
		if err := migrateSlot(ctx, move.Src, move.Dst, slot); err != nil {
			return false, err
		}
	}
	return true, nil
}

func migrateSlot(ctx context.Context, src *valkey.NodeState, dst *valkey.NodeState, slot int) error {
	if src == nil || dst == nil {
		return fmt.Errorf("missing source or destination node")
	}

	if err := src.Client.Do(ctx, src.Client.B().ClusterSetslot().Slot(int64(slot)).Migrating().NodeId(dst.Id).Build()).Error(); err != nil {
		return fmt.Errorf("setslot migrating failed for slot %d on %s: %w", slot, src.Address, err)
	}
	if err := dst.Client.Do(ctx, dst.Client.B().ClusterSetslot().Slot(int64(slot)).Importing().NodeId(src.Id).Build()).Error(); err != nil {
		return fmt.Errorf("setslot importing failed for slot %d on %s: %w", slot, dst.Address, err)
	}

	for {
		keys, err := src.Client.Do(ctx, src.Client.B().ClusterGetkeysinslot().Slot(int64(slot)).Count(int64(rebalanceKeysBatchSize)).Build()).AsStrSlice()
		if err != nil {
			return fmt.Errorf("getkeysinslot failed for slot %d on %s: %w", slot, src.Address, err)
		}
		if len(keys) == 0 {
			break
		}
		for _, key := range keys {
			if err := src.Client.Do(ctx, src.Client.B().Migrate().Host(dst.Address).Port(int64(dst.Port)).Key(key).DestinationDb(0).Timeout(rebalanceMigrateTimeoutMs).Replace().Build()).Error(); err != nil {
				return fmt.Errorf("migrate failed for slot %d key %s from %s to %s: %w", slot, key, src.Address, dst.Address, err)
			}
		}
	}

	if err := dst.Client.Do(ctx, dst.Client.B().ClusterSetslot().Slot(int64(slot)).Node().NodeId(dst.Id).Build()).Error(); err != nil {
		return fmt.Errorf("setslot node failed for slot %d on %s: %w", slot, dst.Address, err)
	}
	if err := src.Client.Do(ctx, src.Client.B().ClusterSetslot().Slot(int64(slot)).Node().NodeId(dst.Id).Build()).Error(); err != nil {
		return fmt.Errorf("setslot node failed for slot %d on %s: %w", slot, src.Address, err)
	}
	return nil
}
