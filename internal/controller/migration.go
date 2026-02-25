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
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
	"valkey.io/valkey-operator/internal/valkey"
)

const (
	rebalanceSlotBatchSize    = 400
	rebalanceKeysBatchSize    = 100
	rebalanceMigrateTimeoutMs = 5000
)

func (r *ValkeyClusterReconciler) rebalanceSlots(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, shards []*valkey.ShardState) (bool, error) {
	move, err := valkey.PlanRebalanceMove(shards, int(cluster.Spec.Shards), rebalanceSlotBatchSize)
	if err != nil {
		return false, err
	}
	if move == nil {
		return false, nil
	}

	log := logf.FromContext(ctx)
	inProgress, err := slotMigrationInProgress(ctx, move.Src)
	if err != nil {
		return false, err
	}
	if inProgress {
		log.V(1).Info("slot migration already in progress", "src", move.Src.Address)
		return true, nil
	}

	known, err := nodeKnownToSource(ctx, move.Src, move.Dst)
	if err != nil {
		return false, err
	}
	if !known {
		log.V(1).Info("destination not yet visible to source via gossip; will retry", "src", move.Src.Address, "dst", move.Dst.Address, "dstId", move.Dst.Id)
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "SlotsRebalancePending", "RebalanceSlots", "Waiting for %s to learn node %s", move.Src.Address, move.Dst.Address)
		return true, nil
	}

	log.V(1).Info("rebalancing slots", "src", move.Src.Address, "dst", move.Dst.Address, "slots", len(move.Slots))
	r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "SlotsRebalancing", "RebalanceSlots", "Moving %d slots from %s to %s", len(move.Slots), move.Src.Address, move.Dst.Address)

	ranges := slotsToRanges(move.Slots)
	if err := migrateSlotsAtomic(ctx, move.Src, move.Dst, ranges); err != nil {
		if isSlotsNotServedByNode(err) {
			log.V(1).Info("slots no longer served by source; will retry with fresh state", "src", move.Src.Address, "dst", move.Dst.Address)
			return true, nil
		}
		if !isAtomicMigrationUnsupported(err) {
			return false, err
		}
		log.V(1).Info("atomic slot migration unsupported; falling back to legacy migration", "src", move.Src.Address, "dst", move.Dst.Address)
		for _, slot := range move.Slots {
			if err := migrateSlotLegacy(ctx, move.Src, move.Dst, slot); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

func slotMigrationInProgress(ctx context.Context, src *valkey.NodeState) (bool, error) {
	log := logf.FromContext(ctx)
	cmd := src.Client.B().Arbitrary("CLUSTER", "GETSLOTMIGRATIONS").Build()
	migrations, err := src.Client.Do(ctx, cmd).ToArray()
	if err != nil {
		if isAtomicMigrationUnsupported(err) {
			return false, nil
		}
		return false, fmt.Errorf("getslotmigrations failed on %s: %w", src.Address, err)
	}
	for _, migration := range migrations {
		values, parseErr := migration.AsStrMap()
		if parseErr != nil {
			log.V(1).Info("unable to parse slot migration entry; treating as in progress", "src", src.Address, "error", parseErr)
			return true, nil
		}
		state := strings.ToLower(values["state"])
		if !isSlotMigrationTerminal(state) {
			return true, nil
		}
	}
	return false, nil
}

func isSlotMigrationTerminal(state string) bool {
	switch state {
	case "success", "failed", "canceled", "cancelled":
		return true
	default:
		return false
	}
}

func nodeKnownToSource(ctx context.Context, src *valkey.NodeState, dst *valkey.NodeState) (bool, error) {
	if dst.Id == "" {
		return false, nil
	}
	cmd := src.Client.B().ClusterNodes().Build()
	nodes, err := src.Client.Do(ctx, cmd).ToString()
	if err != nil {
		return false, fmt.Errorf("cluster nodes failed on %s: %w", src.Address, err)
	}
	return strings.Contains(nodes, dst.Id), nil
}

func migrateSlotsAtomic(ctx context.Context, src *valkey.NodeState, dst *valkey.NodeState, ranges []valkey.SlotsRange) error {
	cmd := src.Client.B().Arbitrary("CLUSTER", "MIGRATESLOTS")
	for _, slotRange := range ranges {
		cmd = cmd.Args(
			"SLOTSRANGE",
			strconv.Itoa(slotRange.Start),
			strconv.Itoa(slotRange.End),
			"NODE",
			dst.Id,
		)
	}
	if err := src.Client.Do(ctx, cmd.Build()).Error(); err != nil {
		return fmt.Errorf("migrateslots failed from %s to %s: %w", src.Address, dst.Address, err)
	}
	return nil
}

func migrateSlotLegacy(ctx context.Context, src *valkey.NodeState, dst *valkey.NodeState, slot int) error {
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

func slotsToRanges(slots []int) []valkey.SlotsRange {
	if len(slots) == 0 {
		return nil
	}
	ordered := append([]int(nil), slots...)
	sort.Ints(ordered)
	ranges := make([]valkey.SlotsRange, 0, len(ordered))
	start := ordered[0]
	prev := ordered[0]
	for _, slot := range ordered[1:] {
		if slot == prev+1 {
			prev = slot
			continue
		}
		ranges = append(ranges, valkey.SlotsRange{Start: start, End: prev})
		start = slot
		prev = slot
	}
	ranges = append(ranges, valkey.SlotsRange{Start: start, End: prev})
	return ranges
}

func isAtomicMigrationUnsupported(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unknown command") ||
		strings.Contains(msg, "unknown subcommand") ||
		strings.Contains(msg, "syntax error") ||
		strings.Contains(msg, "wrong number of arguments") ||
		strings.Contains(msg, "not supported")
}

func isSlotsNotServedByNode(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "slots are not served by this node")
}
