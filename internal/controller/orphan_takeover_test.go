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

	"github.com/valkey-io/valkey-operator/internal/valkey"
)

// orphanTestNode builds a scraped node of a 3-primary cluster. lines is its own
// CLUSTER NODES view; the first line is its myself entry.
func orphanTestNode(id string, lines ...string) *valkey.NodeState {
	n := &valkey.NodeState{Id: id, Address: "10.0.0." + id, ClusterInfo: map[string]string{"cluster_size": "3"}}
	view := ""
	for _, line := range lines {
		view += line + "\n"
	}
	n.SetClusterNodesForTesting(view)
	return n
}

// TestOrphansToTakeOver covers which dead primaries' replicas are promoted with
// TAKEOVER, with and without failover quorum.
func TestOrphansToTakeOver(t *testing.T) {
	// The flags on the orphaned replica's own CLUSTER NODES entry. Valkey adds
	// nofailover when native failover is disabled on the node.
	const failoverEnabled, failoverDisabled = "myself,slave", "myself,slave,nofailover"

	// Shards 0 and 1 lost their primaries; shard 2's primary reports both failed.
	quorumLost := func() *valkey.ClusterState {
		return &valkey.ClusterState{Shards: []*valkey.ShardState{
			{Id: "s0", Nodes: []*valkey.NodeState{orphanTestNode("r0", "r0 10.0.0.r0:6379@16379 myself,slave p0 0 0 1 connected")}},
			{Id: "s1", Nodes: []*valkey.NodeState{orphanTestNode("r1", "r1 10.0.0.r1:6379@16379 myself,slave p1 0 0 1 connected")}},
			{Id: "s2", PrimaryId: "p2", Slots: []valkey.SlotsRange{{Start: 10923, End: 16383}}, Nodes: []*valkey.NodeState{
				orphanTestNode("p2", "p2 10.0.0.p2:6379@16379 myself,master - 0 0 1 connected 10923-16383",
					"p0 10.0.0.p0:6379@16379 master,fail? - 0 0 1 connected 0-5460",
					"p1 10.0.0.p1:6379@16379 master,fail? - 0 0 1 connected 5461-10922"),
			}},
		}}
	}

	// Only shard 0 lost its primary; shards 1 and 2 hold quorum. failFlag is how
	// the two live primaries see p0, replicaFlags how r0 sees itself.
	quorumHeld := func(failFlag, replicaFlags string) *valkey.ClusterState {
		p0 := "p0 10.0.0.p0:6379@16379 master," + failFlag + " - 0 0 1 connected 0-5460"
		return &valkey.ClusterState{Shards: []*valkey.ShardState{
			{Id: "s0", Nodes: []*valkey.NodeState{orphanTestNode("r0", "r0 10.0.0.r0:6379@16379 "+replicaFlags+" p0 0 0 1 connected")}},
			{Id: "s1", PrimaryId: "p1", Slots: []valkey.SlotsRange{{Start: 5461, End: 10922}}, Nodes: []*valkey.NodeState{
				orphanTestNode("p1", "p1 10.0.0.p1:6379@16379 myself,master - 0 0 1 connected 5461-10922", p0),
			}},
			{Id: "s2", PrimaryId: "p2", Slots: []valkey.SlotsRange{{Start: 10923, End: 16383}}, Nodes: []*valkey.NodeState{
				orphanTestNode("p2", "p2 10.0.0.p2:6379@16379 myself,master - 0 0 1 connected 10923-16383", p0),
			}},
		}}
	}

	// pairs lists each candidate as "primary/replica", in order.
	pairs := func(orphans []orphan) []string {
		out := make([]string, 0, len(orphans))
		for _, o := range orphans {
			out = append(out, o.primaryId+"/"+o.replica.Id)
		}
		return out
	}

	t.Run("without quorum, lists every dead primary in a stable order", func(t *testing.T) {
		// Every candidate is listed, so a replica whose TAKEOVER fails does not
		// hold back the others.
		assert.Equal(t, []string{"p0/r0", "p1/r1"}, pairs(orphansToTakeOver(quorumLost())))
	})

	t.Run("with quorum, leaves the shard to native failover", func(t *testing.T) {
		assert.Empty(t, orphansToTakeOver(quorumHeld("fail", failoverEnabled)))
	})

	t.Run("with quorum, takes over when the replica has failover disabled", func(t *testing.T) {
		assert.Equal(t, []string{"p0/r0"}, pairs(orphansToTakeOver(quorumHeld("fail", failoverDisabled))))
	})

	t.Run("with quorum, reads nofailover from the replica's own entry", func(t *testing.T) {
		// p1 sees r0 as nofailover, but r0 itself does not report it, so
		// native failover can still promote r0.
		state := quorumHeld("fail", failoverEnabled)
		state.Shards[1].Nodes[0] = orphanTestNode("p1", "p1 10.0.0.p1:6379@16379 myself,master - 0 0 1 connected 5461-10922",
			"p0 10.0.0.p0:6379@16379 master,fail - 0 0 1 connected 0-5460",
			"r0 10.0.0.r0:6379@16379 slave,nofailover p0 0 0 1 connected")
		assert.Empty(t, orphansToTakeOver(state))
	})

	t.Run("with quorum, waits for a confirmed failure before taking over", func(t *testing.T) {
		// Only one of the two live primaries suspects p0, short of the
		// size/2+1 that Valkey needs to mark it failed.
		state := quorumHeld("fail", failoverDisabled)
		state.Shards[2].Nodes[0] = orphanTestNode("p2", "p2 10.0.0.p2:6379@16379 myself,master - 0 0 1 connected 10923-16383")
		state.Shards[1].Nodes[0] = orphanTestNode("p1", "p1 10.0.0.p1:6379@16379 myself,master - 0 0 1 connected 5461-10922",
			"p0 10.0.0.p0:6379@16379 master,fail? - 0 0 1 connected 0-5460")
		assert.Empty(t, orphansToTakeOver(state))
	})

	t.Run("never takes over a primary the operator reached", func(t *testing.T) {
		// p0 answered the scrape, here as a slotless pending node so the
		// served-shard rule does not apply; its replica's stale flag must not
		// trigger a takeover, even though quorum is short.
		state := quorumLost()
		state.Shards[0] = &valkey.ShardState{Id: "s0", Nodes: []*valkey.NodeState{
			orphanTestNode("r0", "r0 10.0.0.r0:6379@16379 myself,slave p0 0 0 1 connected",
				"p0 10.0.0.p0:6379@16379 master,fail? - 0 0 1 connected"),
		}}
		state.Shards = state.Shards[:1]
		state.PendingNodes = []*valkey.NodeState{
			orphanTestNode("p0", "p0 10.0.0.p0:6379@16379 myself,master - 0 0 1 connected"),
		}
		assert.Empty(t, orphansToTakeOver(state))
	})

	t.Run("skips a shard already served by a promoted replica", func(t *testing.T) {
		// r0 took over shard 0 on an earlier reconcile; its sibling r0b has not
		// learned that yet and still names the dead p0. Only shard 1 is left.
		state := quorumLost()
		state.Shards[0] = &valkey.ShardState{Id: "s0", PrimaryId: "r0", Slots: []valkey.SlotsRange{{Start: 0, End: 5460}}, Nodes: []*valkey.NodeState{
			orphanTestNode("r0", "r0 10.0.0.r0:6379@16379 myself,master - 0 0 2 connected 0-5460",
				"p0 10.0.0.p0:6379@16379 master,fail - 0 0 1 connected",
				"p1 10.0.0.p1:6379@16379 master,fail? - 0 0 1 connected 5461-10922"),
			orphanTestNode("r0b", "r0b 10.0.0.r0b:6379@16379 myself,slave p0 0 0 1 connected"),
		}}
		state.Shards = state.Shards[:2]
		assert.Equal(t, []string{"p1/r1"}, pairs(orphansToTakeOver(state)))
	})

	t.Run("does nothing when no primary is reported failed", func(t *testing.T) {
		state := quorumHeld("fail", failoverDisabled)
		state.Shards[1].Nodes[0] = orphanTestNode("p1", "p1 10.0.0.p1:6379@16379 myself,master - 0 0 1 connected 5461-10922")
		state.Shards[2].Nodes[0] = orphanTestNode("p2", "p2 10.0.0.p2:6379@16379 myself,master - 0 0 1 connected 10923-16383")
		assert.Empty(t, orphansToTakeOver(state))
	})
}
