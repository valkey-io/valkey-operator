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
	"context"
	"fmt"
	"io"
	"testing"

	"go.uber.org/mock/gomock"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// Run with:
//
//	go test ./internal/valkey/ -run '^$' -bench IsNodeFailedByMajority -benchmem

// everyTenthReplica returns the replica ID of every tenth shard.
func everyTenthReplica(s *ClusterState) map[string]bool {
	ids := make(map[string]bool)
	for i, shard := range s.Shards {
		if i%10 != 0 {
			continue
		}
		for _, n := range shard.Nodes {
			if n.Id != shard.PrimaryId {
				ids[n.Id] = true
			}
		}
	}
	return ids
}

// flagEntries sets flag on every entry for an ID in targets, in the tables of
// the viewers keep selects.
func flagEntries(s *ClusterState, targets map[string]bool, flag NodeFlags, keep func(*NodeState) bool) {
	for _, n := range s.AllNodes() {
		if !keep(n) {
			continue
		}
		for i := range n.nodes {
			if targets[n.nodes[i].Id] && !n.nodes[i].IsMyself() {
				n.nodes[i].Flags |= flag
			}
		}
	}
}

// majorityScenarios flag entries in the scraped tables after the scrape, the
// way a degraded cluster would report them.
var majorityScenarios = []struct {
	name  string
	apply func(s *ClusterState)
}{
	{"healthy", func(*ClusterState) {}},
	{
		// One primary cut off from the bus marks every peer fail?. Nobody
		// reaches the quorum, so every node is still checked.
		"partitioned-viewer", func(s *ClusterState) {
			all := make(map[string]bool)
			for _, n := range s.AllNodes() {
				all[n.Id] = true
			}
			viewer := s.Shards[0].GetPrimaryNode()
			flagEntries(s, all, FlagPFail, func(n *NodeState) bool { return n == viewer })
		},
	},
	{
		// Every primary suspects one replica in ten shards: a quorum of fail?.
		"pfail-majority", func(s *ClusterState) {
			flagEntries(s, everyTenthReplica(s), FlagPFail, (*NodeState).IsPrimary)
		},
	},
	{
		// Every node holds a confirmed fail for one replica in ten shards.
		"fail-confirmed", func(s *ClusterState) {
			flagEntries(s, everyTenthReplica(s), FlagFail, func(*NodeState) bool { return true })
		},
	},
}

// scrapedState scrapes the benchCluster fixture once per size.
func scrapedState(b *testing.B, shards int) *ClusterState {
	ctrl := gomock.NewController(b)
	addresses, dial := benchCluster(ctrl, shards)
	state := GetClusterState(context.Background(), addresses, 6379, dial)
	if len(state.Shards) != shards {
		b.Fatalf("got %d shards, want %d", len(state.Shards), shards)
	}
	return state
}

// BenchmarkIsNodeFailedByMajority is one call on a fresh state, cycling
// through every scraped node ID: the first call a reconcile makes.
func BenchmarkIsNodeFailedByMajority(b *testing.B) {
	logf.SetLogger(zap.New(zap.WriteTo(io.Discard)))
	for _, sc := range majorityScenarios {
		for _, shards := range []int{50, 125, 250, 500} {
			b.Run(fmt.Sprintf("%s/nodes=%d", sc.name, 2*shards), func(b *testing.B) {
				base := scrapedState(b, shards)
				sc.apply(base)
				nodes := base.AllNodes()
				i := 0
				for b.Loop() {
					// A new state, as the next reconcile's scrape builds.
					fresh := &ClusterState{Shards: base.Shards}
					fresh.IsNodeFailedByMajority(nodes[i%len(nodes)].Id)
					i++
				}
			})
		}
	}
}
