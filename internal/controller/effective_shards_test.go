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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

// labeledNode is a ValkeyNode at (shard, node) with the given pod IP.
func labeledNode(shard, node, ip string) valkeyiov1alpha1.ValkeyNode {
	return valkeyiov1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "c-" + shard + "-" + node,
			Labels: map[string]string{LabelCluster: "c", LabelShardIndex: shard, LabelNodeIndex: node},
		},
		Status: valkeyiov1alpha1.ValkeyNodeStatus{PodIP: ip},
	}
}

func TestEffectiveShards(t *testing.T) {
	nodes := &valkeyiov1alpha1.ValkeyNodeList{Items: []valkeyiov1alpha1.ValkeyNode{
		labeledNode("0", "0", "10.0.0.1"), labeledNode("0", "1", "10.0.0.2"),
		labeledNode("1", "0", "10.0.1.1"), labeledNode("1", "1", "10.0.1.2"),
		labeledNode("2", "0", "10.0.2.1"), labeledNode("2", "1", "10.0.2.2"),
	}}
	full := &valkey.ShardState{Id: "shard-0", PrimaryId: "p0", Slots: []valkey.SlotsRange{{Start: 0, End: 16383}},
		Nodes: []*valkey.NodeState{
			{Id: "p0", Address: "10.0.0.1", ShardId: "shard-0", Flags: []string{"master"}},
			{Id: "r0", Address: "10.0.0.2", ShardId: "shard-0", Flags: []string{"slave"}},
		}}
	// Shard 1's primary was drained to zero slots and its replica still
	// follows it: the scrape files the primary under PendingNodes and the
	// replica as a shard without a primary.
	replicaOnly := &valkey.ShardState{Id: "shard-1", Nodes: []*valkey.NodeState{
		{Id: "r1", Address: "10.0.1.2", ShardId: "shard-1", Flags: []string{"slave"}},
	}}
	drained := &valkey.NodeState{Id: "p1", Address: "10.0.1.1", ShardId: "shard-1", Flags: []string{"master"}}

	t.Run("a drained primary rejoins the shard its replica keeps", func(t *testing.T) {
		state := &valkey.ClusterState{Shards: []*valkey.ShardState{full, replicaOnly}, PendingNodes: []*valkey.NodeState{drained}}
		shards := effectiveShards(state, nodes)
		require.Len(t, shards, 2, "the empty shard counts once")
		assert.Equal(t, "p1", shards[1].PrimaryId)
		assert.Same(t, drained, shards[1].GetPrimaryNode())
		assert.Len(t, shards[1].Nodes, 2)
		assert.Empty(t, shards[1].Slots)
		assert.Empty(t, replicaOnly.PrimaryId, "the scraped state is left alone")
		assert.Len(t, replicaOnly.Nodes, 1)

		// With the right shard count the rebalancer moves slots onto it.
		move, err := valkey.PlanRebalanceMove(shards, 2, 400)
		require.NoError(t, err)
		require.NotNil(t, move)
		assert.Equal(t, "p0", move.Src.Id)
		assert.Equal(t, "p1", move.Dst.Id)
		assert.Len(t, move.Slots, 400)
	})

	t.Run("a drained primary without a replica forms a new shard", func(t *testing.T) {
		alone := &valkey.NodeState{Id: "p1", Address: "10.0.1.1", ShardId: "shard-1", Flags: []string{"master"}}
		state := &valkey.ClusterState{Shards: []*valkey.ShardState{full}, PendingNodes: []*valkey.NodeState{alone}}
		shards := effectiveShards(state, nodes)
		require.Len(t, shards, 2)
		assert.Same(t, alone, shards[1].GetPrimaryNode())
		move, err := valkey.PlanRebalanceMove(shards, 2, 400)
		require.NoError(t, err)
		require.NotNil(t, move)
		assert.Equal(t, "p1", move.Dst.Id)
	})

	t.Run("a shard with a primary keeps it when a stray shares its shard id", func(t *testing.T) {
		// A slot-less primary that reports the shard id of a shard with a
		// slot-bearing primary must not take that shard's slots over in the
		// plan. It is listed on its own, as before, and the count mismatch
		// holds the rebalancer until Phase 3 has attached it as a replica.
		stray := &valkey.NodeState{Id: "s0", Address: "10.0.2.1", ShardId: "shard-0", Flags: []string{"master"}}
		state := &valkey.ClusterState{Shards: []*valkey.ShardState{full}, PendingNodes: []*valkey.NodeState{stray}}
		shards := effectiveShards(state, nodes)
		require.Len(t, shards, 2)
		assert.Equal(t, "p0", shards[0].PrimaryId)
		assert.Len(t, shards[0].Nodes, 2)
		assert.Equal(t, "s0", shards[1].PrimaryId)
		move, err := valkey.PlanRebalanceMove(shards, 1, 400)
		require.NoError(t, err)
		assert.Nil(t, move)
	})

	t.Run("a scale-out leader forms a new shard and its follower does not", func(t *testing.T) {
		leader := &valkey.NodeState{Id: "p2", Address: "10.0.2.1", ShardId: "shard-2", Flags: []string{"master"}}
		follower := &valkey.NodeState{Id: "f2", Address: "10.0.2.2", ShardId: "shard-2b", Flags: []string{"master"}}
		state := &valkey.ClusterState{Shards: []*valkey.ShardState{full}, PendingNodes: []*valkey.NodeState{leader, follower}}
		shards := effectiveShards(state, nodes)
		require.Len(t, shards, 2)
		assert.Equal(t, "p2", shards[1].PrimaryId)
		assert.Len(t, shards[1].Nodes, 1)
	})
}
