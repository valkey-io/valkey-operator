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

import "testing"

const threeShardSource = "c 10.0.0.3:6379@16379 master - 0 1 3 connected 10923-16383\n" +
	"a 10.0.0.1:6379@16379 myself,master - 0 1 1 connected 0-5460\n" +
	"b 10.0.0.2:6379@16379 master - 0 1 2 connected 5461-10922\n" +
	"a2 10.0.0.4:6379@16379 slave a 0 1 1 connected\n"

func TestPlanBootstrapSource(t *testing.T) {
	t.Run("orders shards by first slot and ignores replicas", func(t *testing.T) {
		shards, err := PlanBootstrapSource(ParseClusterNodes(threeShardSource), 3)
		if err != nil {
			t.Fatal(err)
		}
		for i, want := range []string{"a", "b", "c"} {
			if shards[i].PrimaryId != want {
				t.Errorf("shard %d: want primary %s, got %s", i, want, shards[i].PrimaryId)
			}
		}
	})

	t.Run("rejects shard count mismatch", func(t *testing.T) {
		if _, err := PlanBootstrapSource(ParseClusterNodes(threeShardSource), 2); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("rejects incomplete slot coverage", func(t *testing.T) {
		raw := "a 10.0.0.1:6379@16379 myself,master - 0 1 1 connected 0-5460\n"
		if _, err := PlanBootstrapSource(ParseClusterNodes(raw), 1); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("rejects failing primary", func(t *testing.T) {
		raw := "a 10.0.0.1:6379@16379 myself,master,fail - 0 1 1 connected 0-16383\n"
		if _, err := PlanBootstrapSource(ParseClusterNodes(raw), 1); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("rejects slots in flight", func(t *testing.T) {
		raw := "a 10.0.0.1:6379@16379 myself,master - 0 1 1 connected 0-16383 [5->-b]\n"
		if _, err := PlanBootstrapSource(ParseClusterNodes(raw), 1); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("rejects empty source", func(t *testing.T) {
		if _, err := PlanBootstrapSource(nil, 1); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestClusterNodesEntryCarriesItsOwnClientPort(t *testing.T) {
	nodes := ParseClusterNodes("" +
		"a 10.0.0.1:6379@16379 myself,master - 0 1 1 connected 0-5460\n" +
		"b 10.0.0.2:7000@17000,host-b slave a 0 1 1 connected\n" +
		"c fd00::2:6380@16380 slave a 0 1 1 connected\n" +
		"d :0@0 slave a 0 1 1 disconnected\n")
	if len(nodes) != 4 {
		t.Fatalf("parsed %d nodes, want 4", len(nodes))
	}
	for i, want := range []int{6379, 7000, 6380, 0} {
		if nodes[i].Port != want {
			t.Errorf("node %s: Port = %d, want %d", nodes[i].Id, nodes[i].Port, want)
		}
	}
}
