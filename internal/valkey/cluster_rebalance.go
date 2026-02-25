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

package valkey

import (
	"fmt"
	"sort"
)

// SlotMove describes a small, incremental slot migration between two primaries.
type SlotMove struct {
	Src   *NodeState
	Dst   *NodeState
	Slots []int
}

type primarySlots struct {
	node           *NodeState
	ranges         []SlotsRange
	numSlots       int
	targetNumSlots int
}

// PlanRebalanceMove computes a single, deterministic slot move to improve balance.
// It returns nil when the cluster is already balanced or not ready for rebalancing.
func PlanRebalanceMove(shards []*ShardState, expectedShards int, maxSlots int) (*SlotMove, error) {
	if expectedShards <= 0 || maxSlots <= 0 {
		return nil, nil
	}
	if len(shards) != expectedShards {
		return nil, nil
	}

	slotAllocations := make([]*primarySlots, 0, len(shards))
	for _, shard := range shards {
		primary := shard.GetPrimaryNode()
		if primary == nil {
			return nil, fmt.Errorf("primary missing for shard %s", shard.Id)
		}
		numSlots := 0
		for _, slot := range shard.Slots {
			numSlots += slot.End - slot.Start + 1
		}
		slotAllocations = append(slotAllocations, &primarySlots{
			node:     primary,
			ranges:   append([]SlotsRange(nil), shard.Slots...),
			numSlots: numSlots,
		})
	}
	// Sort by address for deterministic target assignment.
	sort.Slice(slotAllocations, func(i, j int) bool { return slotAllocations[i].node.Address < slotAllocations[j].node.Address })

	// Assign per-shard slot targets and find the first imbalance.
	numSlotsPerShard, rem := TotalSlots/expectedShards, TotalSlots%expectedShards
	var src, dst *primarySlots
	for idx, alloc := range slotAllocations {
		alloc.targetNumSlots = numSlotsPerShard
		if idx < rem {
			alloc.targetNumSlots++
		}
		// Tolerate ±1-slot rounding differences that arise because the initial
		// slot assignment order may not match the address-sorted target order.
		if src == nil && alloc.numSlots-alloc.targetNumSlots > 1 {
			src = alloc
		}
		if dst == nil && alloc.targetNumSlots-alloc.numSlots > 1 {
			dst = alloc
		}
	}
	if src == nil || dst == nil {
		return nil, nil
	}

	numSlotsToMove := min(src.numSlots-src.targetNumSlots, dst.targetNumSlots-dst.numSlots, maxSlots)
	slots := takeSlotsFromRanges(src.ranges, numSlotsToMove)
	return &SlotMove{Src: src.node, Dst: dst.node, Slots: slots}, nil
}

func takeSlotsFromRanges(ranges []SlotsRange, slotsToMove int) []int {
	if slotsToMove <= 0 {
		return nil
	}
	out := make([]int, 0, slotsToMove)
	for _, slotRange := range ranges {
		for slot := slotRange.Start; slot <= slotRange.End && len(out) < slotsToMove; slot++ {
			out = append(out, slot)
		}
		if len(out) == slotsToMove {
			break
		}
	}
	return out
}
