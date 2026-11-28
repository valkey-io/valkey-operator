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
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

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

	// Not a slot or a range
	for _, bad := range []string{"", "x", "-5", "5-", "1-2-3", "9-3", "0--0"} {
		if _, err := parseSlotsRange(bad); err == nil {
			t.Errorf("Expected an error for %q", bad)
		}
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
					{Address: "10.0.0.1", Id: "node-1", Flags: FlagMyself | FlagMaster},
					{Address: "10.0.0.2", Id: "node-2", Flags: FlagSlave},
				},
			},
			{
				Id:        "shard-2",
				PrimaryId: "node-3",
				Nodes: []*NodeState{
					{Address: "10.0.0.3", Id: "node-3", Flags: FlagMaster},
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
		Flags:       FlagMyself | FlagMaster,
		Info:        map[string]string{"role": "master"},
		ClusterInfo: map[string]string{"cluster_size": "1"},
	}
	syncedReplica := &NodeState{
		Id:      "replica-1-id",
		Address: "10.0.0.2",
		Flags:   FlagSlave,
		Info:    map[string]string{"role": "slave", "master_link_status": "up"},
	}
	unsyncedReplica := &NodeState{
		Id:      "replica-2-id",
		Address: "10.0.0.3",
		Flags:   FlagSlave,
		Info:    map[string]string{"role": "slave", "master_link_status": "down"},
	}
	// A replica whose own scrape looks healthy: its link is up and its own
	// CLUSTER NODES entry, like every node's own entry, carries no failure
	// flag. Only the primary's view of the cluster says it is failing.
	partitionedReplica := &NodeState{
		Id:      "replica-3-id",
		Address: "10.0.0.4",
		Flags:   FlagSlave,
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
		Flags:   FlagMyself | FlagMaster,
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
					{Id: "primary1", Flags: FlagMaster, nodes: ParseClusterNodes("primary1 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-5461\n")},
					{Id: "r1", Flags: FlagSlave, Info: map[string]string{"slave_repl_offset": "100"}, nodes: ParseClusterNodes("r1 10.0.0.2:6379@16379 myself,slave primary1 0 0 1 connected\n")},
					{Id: "r2", Flags: FlagSlave, Info: map[string]string{"slave_repl_offset": "500"}, nodes: ParseClusterNodes("r2 10.0.0.3:6379@16379 myself,slave primary1 0 0 1 connected\n")},
					{Id: "r3", Flags: FlagSlave, Info: map[string]string{"slave_repl_offset": "200"}, nodes: ParseClusterNodes("r3 10.0.0.4:6379@16379 myself,slave primary1 0 0 1 connected\n")},
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

func TestNodeState_SeesSlotsOwnedBy(t *testing.T) {
	node := &NodeState{}
	node.nodes = ParseClusterNodes("abc123 10.0.0.1:6379@16379 myself,master - 0 0 1 connected\n" +
		"def456 10.0.0.2:6379@16379 master - 0 0 2 connected 0-99 200-299\n" +
		"ghi789 10.0.0.3:6379@16379 master - 0 0 0 connected\n")

	cases := []struct {
		name  string
		id    string
		slots []int
		want  bool
	}{
		{"every slot in the peer's ranges", "def456", []int{0, 99, 200, 299}, true},
		{"one slot outside the peer's ranges", "def456", []int{0, 150}, false},
		// Gossip introduced the peer but not yet its slots.
		{"known peer with no slots", "ghi789", []int{0}, false},
		{"unknown peer", "zzz000", []int{0}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := node.SeesSlotsOwnedBy(tc.id, tc.slots); got != tc.want {
				t.Errorf("SeesSlotsOwnedBy(%q, %v) = %v, want %v", tc.id, tc.slots, got, tc.want)
			}
		})
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
		return &NodeState{Id: id, Flags: FlagMyself | FlagMaster, ClusterInfo: size("3"),
			nodes: ParseClusterNodes(id + " 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-100\n" + view)}
	}
	replica := func(id, view string) *NodeState {
		return &NodeState{Id: id, Flags: FlagMyself | FlagSlave, ClusterInfo: size("3"),
			nodes: ParseClusterNodes(id + " 10.0.0.1:6379@16379 myself,slave p1 0 0 1 connected\n" + view)}
	}
	target := &NodeState{Id: "target", Flags: FlagMyself | FlagSlave,
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
	primary := &NodeState{Id: "p", Flags: FlagMyself | FlagMaster, Info: map[string]string{"role": "master"},
		ClusterInfo: map[string]string{"cluster_size": "1"}}
	r1 := &NodeState{Id: "r1", Flags: FlagSlave, Info: map[string]string{"role": "slave", "master_link_status": "up"}}
	r2 := &NodeState{Id: "r2", Flags: FlagSlave, Info: map[string]string{"role": "slave", "master_link_status": "up"}}
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

// recordingDial returns a dial that fails every address and records it, in
// whatever order the scrape's goroutines reach it.
func recordingDial() (DialFunc, func() []string) {
	var mu sync.Mutex
	var dialled []string
	dial := func(_ context.Context, address string) (vclient.Client, error) {
		mu.Lock()
		defer mu.Unlock()
		dialled = append(dialled, address)
		return nil, errors.New("connection refused")
	}
	return dial, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Sorted(slices.Values(dialled))
	}
}

func TestGetClusterStateSkipsNodesThatFailToDial(t *testing.T) {
	dial, dialled := recordingDial()

	state := GetClusterState(context.Background(), []string{"10.0.0.1", "10.0.0.2"}, 6379, dial)

	if want := []string{"10.0.0.1:6379", "10.0.0.2:6379"}; !reflect.DeepEqual(dialled(), want) {
		t.Fatalf("dialled %v, want %v", dialled(), want)
	}
	if len(state.Shards) != 0 || len(state.PendingNodes) != 0 {
		t.Fatalf("expected an empty state, got %d shards and %d pending nodes", len(state.Shards), len(state.PendingNodes))
	}
}

func TestGetClusterStateBracketsIPv6Addresses(t *testing.T) {
	dial, dialled := recordingDial()

	GetClusterState(context.Background(), []string{"fd00::1", "10.0.0.2"}, 6379, dial)

	if want := []string{"10.0.0.2:6379", "[fd00::1]:6379"}; !reflect.DeepEqual(dialled(), want) {
		t.Fatalf("dialled %v, want %v", dialled(), want)
	}
}

// Dead addresses each cost a full dial timeout. Dialled one at a time, k of
// them cost k timeouts; dialled together, up to scrapeConcurrency share one.
func TestGetClusterStateDialsConcurrentlyUpToTheCap(t *testing.T) {
	addresses := make([]string, 40)
	for i := range addresses {
		addresses[i] = fmt.Sprintf("10.0.0.%d", i+1)
	}
	var mu sync.Mutex
	active, peak, dialled := 0, 0, 0
	full := make(chan struct{})
	var fullOnce sync.Once
	release := make(chan struct{})
	var dial DialFunc = func(_ context.Context, _ string) (vclient.Client, error) {
		mu.Lock()
		active++
		dialled++
		peak = max(peak, active)
		// Queued dials can refill the slots after release; signal only the first.
		if active == scrapeConcurrency {
			fullOnce.Do(func() { close(full) })
		}
		mu.Unlock()
		<-release
		mu.Lock()
		active--
		mu.Unlock()
		return nil, errors.New("dial timeout")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		GetClusterState(context.Background(), addresses, 6379, dial)
	}()

	select {
	case <-full:
	case <-time.After(2 * time.Second):
		close(release)
		<-done
		t.Fatalf("only %d dials in flight at once, want %d", peak, scrapeConcurrency)
	}
	// Give a dial past the cap time to enter before releasing them all.
	time.Sleep(50 * time.Millisecond)
	close(release)
	<-done

	if dialled != len(addresses) {
		t.Errorf("dialled %d addresses, want %d", dialled, len(addresses))
	}
	if peak != scrapeConcurrency {
		t.Errorf("%d dials in flight at once, want at most %d", peak, scrapeConcurrency)
	}
}

// A panic while scraping one node reaches the caller, as it did when dials ran
// on the caller's goroutine: controller-runtime recovers it there, where a
// panic on another goroutine would end the process.
func TestGetClusterStatePassesAPanicToTheCaller(t *testing.T) {
	dial := func(_ context.Context, address string) (vclient.Client, error) {
		if address == "10.0.0.2:6379" {
			panic("bad reply")
		}
		return nil, errors.New("connection refused")
	}

	defer func() {
		if r := recover(); r != "bad reply" {
			t.Errorf("recovered %v, want the scrape's panic", r)
		}
	}()
	GetClusterState(context.Background(), []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}, 6379, dial)
	t.Error("GetClusterState returned, want a panic")
}

// scrapedNode returns a client that answers the scrape as the node with the
// given id, shard id and CLUSTER NODES "myself" line.
func scrapedNode(ctrl *gomock.Controller, id, shardID, myself string) vclient.Client {
	client := vmock.NewClient(ctrl)
	client.EXPECT().
		DoMulti(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]vclient.ValkeyResult{
			vmock.Result(vmock.ValkeyString(id)),
			vmock.Result(vmock.ValkeyString(shardID)),
			vmock.Result(vmock.ValkeyString("")),
			// An empty CLUSTER INFO drops the node from the scrape.
			vmock.Result(vmock.ValkeyString("cluster_state:ok\r\n")),
			vmock.Result(vmock.ValkeyString(myself + "\n")),
		})
	return client
}

// The state is built in address order, whatever order the nodes answer in:
// findMeetTarget and HighestOffsetReplica depend on it.
func TestGetClusterStateKeepsInputOrder(t *testing.T) {
	ctrl := gomock.NewController(t)
	addresses := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"}
	clients := map[string]vclient.Client{
		"10.0.0.1:6379": scrapedNode(ctrl, "r1", "s1", "r1 10.0.0.1:6379@16379 myself,slave p1 0 0 1 connected"),
		"10.0.0.2:6379": scrapedNode(ctrl, "p2", "s2", "p2 10.0.0.2:6379@16379 myself,master - 0 0 2 connected 0-8191"),
		"10.0.0.3:6379": scrapedNode(ctrl, "p3", "s3", "p3 10.0.0.3:6379@16379 myself,master - 0 0 3 connected"),
		"10.0.0.4:6379": scrapedNode(ctrl, "p1", "s1", "p1 10.0.0.4:6379@16379 myself,master - 0 0 1 connected 8192-16383"),
	}
	// Each dial returns only after the next address's dial has, so answers
	// arrive in reverse order. The fallback keeps a serial scrape from hanging.
	done := make([]chan struct{}, len(addresses))
	for i := range done {
		done[i] = make(chan struct{})
	}
	dial := func(_ context.Context, address string) (vclient.Client, error) {
		i := slices.Index(addresses, strings.TrimSuffix(address, ":6379"))
		if i+1 < len(done) {
			select {
			case <-done[i+1]:
			case <-time.After(100 * time.Millisecond):
			}
		}
		close(done[i])
		return clients[address], nil
	}

	state := GetClusterState(context.Background(), addresses, 6379, dial)

	addressesOf := func(nodes []*NodeState) []string {
		out := make([]string, 0, len(nodes))
		for _, n := range nodes {
			out = append(out, n.Address)
		}
		return out
	}
	if len(state.Shards) != 2 || state.Shards[0].Id != "s1" || state.Shards[1].Id != "s2" {
		t.Fatalf("shards out of input order: %v", state.Shards)
	}
	if got, want := addressesOf(state.Shards[0].Nodes), []string{"10.0.0.1", "10.0.0.4"}; !reflect.DeepEqual(got, want) {
		t.Errorf("shard s1 nodes %v, want %v", got, want)
	}
	if got, want := addressesOf(state.PendingNodes), []string{"10.0.0.3"}; !reflect.DeepEqual(got, want) {
		t.Errorf("pending nodes %v, want %v", got, want)
	}

	empty := GetClusterState(context.Background(), nil, 6379, dial)
	if len(empty.Shards) != 0 || len(empty.PendingNodes) != 0 {
		t.Errorf("expected an empty state for no addresses, got %d shards and %d pending nodes", len(empty.Shards), len(empty.PendingNodes))
	}
}

// A pooled client can be stale: Get returns it without redialling, and the
// server has gone away since it was last used. getNodeState must not turn
// that into a ghost node with empty Id/Flags/ShardId.
func TestGetClusterStateSkipsAStaleClientThatFailsTheRoundTrip(t *testing.T) {
	ctrl := gomock.NewController(t)
	stale := vmock.NewClient(ctrl)
	stale.EXPECT().
		DoMulti(gomock.Any(),
			vmock.Match("CLUSTER", "MYID"), vmock.Match("CLUSTER", "MYSHARDID"),
			vmock.Match("INFO", "persistence", "replication"), vmock.Match("CLUSTER", "INFO"),
			vmock.Match("CLUSTER", "NODES")).
		Return([]vclient.ValkeyResult{vmock.ErrorResult(errors.New("io: read/write on closed pipe"))})

	dial := func(_ context.Context, _ string) (vclient.Client, error) {
		return stale, nil
	}

	state := GetClusterState(context.Background(), []string{"10.0.0.1"}, 6379, dial)

	if len(state.Shards) != 0 || len(state.PendingNodes) != 0 {
		t.Fatalf("expected an empty state, got %d shards and %d pending nodes", len(state.Shards), len(state.PendingNodes))
	}
}

func TestGetClusterStateKeepsPersistenceAndReplicationInfo(t *testing.T) {
	ctrl := gomock.NewController(t)
	client := vmock.NewClient(ctrl)
	// A Valkey 9 replica reply, trimmed. Valkey orders the sections itself.
	info := "# Persistence\r\nloading:1\r\nasync_loading:0\r\n\r\n" +
		"# Replication\r\nrole:slave\r\nmaster_link_status:down\r\nmaster_sync_in_progress:0\r\nslave_repl_offset:3108\r\n"
	client.EXPECT().
		DoMulti(gomock.Any(),
			vmock.Match("CLUSTER", "MYID"), vmock.Match("CLUSTER", "MYSHARDID"),
			vmock.Match("INFO", "persistence", "replication"), vmock.Match("CLUSTER", "INFO"),
			vmock.Match("CLUSTER", "NODES")).
		Return([]vclient.ValkeyResult{
			vmock.Result(vmock.ValkeyString("r1")),
			vmock.Result(vmock.ValkeyString("s1")),
			vmock.Result(vmock.ValkeyBlobString(info)),
			vmock.Result(vmock.ValkeyString("cluster_state:ok\r\n")),
			vmock.Result(vmock.ValkeyString("r1 10.0.0.1:6379@16379 myself,slave p1 0 0 1 connected\n")),
		})
	dial := func(_ context.Context, _ string) (vclient.Client, error) {
		return client, nil
	}

	state := GetClusterState(context.Background(), []string{"10.0.0.1"}, 6379, dial)

	if len(state.Shards) != 1 || len(state.Shards[0].Nodes) != 1 {
		t.Fatalf("expected one shard with one node, got %+v", state.Shards)
	}
	got := state.Shards[0].Nodes[0].Info
	want := map[string]string{
		"loading":                 "1",
		"master_link_status":      "down",
		"master_sync_in_progress": "0",
		"slave_repl_offset":       "3108",
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("Info[%q] = %q, want %q", key, got[key], value)
		}
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

func dialScrapeByAddress(t *testing.T, byAddr map[string][]vclient.ValkeyResult) DialFunc {
	t.Helper()
	ctrl := gomock.NewController(t)
	return func(_ context.Context, address string) (vclient.Client, error) {
		results, ok := byAddr[address]
		if !ok {
			return nil, fmt.Errorf("unexpected dial %s", address)
		}
		c := vmock.NewClient(ctrl)
		c.EXPECT().
			DoMulti(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(results)
		return c, nil
	}
}

// A primary dropped from the scrape at one of its pod addresses can still be
// serving the ranges a scraped node's table gives it, so they are not
// assignable. GetUnassignedSlots still counts them.
func TestAssignableSlotsLeavesOutAnUnscrapedPrimarysRanges(t *testing.T) {
	viewer := func(ghiHost string) []vclient.ValkeyResult {
		r := healthyScrape()
		r[4] = blobResult("abc 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 4096-16383\n" +
			"ghi " + ghiHost + ":6379@16379 master - 0 0 2 connected 0-4095\n")
		return r
	}
	dropped := healthyScrape()
	dropped[0] = blobResult("ghi")
	dropped[3] = valkeyErrorResult("NOPERM this user has no permissions to run the 'cluster|info' command")
	replacement := healthyScrape()
	replacement[0] = blobResult("new")
	replacement[1] = blobResult("shard-9")
	replacement[4] = blobResult("new 10.0.0.3:6379@16379 myself,master - 0 0 3 connected\n")

	for _, tc := range []struct {
		name       string
		addresses  []string
		byAddr     map[string][]vclient.ValkeyResult
		assignable []SlotsRange
	}{
		{"held while the dropped primary's address is a pod", []string{"10.0.0.1", "10.0.0.3"},
			map[string][]vclient.ValkeyResult{"10.0.0.1:6379": viewer("10.0.0.3"), "10.0.0.3:6379": dropped}, nil},
		{"assignable once that address is no longer a pod", []string{"10.0.0.1"},
			map[string][]vclient.ValkeyResult{"10.0.0.1:6379": viewer("10.0.0.9")}, []SlotsRange{{0, 4095}}},
		{"assignable once that address answered as another id", []string{"10.0.0.1", "10.0.0.3"},
			map[string][]vclient.ValkeyResult{"10.0.0.1:6379": viewer("10.0.0.3"), "10.0.0.3:6379": replacement}, []SlotsRange{{0, 4095}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := GetClusterState(context.Background(), tc.addresses, 6379, dialScrapeByAddress(t, tc.byAddr))
			if got, want := state.GetUnassignedSlots(), []SlotsRange{{0, 4095}}; !reflect.DeepEqual(got, want) {
				t.Fatalf("unassigned slots = %v, want %v", got, want)
			}
			if got := state.AssignableSlots(); !reflect.DeepEqual(got, tc.assignable) {
				t.Fatalf("assignable slots = %v, want %v", got, tc.assignable)
			}
		})
	}
}

// A scraped replica that still follows a primary this pass did not reach holds
// that primary's keys and can still be promoted with its slots, so the ranges
// are not assignable whatever happened to the primary's address. A replica of
// another primary does not hold them.
func TestAssignableSlotsHoldsTheRangesOfAPrimaryAReplicaFollows(t *testing.T) {
	viewer := func(ghiHost string) []vclient.ValkeyResult {
		r := healthyScrape()
		r[4] = blobResult("abc 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 4096-16383\n" +
			"ghi " + ghiHost + ":6379@16379 master - 0 0 2 connected 0-4095\n" +
			"def 10.0.0.2:6379@16379 slave ghi 0 0 2 connected\n")
		return r
	}
	replicaOf := func(primary, ghiHost string) []vclient.ValkeyResult {
		return []vclient.ValkeyResult{
			blobResult("def"),
			blobResult("shard-2"),
			blobResult("role:slave\r\nmaster_link_status:down\r\n"),
			blobResult("cluster_known_nodes:3\r\ncluster_current_epoch:3\r\n"),
			blobResult("def 10.0.0.2:6379@16379 myself,slave " + primary + " 0 0 2 connected\n" +
				"ghi " + ghiHost + ":6379@16379 master - 0 0 2 connected 0-4095\n" +
				"abc 10.0.0.1:6379@16379 master - 0 0 1 connected 4096-16383\n"),
		}
	}
	replacement := healthyScrape()
	replacement[0] = blobResult("new")
	replacement[1] = blobResult("shard-9")
	replacement[4] = blobResult("new 10.0.0.3:6379@16379 myself,master - 0 0 3 connected\n")
	// The follower's own scrape fails, so only the viewer's table lists it.
	followerUnscraped := replicaOf("ghi", "10.0.0.3")
	followerUnscraped[3] = valkeyErrorResult("NOPERM this user has no permissions to run the 'cluster|info' command")
	// A node that answers at the follower's address under another id.
	followerReplaced := healthyScrape()
	followerReplaced[0] = blobResult("xyz")
	followerReplaced[1] = blobResult("shard-8")
	followerReplaced[4] = blobResult("xyz 10.0.0.2:6379@16379 myself,master - 0 0 4 connected\n")
	// A node that joined after ghi went away lists it, and its follower,
	// without ghi's slots: only ghi's own shard learned them.
	newcomer := func() []vclient.ValkeyResult {
		r := healthyScrape()
		r[4] = blobResult("abc 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 4096-16383\n" +
			"ghi 10.0.0.3:6379@16379 master - 0 0 2 connected\n" +
			"def 10.0.0.2:6379@16379 slave ghi 0 0 2 connected\n")
		return r
	}

	for _, tc := range []struct {
		name       string
		addresses  []string
		byAddr     map[string][]vclient.ValkeyResult
		assignable []SlotsRange
	}{
		{"held while the follower's own scrape fails", []string{"10.0.0.1", "10.0.0.2"},
			map[string][]vclient.ValkeyResult{
				"10.0.0.1:6379": viewer("10.0.0.3"),
				"10.0.0.2:6379": followerUnscraped,
			}, nil},
		{"held when no scraped table knows the slots of the primary an unscraped follower follows", []string{"10.0.0.1", "10.0.0.2"},
			map[string][]vclient.ValkeyResult{
				"10.0.0.1:6379": newcomer(),
				"10.0.0.2:6379": followerUnscraped,
			}, nil},
		{"assignable when the unscraped follower of a primary with unknown slots is no longer a pod", []string{"10.0.0.1"},
			map[string][]vclient.ValkeyResult{
				"10.0.0.1:6379": newcomer(),
			}, []SlotsRange{{0, 4095}}},
		{"a scraped primary without slots does not hold the free ranges for its replica", []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"},
			map[string][]vclient.ValkeyResult{
				"10.0.0.1:6379": viewer("10.0.0.9"),
				"10.0.0.2:6379": replicaOf("new", "10.0.0.9"),
				"10.0.0.3:6379": replacement,
			}, []SlotsRange{{0, 4095}}},
		{"assignable once the unscraped follower's address is no longer a pod", []string{"10.0.0.1"},
			map[string][]vclient.ValkeyResult{
				"10.0.0.1:6379": viewer("10.0.0.3"),
			}, []SlotsRange{{0, 4095}}},
		{"assignable once the follower's address answered as another id", []string{"10.0.0.1", "10.0.0.2"},
			map[string][]vclient.ValkeyResult{
				"10.0.0.1:6379": viewer("10.0.0.3"),
				"10.0.0.2:6379": followerReplaced,
			}, []SlotsRange{{0, 4095}}},
		{"a scraped follower's own view wins over a peer's table", []string{"10.0.0.1", "10.0.0.2"},
			map[string][]vclient.ValkeyResult{
				"10.0.0.1:6379": viewer("10.0.0.3"),
				"10.0.0.2:6379": replicaOf("abc", "10.0.0.3"),
			}, []SlotsRange{{0, 4095}}},
		{"held once the primary's pod is gone", []string{"10.0.0.1", "10.0.0.2"},
			map[string][]vclient.ValkeyResult{
				"10.0.0.1:6379": viewer("10.0.0.3"),
				"10.0.0.2:6379": replicaOf("ghi", "10.0.0.3"),
			}, nil},
		{"held once the primary's address answered as another id", []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"},
			map[string][]vclient.ValkeyResult{
				"10.0.0.1:6379": viewer("10.0.0.3"),
				"10.0.0.2:6379": replicaOf("ghi", "10.0.0.3"),
				"10.0.0.3:6379": replacement,
			}, nil},
		{"assignable when the replica follows another primary", []string{"10.0.0.1", "10.0.0.2"},
			map[string][]vclient.ValkeyResult{
				"10.0.0.1:6379": viewer("10.0.0.3"),
				"10.0.0.2:6379": replicaOf("abc", "10.0.0.3"),
			}, []SlotsRange{{0, 4095}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := GetClusterState(context.Background(), tc.addresses, 6379, dialScrapeByAddress(t, tc.byAddr))
			if got, want := state.GetUnassignedSlots(), []SlotsRange{{0, 4095}}; !reflect.DeepEqual(got, want) {
				t.Fatalf("unassigned slots = %v, want %v", got, want)
			}
			if got := state.AssignableSlots(); !reflect.DeepEqual(got, tc.assignable) {
				t.Fatalf("assignable slots = %v, want %v", got, tc.assignable)
			}
		})
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

// Every scraped node lists every member, so the scrape keeps one copy of each
// id and host for all the tables rather than one per table.
func TestGetClusterStateSharesPeerStrings(t *testing.T) {
	addresses, dial := benchCluster(gomock.NewController(t), 3)
	state := GetClusterState(context.Background(), addresses, 6379, dial)
	nodes := state.AllNodes()
	if len(nodes) != 6 {
		t.Fatalf("got %d nodes, want 6", len(nodes))
	}
	first := nodes[0].nodes
	for _, node := range nodes[1:] {
		for i, entry := range node.nodes {
			for _, pair := range [][2]string{{entry.Id, first[i].Id}, {entry.Host, first[i].Host}, {entry.PrimaryId, first[i].PrimaryId}} {
				if unsafe.StringData(pair[0]) != unsafe.StringData(pair[1]) {
					t.Fatalf("%s's entry %d holds its own copy of %q", node.Address, i, pair[0])
				}
			}
		}
	}
}
