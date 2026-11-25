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

	"github.com/stretchr/testify/require"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

// replicaOf builds a node that replicates primaryId, with the given link state.
func replicaOf(primaryId, link string) *valkey.NodeState {
	n := &valkey.NodeState{Info: map[string]string{"master_link_status": link}}
	n.SetClusterNodesForTesting("me 10.0.0.9:6379@16379 myself,slave " + primaryId + " 0 1 1 connected\n")
	return n
}

func TestClusterMigrated(t *testing.T) {
	owning := &valkey.ShardState{Slots: []valkey.SlotsRange{{Start: 0, End: 16383}}}
	replicasOnly := &valkey.ShardState{}

	require.False(t, clusterMigrated(&valkey.ClusterState{}, 1), "no shards yet")
	require.False(t, clusterMigrated(&valkey.ClusterState{Shards: []*valkey.ShardState{replicasOnly}}, 1), "before cutover no new node owns slots")
	require.True(t, clusterMigrated(&valkey.ClusterState{Shards: []*valkey.ShardState{owning}}, 1))
	require.False(t, clusterMigrated(&valkey.ClusterState{Shards: []*valkey.ShardState{owning, replicasOnly}}, 2), "one shard still pending cutover")
}

func TestBootstrapSyncProgress(t *testing.T) {
	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	cluster.Spec.Shards = 1
	cluster.Spec.ReplicasPerShard = 1 // two nodes per shard
	plan := &bootstrapPlan{shards: []valkey.SourceShard{{PrimaryId: "src"}}}

	state := func(links ...string) *valkey.ClusterState {
		shard := &valkey.ShardState{}
		for _, l := range links {
			shard.Nodes = append(shard.Nodes, replicaOf("src", l))
		}
		return &valkey.ClusterState{Shards: []*valkey.ShardState{shard}}
	}

	synced, total := bootstrapSyncProgress(state("up", "up"), cluster, plan)
	require.Equal(t, 1, synced)
	require.Equal(t, 1, total)

	synced, _ = bootstrapSyncProgress(state("up", "down"), cluster, plan)
	require.Equal(t, 0, synced, "one link down")

	synced, _ = bootstrapSyncProgress(state("up"), cluster, plan)
	require.Equal(t, 0, synced, "a node has not attached yet")
}

func TestSyncedNodesSkipsInitialSync(t *testing.T) {
	syncing := replicaOf("src", "up")
	syncing.Info["master_sync_in_progress"] = "1"
	ready := replicaOf("src", "up")
	got := syncedNodes(&valkey.ShardState{Nodes: []*valkey.NodeState{syncing, ready}})
	require.Equal(t, []*valkey.NodeState{ready}, got)
}
