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
	"context"
	"errors"
	"reflect"
	"testing"

	vclient "github.com/valkey-io/valkey-go"
	vmock "github.com/valkey-io/valkey-go/mock"
	"go.uber.org/mock/gomock"
)

func TestParseSlotsRange(t *testing.T) {
	// Slot range
	slots, err := parseSlotsRange("0-16383")
	if err != nil {
		t.Errorf("Expected not expected, got %v", err)
	}
	expect := SlotsRange{0, 16383}
	if slots != expect {
		t.Errorf("Expected %v, got %v", expect, slots)
	}

	// Single slot range
	slots, err = parseSlotsRange("5")
	if err != nil {
		t.Errorf("Expected not expected, got %v", err)
	}
	expect = SlotsRange{5, 5}
	if slots != expect {
		t.Errorf("Expected %v, got %v", expect, slots)
	}
}

func TestParseSlotsRanges(t *testing.T) {
	ranges, err := parseSlotsRanges([]string{"0-5460", "5461-10922", "10923-16383"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []SlotsRange{{0, 5460}, {5461, 10922}, {10923, 16383}}
	if !reflect.DeepEqual(ranges, expected) {
		t.Errorf("expected %v, got %v", expected, ranges)
	}

	// Migrating/importing entries from CLUSTER NODES should be skipped.
	ranges, err = parseSlotsRanges([]string{"0-5460", "[5461->-abc123]", "[5462-<-def456]"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected = []SlotsRange{{0, 5460}}
	if !reflect.DeepEqual(ranges, expected) {
		t.Errorf("expected %v, got %v", expected, ranges)
	}

	// Empty input.
	ranges, err = parseSlotsRanges([]string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ranges) != 0 {
		t.Errorf("expected empty, got %v", ranges)
	}

	// Only migration entries — should return empty.
	ranges, err = parseSlotsRanges([]string{"[5461->-abc123]"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ranges) != 0 {
		t.Errorf("expected empty, got %v", ranges)
	}
}

func TestSubtractSlotsRange(t *testing.T) {
	base := SlotsRange{0, 16383}
	remove := SlotsRange{10, 16380}
	expect := []SlotsRange{{0, 9}, {16381, 16383}}
	result := SubtractSlotsRange(base, remove)
	if !reflect.DeepEqual(result, expect) {
		t.Errorf("Expected %v, got %v", expect, result)
	}

	base = SlotsRange{0, 10}
	remove = SlotsRange{5, 10}
	expect = []SlotsRange{{0, 4}}
	result = SubtractSlotsRange(base, remove)
	if !reflect.DeepEqual(result, expect) {
		t.Errorf("Expected %v, got %v", expect, result)
	}

	base = SlotsRange{0, 10}
	remove = SlotsRange{0, 9}
	expect = []SlotsRange{{10, 10}}
	result = SubtractSlotsRange(base, remove)
	if !reflect.DeepEqual(result, expect) {
		t.Errorf("Expected %v, got %v", expect, result)
	}

	base = SlotsRange{0, 10}
	remove = SlotsRange{0, 10}
	result = SubtractSlotsRange(base, remove)
	if result != nil {
		t.Errorf("Expected nil, got %v", result)
	}
}

func TestClusterState_FindShardForAddress(t *testing.T) {
	state := &ClusterState{
		Shards: []*ShardState{
			{
				Id:        "shard-1",
				PrimaryId: "node-1",
				Nodes: []*NodeState{
					{Address: "10.0.0.1", Id: "node-1", Flags: []string{"master", "myself"}},
					{Address: "10.0.0.2", Id: "node-2", Flags: []string{"slave"}},
				},
			},
			{
				Id:        "shard-2",
				PrimaryId: "node-3",
				Nodes: []*NodeState{
					{Address: "10.0.0.3", Id: "node-3", Flags: []string{"master"}},
				},
			},
		},
	}

	t.Run("found in first shard", func(t *testing.T) {
		shard := state.FindShardForAddress("10.0.0.1")
		if shard == nil || shard.Id != "shard-1" {
			t.Errorf("expected shard-1, got %v", shard)
		}
	})

	t.Run("found replica in first shard", func(t *testing.T) {
		shard := state.FindShardForAddress("10.0.0.2")
		if shard == nil || shard.Id != "shard-1" {
			t.Errorf("expected shard-1, got %v", shard)
		}
	})

	t.Run("found in second shard", func(t *testing.T) {
		shard := state.FindShardForAddress("10.0.0.3")
		if shard == nil || shard.Id != "shard-2" {
			t.Errorf("expected shard-2, got %v", shard)
		}
	})

	t.Run("not found", func(t *testing.T) {
		shard := state.FindShardForAddress("10.0.0.99")
		if shard != nil {
			t.Errorf("expected nil, got %v", shard)
		}
	})
}

func TestShardState_GetSyncedReplicas(t *testing.T) {
	// The one primary owning slots reports cluster_size 1, so its own
	// report is the whole quorum.
	primary := &NodeState{
		Id:          "primary-id",
		Address:     "10.0.0.1",
		Flags:       []string{"myself", "master"},
		Info:        map[string]string{"role": "master"},
		ClusterInfo: map[string]string{"cluster_size": "1"},
	}
	syncedReplica := &NodeState{
		Id:      "replica-1-id",
		Address: "10.0.0.2",
		Flags:   []string{"slave"},
		Info:    map[string]string{"role": "slave", "master_link_status": "up"},
	}
	unsyncedReplica := &NodeState{
		Id:      "replica-2-id",
		Address: "10.0.0.3",
		Flags:   []string{"slave"},
		Info:    map[string]string{"role": "slave", "master_link_status": "down"},
	}
	// A replica whose own scrape looks healthy: its link is up and its own
	// CLUSTER NODES entry, like every node's own entry, carries no failure
	// flag. Only the primary's view of the cluster says it is failing.
	partitionedReplica := &NodeState{
		Id:      "replica-3-id",
		Address: "10.0.0.4",
		Flags:   []string{"slave"},
		Info:    map[string]string{"role": "slave", "master_link_status": "up"},
	}
	primary.nodes = ParseClusterNodes("primary-id 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-5461\n" +
		"replica-1-id 10.0.0.2:6379@16379 slave primary-id 0 0 1 connected\n" +
		"replica-2-id 10.0.0.3:6379@16379 slave primary-id 0 0 1 connected\n" +
		"replica-3-id 10.0.0.4:6379@16379 slave,fail? primary-id 0 0 1 connected\n")

	shard := &ShardState{
		Id:        "shard-0",
		PrimaryId: "primary-id",
		Slots:     []SlotsRange{{0, 5461}},
		Nodes:     []*NodeState{primary, syncedReplica, unsyncedReplica, partitionedReplica},
	}
	state := &ClusterState{Shards: []*ShardState{shard}}

	replicas := shard.GetSyncedReplicas(state)
	if len(replicas) != 1 {
		t.Fatalf("expected 1 synced replica, got %d", len(replicas))
	}
	if replicas[0].Id != "replica-1-id" {
		t.Errorf("expected replica-1-id, got %s", replicas[0].Id)
	}
}

func TestShardState_GetSyncedReplicas_Empty(t *testing.T) {
	primary := &NodeState{
		Id:      "primary-id",
		Address: "10.0.0.1",
		Flags:   []string{"myself", "master"},
		Info:    map[string]string{"role": "master"},
	}
	shard := &ShardState{
		Id:        "shard-0",
		PrimaryId: "primary-id",
		Slots:     []SlotsRange{{0, 5461}},
		Nodes:     []*NodeState{primary},
	}

	replicas := shard.GetSyncedReplicas(&ClusterState{Shards: []*ShardState{shard}})
	if len(replicas) != 0 {
		t.Fatalf("expected 0 synced replicas, got %d", len(replicas))
	}
}

func TestGetUnassignedSlots(t *testing.T) {
	// A shard with no unassigned slots
	cluster := ClusterState{
		Shards: []*ShardState{
			{
				Slots: []SlotsRange{{0, 16383}},
			},
		},
	}
	result := cluster.GetUnassignedSlots()
	if len(result) != 0 {
		t.Errorf("Expected empty array, got %v", result)
	}

	// A single shard with the unassigned slot 0
	cluster = ClusterState{
		Shards: []*ShardState{
			{
				Slots: []SlotsRange{{1, 16383}},
			},
		},
	}
	result = cluster.GetUnassignedSlots()
	expect := []SlotsRange{{0, 0}}
	if !reflect.DeepEqual(result, expect) {
		t.Errorf("Expected %v, got %v", expect, result)
	}

	// Three shards with unassigned slots
	cluster = ClusterState{
		Shards: []*ShardState{
			{
				Slots: []SlotsRange{{100, 200}, {300, 400}},
			},
			{
				Slots: []SlotsRange{{700, 800}},
			},
			{
				Slots: []SlotsRange{{500, 600}},
			},
		},
	}
	result = cluster.GetUnassignedSlots()
	expect = []SlotsRange{{0, 99}, {201, 299}, {401, 499}, {601, 699}, {801, 16383}}
	if !reflect.DeepEqual(result, expect) {
		t.Errorf("Expected %v, got %v", expect, result)
	}
}

func TestCurrentEpoch(t *testing.T) {
	t.Run("parses valid epoch", func(t *testing.T) {
		n := &NodeState{ClusterInfo: map[string]string{"cluster_current_epoch": "42"}}
		if n.CurrentEpoch() != 42 {
			t.Errorf("expected 42, got %d", n.CurrentEpoch())
		}
	})

	t.Run("returns 0 for missing key", func(t *testing.T) {
		n := &NodeState{ClusterInfo: map[string]string{}}
		if n.CurrentEpoch() != 0 {
			t.Errorf("expected 0, got %d", n.CurrentEpoch())
		}
	})

	t.Run("returns 0 for nil map", func(t *testing.T) {
		n := &NodeState{}
		if n.CurrentEpoch() != 0 {
			t.Errorf("expected 0, got %d", n.CurrentEpoch())
		}
	})

	t.Run("returns 0 for non-numeric value", func(t *testing.T) {
		n := &NodeState{ClusterInfo: map[string]string{"cluster_current_epoch": "abc"}}
		if n.CurrentEpoch() != 0 {
			t.Errorf("expected 0, got %d", n.CurrentEpoch())
		}
	})
}

func TestClusterState_HasFailoverQuorum(t *testing.T) {
	t.Run("no shards", func(t *testing.T) {
		state := &ClusterState{Shards: []*ShardState{}}
		if state.HasFailoverQuorum() {
			t.Error("expected false with no shards")
		}
	})

	t.Run("majority alive (2 of 3)", func(t *testing.T) {
		state := &ClusterState{
			Shards: []*ShardState{
				{PrimaryId: "p1", Slots: []SlotsRange{{0, 5461}}, Nodes: []*NodeState{{Id: "p1", ClusterInfo: map[string]string{"cluster_size": "3"}}}},
				{PrimaryId: "p2", Slots: []SlotsRange{{5462, 10922}}, Nodes: []*NodeState{{Id: "p2", ClusterInfo: map[string]string{"cluster_size": "3"}}}},
			},
		}
		if !state.HasFailoverQuorum() {
			t.Error("expected true: 2 > 3/2")
		}
	})

	t.Run("no majority (1 of 3)", func(t *testing.T) {
		state := &ClusterState{
			Shards: []*ShardState{
				{PrimaryId: "p1", Slots: []SlotsRange{{0, 5461}}, Nodes: []*NodeState{{Id: "p1", ClusterInfo: map[string]string{"cluster_size": "3"}}}},
			},
		}
		if state.HasFailoverQuorum() {
			t.Error("expected false: 1 <= 3/2")
		}
	})

	t.Run("all alive (3 of 3)", func(t *testing.T) {
		state := &ClusterState{
			Shards: []*ShardState{
				{PrimaryId: "p1", Slots: []SlotsRange{{0, 5461}}, Nodes: []*NodeState{{Id: "p1", ClusterInfo: map[string]string{"cluster_size": "3"}}}},
				{PrimaryId: "p2", Slots: []SlotsRange{{5462, 10922}}, Nodes: []*NodeState{{Id: "p2", ClusterInfo: map[string]string{"cluster_size": "3"}}}},
				{PrimaryId: "p3", Slots: []SlotsRange{{10923, 16383}}, Nodes: []*NodeState{{Id: "p3", ClusterInfo: map[string]string{"cluster_size": "3"}}}},
			},
		}
		if !state.HasFailoverQuorum() {
			t.Error("expected true: 3 > 3/2")
		}
	})

	t.Run("half alive (2 of 4)", func(t *testing.T) {
		state := &ClusterState{
			Shards: []*ShardState{
				{PrimaryId: "p1", Slots: []SlotsRange{{0, 4095}}, Nodes: []*NodeState{{Id: "p1", ClusterInfo: map[string]string{"cluster_size": "4"}}}},
				{PrimaryId: "p2", Slots: []SlotsRange{{4096, 8191}}, Nodes: []*NodeState{{Id: "p2", ClusterInfo: map[string]string{"cluster_size": "4"}}}},
			},
		}
		if state.HasFailoverQuorum() {
			t.Error("expected false: 2 <= 4/2")
		}
	})

	t.Run("only replicas alive (0 primaries of 3)", func(t *testing.T) {
		state := &ClusterState{
			Shards: []*ShardState{
				{PrimaryId: "", Nodes: []*NodeState{{Id: "r1", ClusterInfo: map[string]string{"cluster_size": "3"}}}},
				{PrimaryId: "", Nodes: []*NodeState{{Id: "r2", ClusterInfo: map[string]string{"cluster_size": "3"}}}},
				{PrimaryId: "", Nodes: []*NodeState{{Id: "r3", ClusterInfo: map[string]string{"cluster_size": "3"}}}},
			},
		}
		if state.HasFailoverQuorum() {
			t.Error("expected false: 0 live primaries")
		}
	})

	t.Run("returns false when cluster_size missing", func(t *testing.T) {
		state := &ClusterState{
			Shards: []*ShardState{
				{PrimaryId: "p1", Slots: []SlotsRange{{0, 8191}}, Nodes: []*NodeState{{Id: "p1", ClusterInfo: map[string]string{}}}},
				{PrimaryId: "p2", Slots: []SlotsRange{{8192, 16383}}, Nodes: []*NodeState{{Id: "p2", ClusterInfo: map[string]string{}}}},
			},
		}
		// Cannot determine cluster size but assume no quorum
		if state.HasFailoverQuorum() {
			t.Error("expected false when cluster_size unavailable")
		}
	})
}

func TestClusterState_IsNodeFailed(t *testing.T) {
	state := &ClusterState{
		Shards: []*ShardState{
			{
				Nodes: []*NodeState{
					{nodes: ParseClusterNodes("abc123 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-5461\ndead1 10.0.0.99:6379@16379 master,fail - 0 0 2 connected 5462-10922\npfail1 10.0.0.98:6379@16379 master,fail? - 0 0 3 connected 10923-16383\n")},
				},
			},
		},
	}

	t.Run("fail", func(t *testing.T) {
		if !state.IsNodeFailed("dead1") {
			t.Error("expected true")
		}
	})

	t.Run("pfail", func(t *testing.T) {
		if !state.IsNodeFailed("pfail1") {
			t.Error("expected true for pfail")
		}
	})

	t.Run("not failed", func(t *testing.T) {
		if state.IsNodeFailed("abc123") {
			t.Error("expected false")
		}
	})

	t.Run("unknown", func(t *testing.T) {
		if state.IsNodeFailed("unknown") {
			t.Error("expected false")
		}
	})
}

func TestClusterState_BestReplicaOf(t *testing.T) {
	state := &ClusterState{
		Shards: []*ShardState{
			{
				PrimaryId: "primary1",
				Nodes: []*NodeState{
					{Id: "primary1", Flags: []string{"master"}, nodes: ParseClusterNodes("primary1 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-5461\n")},
					{Id: "r1", Flags: []string{"slave"}, Info: map[string]string{"slave_repl_offset": "100"}, nodes: ParseClusterNodes("r1 10.0.0.2:6379@16379 myself,slave primary1 0 0 1 connected\n")},
					{Id: "r2", Flags: []string{"slave"}, Info: map[string]string{"slave_repl_offset": "500"}, nodes: ParseClusterNodes("r2 10.0.0.3:6379@16379 myself,slave primary1 0 0 1 connected\n")},
					{Id: "r3", Flags: []string{"slave"}, Info: map[string]string{"slave_repl_offset": "200"}, nodes: ParseClusterNodes("r3 10.0.0.4:6379@16379 myself,slave primary1 0 0 1 connected\n")},
				},
			},
		},
	}

	t.Run("picks highest offset", func(t *testing.T) {
		best := state.BestReplicaOf("primary1")
		if best == nil || best.Id != "r2" {
			t.Errorf("expected r2 (offset 500), got %v", best)
		}
	})

	t.Run("not found", func(t *testing.T) {
		best := state.BestReplicaOf("unknown")
		if best != nil {
			t.Errorf("expected nil, got %v", best.Id)
		}
	})
}

func TestHighestOffsetReplica(t *testing.T) {
	replica := func(id, offset string) *NodeState {
		info := map[string]string{}
		if offset != "" {
			info["slave_repl_offset"] = offset
		}
		return &NodeState{Id: id, Info: info}
	}

	t.Run("nil for no replicas", func(t *testing.T) {
		if got := HighestOffsetReplica(nil); got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	t.Run("picks the greatest offset", func(t *testing.T) {
		got := HighestOffsetReplica([]*NodeState{replica("a", "100"), replica("b", "300"), replica("c", "200")})
		if got == nil || got.Id != "b" {
			t.Errorf("expected b, got %v", got)
		}
	})

	t.Run("a replica with no offset sorts last", func(t *testing.T) {
		got := HighestOffsetReplica([]*NodeState{replica("a", ""), replica("b", "5")})
		if got == nil || got.Id != "b" {
			t.Errorf("expected b, got %v", got)
		}
	})

	t.Run("ties keep slice order", func(t *testing.T) {
		got := HighestOffsetReplica([]*NodeState{replica("a", "10"), replica("b", "10")})
		if got == nil || got.Id != "a" {
			t.Errorf("expected a, got %v", got)
		}
	})

	t.Run("all without an offset keep slice order", func(t *testing.T) {
		got := HighestOffsetReplica([]*NodeState{replica("a", ""), replica("b", "")})
		if got == nil || got.Id != "a" {
			t.Errorf("expected a, got %v", got)
		}
	})
}

func TestClusterState_FindStaleAddressPeers(t *testing.T) {
	// Node table template: viewer knows itself at its new address and its
	// peers at whatever address the persisted nodes.conf recorded.
	node := func(id, address, clusterNodes string) *NodeState {
		return &NodeState{Id: id, Address: address, nodes: ParseClusterNodes(clusterNodes)}
	}

	t.Run("full restart: all peers stale", func(t *testing.T) {
		// Both members restarted with new IPs; each table still holds the
		// other's old IP, flagged fail?.
		a := node("aaa", "10.0.1.1",
			"aaa 10.0.1.1:6379@16379 myself,master - 0 0 1 connected 0-8191\n"+
				"bbb 10.0.0.2:6379@16379 master,fail? - 0 0 2 connected 8192-16383\n")
		b := node("bbb", "10.0.1.2",
			"bbb 10.0.1.2:6379@16379 myself,master - 0 0 2 connected 8192-16383\n"+
				"aaa 10.0.0.1:6379@16379 master,fail? - 0 0 1 connected 0-8191\n")
		state := &ClusterState{Shards: []*ShardState{
			{Id: "s1", PrimaryId: "aaa", Nodes: []*NodeState{a}},
			{Id: "s2", PrimaryId: "bbb", Nodes: []*NodeState{b}},
		}}

		stale := state.FindStaleAddressPeers()
		if len(stale) != 2 {
			t.Fatalf("expected 2 stale pairs, got %d", len(stale))
		}
		if stale[0].Viewer.Id != "aaa" || stale[0].Live.Id != "bbb" {
			t.Errorf("expected aaa->bbb, got %s->%s", stale[0].Viewer.Id, stale[0].Live.Id)
		}
		if stale[1].Viewer.Id != "bbb" || stale[1].Live.Id != "aaa" {
			t.Errorf("expected bbb->aaa, got %s->%s", stale[1].Viewer.Id, stale[1].Live.Id)
		}
	})

	t.Run("fail flag also detected", func(t *testing.T) {
		a := node("aaa", "10.0.1.1",
			"aaa 10.0.1.1:6379@16379 myself,master - 0 0 1 connected 0-8191\n"+
				"bbb 10.0.0.2:6379@16379 slave,fail aaa 0 0 2 connected\n")
		b := node("bbb", "10.0.1.2",
			"bbb 10.0.1.2:6379@16379 myself,slave aaa 0 0 2 connected\n"+
				"aaa 10.0.1.1:6379@16379 master - 0 0 1 connected 0-8191\n")
		state := &ClusterState{Shards: []*ShardState{
			{Id: "s1", PrimaryId: "aaa", Nodes: []*NodeState{a, b}},
		}}

		stale := state.FindStaleAddressPeers()
		if len(stale) != 1 || stale[0].Viewer.Id != "aaa" || stale[0].Live.Id != "bbb" {
			t.Fatalf("expected exactly aaa->bbb, got %v", stale)
		}
	})

	t.Run("genuinely dead peer is not a stale pair", func(t *testing.T) {
		// Peer ID matches no live node — that's forgetStaleNodes territory.
		a := node("aaa", "10.0.1.1",
			"aaa 10.0.1.1:6379@16379 myself,master - 0 0 1 connected 0-16383\n"+
				"ded 10.0.0.9:6379@16379 master,fail - 0 0 2 connected\n")
		state := &ClusterState{Shards: []*ShardState{
			{Id: "s1", PrimaryId: "aaa", Nodes: []*NodeState{a}},
		}}

		if stale := state.FindStaleAddressPeers(); len(stale) != 0 {
			t.Fatalf("expected no stale pairs, got %d", len(stale))
		}
	})

	t.Run("failing peer at its current address is not stale", func(t *testing.T) {
		// Transient pfail during normal operation: address still correct,
		// nothing to heal.
		a := node("aaa", "10.0.1.1",
			"aaa 10.0.1.1:6379@16379 myself,master - 0 0 1 connected 0-8191\n"+
				"bbb 10.0.1.2:6379@16379 master,fail? - 0 0 2 connected 8192-16383\n")
		b := node("bbb", "10.0.1.2",
			"bbb 10.0.1.2:6379@16379 myself,master - 0 0 2 connected 8192-16383\n"+
				"aaa 10.0.1.1:6379@16379 master - 0 0 1 connected 0-8191\n")
		state := &ClusterState{Shards: []*ShardState{
			{Id: "s1", PrimaryId: "aaa", Nodes: []*NodeState{a}},
			{Id: "s2", PrimaryId: "bbb", Nodes: []*NodeState{b}},
		}}

		if stale := state.FindStaleAddressPeers(); len(stale) != 0 {
			t.Fatalf("expected no stale pairs, got %d", len(stale))
		}
	})

	t.Run("healthy peer at an old address is not stale", func(t *testing.T) {
		// Gossip already rebinding (no fail flags): don't interfere.
		a := node("aaa", "10.0.1.1",
			"aaa 10.0.1.1:6379@16379 myself,master - 0 0 1 connected 0-8191\n"+
				"bbb 10.0.0.2:6379@16379 master - 0 0 2 connected 8192-16383\n")
		b := node("bbb", "10.0.1.2",
			"bbb 10.0.1.2:6379@16379 myself,master - 0 0 2 connected 8192-16383\n"+
				"aaa 10.0.1.1:6379@16379 master - 0 0 1 connected 0-8191\n")
		state := &ClusterState{Shards: []*ShardState{
			{Id: "s1", PrimaryId: "aaa", Nodes: []*NodeState{a}},
			{Id: "s2", PrimaryId: "bbb", Nodes: []*NodeState{b}},
		}}

		if stale := state.FindStaleAddressPeers(); len(stale) != 0 {
			t.Fatalf("expected no stale pairs, got %d", len(stale))
		}
	})

	t.Run("pending nodes participate as viewers and live targets", func(t *testing.T) {
		a := node("aaa", "10.0.1.1",
			"aaa 10.0.1.1:6379@16379 myself,master - 0 0 1 connected 0-16383\n"+
				"ppp 10.0.0.7:6379@16379 master,fail? - 0 0 2 connected\n")
		p := node("ppp", "10.0.1.7",
			"ppp 10.0.1.7:6379@16379 myself,master - 0 0 2 connected\n"+
				"aaa 10.0.0.1:6379@16379 master,fail? - 0 0 1 connected 0-16383\n")
		state := &ClusterState{
			Shards:       []*ShardState{{Id: "s1", PrimaryId: "aaa", Nodes: []*NodeState{a}}},
			PendingNodes: []*NodeState{p},
		}

		stale := state.FindStaleAddressPeers()
		if len(stale) != 2 {
			t.Fatalf("expected 2 stale pairs, got %d", len(stale))
		}
	})
}

func TestClusterState_FindNodeById(t *testing.T) {
	state := &ClusterState{
		Shards:       []*ShardState{{Nodes: []*NodeState{{Id: "aaa"}}}},
		PendingNodes: []*NodeState{{Id: "ppp"}},
	}
	if state.FindNodeById("aaa") == nil {
		t.Error("expected shard node found")
	}
	if state.FindNodeById("ppp") == nil {
		t.Error("expected pending node found")
	}
	if state.FindNodeById("zzz") != nil {
		t.Error("expected nil for unknown id")
	}
}

func TestHostFromClusterNodesEndpoint(t *testing.T) {
	cases := []struct{ in, want string }{
		{"10.0.1.2:6379@16379", "10.0.1.2"},
		{"10.0.1.2:6379@16379,node-1.example", "10.0.1.2"},
		{"[fd00::2]:6379@16379", "fd00::2"},
		{"[fd00::2]:6379@16379,node-1.example", "fd00::2"},
		{"[fd00::2]:6379", "fd00::2"},
		{"10.0.1.2:6379", "10.0.1.2"},
		{"garbage", ""},
	}
	for _, c := range cases {
		if got := hostFromClusterNodesEndpoint(c.in); got != c.want {
			t.Errorf("hostFromClusterNodesEndpoint(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestClusterState_FindStaleAddressPeers_IPv6(t *testing.T) {
	node := func(id, address, clusterNodes string) *NodeState {
		return &NodeState{Id: id, Address: address, nodes: ParseClusterNodes(clusterNodes)}
	}

	t.Run("failing IPv6 peer at its current address is not stale", func(t *testing.T) {
		// Bracketed table entry vs bare pod IP must compare equal.
		a := node("aaa", "fd00::1",
			"aaa [fd00::1]:6379@16379 myself,master - 0 0 1 connected 0-8191\n"+
				"bbb [fd00::2]:6379@16379 master,fail? - 0 0 2 connected 8192-16383\n")
		b := node("bbb", "fd00::2",
			"bbb [fd00::2]:6379@16379 myself,master - 0 0 2 connected 8192-16383\n"+
				"aaa [fd00::1]:6379@16379 master - 0 0 1 connected 0-8191\n")
		state := &ClusterState{Shards: []*ShardState{
			{Id: "s1", PrimaryId: "aaa", Nodes: []*NodeState{a}},
			{Id: "s2", PrimaryId: "bbb", Nodes: []*NodeState{b}},
		}}

		if stale := state.FindStaleAddressPeers(); len(stale) != 0 {
			t.Fatalf("expected no stale pairs for current bracketed IPv6 address, got %d", len(stale))
		}
	})

	t.Run("moved IPv6 peer is stale", func(t *testing.T) {
		a := node("aaa", "fd00::11",
			"aaa [fd00::11]:6379@16379 myself,master - 0 0 1 connected 0-8191\n"+
				"bbb [fd00::2]:6379@16379 master,fail? - 0 0 2 connected 8192-16383\n")
		b := node("bbb", "fd00::12",
			"bbb [fd00::12]:6379@16379 myself,master - 0 0 2 connected 8192-16383\n"+
				"aaa [fd00::11]:6379@16379 master - 0 0 1 connected 0-8191\n")
		state := &ClusterState{Shards: []*ShardState{
			{Id: "s1", PrimaryId: "aaa", Nodes: []*NodeState{a}},
			{Id: "s2", PrimaryId: "bbb", Nodes: []*NodeState{b}},
		}}

		stale := state.FindStaleAddressPeers()
		if len(stale) != 1 || stale[0].Viewer.Id != "aaa" || stale[0].Live.Id != "bbb" {
			t.Fatalf("expected exactly aaa->bbb, got %v", stale)
		}
	})
}

func TestHostFromClusterNodesEndpoint_UnbracketedIPv6(t *testing.T) {
	// CLUSTER NODES reports IPv6 endpoints without brackets
	// (fd00::2:6379@16379); net.SplitHostPort rejects those, so they take
	// the last-colon fallback path.
	cases := []struct{ in, want string }{
		{"fd00::2:6379@16379", "fd00::2"},
		{"fd00::2:6379@16379,node-1.example", "fd00::2"},
		{"fd00::2:6379", "fd00::2"},
		{"2001:db8::10:6379@16379", "2001:db8::10"},
		{":0@0", ""}, // noaddr placeholder — no host to extract
	}
	for _, c := range cases {
		if got := hostFromClusterNodesEndpoint(c.in); got != c.want {
			t.Errorf("hostFromClusterNodesEndpoint(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestClusterState_FindStaleAddressPeers_UnbracketedIPv6(t *testing.T) {
	node := func(id, address, clusterNodes string) *NodeState {
		return &NodeState{Id: id, Address: address, nodes: ParseClusterNodes(clusterNodes)}
	}

	t.Run("failing unbracketed IPv6 peer at its current address is not stale", func(t *testing.T) {
		a := node("aaa", "fd00::1",
			"aaa fd00::1:6379@16379 myself,master - 0 0 1 connected 0-8191\n"+
				"bbb fd00::2:6379@16379 master,fail? - 0 0 2 connected 8192-16383\n")
		b := node("bbb", "fd00::2",
			"bbb fd00::2:6379@16379 myself,master - 0 0 2 connected 8192-16383\n"+
				"aaa fd00::1:6379@16379 master - 0 0 1 connected 0-8191\n")
		state := &ClusterState{Shards: []*ShardState{
			{Id: "s1", PrimaryId: "aaa", Nodes: []*NodeState{a}},
			{Id: "s2", PrimaryId: "bbb", Nodes: []*NodeState{b}},
		}}

		if stale := state.FindStaleAddressPeers(); len(stale) != 0 {
			t.Fatalf("expected no stale pairs for current unbracketed IPv6 address, got %d", len(stale))
		}
	})

	t.Run("moved unbracketed IPv6 peer is stale", func(t *testing.T) {
		a := node("aaa", "fd00::11",
			"aaa fd00::11:6379@16379 myself,master - 0 0 1 connected 0-8191\n"+
				"bbb fd00::2:6379@16379 master,fail? - 0 0 2 connected 8192-16383\n")
		b := node("bbb", "fd00::12",
			"bbb fd00::12:6379@16379 myself,master - 0 0 2 connected 8192-16383\n"+
				"aaa fd00::11:6379@16379 master - 0 0 1 connected 0-8191\n")
		state := &ClusterState{Shards: []*ShardState{
			{Id: "s1", PrimaryId: "aaa", Nodes: []*NodeState{a}},
			{Id: "s2", PrimaryId: "bbb", Nodes: []*NodeState{b}},
		}}

		stale := state.FindStaleAddressPeers()
		if len(stale) != 1 || stale[0].Viewer.Id != "aaa" || stale[0].Live.Id != "bbb" {
			t.Fatalf("expected exactly aaa->bbb, got %v", stale)
		}
	})
}

func TestClusterState_FindStaleAddressPeers_Noaddr(t *testing.T) {
	node := func(id, address, clusterNodes string) *NodeState {
		return &NodeState{Id: id, Address: address, nodes: ParseClusterNodes(clusterNodes)}
	}

	t.Run("noaddr entry with a live node is stale despite empty endpoint", func(t *testing.T) {
		// noaddr entries carry the :0@0 placeholder — no address to
		// compare, but a live node with that ID always needs a MEET.
		a := node("aaa", "10.0.1.1",
			"aaa 10.0.1.1:6379@16379 myself,master - 0 0 1 connected 0-8191\n"+
				"bbb :0@0 master,noaddr - 0 0 2 connected 8192-16383\n")
		b := node("bbb", "10.0.1.2",
			"bbb 10.0.1.2:6379@16379 myself,master - 0 0 2 connected 8192-16383\n"+
				"aaa 10.0.1.1:6379@16379 master - 0 0 1 connected 0-8191\n")
		state := &ClusterState{Shards: []*ShardState{
			{Id: "s1", PrimaryId: "aaa", Nodes: []*NodeState{a}},
			{Id: "s2", PrimaryId: "bbb", Nodes: []*NodeState{b}},
		}}

		stale := state.FindStaleAddressPeers()
		if len(stale) != 1 || stale[0].Viewer.Id != "aaa" || stale[0].Live.Id != "bbb" {
			t.Fatalf("expected exactly aaa->bbb for noaddr entry, got %v", stale)
		}
	})

	t.Run("noaddr entry with no live node is left to forgetStaleNodes", func(t *testing.T) {
		a := node("aaa", "10.0.1.1",
			"aaa 10.0.1.1:6379@16379 myself,master - 0 0 1 connected 0-16383\n"+
				"ded :0@0 master,noaddr - 0 0 2 connected\n")
		state := &ClusterState{Shards: []*ShardState{
			{Id: "s1", PrimaryId: "aaa", Nodes: []*NodeState{a}},
		}}

		if stale := state.FindStaleAddressPeers(); len(stale) != 0 {
			t.Fatalf("expected no stale pairs, got %d", len(stale))
		}
	})
}

func TestNodeState_Myself(t *testing.T) {
	node := &NodeState{
		nodes: ParseClusterNodes("abc123 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-16383\n"),
	}
	myself := node.Myself()
	if myself == nil {
		t.Fatal("expected a myself entry")
		return
	}
	if myself.Id != "abc123" {
		t.Errorf("expected abc123, got %q", myself.Id)
	}

	empty := &NodeState{}
	if empty.Myself() != nil {
		t.Error("expected nil for empty CLUSTER NODES output")
	}
}

func TestNodeState_GetSlots(t *testing.T) {
	t.Run("primary returns owned ranges only", func(t *testing.T) {
		node := &NodeState{
			nodes: ParseClusterNodes("abc123 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-5460 [5461->-def456]\n"),
		}
		if want := []SlotsRange{{0, 5460}}; !reflect.DeepEqual(node.GetSlots(), want) {
			t.Errorf("expected %v, got %v", want, node.GetSlots())
		}
	})

	t.Run("replica returns nil", func(t *testing.T) {
		node := &NodeState{
			nodes: ParseClusterNodes("abc123 10.0.0.1:6379@16379 myself,slave def456 0 0 1 connected\n"),
		}
		if slots := node.GetSlots(); slots != nil {
			t.Errorf("expected nil, got %v", slots)
		}
	})

	t.Run("no myself line returns nil", func(t *testing.T) {
		node := &NodeState{
			nodes: ParseClusterNodes("def456 10.0.0.2:6379@16379 master - 0 0 1 connected 0-16383\n"),
		}
		if slots := node.GetSlots(); slots != nil {
			t.Errorf("expected nil, got %v", slots)
		}
	})
}

// IsNodeFailed reports a failure when any single viewer sees one, even while
// another viewer still considers the same member healthy.
func TestClusterState_IsNodeFailed_ViewersDisagree(t *testing.T) {
	// Viewer aaa still has bbb at its old address and marks it failed; bbb
	// reports itself alive at the new address.
	viewerA := &NodeState{Id: "aaa", Address: "10.0.0.1",
		nodes: ParseClusterNodes("aaa 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-8191\n" +
			"bbb 10.0.0.99:6379@16379 master,fail - 0 0 2 disconnected 8192-16383\n")}
	viewerB := &NodeState{Id: "bbb", Address: "10.0.0.2",
		nodes: ParseClusterNodes("bbb 10.0.0.2:6379@16379 myself,master - 0 0 2 connected 8192-16383\n" +
			"aaa 10.0.0.1:6379@16379 master - 0 0 1 connected 0-8191\n")}

	state := &ClusterState{Shards: []*ShardState{{Nodes: []*NodeState{viewerA, viewerB}}}}

	if !state.IsNodeFailed("bbb") {
		t.Error("expected bbb failed: viewer aaa reports it so")
	}
	if state.IsNodeFailed("aaa") {
		t.Error("expected aaa healthy: no viewer reports it failed")
	}
}

func TestNodeState_KnowsNode(t *testing.T) {
	node := &NodeState{}
	node.nodes = ParseClusterNodes("abc123 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-5460\n" +
		"def456 10.0.0.2:6379@16379 slave abc123 0 0 1 connected\n")

	if !node.KnowsNode("def456") {
		t.Error("expected the peer entry to be known")
	}
	if !node.KnowsNode("abc123") {
		t.Error("expected the myself entry to be known")
	}
	if node.KnowsNode("ghi789") {
		t.Error("expected an unknown id to be reported absent")
	}

	// An ID is only known when it names an entry. Appearing as another entry's
	// primary or as a migration marker peer is not the same thing, which a
	// substring search over the raw output could not distinguish.
	referencing := &NodeState{}
	referencing.nodes = ParseClusterNodes("abc123 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-5460 [5461->-ghi789]\n" +
		"def456 10.0.0.2:6379@16379 slave ghi789 0 0 1 connected\n")
	if referencing.KnowsNode("ghi789") {
		t.Error("ghi789 has no entry; it is only referenced by others")
	}
}

func TestNodeState_GetFailingNodes(t *testing.T) {
	// fail and noaddr are reported; fail? is not, because the caller forgets
	// these nodes and a pfail entry may still recover.
	node := &NodeState{
		nodes: ParseClusterNodes("abc123 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-5460\n" +
			"dead1 10.0.0.99:6379@16379 master,fail - 0 0 2 disconnected 5461-10922\n" +
			"pfail1 10.0.0.98:6379@16379 master,fail? - 0 0 3 connected 10923-16383\n" +
			"gone1 :0@0 master,noaddr - 0 0 4 disconnected\n"),
	}

	failing := node.GetFailingNodes()
	if len(failing) != 2 {
		t.Fatalf("expected 2 failing nodes, got %d: %v", len(failing), failing)
	}
	if failing[0].Id != "dead1" || failing[0].Host != "10.0.0.99" {
		t.Errorf("unexpected first entry %+v", failing[0])
	}
	// A noaddr entry is still reported so the caller can act on the ID.
	if failing[1].Id != "gone1" || failing[1].Host != "" {
		t.Errorf("unexpected second entry %+v", failing[1])
	}
}

// Valkey writes the endpoint as "%s:%i@%i" with a bare IP, so an IPv6 address is
// unbracketed and its own colons run into the port. The extracted address must
// still compare equal to the bare pod IP Kubernetes reports.
func TestNodeState_GetFailingNodes_IPv6(t *testing.T) {
	node := &NodeState{
		nodes: ParseClusterNodes("aaa fd00::1:6379@16379 myself,master - 0 0 1 connected 0-8191\n" +
			"bbb fd00::2:6379@16379 master,fail - 0 0 2 disconnected 8192-16383\n"),
	}

	failing := node.GetFailingNodes()
	if len(failing) != 1 {
		t.Fatalf("expected 1 failing node, got %d", len(failing))
	}
	if failing[0].Host != "fd00::2" {
		t.Errorf("expected fd00::2, got %q", failing[0].Host)
	}
}

func TestClusterState_IsNodeFailedByMajority(t *testing.T) {
	// Four nodes. Views of "target" are injected per viewer.
	// Viewers a, b and c are voting primaries (own slots) unless built as
	// replicas below. Each viewer's table carries its own myself line plus
	// its view of "target", and each reports cluster_size 3: three primaries
	// own slots, so the quorum is two reports.
	size := func(n string) map[string]string { return map[string]string{"cluster_size": n} }
	primary := func(id, view string) *NodeState {
		return &NodeState{Id: id, Flags: []string{"myself", "master"}, ClusterInfo: size("3"),
			nodes: ParseClusterNodes(id + " 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-100\n" + view)}
	}
	replica := func(id, view string) *NodeState {
		return &NodeState{Id: id, Flags: []string{"myself", "slave"}, ClusterInfo: size("3"),
			nodes: ParseClusterNodes(id + " 10.0.0.1:6379@16379 myself,slave p1 0 0 1 connected\n" + view)}
	}
	target := &NodeState{Id: "target", Flags: []string{"myself", "slave"},
		nodes: ParseClusterNodes("target 10.0.0.9:6379@16379 myself,slave p1 0 0 1 connected\n")}
	build := func(a, b, c string) *ClusterState {
		return &ClusterState{Shards: []*ShardState{{Nodes: []*NodeState{
			target, primary("a", a), primary("b", b), primary("c", c),
		}}}}
	}
	healthy := "target 10.0.0.9:6379@16379 slave p1 0 0 1 connected\n"
	failing := "target 10.0.0.9:6379@16379 slave,fail? p1 0 0 1 connected\n"
	confirmed := "target 10.0.0.9:6379@16379 slave,fail p1 0 0 1 connected\n"

	t.Run("replica suspicion does not count, however many replicas", func(t *testing.T) {
		// Two replicas and one primary know the target; only the replicas
		// suspect it. Valkey would never promote that to fail, so neither
		// does the operator.
		st := &ClusterState{Shards: []*ShardState{{Nodes: []*NodeState{
			target, replica("r1", failing), replica("r2", failing), primary("a", healthy),
		}}}}
		if st.IsNodeFailedByMajority("target") {
			t.Error("expected false when only replicas suspect the node")
		}
	})

	t.Run("a confirmed fail from one viewer is authoritative", func(t *testing.T) {
		// Valkey sets fail only after a majority of primaries agreed and
		// broadcasts it, so a single table carrying it is not one opinion.
		if !build(confirmed, healthy, healthy).IsNodeFailedByMajority("target") {
			t.Error("expected true for a confirmed fail")
		}
	})
	t.Run("a primary that has not learned the node is still a voter", func(t *testing.T) {
		// Only a knows the target and suspects it; b and c own slots but
		// have no entry yet. The quorum is still 2 of 3, so one report is
		// not enough. Counting only the primaries that know the node would
		// have made this a one-of-one majority.
		if build(failing, "", "").IsNodeFailedByMajority("target") {
			t.Error("expected false: one report out of three voting primaries")
		}
	})
	t.Run("primaries the scrape did not reach still count", func(t *testing.T) {
		// cluster_size says five primaries own slots. The scrape reached
		// three, and two of those suspect the target. Valkey needs three
		// reports out of five, so two is not a majority, even though it
		// would be a majority of the primaries the operator can see.
		st := build(failing, failing, healthy)
		for _, n := range st.Shards[0].Nodes {
			if n.Id != "target" {
				n.ClusterInfo = size("5")
			}
		}
		if st.IsNodeFailedByMajority("target") {
			t.Error("expected false: two reports out of five voting primaries")
		}
	})
	t.Run("one of three viewers is not a majority", func(t *testing.T) {
		if build(failing, healthy, healthy).IsNodeFailedByMajority("target") {
			t.Error("expected false for a single report")
		}
	})
	t.Run("two of three viewers is", func(t *testing.T) {
		if !build(failing, failing, healthy).IsNodeFailedByMajority("target") {
			t.Error("expected true for a majority")
		}
	})
	t.Run("own entry is not a viewer", func(t *testing.T) {
		// Nobody else knows the target: no viewers, so not failed.
		if build("", "", "").IsNodeFailedByMajority("target") {
			t.Error("expected false with no viewers")
		}
	})
	t.Run("any single report still satisfies IsNodeFailed", func(t *testing.T) {
		if !build(failing, healthy, healthy).IsNodeFailed("target") {
			t.Error("expected the any-viewer rule to report true")
		}
	})
}

// A node cut off from the cluster bus marks every peer fail? in its own
// table. That one opinion must not empty the failover target list.
func TestShardState_GetSyncedReplicas_PartitionedPeerDoesNotExcludeEveryone(t *testing.T) {
	primary := &NodeState{Id: "p", Flags: []string{"myself", "master"}, Info: map[string]string{"role": "master"},
		ClusterInfo: map[string]string{"cluster_size": "1"}}
	r1 := &NodeState{Id: "r1", Flags: []string{"slave"}, Info: map[string]string{"role": "slave", "master_link_status": "up"}}
	r2 := &NodeState{Id: "r2", Flags: []string{"slave"}, Info: map[string]string{"role": "slave", "master_link_status": "up"}}
	primary.nodes = ParseClusterNodes("p 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-16383\nr1 10.0.0.2:6379@16379 slave p 0 0 1 connected\nr2 10.0.0.3:6379@16379 slave p 0 0 1 connected\n")
	r1.nodes = ParseClusterNodes("p 10.0.0.1:6379@16379 master - 0 0 1 connected 0-16383\nr1 10.0.0.2:6379@16379 myself,slave p 0 0 1 connected\nr2 10.0.0.3:6379@16379 slave p 0 0 1 connected\n")
	// r2 is cut off and sees everyone as fail?.
	r2.nodes = ParseClusterNodes("p 10.0.0.1:6379@16379 master,fail? - 0 0 1 connected 0-16383\nr1 10.0.0.2:6379@16379 slave,fail? p 0 0 1 connected\nr2 10.0.0.3:6379@16379 myself,slave p 0 0 1 connected\n")

	shard := &ShardState{Id: "s", PrimaryId: "p", Nodes: []*NodeState{primary, r1, r2}}
	state := &ClusterState{Shards: []*ShardState{shard}}
	got := shard.GetSyncedReplicas(state)
	if len(got) != 2 {
		t.Fatalf("expected both replicas to stay synced, got %d", len(got))
	}
	seen := map[string]bool{}
	for _, n := range got {
		seen[n.Id] = true
	}
	if !seen["r1"] || !seen["r2"] {
		t.Errorf("expected r1 and r2, got %v", seen)
	}
}

func TestGetClusterStateSkipsNodesThatFailToDial(t *testing.T) {
	var dialled []string
	dial := func(_ context.Context, address string) (vclient.Client, error) {
		dialled = append(dialled, address)
		return nil, errors.New("connection refused")
	}

	state := GetClusterState(context.Background(), []string{"10.0.0.1", "10.0.0.2"}, 6379, dial)

	if want := []string{"10.0.0.1:6379", "10.0.0.2:6379"}; !reflect.DeepEqual(dialled, want) {
		t.Fatalf("dialled %v, want %v", dialled, want)
	}
	if len(state.Shards) != 0 || len(state.PendingNodes) != 0 {
		t.Fatalf("expected an empty state, got %d shards and %d pending nodes", len(state.Shards), len(state.PendingNodes))
	}
}

func TestGetClusterStateBracketsIPv6Addresses(t *testing.T) {
	var dialled []string
	dial := func(_ context.Context, address string) (vclient.Client, error) {
		dialled = append(dialled, address)
		return nil, errors.New("connection refused")
	}

	GetClusterState(context.Background(), []string{"fd00::1", "10.0.0.2"}, 6379, dial)

	if want := []string{"[fd00::1]:6379", "10.0.0.2:6379"}; !reflect.DeepEqual(dialled, want) {
		t.Fatalf("dialled %v, want %v", dialled, want)
	}
}

// A pooled client can be stale: Get returns it without redialling, and the
// server has gone away since it was last used. getNodeState must not turn
// that into a ghost node with empty Id/Flags/ShardId.
func TestGetClusterStateSkipsAStaleClientThatFailsTheRoundTrip(t *testing.T) {
	ctrl := gomock.NewController(t)
	stale := vmock.NewClient(ctrl)
	stale.EXPECT().
		DoMulti(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]vclient.ValkeyResult{vmock.ErrorResult(errors.New("io: read/write on closed pipe"))})

	dial := func(_ context.Context, _ string) (vclient.Client, error) {
		return stale, nil
	}

	state := GetClusterState(context.Background(), []string{"10.0.0.1"}, 6379, dial)

	if len(state.Shards) != 0 || len(state.PendingNodes) != 0 {
		t.Fatalf("expected an empty state, got %d shards and %d pending nodes", len(state.Shards), len(state.PendingNodes))
	}
}

func blobResult(s string) vclient.ValkeyResult {
	return vmock.Result(vmock.ValkeyBlobString(s))
}

func valkeyErrorResult(s string) vclient.ValkeyResult {
	return vmock.Result(vmock.ValkeyError(s))
}

// primaryNodesLine is a CLUSTER NODES myself line for node id "abc".
const primaryNodesLine = "abc 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-16383\n"

// healthyScrape is the five replies getNodeState asks for, in order:
// CLUSTER MYID, CLUSTER MYSHARDID, INFO, CLUSTER INFO, CLUSTER NODES.
func healthyScrape() []vclient.ValkeyResult {
	return []vclient.ValkeyResult{
		blobResult("abc"),
		blobResult("shard-1"),
		blobResult("role:master\r\n"),
		blobResult("cluster_known_nodes:2\r\ncluster_current_epoch:3\r\n"),
		blobResult(primaryNodesLine),
	}
}

func dialScrape(t *testing.T, results []vclient.ValkeyResult) DialFunc {
	t.Helper()
	ctrl := gomock.NewController(t)
	c := vmock.NewClient(ctrl)
	c.EXPECT().
		DoMulti(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(results)
	return func(_ context.Context, _ string) (vclient.Client, error) {
		return c, nil
	}
}

func assertNoNodes(t *testing.T, state *ClusterState) {
	t.Helper()
	if len(state.Shards) != 0 || len(state.PendingNodes) != 0 {
		t.Fatalf("expected the node to be skipped, got %d shards and %d pending nodes", len(state.Shards), len(state.PendingNodes))
	}
}

func TestGetClusterStateKeepsACompleteScrape(t *testing.T) {
	state := GetClusterState(context.Background(), []string{"10.0.0.1"}, 6379, dialScrape(t, healthyScrape()))

	if len(state.Shards) != 1 || len(state.PendingNodes) != 0 {
		t.Fatalf("expected one shard, got %d shards and %d pending nodes", len(state.Shards), len(state.PendingNodes))
	}
	shard := state.Shards[0]
	if shard.Id != "shard-1" || shard.PrimaryId != "abc" {
		t.Fatalf("shard id %q primary %q", shard.Id, shard.PrimaryId)
	}
}

// A Valkey error or an empty reply on identity or topology must not become a
// member with blank fields. That member would share a shard with every other
// blank id, look joined rather than isolated, and could be picked as a MEET
// target. INFO is the exception: a missing replication section only marks the
// node out of sync.
func TestGetClusterStateSkipsNodeWhenScrapeCommandFails(t *testing.T) {
	busy := valkeyErrorResult("BUSY Valkey is busy running a script")
	cases := []struct {
		name    string
		results []vclient.ValkeyResult
	}{
		{name: "CLUSTER MYID", results: func() []vclient.ValkeyResult {
			r := healthyScrape()
			r[0] = busy
			return r
		}()},
		{name: "empty CLUSTER MYID", results: func() []vclient.ValkeyResult {
			r := healthyScrape()
			r[0] = blobResult("")
			return r
		}()},
		{name: "CLUSTER MYSHARDID", results: func() []vclient.ValkeyResult {
			r := healthyScrape()
			r[1] = busy
			return r
		}()},
		{name: "CLUSTER INFO", results: func() []vclient.ValkeyResult {
			r := healthyScrape()
			r[3] = busy
			return r
		}()},
		{name: "CLUSTER NODES", results: func() []vclient.ValkeyResult {
			r := healthyScrape()
			r[4] = busy
			return r
		}()},
		{name: "CLUSTER NODES without myself", results: func() []vclient.ValkeyResult {
			r := healthyScrape()
			r[4] = blobResult("peer 10.0.0.2:6379@16379 master - 0 0 1 connected 0-16383\n")
			return r
		}()},
		{name: "every command BUSY", results: []vclient.ValkeyResult{busy, busy, busy, busy, busy}},
		{name: "short DoMulti reply", results: healthyScrape()[:4]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := GetClusterState(context.Background(), []string{"10.0.0.1"}, 6379, dialScrape(t, tc.results))
			assertNoNodes(t, state)
		})
	}
}

func dialScrapeByAddress(t *testing.T, byAddr map[string][]vclient.ValkeyResult) DialFunc {
	t.Helper()
	ctrl := gomock.NewController(t)
	return func(_ context.Context, address string) (vclient.Client, error) {
		results, ok := byAddr[address]
		if !ok {
			t.Fatalf("unexpected dial %s", address)
		}
		c := vmock.NewClient(ctrl)
		c.EXPECT().
			DoMulti(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(results)
		return c, nil
	}
}

// A reachable primary that fails CLUSTER MYID is dropped, but its replica's
// CLUSTER NODES still lists the ranges that primary owns. Those ranges must
// not come back as free, and the missing primary must not be invented as a
// node we can migrate from.
func TestGetClusterStateKeepsSlotsOwnedByAnUnscrapedPrimary(t *testing.T) {
	primary := healthyScrape()
	primary[0] = valkeyErrorResult("BUSY Valkey is busy running a script")

	replicaNodes := "abc 10.0.0.1:6379@16379 master - 0 0 1 connected 0-16383\n" +
		"def 10.0.0.2:6379@16379 myself,slave abc 0 0 1 connected\n"
	replica := []vclient.ValkeyResult{
		blobResult("def"),
		blobResult("shard-1"),
		blobResult("role:slave\r\nmaster_link_status:up\r\n"),
		blobResult("cluster_known_nodes:2\r\ncluster_current_epoch:3\r\ncluster_size:1\r\n"),
		blobResult(replicaNodes),
	}

	state := GetClusterState(context.Background(), []string{"10.0.0.1", "10.0.0.2"}, 6379, dialScrapeByAddress(t, map[string][]vclient.ValkeyResult{
		"10.0.0.1:6379": primary,
		"10.0.0.2:6379": replica,
	}))

	if got := state.GetUnassignedSlots(); len(got) != 0 {
		t.Fatalf("slots owned by the busy primary were reported unassigned: %v", got)
	}
	if len(state.Shards) != 1 || len(state.PendingNodes) != 0 {
		t.Fatalf("expected one replica shard, got %d shards and %d pending", len(state.Shards), len(state.PendingNodes))
	}
	shard := state.Shards[0]
	if shard.GetPrimaryNode() != nil {
		t.Fatal("busy primary must not be invented as a scraped node")
	}
	if shard.PrimaryId != "" {
		t.Fatalf("primary id %q would point at a node this pass did not scrape", shard.PrimaryId)
	}
	if !reflect.DeepEqual(shard.Slots, []SlotsRange{{0, 16383}}) {
		t.Fatalf("shard slots = %v, want 0-16383 from the replica's peer table", shard.Slots)
	}
	if !state.PrimaryUnscrapedButServing(shard) {
		t.Fatal("scale-in must requeue: the shard still serves slots and has no scraped primary")
	}
	if got, want := state.UnscrapedSlotOwnerHosts(), []string{"10.0.0.1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unscraped owner hosts = %v, want %v", got, want)
	}
}

// With replicas: 0 and two shards, a primary replaced behind a roll comes back
// at a new address with a new id, and the survivor can only ever mark the old
// id "fail?": FAIL needs reports from a majority of two primaries. The old
// entry's address is no longer a pod, so its ranges must go back to the free
// set for the replacement, or cluster-require-full-coverage takes the cluster
// down. The same holds when the replacement reuses the address under a new id.
func TestGetClusterStateReleasesSlotsOfAReplacedPrimary(t *testing.T) {
	for name, replacementAddress := range map[string]string{"new address": "10.0.0.4", "same address": "10.0.0.3"} {
		t.Run(name, func(t *testing.T) {
			survivorNodes := "abc 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-8191\n" +
				"ghi 10.0.0.3:6379@16379 master,fail? - 0 0 2 connected 8192-16383\n" +
				"jkl " + replacementAddress + ":6379@16379 master - 0 0 3 connected\n"
			survivor := healthyScrape()
			survivor[4] = blobResult(survivorNodes)

			replacementNodes := "jkl " + replacementAddress + ":6379@16379 myself,master - 0 0 3 connected\n" +
				"abc 10.0.0.1:6379@16379 master - 0 0 1 connected 0-8191\n" +
				"ghi 10.0.0.3:6379@16379 master,fail? - 0 0 2 connected 8192-16383\n"
			replacement := []vclient.ValkeyResult{
				blobResult("jkl"),
				blobResult("shard-3"),
				blobResult("role:master\r\n"),
				blobResult("cluster_known_nodes:3\r\ncluster_current_epoch:3\r\ncluster_size:2\r\n"),
				blobResult(replacementNodes),
			}

			state := GetClusterState(context.Background(), []string{"10.0.0.1", replacementAddress}, 6379, dialScrapeByAddress(t, map[string][]vclient.ValkeyResult{
				"10.0.0.1:6379":              survivor,
				replacementAddress + ":6379": replacement,
			}))

			if got, want := state.GetUnassignedSlots(), []SlotsRange{{8192, 16383}}; !reflect.DeepEqual(got, want) {
				t.Fatalf("unassigned slots = %v, want %v: the replaced primary's ranges must be free for the replacement", got, want)
			}
			if got := state.UnscrapedSlotOwnerHosts(); len(got) != 0 {
				t.Fatalf("a primary whose pod is gone must not count as an unscraped slot owner, got %v", got)
			}
			if len(state.PendingNodes) != 1 || state.PendingNodes[0].Id != "jkl" {
				t.Fatalf("the slotless replacement must be pending for slot assignment, got %+v", state.PendingNodes)
			}
		})
	}
}

// A live replica still follows a primary whose pod is gone, or whose address
// now answers as a new id. The ranges stay on the replica's shard. Releasing
// them makes that shard look empty, and scale-in deletes the replica without
// moving its data. Persistence does not promote the replica either.
func TestGetClusterStateKeepsSlotsWhileAReplicaFollowsAMissingPrimary(t *testing.T) {
	scrape := func(id, shard, info, nodes string) []vclient.ValkeyResult {
		return []vclient.ValkeyResult{
			blobResult(id),
			blobResult(shard),
			blobResult(info),
			blobResult("cluster_known_nodes:3\r\ncluster_current_epoch:3\r\ncluster_size:2\r\n"),
			blobResult(nodes),
		}
	}
	survivorNodes := "abc 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-8191\n" +
		"ghi 10.0.0.3:6379@16379 master,fail? - 0 0 2 connected 8192-16383\n" +
		"def 10.0.0.2:6379@16379 slave ghi 0 0 2 connected\n"
	replicaNodes := "ghi 10.0.0.3:6379@16379 master,fail? - 0 0 2 connected 8192-16383\n" +
		"def 10.0.0.2:6379@16379 myself,slave ghi 0 0 2 connected\n" +
		"abc 10.0.0.1:6379@16379 master - 0 0 1 connected 0-8191\n"
	survivor := scrape("abc", "shard-abc", "role:master\r\n", survivorNodes)
	replica := scrape("def", "shard-ghi", "role:slave\r\nmaster_link_status:up\r\n", replicaNodes)

	for _, tc := range []struct {
		name      string
		addresses []string
		replies   map[string][]vclient.ValkeyResult
	}{
		{
			name:      "pod gone",
			addresses: []string{"10.0.0.1", "10.0.0.2"},
			replies: map[string][]vclient.ValkeyResult{
				"10.0.0.1:6379": survivor,
				"10.0.0.2:6379": replica,
			},
		},
		{
			name:      "address reused",
			addresses: []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"},
			replies: map[string][]vclient.ValkeyResult{
				"10.0.0.1:6379": survivor,
				"10.0.0.2:6379": replica,
				"10.0.0.3:6379": scrape("jkl", "shard-new", "role:master\r\n",
					"jkl 10.0.0.3:6379@16379 myself,master - 0 0 3 connected\n"+
						"abc 10.0.0.1:6379@16379 master - 0 0 1 connected 0-8191\n"+
						"ghi 10.0.0.3:6379@16379 master,fail? - 0 0 2 connected 8192-16383\n"+
						"def 10.0.0.2:6379@16379 slave ghi 0 0 2 connected\n"),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := GetClusterState(context.Background(), tc.addresses, 6379, dialScrapeByAddress(t, tc.replies))

			var replicaShard *ShardState
			for _, shard := range state.Shards {
				if shard.Id == "shard-ghi" {
					replicaShard = shard
				}
			}
			if replicaShard == nil {
				t.Fatal("replica shard missing")
			}
			if !reflect.DeepEqual(replicaShard.Slots, []SlotsRange{{8192, 16383}}) {
				t.Fatalf("replica shard slots = %v, want 8192-16383", replicaShard.Slots)
			}
			if replicaShard.GetPrimaryNode() != nil {
				t.Fatal("missing primary must not be invented as a scraped node")
			}
			if !state.PrimaryUnscrapedButServing(replicaShard) {
				t.Fatal("scale-in must requeue: a live replica still holds the missing primary's slots")
			}
			if got := state.GetUnassignedSlots(); len(got) != 0 {
				t.Fatalf("slots held by the replica were reported unassigned: %v", got)
			}
			if got, want := state.UnscrapedSlotOwnerHosts(), []string{"10.0.0.3"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("unscraped owner hosts = %v, want %v", got, want)
			}
		})
	}
}

// A primary that is only unreachable this pass keeps its slots, whatever its
// peers currently flag it, as long as its address is still a pod.
func TestGetClusterStateKeepsSlotsOfAPrimaryItsPeersMerelySuspect(t *testing.T) {
	viewerNodes := "abc 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-8191\n" +
		"ghi 10.0.0.3:6379@16379 master,fail? - 0 0 2 connected 8192-16383\n"
	viewer := healthyScrape()
	viewer[4] = blobResult(viewerNodes)
	busy := healthyScrape()
	busy[0] = valkeyErrorResult("BUSY Valkey is busy running a script")

	state := GetClusterState(context.Background(), []string{"10.0.0.1", "10.0.0.3"}, 6379, dialScrapeByAddress(t, map[string][]vclient.ValkeyResult{
		"10.0.0.1:6379": viewer,
		"10.0.0.3:6379": busy,
	}))

	if got := state.GetUnassignedSlots(); len(got) != 0 {
		t.Fatalf("a suspected primary that is still a pod lost its ranges: %v", got)
	}
	if got, want := state.UnscrapedSlotOwnerHosts(), []string{"10.0.0.3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unscraped owner hosts = %v, want %v", got, want)
	}
}

// Gossip from another shard must hold the busy primary's ranges without
// writing them onto the viewer's shard.
func TestGetClusterStateHoldsSlotsReportedByAnotherShard(t *testing.T) {
	viewerNodes := "abc 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-8191\n" +
		"ghi 10.0.0.3:6379@16379 master - 0 0 2 connected 8192-16383\n"
	viewer := healthyScrape()
	viewer[4] = blobResult(viewerNodes)

	busy := healthyScrape()
	busy[0] = valkeyErrorResult("BUSY Valkey is busy running a script")

	state := GetClusterState(context.Background(), []string{"10.0.0.1", "10.0.0.3"}, 6379, dialScrapeByAddress(t, map[string][]vclient.ValkeyResult{
		"10.0.0.1:6379": viewer,
		"10.0.0.3:6379": busy,
	}))

	if got := state.GetUnassignedSlots(); len(got) != 0 {
		t.Fatalf("slots owned by the busy primary were reported unassigned: %v", got)
	}
	if len(state.Shards) != 1 {
		t.Fatalf("expected only the viewer's shard, got %d", len(state.Shards))
	}
	if !reflect.DeepEqual(state.Shards[0].Slots, []SlotsRange{{0, 8191}}) {
		t.Fatalf("viewer shard absorbed another primary's slots: %v", state.Shards[0].Slots)
	}
	if got, want := state.UnscrapedSlotOwnerHosts(), []string{"10.0.0.3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unscraped owner hosts = %v, want %v", got, want)
	}
}

// Two peers can disagree after a migration: one still lists the old range and
// another lists slots the primary just received. Both have to stay assigned.
func TestGetClusterStateUnionsDisjointReportsOfAnUnscrapedPrimary(t *testing.T) {
	busy := healthyScrape()
	busy[0] = valkeyErrorResult("BUSY Valkey is busy running a script")

	t.Run("other shards", func(t *testing.T) {
		first := []vclient.ValkeyResult{
			blobResult("aaa"),
			blobResult("shard-a"),
			blobResult("role:master\r\n"),
			blobResult("cluster_known_nodes:3\r\ncluster_current_epoch:3\r\n"),
			blobResult("aaa 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-99\n" +
				"bbb 10.0.0.2:6379@16379 master - 0 0 1 connected 151-300\n" +
				"ghi 10.0.0.3:6379@16379 master - 0 0 2 connected 100-150\n"),
		}
		second := []vclient.ValkeyResult{
			blobResult("bbb"),
			blobResult("shard-b"),
			blobResult("role:master\r\n"),
			blobResult("cluster_known_nodes:3\r\ncluster_current_epoch:3\r\n"),
			blobResult("aaa 10.0.0.1:6379@16379 master - 0 0 1 connected 0-99\n" +
				"bbb 10.0.0.2:6379@16379 myself,master - 0 0 1 connected 151-300\n" +
				"ghi 10.0.0.3:6379@16379 master - 0 0 2 connected 400-500\n"),
		}
		state := GetClusterState(context.Background(), []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}, 6379, dialScrapeByAddress(t, map[string][]vclient.ValkeyResult{
			"10.0.0.1:6379": first,
			"10.0.0.2:6379": second,
			"10.0.0.3:6379": busy,
		}))
		free := state.GetUnassignedSlots()
		for _, slot := range []int{100, 150, 400, 500} {
			if slotsContain(free, slot) {
				t.Fatalf("slot %d reported free: %v", slot, free)
			}
		}
		for _, shard := range state.Shards {
			if slotsContain(shard.Slots, 100) || slotsContain(shard.Slots, 400) {
				t.Fatalf("viewer shard absorbed the missing primary's slots: %v", shard.Slots)
			}
		}
	})

	t.Run("replicas", func(t *testing.T) {
		older := []vclient.ValkeyResult{
			blobResult("def"),
			blobResult("shard-1"),
			blobResult("role:slave\r\nmaster_link_status:up\r\n"),
			blobResult("cluster_known_nodes:3\r\ncluster_current_epoch:3\r\n"),
			blobResult("ghi 10.0.0.3:6379@16379 master - 0 0 2 connected 100-150\n" +
				"def 10.0.0.2:6379@16379 myself,slave ghi 0 0 2 connected\n"),
		}
		newer := []vclient.ValkeyResult{
			blobResult("jkl"),
			blobResult("shard-1"),
			blobResult("role:slave\r\nmaster_link_status:up\r\n"),
			blobResult("cluster_known_nodes:3\r\ncluster_current_epoch:3\r\n"),
			blobResult("ghi 10.0.0.3:6379@16379 master - 0 0 2 connected 400-500\n" +
				"jkl 10.0.0.4:6379@16379 myself,slave ghi 0 0 2 connected\n"),
		}
		state := GetClusterState(context.Background(), []string{"10.0.0.2", "10.0.0.4", "10.0.0.3"}, 6379, dialScrapeByAddress(t, map[string][]vclient.ValkeyResult{
			"10.0.0.2:6379": older,
			"10.0.0.4:6379": newer,
			"10.0.0.3:6379": busy,
		}))
		if len(state.Shards) != 1 {
			t.Fatalf("expected one replica shard, got %d", len(state.Shards))
		}
		want := []SlotsRange{{100, 150}, {400, 500}}
		if !reflect.DeepEqual(state.Shards[0].Slots, want) {
			t.Fatalf("shard slots = %v, want union %v", state.Shards[0].Slots, want)
		}
		free := state.GetUnassignedSlots()
		for _, slot := range []int{100, 150, 400, 500} {
			if slotsContain(free, slot) {
				t.Fatalf("slot %d reported free: %v", slot, free)
			}
		}
	})
}

func slotsContain(ranges []SlotsRange, slot int) bool {
	for _, r := range ranges {
		if slot >= r.Start && slot <= r.End {
			return true
		}
	}
	return false
}

// A primary that answered and owns nothing is drained. It joins the shard of
// the replica that names it, and a replica whose table is behind must not put
// those ranges back.
func TestGetClusterStateTrustsAScrapedPrimaryWithNoSlots(t *testing.T) {
	primaryNodes := "abc 10.0.0.1:6379@16379 myself,master - 0 0 1 connected\n" +
		"def 10.0.0.2:6379@16379 slave abc 0 0 1 connected\n"
	primary := healthyScrape()
	primary[4] = blobResult(primaryNodes)

	replicaNodes := "abc 10.0.0.1:6379@16379 master - 0 0 1 connected 0-16383\n" +
		"def 10.0.0.2:6379@16379 myself,slave abc 0 0 1 connected\n"
	replica := []vclient.ValkeyResult{
		blobResult("def"),
		blobResult("shard-1"),
		blobResult("role:slave\r\nmaster_link_status:up\r\n"),
		blobResult("cluster_known_nodes:2\r\ncluster_current_epoch:3\r\n"),
		blobResult(replicaNodes),
	}

	state := GetClusterState(context.Background(), []string{"10.0.0.1", "10.0.0.2"}, 6379, dialScrapeByAddress(t, map[string][]vclient.ValkeyResult{
		"10.0.0.1:6379": primary,
		"10.0.0.2:6379": replica,
	}))

	if len(state.PendingNodes) != 0 {
		t.Fatalf("drained primary should join the replica's shard, still pending: %+v", state.PendingNodes)
	}
	if len(state.Shards) != 1 {
		t.Fatalf("expected the replica shard, got %d shards", len(state.Shards))
	}
	shard := state.Shards[0]
	if shard.GetPrimaryNode() == nil || shard.GetPrimaryNode().Id != "abc" {
		t.Fatalf("drained primary was not adopted, primary id %q", shard.PrimaryId)
	}
	if slotCount(shard.Slots) != 0 {
		t.Fatalf("stale replica view restored slots: %v", shard.Slots)
	}
	if state.PrimaryUnscrapedButServing(shard) {
		t.Fatal("a scraped primary that owns nothing is drained, not still serving")
	}
	if hosts := state.UnscrapedSlotOwnerHosts(); len(hosts) != 0 {
		t.Fatalf("scraped primary reported as unscraped owner: %v", hosts)
	}
	if got := state.GetUnassignedSlots(); !reflect.DeepEqual(got, []SlotsRange{{0, 16383}}) {
		t.Fatalf("unassigned = %v, want the full range", got)
	}
}

func TestGetClusterStateKeepsNodeWhenInfoFails(t *testing.T) {
	results := healthyScrape()
	results[2] = valkeyErrorResult("BUSY Valkey is busy running a script")

	state := GetClusterState(context.Background(), []string{"10.0.0.1"}, 6379, dialScrape(t, results))

	if len(state.Shards) != 1 || len(state.Shards[0].Nodes) != 1 {
		t.Fatalf("expected the node to stay, got %d shards", len(state.Shards))
	}
	node := state.Shards[0].Nodes[0]
	if node.Id != "abc" || node.ShardId != "shard-1" || !node.IsPrimary() {
		t.Fatalf("identity lost: id %q shard %q flags %v", node.Id, node.ShardId, node.Flags)
	}
	if node.Info["master_link_status"] == "up" || node.Info["role"] != "" {
		t.Fatalf("INFO failure should leave replication fields unknown, got %v", node.Info)
	}
}
