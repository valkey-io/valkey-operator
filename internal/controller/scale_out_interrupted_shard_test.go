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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vclient "github.com/valkey-io/valkey-go"
	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

func scaleOutInterruptedFixture() (*valkey.ClusterState, *valkeyiov1alpha1.ValkeyNodeList) {
	// gh-491 shape: a scale-out to 4 shards was interrupted after shard 3
	// joined with its slot range but before shard 2 ever joined; the spec
	// was then lowered to 3. Three shards are live, so the count matches
	// the spec, yet shard 3 still owns slots 12288-16383.
	mk := func(name, shard, node, ip string) *valkeyiov1alpha1.ValkeyNode {
		return &valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{
				LabelCluster:    "wedge",
				LabelShardIndex: shard,
				LabelNodeIndex:  node,
			}},
			Status: valkeyiov1alpha1.ValkeyNodeStatus{PodIP: ip},
		}
	}
	n00 := mk("wedge-0-0", "0", "0", "10.0.0.1")
	n10 := mk("wedge-1-0", "1", "0", "10.0.1.1")
	n30 := mk("wedge-3-0", "3", "0", "10.0.3.1")
	nodes := &valkeyiov1alpha1.ValkeyNodeList{Items: []valkeyiov1alpha1.ValkeyNode{*n00, *n10, *n30}}
	shard := func(idx int, ip string, slots []valkey.SlotsRange) *valkey.ShardState {
		id := fmt.Sprintf("node-%d-0", idx)
		return &valkey.ShardState{
			Id:        fmt.Sprintf("shard-%d", idx),
			PrimaryId: id,
			Slots:     slots,
			Nodes:     []*valkey.NodeState{{Address: ip, Id: id, Flags: []string{"master"}}},
		}
	}
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{
		shard(0, "10.0.0.1", []valkey.SlotsRange{{Start: 0, End: 4095}}),
		shard(1, "10.0.1.1", []valkey.SlotsRange{{Start: 4096, End: 8191}}),
		shard(3, "10.0.3.1", []valkey.SlotsRange{{Start: 12288, End: 16383}}),
	}}
	return state, nodes
}

// scriptedPrimaryAt builds a NodeState whose client dials the scripted RESP
// server but whose address stays the fixture address, so shard label
// resolution (shardIndexFromState) keeps matching the ValkeyNode.
func scriptedPrimaryAt(t *testing.T, s *migrationStateServer, id, host string) *valkey.NodeState {
	t.Helper()
	c, err := vclient.NewClient(vclient.ClientOption{
		InitAddress:       []string{s.addr},
		ForceSingleClient: true,
		DisableCache:      true,
		DisableRetry:      true,
		PipelineMultiplex: -1,
	})
	if err != nil {
		t.Fatalf("client init: %v", err)
	}
	t.Cleanup(c.Close)
	return &valkey.NodeState{Client: c, Address: host, Port: 6379, Id: id, Flags: []string{"master"}}
}

func TestLiveShardOutsideSpecDetectsInterruptedScaleOut(t *testing.T) {
	state, nodes := scaleOutInterruptedFixture()
	if !liveShardOutsideSpec(state, nodes, 3) {
		t.Fatal("a live shard at index 3 with spec.shards=3 must be detected")
	}
	if liveShardOutsideSpec(state, nodes, 4) {
		t.Fatal("no shard is outside spec.shards=4")
	}
	if liveShardOutsideSpec(nil, nodes, 3) {
		t.Fatal("a nil state has no shards outside the spec")
	}
	// A shard whose ValkeyNode labels are missing maps to -1 and must not
	// count as outside: shardIndexFromState cannot place it, and -1 is
	// below any real shard count.
	unlabeled := &valkey.ClusterState{Shards: []*valkey.ShardState{{
		Id: "shard-x", PrimaryId: "x", Nodes: []*valkey.NodeState{{Address: "10.9.9.9", Id: "x"}},
	}}}
	if liveShardOutsideSpec(unlabeled, nodes, 3) {
		t.Fatal("an unlabeled shard must not be treated as outside the spec")
	}
}
func TestHandleScaleInDrainsLiveShardOutsideSpec(t *testing.T) {
	// The gh-491 regression: three live shards match spec.shards=3, so the
	// old count guard skipped draining entirely and
	// deleteExcessValkeyNodes deleted shard 3's primary while it still
	// owned slots 12288-16383. With the label-based check the drain pass
	// runs, issues the slot migration and requeues; the shard's
	// ValkeyNode survives until a later pass sees its slots gone.
	server := newMigrationStateServer(t, func() [][]string { return nil })
	state, nodes := scaleOutInterruptedFixture()

	// The drain source is shard 3's primary; it dials the scripted server so
	// the migration-in-progress probe runs on a real client, while keeping
	// the fixture address so the ValkeyNode labels still resolve to
	// shard 3.
	src := state.Shards[2]
	src.Nodes[0] = scriptedPrimaryAt(t, server, src.Nodes[0].Id, "10.0.3.1")
	// Gossip has already introduced the in-spec primaries to shard 3's
	// primary, so the drain plan's KnowsNode gate passes and the pass
	// reaches the actual CLUSTER MIGRATESLOTS instead of requeueing at the
	// peer-table check. The addresses match the fixture ValkeyNodes, so the
	// label-based shard resolution stays intact.
	src.Nodes[0].SetClusterNodesForTesting(
		"node-3-0 10.0.3.1:6379@16379 myself,master - 0 0 3 connected 12288-16383\n" +
			"node-0-0 10.0.0.1:6379@16379 master - 0 0 1 connected 0-4095\n" +
			"node-1-0 10.0.1.1:6379@16379 master - 0 0 2 connected 4096-8191\n")

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

	result, requeue := r.handleScaleIn(context.Background(), cluster, state, nodes)
	if !requeue {
		t.Fatal("handleScaleIn must requeue while a live out-of-spec shard still owns slots")
	}
	if result.RequeueAfter == 0 {
		t.Fatal("the requeue must carry an interval")
	}

	// The drain must have reached the actual slot migration: the source's
	// peer table knows the destination (gossip has introduced it), so the
	// pass issues CLUSTER MIGRATESLOTS for shard 3's batch before requeueing.
	if !server.hasCommand("CLUSTER", "MIGRATESLOTS") {
		t.Fatalf("expected CLUSTER MIGRATESLOTS to be issued, got %v", server.recordedCommands())
	}
	// The destination node-id argument must be the in-spec destination
	// primary the plan picked, not a fixture leftover.
	dstSeen := false
	for _, cmd := range server.recordedCommands() {
		if len(cmd) >= 2 && strings.EqualFold(cmd[0], "CLUSTER") && strings.EqualFold(cmd[1], "MIGRATESLOTS") {
			for _, arg := range cmd[2:] {
				if arg == "node-0-0" {
					dstSeen = true
				}
			}
		}
	}
	if !dstSeen {
		t.Fatalf("CLUSTER MIGRATESLOTS must target the in-spec destination primary node-0-0, got %v", server.recordedCommands())
	}

	// The out-of-spec shard's ValkeyNode must survive the draining pass:
	// its slots are still assigned, so deleting the primary now would lose
	// the keys it owns (the gh-491 failure mode).
	err := c.Get(context.Background(), client.ObjectKey{Name: "wedge-3-0", Namespace: "ns"}, &valkeyiov1alpha1.ValkeyNode{})
	require.NoError(t, err, "the drained shard's primary must not be deleted while it still owns slots")

	// Once a later scrape sees the shard's slots gone, the same pass
	// deletes its ValkeyNode: drain first, delete after.
	drained := state.Shards[2]
	drained.Slots = nil
	_, requeue = r.handleScaleIn(context.Background(), cluster, state, nodes)
	if !requeue {
		t.Fatal("the deletion pass must requeue")
	}
	err = c.Get(context.Background(), client.ObjectKey{Name: "wedge-3-0", Namespace: "ns"}, &valkeyiov1alpha1.ValkeyNode{})
	require.Error(t, err, "once drained, the out-of-spec shard's ValkeyNode is deleted")
	require.True(t, k8serrors.IsNotFound(err))
}

// TestLabelTriggeredDrainSparesUnresolvedShard pins the classification of a
// shard whose ValkeyNode labels cannot resolve it (its address matches no
// ValkeyNode PodIP): when the drain pass runs on the label trigger alone -
// the live count already equals spec.shards - the unresolved shard is not
// provably excess, so it must keep every slot and stay available as a drain
// destination. Only the shard the labels place outside the spec is drained.
func TestLabelTriggeredDrainSparesUnresolvedShard(t *testing.T) {
	server := newMigrationStateServer(t, func() [][]string { return nil })

	mk := func(name, shard, node, ip string) *valkeyiov1alpha1.ValkeyNode {
		return &valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{
				LabelCluster:    "wedge",
				LabelShardIndex: shard,
				LabelNodeIndex:  node,
			}},
			Status: valkeyiov1alpha1.ValkeyNodeStatus{PodIP: ip},
		}
	}
	// No ValkeyNode carries 10.0.2.1, so the middle shard resolves to -1.
	nodes := &valkeyiov1alpha1.ValkeyNodeList{Items: []valkeyiov1alpha1.ValkeyNode{
		*mk("wedge-0-0", "0", "0", "10.0.0.1"),
		*mk("wedge-3-0", "3", "0", "10.0.3.1"),
	}}

	inSpec := &valkey.ShardState{
		Id:        "shard-0",
		PrimaryId: "node-0-0",
		Slots:     []valkey.SlotsRange{{Start: 0, End: 4095}},
		Nodes:     []*valkey.NodeState{{Address: "10.0.0.1", Port: 6379, Id: "node-0-0", Flags: []string{"master"}}},
	}
	unresolved := &valkey.ShardState{
		Id:        "shard-x",
		PrimaryId: "node-x-0",
		Slots:     []valkey.SlotsRange{{Start: 8192, End: 12287}},
		Nodes:     []*valkey.NodeState{scriptedPrimaryAt(t, server, "node-x-0", "10.0.2.1")},
	}
	outOfSpec := &valkey.ShardState{
		Id:        "shard-3",
		PrimaryId: "node-3-0",
		Slots:     []valkey.SlotsRange{{Start: 12288, End: 16383}},
		Nodes:     []*valkey.NodeState{scriptedPrimaryAt(t, server, "node-3-0", "10.0.3.1")},
	}
	// Both scripted primaries know the in-spec destination, so on a body
	// that drained the unresolved shard too, that migration would actually
	// be issued and observable instead of hidden behind the gossip gate.
	peerView := "node-x-0 10.0.2.1:6379@16379 myself,master - 0 0 0 connected 8192-12287\n" +
		"node-3-0 10.0.3.1:6379@16379 master - 0 0 1 connected 12288-16383\n" +
		"node-0-0 10.0.0.1:6379@16379 master - 0 0 2 connected 0-4095\n"
	for _, shard := range []*valkey.ShardState{unresolved, outOfSpec} {
		shard.Nodes[0].SetClusterNodesForTesting(peerView)
	}
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{inSpec, unresolved, outOfSpec}}

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

	if _, requeue := r.handleScaleIn(context.Background(), cluster, state, nodes); !requeue {
		t.Fatal("the out-of-spec shard's slots must trigger a requeue")
	}

	// The drain must have issued exactly the out-of-spec shard's batch:
	// slots starting at 12288, targeting the in-spec primary.
	migrations := 0
	for _, cmd := range server.recordedCommands() {
		if len(cmd) >= 2 && strings.EqualFold(cmd[0], "CLUSTER") && strings.EqualFold(cmd[1], "MIGRATESLOTS") {
			migrations++
			joined := strings.Join(cmd, " ")
			if !strings.Contains(joined, "12288") {
				t.Fatalf("slot migration issued from a shard other than the out-of-spec one: %q", joined)
			}
			if !strings.Contains(joined, "node-0-0") {
				t.Fatalf("the drain must target the in-spec primary: %q", joined)
			}
		}
	}
	if migrations == 0 {
		t.Fatalf("expected a CLUSTER MIGRATESLOTS draining shard 3, got %v", server.recordedCommands())
	}
	// The unresolved shard keeps every slot it owns: its batch would start
	// at 8192, and that slot appears in no recorded command.
	for _, cmd := range server.recordedCommands() {
		if joined := strings.Join(cmd, " "); strings.Contains(joined, "8192") {
			t.Fatalf("the unresolved shard must not be drained: %q", joined)
		}
	}
}

// TestHandleScaleInDefersLabelDeletionWhileUnresolvedShardOwnsSlots pins the
// cleanup guard in front of deleteExcessValkeyNodes. That pass deletes purely
// by shard-index label, and an unresolved live shard's serving pod cannot be
// identified, so a node that looks excess by label cannot be proven not to be
// that pod - deleting it would delete its StatefulSet and pod while the shard
// still owns slots, undoing the drain pass's protection. Here the live count
// matches the spec and no resolved shard is out of spec, so the drain pass
// never runs and only the guard keeps the labeled node alive.
func TestHandleScaleInDefersLabelDeletionWhileUnresolvedShardOwnsSlots(t *testing.T) {
	// Two live shards, spec.Shards=3: the count alone sees nothing to do.
	// shard-x owns 8192-12287 and matches no ValkeyNode (wedge-3-0's
	// recorded PodIP is stale), so the labels cannot place it either.
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{
		{
			Id:        "shard-0",
			PrimaryId: "node-0-0",
			Slots:     []valkey.SlotsRange{{Start: 0, End: 4095}},
			Nodes:     []*valkey.NodeState{{Address: "10.0.0.1", Port: 6379, Id: "node-0-0", Flags: []string{"master"}}},
		},
		{
			Id:        "shard-x",
			PrimaryId: "node-x-0",
			Slots:     []valkey.SlotsRange{{Start: 8192, End: 12287}},
			Nodes:     []*valkey.NodeState{{Address: "10.0.2.1", Port: 6379, Id: "node-x-0", Flags: []string{"master"}}},
		},
	}}
	mk := func(name, shard, node, ip string) *valkeyiov1alpha1.ValkeyNode {
		return &valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{
				LabelCluster:    "wedge",
				LabelShardIndex: shard,
				LabelNodeIndex:  node,
			}},
			Status: valkeyiov1alpha1.ValkeyNodeStatus{PodIP: ip},
		}
	}
	n00 := mk("wedge-0-0", "0", "0", "10.0.0.1")
	n30 := mk("wedge-3-0", "3", "0", "10.0.3.1")
	nodes := &valkeyiov1alpha1.ValkeyNodeList{Items: []valkeyiov1alpha1.ValkeyNode{*n00, *n30}}

	scheme := runtime.NewScheme()
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(n00, n30).Build()
	recorder := events.NewFakeRecorder(64)
	r := &ValkeyClusterReconciler{Client: c, Recorder: recorder}
	cluster := &valkeyiov1alpha1.ValkeyCluster{ObjectMeta: metav1.ObjectMeta{Name: "wedge", Namespace: "ns"}}
	cluster.Spec.Shards = 3

	result, requeue := r.handleScaleIn(context.Background(), cluster, state, nodes)
	require.True(t, requeue, "the deferral must requeue instead of deleting")
	require.NotZero(t, result.RequeueAfter, "the deferral carries a requeue interval")

	// The label-excess ValkeyNode must survive: deleting it now could delete
	// the unresolved shard's serving pod, which still owns 8192-12287.
	err := c.Get(context.Background(), client.ObjectKey{Name: "wedge-3-0", Namespace: "ns"}, &valkeyiov1alpha1.ValkeyNode{})
	require.NoError(t, err, "a slot-owning unresolved shard must not have its ValkeyNode deleted by label")

	// The deferral surfaces as an event instead of a silent stall.
	select {
	case e := <-recorder.Events:
		require.Contains(t, e, "ExcessValkeyNodeCleanupDeferred")
	default:
		t.Fatal("expected an ExcessValkeyNodeCleanupDeferred event")
	}
}
