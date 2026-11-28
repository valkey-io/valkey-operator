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
	"fmt"
	"strings"
	"testing"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

// Run with:
//
//	go test ./internal/controller/ -run '^$' -bench CountReadyShards -benchmem
//
// A healthy cluster of shards primaries with one replica each. Every node
// holds the full table with its own line marked myself, as in the
// internal/valkey benchCluster fixture. Each op is one status update: a fresh
// ClusterState over the same nodes.
func BenchmarkCountReadyShards(b *testing.B) {
	cluster := &valkeyiov1alpha1.ValkeyCluster{Spec: valkeyiov1alpha1.ValkeyClusterSpec{ReplicasPerShard: 1}}
	r := &ValkeyClusterReconciler{}
	for _, shards := range []int{50, 125, 250, 500} {
		b.Run(fmt.Sprintf("nodes=%d", 2*shards), func(b *testing.B) {
			type member struct{ id, primaryID, address, slots string }
			members := make([]member, 0, 2*shards)
			for s := range shards {
				p := fmt.Sprintf("%040x", 2<<32|s)
				start, end := s*16384/shards, (s+1)*16384/shards-1
				members = append(members,
					member{p, "-", fmt.Sprintf("10.244.%d.%d", s/250, s%250+1), fmt.Sprintf(" %d-%d", start, end)},
					member{fmt.Sprintf("%040x", 3<<32|s), p, fmt.Sprintf("10.245.%d.%d", s/250, s%250+1), ""})
			}
			table := func(self int) string {
				var t strings.Builder
				for j, m := range members {
					flags := "master"
					if m.primaryID != "-" {
						flags = "slave"
					}
					if j == self {
						flags = "myself," + flags
					}
					fmt.Fprintf(&t, "%s %s:6379@16379 %s %s 0 0 1 connected%s\n", m.id, m.address, flags, m.primaryID, m.slots)
				}
				return t.String()
			}
			info := map[string]string{"cluster_size": fmt.Sprint(shards)}
			var shardStates []*valkey.ShardState
			for s := range shards {
				p, rp := members[2*s], members[2*s+1]
				primary := &valkey.NodeState{Id: p.id, Address: p.address, Flags: valkey.FlagMaster,
					ClusterInfo: info, Info: map[string]string{"role": "master"}}
				primary.SetClusterNodesForTesting(table(2 * s))
				replica := &valkey.NodeState{Id: rp.id, Address: rp.address, Flags: valkey.FlagSlave,
					ClusterInfo: info, Info: map[string]string{"role": "slave", "master_link_status": "up"}}
				replica.SetClusterNodesForTesting(table(2*s + 1))
				shardStates = append(shardStates, &valkey.ShardState{
					Id: fmt.Sprintf("%040x", 1<<32|s), PrimaryId: p.id,
					Slots: primary.GetSlots(), Nodes: []*valkey.NodeState{primary, replica},
				})
			}
			var ready int32
			for b.Loop() {
				ready = r.countReadyShards(&valkey.ClusterState{Shards: shardStates}, cluster)
			}
			if int(ready) != shards {
				b.Fatalf("got %d ready shards, want %d", ready, shards)
			}
		})
	}
}
