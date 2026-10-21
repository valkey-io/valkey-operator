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
	isolatedA := &valkey.NodeState{Id: "isoa", Address: "10.0.1.1", ClusterInfo: map[string]string{"cluster_known_nodes": "1"}}
	isolatedB := &valkey.NodeState{Id: "isob", Address: "10.0.1.2", ClusterInfo: map[string]string{"cluster_known_nodes": "1"}}
	isolated := []*valkey.NodeState{isolatedA, isolatedB}

	t.Run("prefers a live slot-owning primary over the shard's other members", func(t *testing.T) {
		r := meetTestNode("11111111", "10.0.0.1", "slave", "22222222", "")
		p := meetTestNode("22222222", "10.0.0.2", "master", "-", "0-16383")
		state := &valkey.ClusterState{Shards: []*valkey.ShardState{
			{Id: "s1", PrimaryId: "22222222", Nodes: []*valkey.NodeState{r, p}},
		}}
		got := findMeetTarget(state, isolated)
		require.NotNil(t, got)
		assert.Equal(t, "22222222", got.Id)
	})

	t.Run("skips a primary that a peer reports failed", func(t *testing.T) {
		dying := meetTestNode("aaaa", "10.0.0.1", "master", "-", "0-8191")
		live := meetTestNode("bbbb", "10.0.0.2", "master", "-", "8192-16383")
		peer := &valkey.NodeState{Id: "zzzz", Address: "10.0.0.9"}
		peer.SetClusterNodesForTesting("zzzz 10.0.0.9:6379@16379 myself,slave bbbb 0 0 1 connected\n" +
			"aaaa 10.0.0.1:6379@16379 master,fail - 0 0 1 connected 0-8191\n")
		state := &valkey.ClusterState{Shards: []*valkey.ShardState{
			{Id: "s1", PrimaryId: "aaaa", Nodes: []*valkey.NodeState{dying}},
			{Id: "s2", PrimaryId: "bbbb", Nodes: []*valkey.NodeState{live, peer}},
		}}
		got := findMeetTarget(state, isolated)
		require.NotNil(t, got)
		assert.Equal(t, "bbbb", got.Id)
	})

	t.Run("uses the only primary when one replica reports it fail?", func(t *testing.T) {
		// A replica cut off from the bus marks every peer fail?. That alone
		// must not rule out the primary that is serving every slot.
		p := meetTestNode("pppp", "10.0.0.1", "master", "-", "0-16383")
		r := &valkey.NodeState{Id: "rrrr", Address: "10.0.0.2"}
		r.SetClusterNodesForTesting("rrrr 10.0.0.2:6379@16379 myself,slave pppp 0 0 1 connected\n" +
			"pppp 10.0.0.1:6379@16379 master,fail? - 0 0 1 connected 0-16383\n")
		state := &valkey.ClusterState{Shards: []*valkey.ShardState{
			{Id: "s1", PrimaryId: "pppp", Slots: []valkey.SlotsRange{{Start: 0, End: 16383}}, Nodes: []*valkey.NodeState{r, p}},
		}}
		got := findMeetTarget(state, isolated)
		require.NotNil(t, got)
		assert.Equal(t, "pppp", got.Id)
	})

	t.Run("uses a replica when every primary is down", func(t *testing.T) {
		// All primaries were deleted at once: each shard has only its replica
		// left, and the recreated primaries are isolated. They must join the
		// existing cluster, not form a new one around an isolated seed.
		r2 := meetTestNode("rrrr2222", "10.0.0.2", "slave", "deadp2", "")
		r1 := meetTestNode("rrrr1111", "10.0.0.1", "slave", "deadp1", "")
		state := &valkey.ClusterState{
			Shards: []*valkey.ShardState{
				{Id: "s1", PrimaryId: "deadp1", Nodes: []*valkey.NodeState{r1}},
				{Id: "s2", PrimaryId: "deadp2", Nodes: []*valkey.NodeState{r2}},
			},
			PendingNodes: isolated,
		}
		got := findMeetTarget(state, isolated)
		require.NotNil(t, got)
		assert.Equal(t, "rrrr1111", got.Id)
	})

	t.Run("prefers a replica that no peer reports failed", func(t *testing.T) {
		// aaaa comes first but its shard peer reports it failed.
		failed := meetTestNode("aaaa", "10.0.0.1", "slave", "deadp1", "")
		healthy := meetTestNode("bbbb", "10.0.0.2", "slave", "deadp1", "")
		healthy.SetClusterNodesForTesting("bbbb 10.0.0.2:6379@16379 myself,slave deadp1 0 0 1 connected\n" +
			"aaaa 10.0.0.1:6379@16379 slave,fail deadp1 0 0 1 connected\n")
		state := &valkey.ClusterState{
			Shards:       []*valkey.ShardState{{Id: "s1", Nodes: []*valkey.NodeState{failed, healthy}}},
			PendingNodes: isolated,
		}
		got := findMeetTarget(state, isolated)
		require.NotNil(t, got)
		assert.Equal(t, "bbbb", got.Id)
	})

	t.Run("uses a shard member that a peer reports failed rather than an isolated seed", func(t *testing.T) {
		// Each replica reports the other failed, but both still belong to the
		// existing cluster.
		a := meetTestNode("aaaa", "10.0.0.1", "slave", "deadp1", "")
		a.SetClusterNodesForTesting("aaaa 10.0.0.1:6379@16379 myself,slave deadp1 0 0 1 connected\n" +
			"bbbb 10.0.0.2:6379@16379 slave,fail deadp1 0 0 1 connected\n")
		b := meetTestNode("bbbb", "10.0.0.2", "slave", "deadp1", "")
		b.SetClusterNodesForTesting("bbbb 10.0.0.2:6379@16379 myself,slave deadp1 0 0 1 connected\n" +
			"aaaa 10.0.0.1:6379@16379 slave,fail deadp1 0 0 1 connected\n")
		state := &valkey.ClusterState{
			Shards:       []*valkey.ShardState{{Id: "s1", Nodes: []*valkey.NodeState{b, a}}},
			PendingNodes: isolated,
		}
		got := findMeetTarget(state, isolated)
		require.NotNil(t, got)
		assert.Equal(t, "bbbb", got.Id)
	})

	t.Run("uses a primary that owns no slots yet", func(t *testing.T) {
		p := meetTestNode("nnnn", "10.0.0.1", "master", "-", "")
		state := &valkey.ClusterState{Shards: []*valkey.ShardState{
			{Id: "s1", PrimaryId: "nnnn", Nodes: []*valkey.NodeState{p}},
		}}
		got := findMeetTarget(state, isolated)
		require.NotNil(t, got)
		assert.Equal(t, "nnnn", got.Id)
	})

	t.Run("falls back to a non-isolated pending node", func(t *testing.T) {
		joined := &valkey.NodeState{Id: "joined", Address: "10.0.0.5", ClusterInfo: map[string]string{"cluster_known_nodes": "3"}}
		state := &valkey.ClusterState{PendingNodes: []*valkey.NodeState{isolatedA, joined}}
		got := findMeetTarget(state, []*valkey.NodeState{isolatedA})
		require.NotNil(t, got)
		assert.Equal(t, "joined", got.Id)
	})

	t.Run("seeds from the first isolated node when no cluster exists yet", func(t *testing.T) {
		state := &valkey.ClusterState{PendingNodes: isolated}
		got := findMeetTarget(state, isolated)
		require.NotNil(t, got)
		assert.Equal(t, "isoa", got.Id)
	})
}
