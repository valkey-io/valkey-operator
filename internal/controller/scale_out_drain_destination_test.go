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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

// drainDestinationNode builds one labeled ValkeyNode fixture.
func drainDestinationNode(name, shard, node, ip string) *valkeyiov1alpha1.ValkeyNode {
	return &valkeyiov1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{
			LabelCluster:    "wedge",
			LabelShardIndex: shard,
			LabelNodeIndex:  node,
		}},
		Status: valkeyiov1alpha1.ValkeyNodeStatus{PodIP: ip},
	}
}

// TestLabelTriggeredDrainNeverMigratesOntoUnresolvedShard pins the drain
// destination choice on the label trigger: a shard the ValkeyNode labels
// cannot resolve must never receive migrated slots. deleteExcessValkeyNodes
// deletes by the labels, so an unresolved shard whose node carries an
// out-of-spec shard-index label can be deleted right after a migration onto
// it, losing the moved slots. The unresolved shard sits first in the live
// topology, so a body that offered it as a destination would pick it:
// PlanDrainMove takes the first destination with a primary.
func TestLabelTriggeredDrainNeverMigratesOntoUnresolvedShard(t *testing.T) {
	server := newMigrationStateServer(t, func() [][]string { return nil })

	// No ValkeyNode carries 10.0.2.1, so the first live shard resolves to -1
	// and would be the first drain destination a naive body offers.
	nodes := &valkeyiov1alpha1.ValkeyNodeList{Items: []valkeyiov1alpha1.ValkeyNode{
		*drainDestinationNode("wedge-0-0", "0", "0", "10.0.0.1"),
		*drainDestinationNode("wedge-3-0", "3", "0", "10.0.3.1"),
	}}

	unresolved := &valkey.ShardState{
		Id:        "shard-x",
		PrimaryId: "node-x-0",
		Slots:     []valkey.SlotsRange{{Start: 8192, End: 12287}},
		Nodes:     []*valkey.NodeState{scriptedPrimaryAt(t, server, "node-x-0", "10.0.2.1", 6379)},
	}
	inSpec := &valkey.ShardState{
		Id:        "shard-0",
		PrimaryId: "node-0-0",
		Slots:     []valkey.SlotsRange{{Start: 0, End: 4095}},
		Nodes:     []*valkey.NodeState{{Address: "10.0.0.1", Port: 6379, Id: "node-0-0", Flags: []string{"master"}}},
	}
	outOfSpec := &valkey.ShardState{
		Id:        "shard-3",
		PrimaryId: "node-3-0",
		Slots:     []valkey.SlotsRange{{Start: 12288, End: 16383}},
		Nodes:     []*valkey.NodeState{scriptedPrimaryAt(t, server, "node-3-0", "10.0.3.1", 6379)},
	}
	// The drain source knows both candidate destinations, so the gossip gate
	// cannot mask the destination choice: whichever primary the plan picks,
	// the CLUSTER MIGRATESLOTS is issued against it.
	outOfSpec.Nodes[0].SetClusterNodesForTesting(
		"node-3-0 10.0.3.1:6379@16379 myself,master - 0 0 1 connected 12288-16383\n" +
			"node-x-0 10.0.2.1:6379@16379 master - 0 0 2 connected 8192-12287\n" +
			"node-0-0 10.0.0.1:6379@16379 master - 0 0 3 connected 0-4095\n")
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{unresolved, inSpec, outOfSpec}}

	scheme := runtime.NewScheme()
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	objs := make([]client.Object, 0, len(nodes.Items))
	for i := range nodes.Items {
		objs = append(objs, &nodes.Items[i])
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	r := &ValkeyClusterReconciler{Client: c, Recorder: events.NewFakeRecorder(64)}
	cluster := &valkeyiov1alpha1.ValkeyCluster{ObjectMeta: metav1.ObjectMeta{Name: "wedge", Namespace: "ns"}}
	cluster.Spec.Shards = 3

	_, requeue := r.handleScaleIn(context.Background(), cluster, state, nodes)
	require.True(t, requeue, "the out-of-spec shard must trigger a requeue")

	migrations := 0
	for _, cmd := range server.recordedCommands() {
		if len(cmd) >= 2 && strings.EqualFold(cmd[0], "CLUSTER") && strings.EqualFold(cmd[1], "MIGRATESLOTS") {
			migrations++
			joined := strings.Join(cmd, " ")
			require.Contains(t, joined, "12288", "only the out-of-spec shard may drain: %q", joined)
			require.Contains(t, joined, "node-0-0", "the drain must target the in-spec primary: %q", joined)
			require.NotContains(t, joined, "node-x-0", "the unresolved shard must never receive migrated slots: %q", joined)
		}
	}
	require.NotZero(t, migrations, "expected CLUSTER MIGRATESLOTS draining shard 3, got %v", server.recordedCommands())

	// No command may touch the unresolved shard slots either: it keeps every
	// slot it owns until its identity resolves.
	for _, cmd := range server.recordedCommands() {
		joined := strings.Join(cmd, " ")
		require.NotContains(t, joined, "8192", "the unresolved shard must not be drained: %q", joined)
	}
}

// TestHandleScaleInRefusesToDeleteUndrainedShard pins the deletion loop's
// zero-slot verification. PlanDrainMove returns nil both when the source is
// empty and when no destination exists, so a pass whose destination list is
// empty must not fall through to the deletion loop and delete a shard that
// verifiably still owns slots. Here the label trigger fires with a single
// live out-of-spec shard and no destination at all.
func TestHandleScaleInRefusesToDeleteUndrainedShard(t *testing.T) {
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{{
		Id:        "shard-3",
		PrimaryId: "node-3-0",
		Slots:     []valkey.SlotsRange{{Start: 12288, End: 16383}},
		Nodes:     []*valkey.NodeState{{Address: "10.0.3.1", Port: 6379, Id: "node-3-0", Flags: []string{"master"}}},
	}}}
	n00 := drainDestinationNode("wedge-0-0", "0", "0", "10.0.0.1")
	n30 := drainDestinationNode("wedge-3-0", "3", "0", "10.0.3.1")
	nodes := &valkeyiov1alpha1.ValkeyNodeList{Items: []valkeyiov1alpha1.ValkeyNode{*n00, *n30}}

	scheme := runtime.NewScheme()
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(n00, n30).Build()
	recorder := events.NewFakeRecorder(64)
	r := &ValkeyClusterReconciler{Client: c, Recorder: recorder}
	cluster := &valkeyiov1alpha1.ValkeyCluster{ObjectMeta: metav1.ObjectMeta{Name: "wedge", Namespace: "ns"}}
	cluster.Spec.Shards = 3

	_, requeue := r.handleScaleIn(context.Background(), cluster, state, nodes)
	require.True(t, requeue, "the refusal must requeue with backoff, not delete")

	// The slot-owning shard's ValkeyNode must survive: deleting it now would
	// lose the keys it owns - the exact gh-491 failure mode.
	err := c.Get(context.Background(), client.ObjectKey{Name: "wedge-3-0", Namespace: "ns"}, &valkeyiov1alpha1.ValkeyNode{})
	require.NoError(t, err, "a shard that still owns slots must not have its ValkeyNode deleted")

	// The refusal surfaces as a DrainFailed warning event.
	select {
	case e := <-recorder.Events:
		require.Contains(t, e, "DrainFailed")
		require.Contains(t, e, "still owns")
	default:
		t.Fatal("expected a DrainFailed warning event carrying the refusal")
	}
}
