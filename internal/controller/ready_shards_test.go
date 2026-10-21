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

package controller

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

func TestCountReadyShards(t *testing.T) {
	cluster := &valkeyiov1alpha1.ValkeyCluster{
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{Replicas: 1},
	}
	// shard returns a one-primary, one-replica shard whose replica has its
	// link up. peerView is the primary's CLUSTER NODES output, which is where
	// a failure flag on the replica would appear.
	shard := func(peerView string) *valkey.ClusterState {
		primary := &valkey.NodeState{Id: "node-1", Address: "10.0.0.1", Flags: []string{"myself", "master"},
			ClusterInfo: map[string]string{"cluster_size": "1"},
			Info:        map[string]string{"role": "master"}}
		primary.SetClusterNodesForTesting(peerView)
		return &valkey.ClusterState{
			Shards: []*valkey.ShardState{
				{
					Id:        "shard-0",
					PrimaryId: "node-1",
					Nodes: []*valkey.NodeState{
						primary,
						{Id: "node-2", Address: "10.0.0.2", Flags: []string{"slave"},
							Info: map[string]string{"role": "slave", "master_link_status": "up"}},
					},
				},
			},
		}
	}
	r := &ValkeyClusterReconciler{}

	t.Run("healthy shard is counted", func(t *testing.T) {
		state := shard("node-1 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-16383\n" +
			"node-2 10.0.0.2:6379@16379 slave node-1 0 0 1 connected\n")
		assert.Equal(t, int32(1), r.countReadyShards(state, cluster))
	})

	t.Run("a present node the primary reports as fail? is not ready", func(t *testing.T) {
		// The replica still answers the operator, so it is in shard.Nodes
		// and its own scrape is clean. Only the primary's view says fail?.
		state := shard("node-1 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-16383\n" +
			"node-2 10.0.0.2:6379@16379 slave,fail? node-1 0 0 1 connected\n")
		assert.Equal(t, int32(0), r.countReadyShards(state, cluster))
	})
}

// One node cut off from the cluster bus flags every peer fail? in its own
// table. That single opinion must not zero the ready count; a majority of the
// peers agreeing on a node must.
func TestCountReadyShardsUnderPartition(t *testing.T) {
	cluster := &valkeyiov1alpha1.ValkeyCluster{Spec: valkeyiov1alpha1.ValkeyClusterSpec{Replicas: 0}}
	ids := []string{"n0", "n1", "n2"}
	clean := "n0 10.0.0.0:6379@16379 master - 0 0 1 connected 0-5460\n" +
		"n1 10.0.0.1:6379@16379 master - 0 0 1 connected 5461-10922\n" +
		"n2 10.0.0.2:6379@16379 master - 0 0 1 connected 10923-16383\n"
	allFailing := "n0 10.0.0.0:6379@16379 master,fail? - 0 0 1 connected 0-5460\n" +
		"n1 10.0.0.1:6379@16379 master,fail? - 0 0 1 connected 5461-10922\n" +
		"n2 10.0.0.2:6379@16379 master - 0 0 1 connected 10923-16383\n"
	n2Failing := "n0 10.0.0.0:6379@16379 master - 0 0 1 connected 0-5460\n" +
		"n1 10.0.0.1:6379@16379 master - 0 0 1 connected 5461-10922\n" +
		"n2 10.0.0.2:6379@16379 master,fail? - 0 0 1 connected 10923-16383\n"
	// Each node's table marks its own line myself, so it counts as a voting
	// primary for the others, and each reports cluster_size 3.
	withMyself := func(view, id string) string {
		return strings.Replace(view, id+" 10.0.0."+id[1:]+":6379@16379 master", id+" 10.0.0."+id[1:]+":6379@16379 myself,master", 1)
	}
	build := func(views ...string) *valkey.ClusterState {
		st := &valkey.ClusterState{}
		for i, id := range ids {
			n := &valkey.NodeState{Id: id, Address: "10.0.0." + string(rune('0'+i)), Flags: []string{"myself", "master"},
				Info: map[string]string{"role": "master"}, ClusterInfo: map[string]string{"cluster_size": "3"}}
			n.SetClusterNodesForTesting(withMyself(views[i], id))
			st.Shards = append(st.Shards, &valkey.ShardState{Id: "s" + id, PrimaryId: id, Nodes: []*valkey.NodeState{n}})
		}
		return st
	}
	r := &ValkeyClusterReconciler{}

	t.Run("one cut-off node flagging everyone changes nothing", func(t *testing.T) {
		assert.Equal(t, int32(3), r.countReadyShards(build(clean, clean, allFailing), cluster))
	})
	t.Run("a node the other two agree is failing is not ready", func(t *testing.T) {
		assert.Equal(t, int32(2), r.countReadyShards(build(n2Failing, n2Failing, clean), cluster))
	})
}
