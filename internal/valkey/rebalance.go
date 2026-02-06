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

const TotalSlots = 16384

// SlotMove describes a small, incremental slot migration between two primaries.
type SlotMove struct {
	Src   *NodeState
	Dst   *NodeState
	Slots []int
}

type primarySlots struct {
	node   *NodeState
	ranges []SlotsRange
	count  int
	target int
}

// BuildRebalanceMove computes a single, deterministic slot move to improve balance.
// It returns nil when the cluster is already balanced or not ready for rebalancing.
func BuildRebalanceMove(state *ClusterState, expectedShards int, maxSlots int) (*SlotMove, error) {
	if state == nil || expectedShards <= 0 || maxSlots <= 0 {
		return nil, nil
	}
	if len(state.Shards) < expectedShards {
		// Defer rebalancing until enough primaries are present.
		return nil, nil
	}

	primaries := make([]*primarySlots, 0, expectedShards)
	nonEmpty := make([]*primarySlots, 0, expectedShards)
	empty := make([]*primarySlots, 0, expectedShards)
	for _, shard := range state.Shards {
		primary := shard.GetPrimaryNode()
		if primary == nil {
			return nil, fmt.Errorf("primary missing for shard %s", shard.Id)
		}
		count := 0
		for _, slot := range shard.Slots {
			count += slot.End - slot.Start + 1
		}
		primaries = append(primaries, &primarySlots{
			node:   primary,
			ranges: append([]SlotsRange(nil), shard.Slots...),
			count:  count,
		})
		if count == 0 {
			empty = append(empty, primaries[len(primaries)-1])
		} else {
			nonEmpty = append(nonEmpty, primaries[len(primaries)-1])
		}
	}
	if len(nonEmpty) > expectedShards {
		return nil, fmt.Errorf("too many slot-bearing primaries (%d) for expected shards %d", len(nonEmpty), expectedShards)
	}

	sort.Slice(nonEmpty, func(i, j int) bool {
		return nonEmpty[i].node.Address < nonEmpty[j].node.Address
	})
	sort.Slice(empty, func(i, j int) bool {
		return empty[i].node.Address < empty[j].node.Address
	})

	neededEmpty := expectedShards - len(nonEmpty)
	if neededEmpty > len(empty) {
		return nil, nil
	}
	primaries = append(primaries, nonEmpty...)
	primaries = append(primaries, empty[:neededEmpty]...)

	sort.Slice(primaries, func(i, j int) bool {
		return primaries[i].node.Address < primaries[j].node.Address
	})

	base := TotalSlots / expectedShards
	remaining := TotalSlots % expectedShards
	for i, primary := range primaries {
		primary.target = base
		if i < remaining {
			primary.target++
		}
	}

	var src *primarySlots
	var dst *primarySlots
	for _, primary := range primaries {
		if primary.count > primary.target && src == nil {
			src = primary
		}
		if primary.count < primary.target && dst == nil {
			dst = primary
		}
		if src != nil && dst != nil {
			break
		}
	}

	if src == nil || dst == nil {
		return nil, nil
	}

	moveCount := src.count - src.target
	if dstNeed := dst.target - dst.count; dstNeed < moveCount {
		moveCount = dstNeed
	}
	if moveCount > maxSlots {
		moveCount = maxSlots
	}

	slots, err := takeSlotsFromRanges(src.ranges, moveCount)
	if err != nil {
		return nil, err
	}

	return &SlotMove{
		Src:   src.node,
		Dst:   dst.node,
		Slots: slots,
	}, nil
}

func takeSlotsFromRanges(ranges []SlotsRange, count int) ([]int, error) {
	if count <= 0 {
		return nil, nil
	}
	out := make([]int, 0, count)
	for _, slotRange := range ranges {
		for slot := slotRange.Start; slot <= slotRange.End && len(out) < count; slot++ {
			out = append(out, slot)
		}
		if len(out) == count {
			break
		}
	}
	if len(out) != count {
		return nil, fmt.Errorf("insufficient slots available to move: requested %d, got %d", count, len(out))
	}
	return out, nil
}
