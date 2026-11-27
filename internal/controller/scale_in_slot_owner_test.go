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
	vmock "github.com/valkey-io/valkey-go/mock"
	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
	"go.uber.org/mock/gomock"
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

// replicaNode returns the node-index 1 ValkeyNode of cluster "c" for a shard,
// last seen as a replica.
func replicaNode(shard int) *valkeyiov1alpha1.ValkeyNode {
	return &valkeyiov1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("c-%d-1", shard),
			Namespace: "ns",
			Labels:    map[string]string{LabelCluster: "c", LabelShardIndex: strconv.Itoa(shard), LabelNodeIndex: "1"},
		},
		Status: valkeyiov1alpha1.ValkeyNodeStatus{PodIP: fmt.Sprintf("10.0.1.%d", shard), Role: RoleReplica},
	}
}

// clusterNodesLine formats one CLUSTER NODES line. primary is "-" for a
// primary.
func clusterNodesLine(id, host, flags, primary string, slots ...valkey.SlotsRange) string {
	line := fmt.Sprintf("%s %s:6379@16379 %s %s 0 0 1 connected", id, host, flags, primary)
	for _, s := range slots {
		line += " " + s.String()
	}
	return line
}

// failedP3 is how a peer lists p3, the excess primary, once it fails with its
// slots undrained.
var failedP3 = clusterNodesLine("p3", "10.0.0.3", "master,fail", "-", valkey.SlotsRange{Start: 8192, End: 16383})

// withPeers sets what the node's CLUSTER NODES table says about other nodes:
// its own line is kept and lines follow it.
func withPeers(n *valkey.NodeState, lines ...string) *valkey.NodeState {
	table := ""
	if m := n.Myself(); m != nil {
		table = clusterNodesLine(m.Id, m.Host, m.Flags.String(), m.PrimaryId, m.Slots...) + "\n"
	}
	for _, l := range lines {
		table += l + "\n"
	}
	n.SetClusterNodesForTesting(table)
	return n
}

// scrapedNode returns a node that answered the scrape with its own line.
func scrapedNode(id, address, flags, primary string, slots ...valkey.SlotsRange) *valkey.NodeState {
	n := &valkey.NodeState{Id: id, Address: address}
	n.SetClusterNodesForTesting(clusterNodesLine(id, address, flags, primary, slots...) + "\n")
	return n
}

// primaryShard returns a shard whose primary is primaryNode(shard), scraped
// with a myself line that owns the slots.
func primaryShard(shard int, slots ...valkey.SlotsRange) *valkey.ShardState {
	id := fmt.Sprintf("p%d", shard)
	return &valkey.ShardState{
		Id:        fmt.Sprintf("s%d", shard),
		PrimaryId: id,
		Slots:     slots,
		Nodes:     []*valkey.NodeState{scrapedNode(id, fmt.Sprintf("10.0.0.%d", shard), "myself,master", "-", slots...)},
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

// TestSplitShardsForScaleInOrderIsStable: the scrape lists shards in no fixed
// order. Equal destinations and the excess shards must come out the same way
// every pass, so a failed move is retried against the same pair.
func TestSplitShardsForScaleInOrderIsStable(t *testing.T) {
	nodes := &valkeyiov1alpha1.ValkeyNodeList{}
	for i := range 4 {
		nodes.Items = append(nodes.Items, *primaryNode(i))
	}
	shards := []*valkey.ShardState{
		primaryShard(0, valkey.SlotsRange{Start: 0, End: 4095}),
		primaryShard(1, valkey.SlotsRange{Start: 4096, End: 8191}),
		primaryShard(2, valkey.SlotsRange{Start: 8192, End: 12287}),
		primaryShard(3, valkey.SlotsRange{Start: 12288, End: 16383}),
	}
	for _, order := range [][]int{{1, 0, 3, 2}, {0, 1, 2, 3}} {
		var listed []*valkey.ShardState
		for _, i := range order {
			listed = append(listed, shards[i])
		}
		remaining, draining := splitShardsForScaleIn(listed, nodes, 2)
		assert.Equal(t, []string{"p0", "p1"}, primaryIDs(remaining), "order %v", order)
		assert.Equal(t, []string{"p2", "p3"}, primaryIDs(draining), "order %v", order)
	}
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

// TestDeleteExcessValkeyNodesKeepsSlotOwners checks every node of an excess
// shard that owns slots is kept while a drained shard is deleted.
func TestDeleteExcessValkeyNodesKeepsSlotOwners(t *testing.T) {
	inSpec, owner, replica, drained := primaryNode(0), primaryNode(3), replicaNode(3), primaryNode(2)
	r, cluster, recorder := scaleInReconciler(t, inSpec, owner, replica, drained)
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{
		primaryShard(0, valkey.SlotsRange{Start: 0, End: 8191}),
		primaryShard(3, valkey.SlotsRange{Start: 8192, End: 16383}),
		primaryShard(2),
	}}

	deleted, _, kept, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodeList(inSpec, owner, replica, drained))
	require.NoError(t, err)
	assert.True(t, deleted)
	assert.Equal(t, []string{"c-3-0", "c-3-1"}, kept)
	assert.True(t, nodeExists(t, r.Client, inSpec))
	assert.True(t, nodeExists(t, r.Client, owner), "excess node that owns slots must be kept")
	assert.True(t, nodeExists(t, r.Client, replica), "replica of an excess shard that owns slots must be kept")
	assert.False(t, nodeExists(t, r.Client, drained))
	got := drainEvents(recorder)
	assert.Contains(t, got, "Warning ScaleInBlocked Keeping excess ValkeyNode c-3-0: shard 3 still owns slots; waiting for drain")
	assert.Contains(t, got, "Warning ScaleInBlocked Keeping excess ValkeyNode c-3-1: shard 3 still owns slots; waiting for drain")
}

// TestDeleteExcessValkeyNodesKeepsShardOfUnreachableOwner: the
// excess primary crashed mid-drain. Its replica holds the only other copy of
// the undrained slots.
func TestDeleteExcessValkeyNodesKeepsShardOfUnreachableOwner(t *testing.T) {
	inSpec, crashed, replica := primaryNode(0), primaryNode(3), replicaNode(3)
	crashed.Status.ClusterNodeID = "p3"
	r, cluster, recorder := scaleInReconciler(t, inSpec, crashed, replica)
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{
		primaryShard(0, valkey.SlotsRange{Start: 0, End: 8191}),
		{Id: "s3", PrimaryId: "p3", Nodes: []*valkey.NodeState{withPeers(
			scrapedNode("r3", "10.0.1.3", "myself,slave", "p3"),
			failedP3,
		)}},
	}}

	deleted, _, kept, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodeList(inSpec, crashed, replica))
	require.NoError(t, err)
	assert.False(t, deleted)
	assert.Equal(t, []string{"c-3-0", "c-3-1"}, kept)
	assert.True(t, nodeExists(t, r.Client, crashed))
	assert.True(t, nodeExists(t, r.Client, replica), "replica of a crashed excess primary must be kept")
	got := drainEvents(recorder)
	assert.Contains(t, got, "Warning ScaleInBlocked Keeping excess ValkeyNode c-3-0: shard 3 owns slots and its primary is unreachable")
	assert.Contains(t, got, "Warning ScaleInBlocked Keeping excess ValkeyNode c-3-1: shard 3 owns slots and its primary is unreachable")
}

// TestDeleteExcessValkeyNodesKeepsNodeWithRecordedIDAndNoPodIP: a persistent
// primary's pod is being recreated. Its peers still list its ID with slots.
func TestDeleteExcessValkeyNodesKeepsNodeWithRecordedIDAndNoPodIP(t *testing.T) {
	inSpec, recreating := primaryNode(0), primaryNode(3)
	recreating.Status.ClusterNodeID = "p3"
	recreating.Status.PodIP = ""
	r, cluster, _ := scaleInReconciler(t, inSpec, recreating)
	shard0 := primaryShard(0, valkey.SlotsRange{Start: 0, End: 8191})
	withPeers(shard0.Nodes[0], failedP3)
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{shard0}}

	deleted, _, kept, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodeList(inSpec, recreating))
	require.NoError(t, err)
	assert.False(t, deleted)
	assert.Equal(t, []string{"c-3-0"}, kept)
	assert.True(t, nodeExists(t, r.Client, recreating))
}

// TestDeleteExcessValkeyNodesKeepsShardWhenPrimaryMatchesNoValkeyNode: the
// excess primary's pod was replaced without persistence, so its ID is gone
// from every ValkeyNode. Its replica still follows the old ID.
func TestDeleteExcessValkeyNodesKeepsShardWhenPrimaryMatchesNoValkeyNode(t *testing.T) {
	for name, shards := range map[string]func() []*valkey.ShardState{
		"replica scraped": func() []*valkey.ShardState {
			return []*valkey.ShardState{
				primaryShard(0, valkey.SlotsRange{Start: 0, End: 8191}),
				{Id: "s3", PrimaryId: "p3", Nodes: []*valkey.NodeState{
					withPeers(scrapedNode("r3", "10.0.1.3", "myself,slave", "p3"), failedP3),
				}},
			}
		},
		"replica missed the scrape": func() []*valkey.ShardState {
			shard0 := primaryShard(0, valkey.SlotsRange{Start: 0, End: 8191})
			withPeers(shard0.Nodes[0], failedP3, clusterNodesLine("r3", "10.0.1.3", "slave,fail?", "p3"))
			return []*valkey.ShardState{shard0}
		},
	} {
		t.Run(name, func(t *testing.T) {
			inSpec, replaced, replica := primaryNode(0), primaryNode(3), replicaNode(3)
			replaced.Status.PodIP = "10.0.0.13"
			replica.Status.ClusterNodeID = "r3"
			r, cluster, _ := scaleInReconciler(t, inSpec, replaced, replica)
			state := &valkey.ClusterState{Shards: shards()}

			deleted, _, kept, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodeList(inSpec, replaced, replica))
			require.NoError(t, err)
			assert.False(t, deleted)
			assert.Equal(t, []string{"c-3-0", "c-3-1"}, kept)
			assert.True(t, nodeExists(t, r.Client, replaced), "the new empty pod stays with its shard")
			assert.True(t, nodeExists(t, r.Client, replica))
		})
	}
}

// TestDeleteExcessValkeyNodesMatchesOwnerByPodIPWithoutRecordedID: a node that
// was down when the operator was upgraded has no recorded ID.
func TestDeleteExcessValkeyNodesMatchesOwnerByPodIPWithoutRecordedID(t *testing.T) {
	inSpec, owner := primaryNode(0), primaryNode(3)
	r, cluster, recorder := scaleInReconciler(t, inSpec, owner)
	shard0 := primaryShard(0, valkey.SlotsRange{Start: 0, End: 8191})
	withPeers(shard0.Nodes[0], failedP3)
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{shard0}}

	_, _, kept, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodeList(inSpec, owner))
	require.NoError(t, err)
	assert.Equal(t, []string{"c-3-0"}, kept)
	assert.True(t, nodeExists(t, r.Client, owner))
	assert.Contains(t, drainEvents(recorder), "Warning ScaleInBlocked Keeping excess ValkeyNode c-3-0: shard 3 owns slots and its primary is unreachable")
}

// TestDeleteExcessValkeyNodesIgnoresEntryAtPodIPOnceIDIsRecorded: the excess
// primary was evicted mid-drain without persistence or a replica. Its new pod
// took the same IP and recorded its own ID, so it holds none of the old data.
func TestDeleteExcessValkeyNodesIgnoresEntryAtPodIPOnceIDIsRecorded(t *testing.T) {
	inSpec, rejoined := primaryNode(0), primaryNode(3)
	rejoined.Status.ClusterNodeID = "new"
	r, cluster, _ := scaleInReconciler(t, inSpec, rejoined)
	shard0 := primaryShard(0, valkey.SlotsRange{Start: 0, End: 8191})
	withPeers(shard0.Nodes[0], failedP3)
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{
		shard0,
		{Id: "s3", PrimaryId: "new", Nodes: []*valkey.NodeState{scrapedNode("new", "10.0.0.3", "myself,master", "-")}},
	}}

	deleted, _, kept, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodeList(inSpec, rejoined))
	require.NoError(t, err)
	assert.True(t, deleted)
	assert.Empty(t, kept)
	assert.False(t, nodeExists(t, r.Client, rejoined))
}

// TestDeleteExcessValkeyNodesDeletesPendingExcessNodeWhileOtherSlotsUnowned: a
// pod that never ran holds no slots and has no say over slots it never owned.
func TestDeleteExcessValkeyNodesDeletesPendingExcessNodeWhileOtherSlotsUnowned(t *testing.T) {
	inSpec, pending := primaryNode(0), primaryNode(4)
	pending.Status.PodIP = ""
	r, cluster, _ := scaleInReconciler(t, inSpec, pending)
	shard0 := primaryShard(0, valkey.SlotsRange{Start: 0, End: 8191})
	// A dead owner no ValkeyNode matches keeps no unrelated shard.
	withPeers(shard0.Nodes[0], clusterNodesLine("px", "10.0.0.99", "master,fail", "-", valkey.SlotsRange{Start: 8192, End: 16383}))
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{shard0}}

	deleted, _, kept, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodeList(inSpec, pending))
	require.NoError(t, err)
	assert.True(t, deleted)
	assert.Empty(t, kept)
	assert.False(t, nodeExists(t, r.Client, pending))
}

// TestDeleteExcessValkeyNodesIgnoresStaleTableAboutScrapedNode: a node cut off
// from the bus still lists a drained primary with slots. The drained primary
// answered, so its own line counts.
func TestDeleteExcessValkeyNodesIgnoresStaleTableAboutScrapedNode(t *testing.T) {
	inSpec, drained := primaryNode(0), primaryNode(3)
	r, cluster, _ := scaleInReconciler(t, inSpec, drained)
	shard0 := primaryShard(0, valkey.SlotsRange{Start: 0, End: 16383})
	withPeers(shard0.Nodes[0], clusterNodesLine("p3", "10.0.0.3", "master", "-", valkey.SlotsRange{Start: 8192, End: 16383}))
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{shard0, primaryShard(3)}}

	deleted, _, kept, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodeList(inSpec, drained))
	require.NoError(t, err)
	assert.True(t, deleted)
	assert.Empty(t, kept)
	assert.False(t, nodeExists(t, r.Client, drained))
}

// TestDeleteExcessValkeyNodesUsesScrapedIDOverStaleRecord: the hand-off wiped
// the former primary, which answers under a new ID before its status catches
// up.
func TestDeleteExcessValkeyNodesUsesScrapedIDOverStaleRecord(t *testing.T) {
	inSpec, wiped, promoted := primaryNode(0), primaryNode(3), replicaNode(3)
	wiped.Status.ClusterNodeID = "old"
	r, cluster, _ := scaleInReconciler(t, inSpec, wiped, promoted)
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{
		primaryShard(0, valkey.SlotsRange{Start: 0, End: 8191}),
		{Id: "s3", PrimaryId: "r3", Slots: []valkey.SlotsRange{{Start: 8192, End: 16383}}, Nodes: []*valkey.NodeState{
			scrapedNode("r3", "10.0.1.3", "myself,master", "-", valkey.SlotsRange{Start: 8192, End: 16383}),
			scrapedNode("new", "10.0.0.3", "myself,slave", "r3"),
		}},
	}}
	nodes := nodeList(inSpec, wiped, promoted)

	assert.ElementsMatch(t, []string{"new", "old"}, candidateIDs(&nodes.Items[1], knownIDs(&nodes.Items[1], state), state.NodeViews()))
	_, _, kept, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodes)
	require.NoError(t, err)
	assert.Equal(t, []string{"c-3-0", "c-3-1"}, kept)
	assert.True(t, nodeExists(t, r.Client, wiped))
}

// TestDeleteExcessValkeyNodesKeepsRecordedIDWhenPodIPIsTakenByAnotherNode: a
// stale pod IP that another pod now answers on must not hide the recorded ID.
func TestDeleteExcessValkeyNodesKeepsRecordedIDWhenPodIPIsTakenByAnotherNode(t *testing.T) {
	newPrimary, inSpec, stale := primaryNode(0), primaryNode(1), primaryNode(3)
	stale.Status.ClusterNodeID = "p3"
	stale.Status.PodIP = newPrimary.Status.PodIP
	r, cluster, _ := scaleInReconciler(t, newPrimary, inSpec, stale)
	shard0 := primaryShard(0)
	withPeers(shard0.Nodes[0], failedP3)
	state := &valkey.ClusterState{
		Shards:       []*valkey.ShardState{primaryShard(1, valkey.SlotsRange{Start: 0, End: 8191})},
		PendingNodes: shard0.Nodes,
	}

	_, _, kept, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodeList(newPrimary, inSpec, stale))
	require.NoError(t, err)
	assert.Equal(t, []string{"c-3-0"}, kept)
	assert.True(t, nodeExists(t, r.Client, stale))
}

// TestDeleteExcessValkeyNodesDeletesLeftoverReplicaOfRecordedInSpecPrimary: the
// in-spec primary did not answer, and its ValkeyNode is matched by its recorded
// ID alone.
func TestDeleteExcessValkeyNodesDeletesLeftoverReplicaOfRecordedInSpecPrimary(t *testing.T) {
	inSpec, other, leftover := primaryNode(0), primaryNode(1), primaryNode(3)
	inSpec.Status.ClusterNodeID = "p0"
	inSpec.Status.PodIP = "10.0.0.20"
	r, cluster, _ := scaleInReconciler(t, inSpec, other, leftover)
	shard1 := primaryShard(1, valkey.SlotsRange{Start: 8192, End: 16383})
	withPeers(shard1.Nodes[0], clusterNodesLine("p0", "10.0.0.0", "master,fail", "-", valkey.SlotsRange{Start: 0, End: 8191}))
	shard1.Nodes = append(shard1.Nodes, scrapedNode("p3", "10.0.0.3", "myself,slave", "p0"))
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{shard1}}

	deleted, _, kept, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodeList(inSpec, other, leftover))
	require.NoError(t, err)
	assert.True(t, deleted)
	assert.Empty(t, kept)
	assert.False(t, nodeExists(t, r.Client, leftover))
}

// TestDeleteExcessValkeyNodesDeletesLeftoverReplicaOfInSpecPrimary: a drained
// primary that became a replica of a shard that stays holds none of its own
// shard's data.
func TestDeleteExcessValkeyNodesDeletesLeftoverReplicaOfInSpecPrimary(t *testing.T) {
	inSpec, leftover := primaryNode(0), primaryNode(3)
	r, cluster, _ := scaleInReconciler(t, inSpec, leftover)
	shard0 := primaryShard(0, valkey.SlotsRange{Start: 0, End: 16383})
	shard0.Nodes = append(shard0.Nodes, scrapedNode("p3", "10.0.0.3", "myself,slave", "p0"))
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{shard0}}

	deleted, _, kept, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodeList(inSpec, leftover))
	require.NoError(t, err)
	assert.True(t, deleted)
	assert.Empty(t, kept)
	assert.False(t, nodeExists(t, r.Client, leftover))
}

// TestDeleteExcessValkeyNodesKeepsReplicaWhenPrimaryLabelDoesNotParse: with
// the primary's shard unknown, the replica may hold its own shard's data.
func TestDeleteExcessValkeyNodesKeepsReplicaWhenPrimaryLabelDoesNotParse(t *testing.T) {
	unlabelled, leftover := primaryNode(0), primaryNode(3)
	unlabelled.Labels[LabelShardIndex] = "x"
	r, cluster, _ := scaleInReconciler(t, unlabelled, leftover)
	shard0 := primaryShard(0, valkey.SlotsRange{Start: 0, End: 16383})
	shard0.Nodes = append(shard0.Nodes, scrapedNode("p3", "10.0.0.3", "myself,slave", "p0"))
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{shard0}}

	_, _, kept, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodeList(unlabelled, leftover))
	require.NoError(t, err)
	assert.Equal(t, []string{"c-3-0"}, kept)
	assert.True(t, nodeExists(t, r.Client, leftover))
}

// TestDeleteExcessValkeyNodesKeepsReplicaWhenPrimaryHostIsReused: the excess
// primary's pod was replaced without persistence, and an in-spec pod that has
// not answered yet took its old IP. A pod-IP match does not make the dead
// primary that in-spec node's.
func TestDeleteExcessValkeyNodesKeepsReplicaWhenPrimaryHostIsReused(t *testing.T) {
	inSpec, reused, replaced, replica := primaryNode(0), primaryNode(1), primaryNode(3), replicaNode(3)
	reused.Status.PodIP = "10.0.0.3"
	replaced.Status.PodIP = "10.0.0.13"
	r, cluster, _ := scaleInReconciler(t, inSpec, reused, replaced, replica)
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{
		primaryShard(0, valkey.SlotsRange{Start: 0, End: 8191}),
		{Id: "s3", PrimaryId: "p3", Nodes: []*valkey.NodeState{withPeers(
			scrapedNode("r3", "10.0.1.3", "myself,slave", "p3"),
			failedP3,
		)}},
	}}

	deleted, _, kept, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodeList(inSpec, reused, replaced, replica))
	require.NoError(t, err)
	assert.False(t, deleted)
	assert.Equal(t, []string{"c-3-0", "c-3-1"}, kept)
	assert.True(t, nodeExists(t, r.Client, replica), "replica of the dead excess primary must be kept")
}

// TestDeleteExcessValkeyNodesStillTrimsInSpecShards: only excess shards are
// kept for their slots. A replica scale-down goes ahead.
func TestDeleteExcessValkeyNodesStillTrimsInSpecShards(t *testing.T) {
	primary, replica := primaryNode(0), replicaNode(0)
	r, cluster, _ := scaleInReconciler(t, primary, replica)
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{
		primaryShard(0, valkey.SlotsRange{Start: 0, End: 8191}),
	}}

	deleted, _, kept, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodeList(primary, replica))
	require.NoError(t, err)
	assert.True(t, deleted)
	assert.Empty(t, kept)
	assert.True(t, nodeExists(t, r.Client, primary))
	assert.False(t, nodeExists(t, r.Client, replica))
}

// TestDeleteExcessValkeyNodesKeepsPrimaryOnExcessNodeIndex: a live primary
// past replicasPerShard waits for a synced replica the new count keeps.
func TestDeleteExcessValkeyNodesKeepsPrimaryOnExcessNodeIndex(t *testing.T) {
	promoted := primaryNode(0)
	promoted.Name = "c-0-1"
	promoted.Labels[LabelNodeIndex] = "1"
	r, cluster, recorder := scaleInReconciler(t, promoted)
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{
		primaryShard(0, valkey.SlotsRange{Start: 0, End: 16383}),
	}}

	_, deferred, kept, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodeList(promoted))
	require.NoError(t, err)
	assert.Empty(t, kept)
	assert.True(t, deferred)
	assert.True(t, nodeExists(t, r.Client, promoted))
	assert.Empty(t, drainEvents(recorder))
	assert.Equal(t, "Scale-in is waiting to delete ValkeyNode c-0-1: it is the primary of shard 0 and no kept replica is synced",
		meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionProgressing).Message)
}

// TestDeleteExcessValkeyNodesKeepsReplicasFromEitherField: node index 1 is in
// spec whether the cluster asks for one replica through replicasPerShard or
// through the deprecated replicas field.
func TestDeleteExcessValkeyNodesKeepsReplicasFromEitherField(t *testing.T) {
	for name, spec := range map[string]valkeyiov1alpha1.ValkeyClusterSpec{
		"replicasPerShard": {Shards: 2, ReplicasPerShard: 1},
		"replicas":         {Shards: 2, Replicas: 1}, //nolint:staticcheck // the deprecated field must keep its replicas
	} {
		t.Run(name, func(t *testing.T) {
			replica := primaryNode(0)
			replica.Name = "c-0-1"
			replica.Labels[LabelNodeIndex] = "1"
			replica.Status.Role = RoleReplica
			r, cluster, _ := scaleInReconciler(t, primaryNode(0), replica)
			cluster.Spec = spec
			state := &valkey.ClusterState{Shards: []*valkey.ShardState{
				primaryShard(0, valkey.SlotsRange{Start: 0, End: 16383}),
			}}

			deleted, _, _, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state, nodeList(primaryNode(0), replica))
			require.NoError(t, err)
			assert.False(t, deleted)
			assert.True(t, nodeExists(t, r.Client, replica))
		})
	}
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

// unreachableExcessOwner returns excess ValkeyNode c-3-0, which did not
// answer the scrape, and a scrape whose in-spec primary lists its recorded ID
// with the slots it has not drained.
func unreachableExcessOwner() (*valkeyiov1alpha1.ValkeyNode, *valkey.ClusterState) {
	node := primaryNode(3)
	node.Status.ClusterNodeID = "p3"
	shard0 := primaryShard(0, valkey.SlotsRange{Start: 0, End: 8191})
	withPeers(shard0.Nodes[0], failedP3)
	return node, &valkey.ClusterState{Shards: []*valkey.ShardState{shard0}}
}

// TestHandleScaleInReportsKeptNodes: a kept excess node blocks scale-in
// visibly instead of letting the cluster go Ready around it.
func TestHandleScaleInReportsKeptNodes(t *testing.T) {
	unreachable, state := unreachableExcessOwner()
	r, cluster, _ := scaleInReconciler(t, primaryNode(0), unreachable)

	_, requeue := r.handleScaleIn(context.Background(), cluster, state, nodeList(primaryNode(0), unreachable))
	assert.True(t, requeue)
	progressing := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionProgressing)
	require.NotNil(t, progressing)
	assert.Equal(t, valkeyiov1alpha1.ReasonScaleInBlocked, progressing.Reason)
	assert.Equal(t, "Keeping the nodes of excess shards (c-3-0) until their slots are drained", progressing.Message)
}

// TestForgetHolds: a failing ID is not forgotten while a ValkeyNode
// records it, or records a replica of it that did not answer the scrape.
func TestForgetHolds(t *testing.T) {
	recorded := func(node *valkeyiov1alpha1.ValkeyNode, id string) *valkeyiov1alpha1.ValkeyNode {
		node.Status.ClusterNodeID = id
		return node
	}
	// shardWith returns a shard whose primary p0 lists the given lines.
	shardWith := func(lines ...string) *valkey.ClusterState {
		shard := primaryShard(0, valkey.SlotsRange{Start: 0, End: 5295})
		withPeers(shard.Nodes[0], lines...)
		return &valkey.ClusterState{Shards: []*valkey.ShardState{shard}}
	}
	failingLine := clusterNodesLine("p3", "10.0.0.3", "master,fail", "-", valkey.SlotsRange{Start: 5296, End: 8191})
	unscrapedReplica := clusterNodesLine("r3", "10.0.1.3", "slave,fail", "p3")

	answered := shardWith(failingLine, unscrapedReplica)
	answered.PendingNodes = []*valkey.NodeState{scrapedNode("r4", "10.0.1.4", "myself,slave", "p3")}

	repliedAsSlave := shardWith(failingLine)
	repliedAsSlave.PendingNodes = []*valkey.NodeState{scrapedNode("r3", "10.0.1.3", "myself,slave", "p3")}

	for name, tc := range map[string]struct {
		state *valkey.ClusterState
		nodes *valkeyiov1alpha1.ValkeyNodeList
		want  string
	}{
		"a ValkeyNode records the failing ID": {
			state: shardWith(failingLine),
			nodes: nodeList(primaryNode(0), recorded(primaryNode(3), "p3")),
			want:  "c-3-0",
		},
		"a recorded replica did not answer": {
			state: shardWith(failingLine, unscrapedReplica),
			nodes: nodeList(primaryNode(0), recorded(replicaNode(3), "r3")),
			want:  "c-3-1",
		},
		"a recorded replica did not answer while another replica did": {
			state: answered,
			nodes: nodeList(primaryNode(0), recorded(replicaNode(3), "r3")),
			want:  "c-3-1",
		},
		"the recorded replica answered": {
			state: repliedAsSlave,
			nodes: nodeList(primaryNode(0), recorded(replicaNode(3), "r3")),
		},
		"the listed replica is recorded by no ValkeyNode": {
			state: shardWith(failingLine, clusterNodesLine("r9", "10.0.1.9", "slave,fail", "p3")),
			nodes: nodeList(primaryNode(0), recorded(replicaNode(3), "r3")),
		},
		"nothing records or replicates the failing ID": {
			state: shardWith(failingLine),
			nodes: nodeList(primaryNode(0)),
		},
	} {
		t.Run(name, func(t *testing.T) {
			name := replicaAwaitedBy("p3", tc.state.NodeViews(), tc.nodes)
			if name == "" {
				name = recordedBy("p3", tc.nodes)
			}
			assert.Equal(t, tc.want, name)
		})
	}
}

// TestForgetStaleNodes: a failing ID that a ValkeyNode records is not
// forgotten, unless a replica that answered the scrape follows it and the
// cluster has no failover quorum and no persistence. The ValkeyNode's new pod
// is not ready, so no pod IP matches the failing entry.
func TestForgetStaleNodes(t *testing.T) {
	for name, tc := range map[string]struct {
		recordedID string
		replica    bool
		persistent bool
		forget     bool
	}{
		"a ValkeyNode records the failing ID":      {recordedID: "p3"},
		"no ValkeyNode records the failing ID":     {forget: true},
		"a ValkeyNode records a different node ID": {recordedID: "new", forget: true},
		"a recorded ID with a scraped replica and no quorum": {
			recordedID: "p3", replica: true, forget: true,
		},
		"a recorded ID with a scraped replica and no quorum, with persistence": {
			recordedID: "p3", replica: true, persistent: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			c := vmock.NewClient(ctrl)
			if tc.forget {
				c.EXPECT().Do(gomock.Any(), vmock.Match("CLUSTER", "FORGET", "p3")).
					Return(vmock.Result(vmock.ValkeyString("OK")))
			}
			shard := primaryShard(0, valkey.SlotsRange{Start: 0, End: 8191})
			shard.Nodes[0].Client = c
			withPeers(shard.Nodes[0], failedP3)
			state := &valkey.ClusterState{Shards: []*valkey.ShardState{shard}}
			if tc.replica {
				state.Shards = append(state.Shards, &valkey.ShardState{Id: "s3", PrimaryId: "p3",
					Nodes: []*valkey.NodeState{scrapedNode("r3", "10.0.1.3", "myself,slave", "p3")}})
			}
			recreating := primaryNode(3)
			recreating.Status.PodIP = "10.0.0.13"
			recreating.Status.ClusterNodeID = tc.recordedID
			r, cluster, _ := scaleInReconciler(t)
			if tc.persistent {
				cluster.Spec.Persistence = &valkeyiov1alpha1.PersistenceSpec{}
			}
			require.False(t, state.HasFailoverQuorum())

			r.forgetStaleNodes(context.Background(), cluster, state, nodeList(primaryNode(0), recreating))
		})
	}
}

// TestHandleScaleInClearsStaleRebalanceFailed: a failed drain batch must not
// leave Degraded behind once scale-in is only waiting.
func TestHandleScaleInClearsStaleRebalanceFailed(t *testing.T) {
	unreachable, state := unreachableExcessOwner()
	r, cluster, _ := scaleInReconciler(t, primaryNode(0), unreachable)
	setCondition(cluster, valkeyiov1alpha1.ConditionDegraded, valkeyiov1alpha1.ReasonRebalanceFailed, "drain failed", metav1.ConditionTrue)

	_, requeue := r.handleScaleIn(context.Background(), cluster, state, nodeList(primaryNode(0), unreachable))
	assert.True(t, requeue)
	assert.Nil(t, meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionDegraded))
}

// TestHandleScaleInKeepsOtherDegradedReasons: only RebalanceFailed belongs to
// scale-in.
func TestHandleScaleInKeepsOtherDegradedReasons(t *testing.T) {
	unreachable, state := unreachableExcessOwner()
	r, cluster, _ := scaleInReconciler(t, primaryNode(0), unreachable)
	setCondition(cluster, valkeyiov1alpha1.ConditionDegraded, valkeyiov1alpha1.ReasonUsersACLUnresolved, "secret missing", metav1.ConditionTrue)

	_, requeue := r.handleScaleIn(context.Background(), cluster, state, nodeList(primaryNode(0), unreachable))
	assert.True(t, requeue)
	degraded := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)
	require.NotNil(t, degraded)
	assert.Equal(t, valkeyiov1alpha1.ReasonUsersACLUnresolved, degraded.Reason)
}
