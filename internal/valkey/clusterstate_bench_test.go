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
	"runtime"
	"strings"
	"testing"

	vclient "github.com/valkey-io/valkey-go"
	vmock "github.com/valkey-io/valkey-go/mock"
	"go.uber.org/mock/gomock"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// Run with:
//
//	go test ./internal/valkey/ -run '^$' -bench GetClusterState -benchmem
//
// B/op is what one scrape allocates. retained-MB is the heap still reachable
// while the returned state is held, which is what decides the OOM point during
// a scale-up: the cluster reconcile keeps the state for its whole pass.

// benchNode answers the scrape as one node of a synthetic cluster. It embeds a
// mock client for B() and answers DoMulti itself, so gomock's own allocations
// stay out of the measurement.
type benchNode struct {
	*vmock.Client
	id, shardID, info, clusterInfo, nodes string
}

// DoMulti returns fresh copies of the replies, as a real connection does: the
// scrape keeps substrings of them, so a shared reply would hide what a held
// state retains.
func (n *benchNode) DoMulti(_ context.Context, _ ...vclient.Completed) []vclient.ValkeyResult {
	return []vclient.ValkeyResult{
		vmock.Result(vmock.ValkeyBlobString(strings.Clone(n.id))),
		vmock.Result(vmock.ValkeyBlobString(strings.Clone(n.shardID))),
		vmock.Result(vmock.ValkeyBlobString(strings.Clone(n.info))),
		vmock.Result(vmock.ValkeyBlobString(strings.Clone(n.clusterInfo))),
		vmock.Result(vmock.ValkeyBlobString(strings.Clone(n.nodes))),
	}
}

// benchCluster builds a cluster of shards primaries with one replica each, the
// shape the operator's scale-up scenarios create. Every node reports the same
// CLUSTER NODES table, with its own line marked myself.
func benchCluster(ctrl *gomock.Controller, shards int) ([]string, DialFunc) {
	type member struct{ id, shardID, address, primaryID, slots string }
	members := make([]member, 0, 2*shards)
	for s := range shards {
		shardID := fmt.Sprintf("%040x", 1<<32|s)
		primaryID := fmt.Sprintf("%040x", 2<<32|s)
		start, end := s*16384/shards, (s+1)*16384/shards-1
		members = append(members,
			member{primaryID, shardID, fmt.Sprintf("10.244.%d.%d", s/250, s%250+1), "-",
				fmt.Sprintf("%d-%d", start, end)},
			member{fmt.Sprintf("%040x", 3<<32|s), shardID,
				fmt.Sprintf("10.245.%d.%d", s/250, s%250+1), primaryID, ""})
	}

	line := func(m member, myself bool) string {
		flags := "master"
		if m.primaryID != "-" {
			flags = "slave"
		}
		if myself {
			flags = "myself," + flags
		}
		l := fmt.Sprintf("%s %s:6379@16379 %s %s 0 1763650000000 %d connected",
			m.id, m.address, flags, m.primaryID, 1)
		if m.slots != "" {
			l += " " + m.slots
		}
		return l + "\n"
	}

	clusterInfo := fmt.Sprintf("cluster_enabled:1\r\ncluster_state:ok\r\ncluster_slots_assigned:16384\r\n"+
		"cluster_slots_ok:16384\r\ncluster_slots_pfail:0\r\ncluster_slots_fail:0\r\n"+
		"cluster_known_nodes:%d\r\ncluster_size:%d\r\ncluster_current_epoch:%d\r\ncluster_my_epoch:1\r\n"+
		"cluster_stats_messages_sent:123456\r\ncluster_stats_messages_received:123456\r\n"+
		"total_cluster_links_buffer_limit_exceeded:0\r\n", len(members), shards, shards)

	clients := make(map[string]vclient.Client, len(members))
	addresses := make([]string, 0, len(members))
	for i, m := range members {
		var table strings.Builder
		for j, other := range members {
			table.WriteString(line(other, i == j))
		}
		role := "role:master\r\nconnected_slaves:1\r\n"
		if m.primaryID != "-" {
			role = "role:slave\r\nmaster_host:10.244.0.1\r\nmaster_port:6379\r\nmaster_link_status:up\r\n" +
				"master_last_io_seconds_ago:1\r\nmaster_sync_in_progress:0\r\nslave_read_repl_offset:31080\r\n" +
				"slave_repl_offset:31080\r\nslave_priority:100\r\nslave_read_only:1\r\nreplica_announced:1\r\n"
		}
		info := "# Persistence\r\nloading:0\r\nasync_loading:0\r\ncurrent_cow_peak:0\r\ncurrent_cow_size:0\r\n" +
			"rdb_changes_since_last_save:0\r\nrdb_bgsave_in_progress:0\r\nrdb_last_save_time:1763650000\r\n" +
			"rdb_last_bgsave_status:ok\r\naof_enabled:0\r\naof_rewrite_in_progress:0\r\n" +
			"aof_last_bgrewrite_status:ok\r\naof_last_write_status:ok\r\nmodule_fork_in_progress:0\r\n\r\n" +
			"# Replication\r\n" + role + "master_failover_state:no-failover\r\n" +
			"master_replid:" + strings.Repeat("a", 40) + "\r\nmaster_repl_offset:31080\r\n" +
			"repl_backlog_active:1\r\nrepl_backlog_size:10485760\r\n"
		clients[m.address+":6379"] = &benchNode{
			Client:      vmock.NewClient(ctrl),
			id:          m.id,
			shardID:     m.shardID,
			info:        info,
			clusterInfo: clusterInfo,
			nodes:       table.String(),
		}
		addresses = append(addresses, m.address)
	}
	dial := func(_ context.Context, address string) (vclient.Client, error) {
		return clients[address], nil
	}
	return addresses, dial
}

// heapInUse is the live heap after a full collection.
func heapInUse() uint64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

func BenchmarkGetClusterState(b *testing.B) {
	// getNodeState builds a logger per node, as in production; without a sink
	// controller-runtime prints a warning after 30s.
	logf.SetLogger(zap.New(zap.WriteTo(io.Discard)))
	for _, shards := range []int{50, 125, 250, 500} {
		b.Run(fmt.Sprintf("nodes=%d", 2*shards), func(b *testing.B) {
			ctrl := gomock.NewController(b)
			addresses, dial := benchCluster(ctrl, shards)
			ctx := context.Background()

			before := heapInUse()
			state := GetClusterState(ctx, addresses, 6379, dial)
			retained := heapInUse() - before
			if len(state.Shards) != shards {
				b.Fatalf("got %d shards, want %d", len(state.Shards), shards)
			}
			runtime.KeepAlive(state)

			for b.Loop() {
				state = GetClusterState(ctx, addresses, 6379, dial)
			}
			runtime.KeepAlive(state)
			b.ReportMetric(float64(retained)/(1<<20), "retained-MB")
		})
	}
}
