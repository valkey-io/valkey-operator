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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

func TestBootstrapCutoverRequested(t *testing.T) {
	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	require.False(t, bootstrapCutoverRequested(cluster), "no spec.bootstrap")

	cluster.Spec.Bootstrap = &valkeyiov1alpha1.BootstrapSpec{}
	require.False(t, bootstrapCutoverRequested(cluster), "an unset stage means Bootstrap")

	for stage, want := range map[valkeyiov1alpha1.BootstrapStage]bool{
		valkeyiov1alpha1.BootstrapStageBootstrap: false,
		valkeyiov1alpha1.BootstrapStageCutover:   true,
		valkeyiov1alpha1.BootstrapStageComplete:  true,
	} {
		cluster.Spec.Bootstrap.Stage = stage
		require.Equal(t, want, bootstrapCutoverRequested(cluster), string(stage))
	}
}

func TestNodeConfigKeepsNewNodesFromBeingElected(t *testing.T) {
	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	cluster.Spec.Config = map[string]string{"maxmemory": "100mb"}

	require.NotContains(t, nodeConfig(cluster), noFailoverParam, "a cluster without spec.bootstrap is untouched")

	cluster.Spec.Bootstrap = &valkeyiov1alpha1.BootstrapSpec{}
	for _, stage := range []valkeyiov1alpha1.BootstrapStage{
		"", valkeyiov1alpha1.BootstrapStageBootstrap, valkeyiov1alpha1.BootstrapStageCutover,
	} {
		cluster.Spec.Bootstrap.Stage = stage
		got := nodeConfig(cluster)
		require.Equal(t, "yes", got[noFailoverParam], "stage %q", stage)
		require.Equal(t, "100mb", got["maxmemory"], "user config is kept")
	}
	require.NotContains(t, cluster.Spec.Config, noFailoverParam, "the user's map is not modified")

	// Bootstrap and Cutover render the same file, so the move to Cutover rolls nothing.
	cluster.Spec.Bootstrap.Stage = valkeyiov1alpha1.BootstrapStageBootstrap
	atBootstrap := nodeConfig(cluster)
	cluster.Spec.Bootstrap.Stage = valkeyiov1alpha1.BootstrapStageCutover
	require.Equal(t, atBootstrap, nodeConfig(cluster))

	cluster.Spec.Bootstrap.Stage = valkeyiov1alpha1.BootstrapStageComplete
	require.NotContains(t, nodeConfig(cluster), noFailoverParam, "Complete restores normal failover")
}

func TestSourceMembers(t *testing.T) {
	ours := &valkey.NodeState{Id: "a"}
	ours.SetClusterNodesForTesting("a 10.0.0.1:6379@16379 myself,master - 0 1 1 connected 0-16383\n" +
		"b 10.0.0.2:6379@16379 slave a 0 1 1 connected\n" +
		"s1 10.1.0.1:6379@16379 slave a 0 1 1 connected\n" +
		"s2 10.1.0.2:6379@16379 slave a 0 1 1 connected\n")
	sibling := &valkey.NodeState{Id: "b"}
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{{Nodes: []*valkey.NodeState{ours, sibling}}}}

	got := sourceMembers(state)
	require.Len(t, got, 2)
	require.Equal(t, "s1", got[0].Id)
	require.Equal(t, "s2", got[1].Id)

	require.Empty(t, sourceMembers(&valkey.ClusterState{}), "no reachable node, nothing to report")
}

func TestFinishBootstrapConditionsReportsRemainingSource(t *testing.T) {
	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	cluster.Spec.Bootstrap = &valkeyiov1alpha1.BootstrapSpec{Stage: valkeyiov1alpha1.BootstrapStageCutover}

	finishBootstrapConditions(cluster, 6)
	c := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionBootstrapSourceDetached)
	require.NotNil(t, c)
	require.Equal(t, metav1.ConditionFalse, c.Status)
	require.Equal(t, valkeyiov1alpha1.ReasonSourceNodesRemain, c.Reason)
	require.Contains(t, c.Message, "Complete", "tells the user how to finish")

	cluster.Spec.Bootstrap.Stage = valkeyiov1alpha1.BootstrapStageComplete
	finishBootstrapConditions(cluster, 6)
	c = meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionBootstrapSourceDetached)
	require.Contains(t, c.Message, "shut them down", "at Complete the source must be stopped")

	finishBootstrapConditions(cluster, 0)
	c = meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionBootstrapSourceDetached)
	require.Equal(t, metav1.ConditionTrue, c.Status)
	require.Equal(t, valkeyiov1alpha1.ReasonSourceDetached, c.Reason)
}
