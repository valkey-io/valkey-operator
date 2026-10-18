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

	"github.com/valkey-io/valkey-operator/internal/valkey"
)

// meetTestNode builds a node whose own CLUSTER NODES view is the given line
// for itself, flagged myself.
func meetTestNode(id, addr, flags, primary, slots string) *valkey.NodeState {
	n := &valkey.NodeState{Id: id, Address: addr}
	n.SetClusterNodesForTesting(id + " " + addr + ":6379@16379 myself," + flags + " " + primary + " 0 0 1 connected " + slots + "\n")
	return n
}

func TestFindMeetTarget(t *testing.T) {
	// Node IDs name the node's role and shard: p1 and r1 are shard 1's primary
	// and replica, with a suffix for their state; iso-* are isolated nodes.
	isolatedA := &valkey.NodeState{Id: "iso-a", Address: "10.0.1.1", ClusterInfo: map[string]string{"cluster_known_nodes": "1"}}
	isolatedB := &valkey.NodeState{Id: "iso-b", Address: "10.0.1.2", ClusterInfo: map[string]string{"cluster_known_nodes": "1"}}
	isolated := []*valkey.NodeState{isolatedA, isolatedB}

	t.Run("prefers a live slot-owning primary over the shard's other members", func(t *testing.T) {
		r1 := meetTestNode("r1", "10.0.0.1", "slave", "p1", "")
		p1 := meetTestNode("p1", "10.0.0.2", "master", "-", "0-16383")
		state := &valkey.ClusterState{Shards: []*valkey.ShardState{
			{Id: "s1", PrimaryId: "p1", Nodes: []*valkey.NodeState{r1, p1}},
		}}
		got := findMeetTarget(state, isolated)
		require.NotNil(t, got)
		assert.Equal(t, "p1", got.Id)
	})

	t.Run("skips a primary that a peer reports failed", func(t *testing.T) {
		p1 := meetTestNode("p1-failed", "10.0.0.1", "master", "-", "0-8191")
		p2 := meetTestNode("p2", "10.0.0.2", "master", "-", "8192-16383")
		r2 := &valkey.NodeState{Id: "r2", Address: "10.0.0.3"}
		r2.SetClusterNodesForTesting("r2 10.0.0.3:6379@16379 myself,slave p2 0 0 1 connected\n" +
			"p1-failed 10.0.0.1:6379@16379 master,fail - 0 0 1 connected 0-8191\n")
		state := &valkey.ClusterState{Shards: []*valkey.ShardState{
			{Id: "s1", PrimaryId: "p1-failed", Nodes: []*valkey.NodeState{p1}},
			{Id: "s2", PrimaryId: "p2", Nodes: []*valkey.NodeState{p2, r2}},
		}}
		got := findMeetTarget(state, isolated)
		require.NotNil(t, got)
		assert.Equal(t, "p2", got.Id)
	})

	t.Run("uses the only primary when one replica reports it fail?", func(t *testing.T) {
		// A replica cut off from the bus marks every peer fail?. That alone
		// must not rule out the primary that is serving every slot.
		p1 := meetTestNode("p1", "10.0.0.1", "master", "-", "0-16383")
		r1 := &valkey.NodeState{Id: "r1", Address: "10.0.0.2"}
		r1.SetClusterNodesForTesting("r1 10.0.0.2:6379@16379 myself,slave p1 0 0 1 connected\n" +
			"p1 10.0.0.1:6379@16379 master,fail? - 0 0 1 connected 0-16383\n")
		state := &valkey.ClusterState{Shards: []*valkey.ShardState{
			{Id: "s1", PrimaryId: "p1", Slots: []valkey.SlotsRange{{Start: 0, End: 16383}}, Nodes: []*valkey.NodeState{r1, p1}},
		}}
		got := findMeetTarget(state, isolated)
		require.NotNil(t, got)
		assert.Equal(t, "p1", got.Id)
	})

	t.Run("uses a replica when every primary is down", func(t *testing.T) {
		// All primaries were deleted at once: each shard has only its replica
		// left, and the recreated primaries are isolated. They must join the
		// existing cluster, not form a new one around an isolated seed.
		r2 := meetTestNode("r2", "10.0.0.2", "slave", "p2-dead", "")
		r1 := meetTestNode("r1", "10.0.0.1", "slave", "p1-dead", "")
		state := &valkey.ClusterState{
			Shards: []*valkey.ShardState{
				{Id: "s1", PrimaryId: "p1-dead", Nodes: []*valkey.NodeState{r1}},
				{Id: "s2", PrimaryId: "p2-dead", Nodes: []*valkey.NodeState{r2}},
			},
			PendingNodes: isolated,
		}
		got := findMeetTarget(state, isolated)
		require.NotNil(t, got)
		assert.Equal(t, "r1", got.Id)
	})

	t.Run("prefers a replica that no peer reports failed", func(t *testing.T) {
		// r1a comes first but its shard peer reports it failed.
		r1a := meetTestNode("r1a-failed", "10.0.0.1", "slave", "p1-dead", "")
		r1b := meetTestNode("r1b", "10.0.0.2", "slave", "p1-dead", "")
		r1b.SetClusterNodesForTesting("r1b 10.0.0.2:6379@16379 myself,slave p1-dead 0 0 1 connected\n" +
			"r1a-failed 10.0.0.1:6379@16379 slave,fail p1-dead 0 0 1 connected\n")
		state := &valkey.ClusterState{
			Shards:       []*valkey.ShardState{{Id: "s1", Nodes: []*valkey.NodeState{r1a, r1b}}},
			PendingNodes: isolated,
		}
		got := findMeetTarget(state, isolated)
		require.NotNil(t, got)
		assert.Equal(t, "r1b", got.Id)
	})

	t.Run("uses a shard member that a peer reports failed rather than an isolated seed", func(t *testing.T) {
		// Each replica reports the other failed, but both still belong to the
		// existing cluster.
		r1a := meetTestNode("r1a", "10.0.0.1", "slave", "p1-dead", "")
		r1a.SetClusterNodesForTesting("r1a 10.0.0.1:6379@16379 myself,slave p1-dead 0 0 1 connected\n" +
			"r1b 10.0.0.2:6379@16379 slave,fail p1-dead 0 0 1 connected\n")
		r1b := meetTestNode("r1b", "10.0.0.2", "slave", "p1-dead", "")
		r1b.SetClusterNodesForTesting("r1b 10.0.0.2:6379@16379 myself,slave p1-dead 0 0 1 connected\n" +
			"r1a 10.0.0.1:6379@16379 slave,fail p1-dead 0 0 1 connected\n")
		state := &valkey.ClusterState{
			Shards:       []*valkey.ShardState{{Id: "s1", Nodes: []*valkey.NodeState{r1b, r1a}}},
			PendingNodes: isolated,
		}
		got := findMeetTarget(state, isolated)
		require.NotNil(t, got)
		assert.Equal(t, "r1b", got.Id)
	})

	t.Run("prefers a slot-owning primary over a slotless one in an earlier shard", func(t *testing.T) {
		p1 := meetTestNode("p1-slotless", "10.0.0.1", "master", "-", "")
		p2 := meetTestNode("p2", "10.0.0.2", "master", "-", "0-16383")
		state := &valkey.ClusterState{Shards: []*valkey.ShardState{
			{Id: "s1", PrimaryId: "p1-slotless", Nodes: []*valkey.NodeState{p1}},
			{Id: "s2", PrimaryId: "p2", Nodes: []*valkey.NodeState{p2}},
		}}
		got := findMeetTarget(state, isolated)
		require.NotNil(t, got)
		assert.Equal(t, "p2", got.Id)
	})

	t.Run("uses a drained primary that owns no slots", func(t *testing.T) {
		p1 := meetTestNode("p1-drained", "10.0.0.1", "master", "-", "")
		state := &valkey.ClusterState{Shards: []*valkey.ShardState{
			{Id: "s1", PrimaryId: "p1-drained", Nodes: []*valkey.NodeState{p1}},
		}}
		got := findMeetTarget(state, isolated)
		require.NotNil(t, got)
		assert.Equal(t, "p1-drained", got.Id)
	})

	t.Run("falls back to a non-isolated pending node", func(t *testing.T) {
		pending := &valkey.NodeState{Id: "pending", Address: "10.0.0.5", ClusterInfo: map[string]string{"cluster_known_nodes": "3"}}
		state := &valkey.ClusterState{PendingNodes: []*valkey.NodeState{isolatedA, pending}}
		got := findMeetTarget(state, []*valkey.NodeState{isolatedA})
		require.NotNil(t, got)
		assert.Equal(t, "pending", got.Id)
	})

	t.Run("seeds from the first isolated node when no cluster exists yet", func(t *testing.T) {
		state := &valkey.ClusterState{PendingNodes: isolated}
		got := findMeetTarget(state, isolated)
		require.NotNil(t, got)
		assert.Equal(t, "iso-a", got.Id)
	})
}
