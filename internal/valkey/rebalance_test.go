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
	"reflect"
	"testing"
)

func TestBuildRebalanceMove_ScaleOut(t *testing.T) {
	state := &ClusterState{
		Shards: []*ShardState{
			newPrimaryShard("10.0.0.1", "node-1", []SlotsRange{{Start: 0, End: 8191}}),
			newPrimaryShard("10.0.0.2", "node-2", []SlotsRange{{Start: 8192, End: 16383}}),
			newPrimaryShard("10.0.0.3", "node-3", nil),
		},
	}

	move, err := BuildRebalanceMove(state, 3, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if move == nil {
		t.Fatalf("expected move, got nil")
	}
	if move.Src.Address != "10.0.0.1" || move.Dst.Address != "10.0.0.3" {
		t.Fatalf("unexpected src/dst: %s -> %s", move.Src.Address, move.Dst.Address)
	}
	if len(move.Slots) != 20 {
		t.Fatalf("expected 20 slots, got %d", len(move.Slots))
	}
	for i, slot := range move.Slots {
		if slot != i {
			t.Fatalf("unexpected slot %d at index %d", slot, i)
		}
	}
}

func TestBuildRebalanceMove_Balanced(t *testing.T) {
	state := &ClusterState{
		Shards: []*ShardState{
			newPrimaryShard("10.0.0.1", "node-1", []SlotsRange{{Start: 0, End: 8191}}),
			newPrimaryShard("10.0.0.2", "node-2", []SlotsRange{{Start: 8192, End: 16383}}),
		},
	}

	move, err := BuildRebalanceMove(state, 2, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if move != nil {
		t.Fatalf("expected nil move for balanced cluster, got %+v", move)
	}
}

func TestBuildRebalanceMove_MismatchShards(t *testing.T) {
	state := &ClusterState{
		Shards: []*ShardState{
			newPrimaryShard("10.0.0.1", "node-1", []SlotsRange{{Start: 0, End: 8191}}),
			newPrimaryShard("10.0.0.2", "node-2", []SlotsRange{{Start: 8192, End: 16383}}),
		},
	}

	move, err := BuildRebalanceMove(state, 3, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if move != nil {
		t.Fatalf("expected nil move when shard count mismatches, got %+v", move)
	}
}

func TestBuildRebalanceMove_ExtraEmptyShards(t *testing.T) {
	state := &ClusterState{
		Shards: []*ShardState{
			newPrimaryShard("10.0.0.1", "node-1", []SlotsRange{{Start: 0, End: 8191}}),
			newPrimaryShard("10.0.0.2", "node-2", []SlotsRange{{Start: 8192, End: 16383}}),
			newPrimaryShard("10.0.0.3", "node-3", nil),
			newPrimaryShard("10.0.0.4", "node-4", nil),
		},
	}

	move, err := BuildRebalanceMove(state, 3, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if move == nil {
		t.Fatalf("expected move with extra empty shards, got nil")
	}
	if move.Dst.Address != "10.0.0.3" {
		t.Fatalf("unexpected destination: %s", move.Dst.Address)
	}
}

func TestBuildRebalanceMove_ZeroMaxSlots(t *testing.T) {
	state := &ClusterState{
		Shards: []*ShardState{
			newPrimaryShard("10.0.0.1", "node-1", []SlotsRange{{Start: 0, End: 8191}}),
			newPrimaryShard("10.0.0.2", "node-2", []SlotsRange{{Start: 8192, End: 16383}}),
		},
	}

	move, err := BuildRebalanceMove(state, 2, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if move != nil {
		t.Fatalf("expected nil move with maxSlots=0, got %+v", move)
	}
}

func TestTakeSlotsFromRanges(t *testing.T) {
	ranges := []SlotsRange{
		{Start: 0, End: 1},
		{Start: 10, End: 12},
	}
	slots, err := takeSlotsFromRanges(ranges, 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []int{0, 1, 10, 11}
	if !reflect.DeepEqual(slots, expected) {
		t.Fatalf("expected %v, got %v", expected, slots)
	}
}

func newPrimaryShard(address, nodeID string, slots []SlotsRange) *ShardState {
	node := &NodeState{
		Address: address,
		Id:      nodeID,
		Flags:   []string{"master"},
	}
	return &ShardState{
		Id:        "shard-" + nodeID,
		PrimaryId: nodeID,
		Slots:     slots,
		Nodes:     []*NodeState{node},
	}
}
