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
	"context"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	valkeyv1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/valkey-io/valkey-operator/internal/valkey"
)

// scaleInFixture builds a three-node shard (node-index 0, 1, 2) in a
// two-shard cluster where the primary role sits on node-index 2 and the
// replica link states are given per address.
func scaleInFixture(t *testing.T, linkByAddress map[string]string) (*ValkeyClusterReconciler, *valkeyv1.ValkeyCluster, *valkey.ClusterState, *valkeyv1.ValkeyNodeList) {
	t.Helper()

	cluster := &valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "wedge", Namespace: "ns"},
		Spec:       valkeyv1.ValkeyClusterSpec{Shards: 2, Replicas: 1},
	}

	mk := func(name string, shard, node int, ip string) *valkeyv1.ValkeyNode {
		n := newTestValkeyNode(name, "ns")
		n.Labels = map[string]string{
			LabelCluster:    "wedge",
			LabelShardIndex: strconv.Itoa(shard),
			LabelNodeIndex:  strconv.Itoa(node),
		}
		n.Status.PodIP = ip
		return n
	}
	n00 := mk("wedge-0-0", 0, 0, "10.0.0.1")
	n01 := mk("wedge-0-1", 0, 1, "10.0.0.2")
	n02 := mk("wedge-0-2", 0, 2, "10.0.0.3")
	n10 := mk("wedge-1-0", 1, 0, "10.0.1.1")

	nodes := &valkeyv1.ValkeyNodeList{Items: []valkeyv1.ValkeyNode{*n00, *n01, *n02, *n10}}

	shard0 := &valkey.ShardState{
		Id:        "shard-0",
		PrimaryId: "node-0-2",
		Nodes: []*valkey.NodeState{
			{Address: "10.0.0.1", Id: "node-0-0", Flags: []string{"slave"}, Info: map[string]string{"master_link_status": linkByAddress["10.0.0.1"]}},
			{Address: "10.0.0.2", Id: "node-0-1", Flags: []string{"slave"}, Info: map[string]string{"master_link_status": linkByAddress["10.0.0.2"]}},
			{Address: "10.0.0.3", Id: "node-0-2", Flags: []string{"master"}},
		},
	}
	shard1 := &valkey.ShardState{
		Id:        "shard-1",
		PrimaryId: "node-1-0",
		Nodes: []*valkey.NodeState{
			{Address: "10.0.1.1", Id: "node-1-0", Flags: []string{"master"}},
		},
	}
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{shard0, shard1}}

	scheme := runtime.NewScheme()
	require.NoError(t, valkeyv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(n00, n01, n02, n10).Build()

	r := &ValkeyClusterReconciler{Client: c, Recorder: events.NewFakeRecorder(32)}
	return r, cluster, state, nodes
}

func stubFailoverCounter() (*int, func()) {
	calls := 0
	orig := proactiveFailoverFn
	proactiveFailoverFn = func(context.Context, events.EventRecorder, *valkeyv1.ValkeyCluster, *valkey.ShardState, []*valkey.NodeState) error {
		calls++
		return nil
	}
	return &calls, func() { proactiveFailoverFn = orig }
}

func TestDeleteExcessValkeyNodesDefersPrimaryDeletion(t *testing.T) {
	// Replicas 2 -> 1. Node-index 2 is excess but is the shard primary
	// with a synced kept replica available: the failover is initiated and
	// the deletion is deferred until a fresh scrape confirms the new role.
	failoverCalls, restore := stubFailoverCounter()
	t.Cleanup(restore)

	r, cluster, state, nodes := scaleInFixture(t, map[string]string{"10.0.0.1": "up", "10.0.0.2": "down"})

	deleted, deferred, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodes)
	require.NoError(t, err)
	assert.False(t, deleted, "nothing is deleted while the excess node is still the primary")
	assert.True(t, deferred, "the deletion is deferred, so the caller requeues for a fresh scrape")
	assert.Equal(t, 1, *failoverCalls, "the failover to the kept synced replica is initiated exactly once")

	// The excess ValkeyNode still exists.
	node := &valkeyv1.ValkeyNode{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Name: "wedge-0-2", Namespace: "ns"}, node))

	// A fresh scrape now reports the failover completed: the node at
	// 10.0.0.1 is the new primary and wedge-0-2 is a replica.
	newState := &valkey.ClusterState{Shards: []*valkey.ShardState{
		{
			Id:        "shard-0",
			PrimaryId: "node-0-0",
			Nodes: []*valkey.NodeState{
				{Address: "10.0.0.1", Id: "node-0-0", Flags: []string{"master"}},
				{Address: "10.0.0.2", Id: "node-0-1", Flags: []string{"slave"}, Info: map[string]string{"master_link_status": "up"}},
				{Address: "10.0.0.3", Id: "node-0-2", Flags: []string{"slave"}, Info: map[string]string{"master_link_status": "up"}},
			},
		},
		state.Shards[1],
	}}

	deleted, deferred, err = r.deleteExcessValkeyNodes(context.Background(), cluster, newState, nodes)
	require.NoError(t, err)
	assert.True(t, deleted, "once the node is a replica, its ValkeyNode is deleted")
	assert.False(t, deferred, "nothing is deferred once the node is a replica")
	assert.Equal(t, 1, *failoverCalls, "no second failover is issued")

	err = r.Get(context.Background(), client.ObjectKey{Name: "wedge-0-2", Namespace: "ns"}, &valkeyv1.ValkeyNode{})
	require.Error(t, err, "the excess ValkeyNode CR is gone")
	assert.True(t, k8serrors.IsNotFound(err))
}

func TestDeleteExcessValkeyNodesNoSyncedKeptReplicaDefers(t *testing.T) {
	// The excess node is the primary and both kept replicas are unsynced:
	// deletion must be deferred, no failover issued.
	failoverCalls, restore := stubFailoverCounter()
	t.Cleanup(restore)

	r, cluster, state, nodes := scaleInFixture(t, map[string]string{"10.0.0.1": "down", "10.0.0.2": "down"})

	deleted, deferred, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodes)
	require.NoError(t, err)
	assert.False(t, deleted)
	assert.Zero(t, *failoverCalls, "no failover without a synced kept replica")
	assert.True(t, deferred, "deletion is deferred until a kept replica is synced")

	err = r.Get(context.Background(), client.ObjectKey{Name: "wedge-0-2", Namespace: "ns"}, &valkeyv1.ValkeyNode{})
	require.NoError(t, err, "the excess primary ValkeyNode is not deleted")

	progressing := meta.FindStatusCondition(cluster.Status.Conditions, valkeyv1.ConditionProgressing)
	require.NotNil(t, progressing)
	assert.Contains(t, progressing.Message, "wedge-0-2")
	ready := meta.FindStatusCondition(cluster.Status.Conditions, valkeyv1.ConditionReady)
	require.NotNil(t, ready)
	assert.NotEqual(t, metav1.ConditionTrue, ready.Status)
}

func TestDeleteExcessValkeyNodesReplicaDeletedWithoutFailover(t *testing.T) {
	// Node-index 2 is excess and is a replica: deleted with no failover.
	failoverCalls, restore := stubFailoverCounter()
	t.Cleanup(restore)

	r, cluster, state, nodes := scaleInFixture(t, map[string]string{"10.0.0.1": "up", "10.0.0.2": "up"})
	// Fresh scrape reports the primary at node-index 0.
	state.Shards[0].PrimaryId = "node-0-0"
	state.Shards[0].Nodes[0].Flags = []string{"master"}
	state.Shards[0].Nodes[2].Flags = []string{"slave"}
	state.Shards[0].Nodes[2].Info = map[string]string{"master_link_status": "up"}

	deleted, deferred, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodes)
	require.NoError(t, err)
	assert.True(t, deleted)
	assert.Zero(t, *failoverCalls, "a replica is deleted without failover")
	assert.False(t, deferred, "no deferral for a plain replica deletion")

	err = r.Get(context.Background(), client.ObjectKey{Name: "wedge-0-2", Namespace: "ns"}, &valkeyv1.ValkeyNode{})
	require.Error(t, err, "the excess replica ValkeyNode is deleted")
}

func TestDeleteExcessValkeyNodesFailoverErrorDefers(t *testing.T) {
	// The failover is attempted but does not complete: the node survives
	// and the error surfaces to the caller.
	orig := proactiveFailoverFn
	proactiveFailoverFn = func(context.Context, events.EventRecorder, *valkeyv1.ValkeyCluster, *valkey.ShardState, []*valkey.NodeState) error {
		return fmt.Errorf("CLUSTER FAILOVER timed out")
	}
	t.Cleanup(func() { proactiveFailoverFn = orig })

	r, cluster, state, nodes := scaleInFixture(t, map[string]string{"10.0.0.1": "up", "10.0.0.2": "down"})

	_, deferred, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodes)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wedge-0-2")
	assert.True(t, deferred)

	err = r.Get(context.Background(), client.ObjectKey{Name: "wedge-0-2", Namespace: "ns"}, &valkeyv1.ValkeyNode{})
	require.NoError(t, err, "the primary is not deleted when its failover did not complete")
}

func TestDeleteExcessValkeyNodesHighShardFailoverDoesNotStall(t *testing.T) {
	failoverCalls, restore := stubFailoverCounter()
	t.Cleanup(restore)

	cluster := &valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "wedge", Namespace: "ns"},
		Spec:       valkeyv1.ValkeyClusterSpec{Shards: 3, Replicas: 1},
	}

	mk := func(name string, shard, node int, ip string) *valkeyv1.ValkeyNode {
		n := newTestValkeyNode(name, "ns")
		n.Labels = map[string]string{
			LabelCluster:    "wedge",
			LabelShardIndex: strconv.Itoa(shard),
			LabelNodeIndex:  strconv.Itoa(node),
		}
		n.Status.PodIP = ip
		return n
	}
	n00 := mk("wedge-0-0", 0, 0, "10.0.0.1")
	n20 := mk("wedge-2-0", 2, 0, "10.0.2.1")
	n21 := mk("wedge-2-1", 2, 1, "10.0.2.2")
	n22 := mk("wedge-2-2", 2, 2, "10.0.2.3")
	nodes := &valkeyv1.ValkeyNodeList{Items: []valkeyv1.ValkeyNode{*n00, *n20, *n21, *n22}}

	shard2 := &valkey.ShardState{
		Id:        "shard-2",
		PrimaryId: "node-2-2",
		Nodes: []*valkey.NodeState{
			{Address: "10.0.2.1", Id: "node-2-0", Flags: []string{"slave"}, Info: map[string]string{"master_link_status": "down"}},
			{Address: "10.0.2.2", Id: "node-2-1", Flags: []string{"slave"}, Info: map[string]string{"master_link_status": "up"}},
			{Address: "10.0.2.3", Id: "node-2-2", Flags: []string{"master"}},
		},
	}
	shard0 := &valkey.ShardState{
		Id:        "shard-0",
		PrimaryId: "node-0-0",
		Nodes: []*valkey.NodeState{
			{Address: "10.0.0.1", Id: "node-0-0", Flags: []string{"master"}},
		},
	}
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{shard0, shard2}}

	scheme := runtime.NewScheme()
	require.NoError(t, valkeyv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(n00, n20, n21, n22).Build()
	r := &ValkeyClusterReconciler{Client: c, Recorder: events.NewFakeRecorder(32)}

	deleted, deferred, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodes)
	require.NoError(t, err)
	assert.False(t, deleted, "nothing is deleted while the excess node is still the primary")
	assert.True(t, deferred, "the failover is initiated and the deletion deferred for a fresh scrape")
	assert.Equal(t, 1, *failoverCalls, "the synced kept replica in a high-index shard is a failover target")

	err = r.Get(context.Background(), client.ObjectKey{Name: "wedge-2-2", Namespace: "ns"}, &valkeyv1.ValkeyNode{})
	require.NoError(t, err, "the high-shard excess primary is not deleted while it holds the role")

	newState := &valkey.ClusterState{Shards: []*valkey.ShardState{shard0, {
		Id:        "shard-2",
		PrimaryId: "node-2-1",
		Nodes: []*valkey.NodeState{
			{Address: "10.0.2.1", Id: "node-2-0", Flags: []string{"slave"}, Info: map[string]string{"master_link_status": "down"}},
			{Address: "10.0.2.2", Id: "node-2-1", Flags: []string{"master"}},
			{Address: "10.0.2.3", Id: "node-2-2", Flags: []string{"slave"}, Info: map[string]string{"master_link_status": "up"}},
		},
	}}}

	deleted, deferred, err = r.deleteExcessValkeyNodes(context.Background(), cluster, newState, nodes)
	require.NoError(t, err)
	assert.True(t, deleted, "once the node is a replica, its ValkeyNode is deleted")
	assert.False(t, deferred)
	assert.Equal(t, 1, *failoverCalls, "no second failover is issued")

	err = r.Get(context.Background(), client.ObjectKey{Name: "wedge-2-2", Namespace: "ns"}, &valkeyv1.ValkeyNode{})
	require.Error(t, err, "the excess ValkeyNode CR is gone after the role moved")
	assert.True(t, k8serrors.IsNotFound(err))
}

func TestDeleteExcessValkeyNodesNeverFailsOverToAnExcessReplica(t *testing.T) {
	var failoverTargets [][]*valkey.NodeState
	orig := proactiveFailoverFn
	proactiveFailoverFn = func(_ context.Context, _ events.EventRecorder, _ *valkeyv1.ValkeyCluster, _ *valkey.ShardState, targets []*valkey.NodeState) error {
		failoverTargets = append(failoverTargets, targets)
		return nil
	}
	t.Cleanup(func() { proactiveFailoverFn = orig })

	cluster := &valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "wedge", Namespace: "ns"},
		Spec:       valkeyv1.ValkeyClusterSpec{Shards: 2, Replicas: 0},
	}

	mk := func(name string, shard, node int, ip string) *valkeyv1.ValkeyNode {
		n := newTestValkeyNode(name, "ns")
		n.Labels = map[string]string{
			LabelCluster:    "wedge",
			LabelShardIndex: strconv.Itoa(shard),
			LabelNodeIndex:  strconv.Itoa(node),
		}
		n.Status.PodIP = ip
		return n
	}
	n00 := mk("wedge-0-0", 0, 0, "10.0.0.1")
	n01 := mk("wedge-0-1", 0, 1, "10.0.0.2")
	n02 := mk("wedge-0-2", 0, 2, "10.0.0.3")
	n03 := mk("wedge-0-3", 0, 3, "10.0.0.4")
	n10 := mk("wedge-1-0", 1, 0, "10.0.1.1")
	nodes := &valkeyv1.ValkeyNodeList{Items: []valkeyv1.ValkeyNode{*n00, *n01, *n02, *n03, *n10}}

	shard0 := &valkey.ShardState{
		Id:        "shard-0",
		PrimaryId: "node-0-3",
		Nodes: []*valkey.NodeState{
			{Address: "10.0.0.1", Id: "node-0-0", Flags: []string{"slave"}, Info: map[string]string{"master_link_status": "down"}},
			{Address: "10.0.0.2", Id: "node-0-1", Flags: []string{"slave"}, Info: map[string]string{"master_link_status": "up"}},
			{Address: "10.0.0.3", Id: "node-0-2", Flags: []string{"slave"}, Info: map[string]string{"master_link_status": "up"}},
			{Address: "10.0.0.4", Id: "node-0-3", Flags: []string{"master"}},
		},
	}
	shard1 := &valkey.ShardState{
		Id:        "shard-1",
		PrimaryId: "node-1-0",
		Nodes: []*valkey.NodeState{
			{Address: "10.0.1.1", Id: "node-1-0", Flags: []string{"master"}},
		},
	}
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{shard0, shard1}}

	scheme := runtime.NewScheme()
	require.NoError(t, valkeyv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(n00, n01, n02, n03, n10).Build()
	r := &ValkeyClusterReconciler{Client: c, Recorder: events.NewFakeRecorder(32)}

	_, deferred, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodes)
	require.NoError(t, err)
	assert.True(t, deferred, "with no kept synced replica, the deletion waits instead of promoting a doomed node")
	assert.Empty(t, failoverTargets, "no failover may target a replica the scale-in is deleting")

	err = r.Get(context.Background(), client.ObjectKey{Name: "wedge-0-3", Namespace: "ns"}, &valkeyv1.ValkeyNode{})
	require.NoError(t, err, "the excess primary ValkeyNode is not deleted")
	err = r.Get(context.Background(), client.ObjectKey{Name: "wedge-0-1", Namespace: "ns"}, &valkeyv1.ValkeyNode{})
	require.Error(t, err, "the plain excess replica is deleted")
	err = r.Get(context.Background(), client.ObjectKey{Name: "wedge-0-2", Namespace: "ns"}, &valkeyv1.ValkeyNode{})
	require.Error(t, err, "the plain excess replica is deleted")

	shard0.Nodes[0].Info["master_link_status"] = "up"
	_, deferred, err = r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodes)
	require.NoError(t, err)
	assert.True(t, deferred)
	require.Len(t, failoverTargets, 1, "the failover is initiated exactly once")
	require.Len(t, failoverTargets[0], 1, "the only kept replica is the failover target")
	assert.Equal(t, "10.0.0.1", failoverTargets[0][0].Address)
}
