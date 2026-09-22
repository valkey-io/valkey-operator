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
			Info: map[string]string{"role": "master"}}
		primary.SetClusterNodes(peerView)
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
