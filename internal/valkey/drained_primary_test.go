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

package valkey

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// replicaOf returns a replica node whose own CLUSTER NODES line names primary.
func replicaOf(id, address, primary, shardID string) *NodeState {
	n := &NodeState{Id: id, Address: address, ShardId: shardID, Flags: []string{"slave"}}
	n.SetClusterNodesForTesting(id + " " + address + ":6379@16379 myself,slave " + primary + " 0 0 1 connected\n")
	return n
}

func TestAdoptDrainedPrimaries(t *testing.T) {
	full := &ShardState{Id: "s0", PrimaryId: "p0", Slots: []SlotsRange{{Start: 0, End: 16383}},
		Nodes: []*NodeState{{Id: "p0", Address: "10.0.0.1", Flags: []string{"master"}}, replicaOf("r0", "10.0.0.2", "p0", "s0")}}

	t.Run("a drained primary joins the shard of the replica that follows it", func(t *testing.T) {
		drained := &NodeState{Id: "p1", Address: "10.0.1.1", ShardId: "s1", Flags: []string{"master"}}
		state := &ClusterState{
			Shards:       []*ShardState{full, {Id: "s1", Nodes: []*NodeState{replicaOf("r1", "10.0.1.2", "p1", "s1")}}},
			PendingNodes: []*NodeState{drained},
		}
		state.adoptDrainedPrimaries()
		assert.Empty(t, state.PendingNodes)
		require.Len(t, state.Shards, 2)
		assert.Same(t, drained, state.Shards[1].GetPrimaryNode())
		assert.Empty(t, state.Shards[1].Slots)

		move, err := PlanRebalanceMove(state.Shards, 2, 400)
		require.NoError(t, err)
		require.NotNil(t, move)
		assert.Equal(t, "p1", move.Dst.Id)
	})

	t.Run("a slot-less primary that no replica follows stays pending", func(t *testing.T) {
		leader := &NodeState{Id: "p2", Address: "10.0.2.1", ShardId: "s2", Flags: []string{"master"}}
		state := &ClusterState{Shards: []*ShardState{full}, PendingNodes: []*NodeState{leader}}
		state.adoptDrainedPrimaries()
		assert.Equal(t, []*NodeState{leader}, state.PendingNodes)
		assert.Len(t, state.Shards, 1)
	})

	t.Run("a stray beside a live primary stays pending and the shard keeps its primary", func(t *testing.T) {
		stray := &NodeState{Id: "s", Address: "10.0.3.1", ShardId: "s0", Flags: []string{"master"}}
		state := &ClusterState{Shards: []*ShardState{full}, PendingNodes: []*NodeState{stray}}
		state.adoptDrainedPrimaries()
		assert.Equal(t, []*NodeState{stray}, state.PendingNodes)
		assert.Equal(t, "p0", state.Shards[0].PrimaryId)
	})
}
