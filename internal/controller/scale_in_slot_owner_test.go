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
	"context"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// primaryNode returns the node-index 0 ValkeyNode of cluster "c" for a shard.
func primaryNode(shard int) *valkeyiov1alpha1.ValkeyNode {
	return &valkeyiov1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("c-%d-0", shard),
			Namespace: "ns",
			Labels:    map[string]string{LabelCluster: "c", LabelShardIndex: strconv.Itoa(shard), LabelNodeIndex: "0"},
		},
		Status: valkeyiov1alpha1.ValkeyNodeStatus{PodIP: fmt.Sprintf("10.0.0.%d", shard)},
	}
}

// primaryShard returns a shard whose primary is primaryNode(shard).
func primaryShard(shard int, slots ...valkey.SlotsRange) *valkey.ShardState {
	id := fmt.Sprintf("p%d", shard)
	return &valkey.ShardState{
		Id:        fmt.Sprintf("s%d", shard),
		PrimaryId: id,
		Slots:     slots,
		Nodes:     []*valkey.NodeState{{Id: id, Address: fmt.Sprintf("10.0.0.%d", shard)}},
	}
}

// primaryIDs returns the primary node IDs of the shards, in order.
func primaryIDs(shards []*valkey.ShardState) []string {
	ids := make([]string, 0, len(shards))
	for _, s := range shards {
		ids = append(ids, s.PrimaryId)
	}
	return ids
}

// TestSplitShardsForScaleIn checks a high-index shard that owns slots is
// drained even when the shard count matches the spec.
func TestSplitShardsForScaleIn(t *testing.T) {
	nodes := &valkeyiov1alpha1.ValkeyNodeList{}
	for i := range 4 {
		nodes.Items = append(nodes.Items, *primaryNode(i))
	}

	// Scaling 2 -> 4, shard 3 got slots before shard 2 did, then the spec
	// dropped to 3. Shard 2 has no slots yet, so the scrape lists it under
	// PendingNodes; as the emptiest shard it is the first destination.
	state := &valkey.ClusterState{
		Shards: []*valkey.ShardState{
			primaryShard(0, valkey.SlotsRange{Start: 0, End: 5460}),
			primaryShard(1, valkey.SlotsRange{Start: 5461, End: 10922}),
			primaryShard(3, valkey.SlotsRange{Start: 10923, End: 16383}),
		},
		PendingNodes: []*valkey.NodeState{{Id: "p2", Address: "10.0.0.2"}},
	}
	remaining, draining := splitShardsForScaleIn(effectiveShards(state, nodes), nodes, 3)
	assert.Equal(t, []string{"p2", "p0", "p1"}, primaryIDs(remaining))
	assert.Equal(t, []string{"p3"}, primaryIDs(draining))

	// A drained excess shard has nothing left to move.
	state.Shards[2] = primaryShard(3)
	_, draining = splitShardsForScaleIn(effectiveShards(state, nodes), nodes, 3)
	assert.Empty(t, draining)

	// A shard with no ValkeyNode to place it is neither drained nor a destination.
	state.Shards[2] = &valkey.ShardState{
		PrimaryId: "px",
		Slots:     []valkey.SlotsRange{{Start: 10923, End: 16383}},
		Nodes:     []*valkey.NodeState{{Id: "px", Address: "10.0.9.9"}},
	}
	remaining, draining = splitShardsForScaleIn(effectiveShards(state, nodes), nodes, 3)
	assert.Equal(t, []string{"p2", "p0", "p1"}, primaryIDs(remaining))
	assert.Empty(t, draining)
}

// TestEffectiveShardsSkipsPostFailoverReplacement: shard 0's primary restarted
// empty before its replica was promoted. The new pod waits to become that
// replica, so it is not a new shard and must not receive drained slots.
func TestEffectiveShardsSkipsPostFailoverReplacement(t *testing.T) {
	replica := primaryNode(0)
	replica.Name = "c-0-1"
	replica.Labels[LabelNodeIndex] = "1"
	replica.Status.PodIP = "10.0.1.0"
	replacement := primaryNode(0)
	nodes := nodeList(replacement, replica, primaryNode(1), primaryNode(2))
	state := &valkey.ClusterState{
		Shards: []*valkey.ShardState{
			{Id: "s0", Nodes: []*valkey.NodeState{{Id: "r0", Address: "10.0.1.0"}}},
			primaryShard(1, valkey.SlotsRange{Start: 5461, End: 10922}),
			primaryShard(2, valkey.SlotsRange{Start: 10923, End: 16383}),
		},
		PendingNodes: []*valkey.NodeState{{Id: "new0", Address: "10.0.0.0"}},
	}
	assert.Equal(t, []string{"", "p1", "p2"}, primaryIDs(effectiveShards(state, nodes)))

	remaining, draining := splitShardsForScaleIn(effectiveShards(state, nodes), nodes, 2)
	move, err := valkey.PlanDrainMove(draining[0], remaining, 400)
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.1", move.Dst.Address)
}

// scaleInReconciler returns a reconciler for a 2-shard cluster "c" whose
// fake API holds the given ValkeyNodes.
func scaleInReconciler(t *testing.T, objs ...client.Object) (*ValkeyClusterReconciler, *valkeyiov1alpha1.ValkeyCluster, *events.FakeRecorder) {
	scheme := runtime.NewScheme()
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	recorder := events.NewFakeRecorder(8)
	r := &ValkeyClusterReconciler{
		Client:   fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(),
		Scheme:   scheme,
		Recorder: recorder,
	}
	cluster := &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
		Spec:       valkeyiov1alpha1.ValkeyClusterSpec{Shards: 2},
	}
	return r, cluster, recorder
}

// nodeExists reports whether the ValkeyNode is still in the fake API.
func nodeExists(t *testing.T, c client.Client, n *valkeyiov1alpha1.ValkeyNode) bool {
	err := c.Get(context.Background(), client.ObjectKeyFromObject(n), &valkeyiov1alpha1.ValkeyNode{})
	if apierrors.IsNotFound(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

// nodeList returns the ValkeyNodes as the list Reconcile passes around.
func nodeList(nodes ...*valkeyiov1alpha1.ValkeyNode) *valkeyiov1alpha1.ValkeyNodeList {
	list := &valkeyiov1alpha1.ValkeyNodeList{}
	for _, n := range nodes {
		list.Items = append(list.Items, *n)
	}
	return list
}

// drainEvents returns the events recorded so far without blocking.
func drainEvents(recorder *events.FakeRecorder) []string {
	var got []string
	for {
		select {
		case e := <-recorder.Events:
			got = append(got, e)
		default:
			return got
		}
	}
}

// TestDeleteExcessValkeyNodesKeepsSlotOwners checks an excess primary that owns
// slots is kept while a drained one is deleted.
func TestDeleteExcessValkeyNodesKeepsSlotOwners(t *testing.T) {
	inSpec, owner, drained := primaryNode(0), primaryNode(3), primaryNode(2)
	r, cluster, recorder := scaleInReconciler(t, inSpec, owner, drained)
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{
		primaryShard(0, valkey.SlotsRange{Start: 0, End: 8191}),
		primaryShard(3, valkey.SlotsRange{Start: 8192, End: 16383}),
		primaryShard(2),
	}}

	deleted, kept, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodeList(inSpec, owner, drained))
	require.NoError(t, err)
	assert.True(t, deleted)
	assert.Equal(t, []string{"c-3-0"}, kept)
	assert.True(t, nodeExists(t, r.Client, inSpec))
	assert.True(t, nodeExists(t, r.Client, owner), "excess node that owns slots must be kept")
	assert.False(t, nodeExists(t, r.Client, drained))
	assert.Contains(t, drainEvents(recorder), "Warning ScaleInBlocked Excess ValkeyNode c-3-0 still owns slots; waiting for drain")
}

// TestDeleteExcessValkeyNodesKeepsUnreachablePrimaries: a primary the scrape
// missed may still own the slots nobody else claims.
func TestDeleteExcessValkeyNodesKeepsUnreachablePrimaries(t *testing.T) {
	unreachable := primaryNode(3)
	r, cluster, recorder := scaleInReconciler(t, primaryNode(0), unreachable)
	nodes := nodeList(primaryNode(0), unreachable)
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{
		primaryShard(0, valkey.SlotsRange{Start: 0, End: 8191}),
	}}

	_, kept, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodes)
	require.NoError(t, err)
	assert.Equal(t, []string{"c-3-0"}, kept)
	assert.True(t, nodeExists(t, r.Client, unreachable))
	assert.Contains(t, drainEvents(recorder), "Warning ScaleInBlocked Keeping unreachable excess ValkeyNode c-3-0: some slots are unassigned")

	state.Shards[0].Slots = []valkey.SlotsRange{{Start: 0, End: 16383}}
	_, kept, err = r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodes)
	require.NoError(t, err)
	assert.Empty(t, kept)
	assert.False(t, nodeExists(t, r.Client, unreachable), "deleted once every slot is accounted for")
}

// TestDeleteExcessValkeyNodesKeepsUnreadyPromotedPrimary: an unready pod has
// no role recorded, and after a failover the primary can be at node index 1.
func TestDeleteExcessValkeyNodesKeepsUnreadyPromotedPrimary(t *testing.T) {
	promoted := primaryNode(3)
	promoted.Name = "c-3-1"
	promoted.Labels[LabelNodeIndex] = "1"
	promoted.Status = valkeyiov1alpha1.ValkeyNodeStatus{PodIP: "10.0.1.3"}
	r, cluster, _ := scaleInReconciler(t, primaryNode(0), promoted)
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{
		primaryShard(0, valkey.SlotsRange{Start: 0, End: 8191}),
	}}

	_, kept, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodeList(primaryNode(0), promoted))
	require.NoError(t, err)
	assert.Equal(t, []string{"c-3-1"}, kept)
	assert.True(t, nodeExists(t, r.Client, promoted))
}

// TestDeleteExcessValkeyNodesDeletesNodesThatCannotOwnSlots: an unreachable
// replica or a pod with no IP cannot hold the missing slots, so neither holds
// up scale-in.
func TestDeleteExcessValkeyNodesDeletesNodesThatCannotOwnSlots(t *testing.T) {
	replica := primaryNode(3)
	replica.Name = "c-3-1"
	replica.Labels[LabelNodeIndex] = "1"
	replica.Status = valkeyiov1alpha1.ValkeyNodeStatus{PodIP: "10.0.1.3", Role: RoleReplica}
	unscheduled := primaryNode(4)
	unscheduled.Status.PodIP = ""
	r, cluster, _ := scaleInReconciler(t, replica, unscheduled)
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{
		primaryShard(0, valkey.SlotsRange{Start: 0, End: 8191}),
	}}

	deleted, kept, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodeList(replica, unscheduled))
	require.NoError(t, err)
	assert.True(t, deleted)
	assert.Empty(t, kept)
	assert.False(t, nodeExists(t, r.Client, replica))
	assert.False(t, nodeExists(t, r.Client, unscheduled))
}

// TestDeleteExcessValkeyNodesIgnoresSlotsOnExcessNodeIndex: a promoted replica
// past spec.replicas is deleted even though it owns slots. Nothing would move
// the slots off it, so keeping it would block the scale-in for good.
func TestDeleteExcessValkeyNodesIgnoresSlotsOnExcessNodeIndex(t *testing.T) {
	promoted := primaryNode(0)
	promoted.Name = "c-0-1"
	promoted.Labels[LabelNodeIndex] = "1"
	r, cluster, _ := scaleInReconciler(t, promoted)
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{
		primaryShard(0, valkey.SlotsRange{Start: 0, End: 16383}),
	}}

	_, kept, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodeList(promoted))
	require.NoError(t, err)
	assert.Empty(t, kept)
	assert.False(t, nodeExists(t, r.Client, promoted))
}

// TestHandleScaleInWithoutDestination: with no in-spec primary anywhere there
// is nowhere to drain to, and the status says so.
func TestHandleScaleInWithoutDestination(t *testing.T) {
	owner := primaryNode(3)
	r, cluster, recorder := scaleInReconciler(t, owner)
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{
		primaryShard(3, valkey.SlotsRange{Start: 0, End: 16383}),
	}}

	result, requeue := r.handleScaleIn(context.Background(), cluster, state, nodeList(owner))
	assert.True(t, requeue)
	assert.Positive(t, result.RequeueAfter)
	assert.True(t, nodeExists(t, r.Client, owner))
	ready := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, valkeyiov1alpha1.ReasonScaleInBlocked, ready.Reason)
	assert.Nil(t, meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionDegraded))
	assert.Contains(t, drainEvents(recorder), "Warning ScaleInBlocked Scale-in blocked: shard s3 owns slots but there is no in-spec primary to drain into")
}

// TestHandleScaleInReportsKeptNodes: a kept excess node blocks scale-in
// visibly instead of letting the cluster go Ready around it.
func TestHandleScaleInReportsKeptNodes(t *testing.T) {
	unreachable := primaryNode(3)
	r, cluster, _ := scaleInReconciler(t, primaryNode(0), unreachable)
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{
		primaryShard(0, valkey.SlotsRange{Start: 0, End: 8191}),
	}}

	_, requeue := r.handleScaleIn(context.Background(), cluster, state, nodeList(primaryNode(0), unreachable))
	assert.True(t, requeue)
	progressing := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionProgressing)
	require.NotNil(t, progressing)
	assert.Equal(t, valkeyiov1alpha1.ReasonScaleInBlocked, progressing.Reason)
	assert.Contains(t, progressing.Message, "c-3-0")
}
