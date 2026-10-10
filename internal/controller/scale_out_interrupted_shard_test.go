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

func scaleOutInterruptedFixture() (*valkey.ClusterState, *valkeyiov1alpha1.ValkeyNodeList, []valkeyiov1alpha1.ValkeyNode) {
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
	return state, nodes, []valkeyiov1alpha1.ValkeyNode{*n00, *n10, *n30}
}

// scriptedPrimaryAt builds a NodeState whose client dials the scripted RESP
// server but whose address stays the fixture address, so shard label
// resolution (shardIndexFromState) keeps matching the ValkeyNode.
func scriptedPrimaryAt(t *testing.T, s *migrationStateServer, id, host string, port int) *valkey.NodeState {
	t.Helper()
	client, err := vclient.NewClient(vclient.ClientOption{
		InitAddress:       []string{s.addr},
		ForceSingleClient: true,
		DisableCache:      true,
		DisableRetry:      true,
		PipelineMultiplex: -1,
	})
	if err != nil {
		t.Fatalf("client init: %v", err)
	}
	t.Cleanup(client.Close)
	return &valkey.NodeState{Client: client, Address: host, Port: port, Id: id, Flags: []string{"master"}}
}

func TestLiveShardOutsideSpecDetectsInterruptedScaleOut(t *testing.T) {
	state, nodes, _ := scaleOutInterruptedFixture()
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
	state, nodes, _ := scaleOutInterruptedFixture()

	// The drain source is shard 3's primary; it dials the scripted server so
	// the migration-in-progress probe runs on a real client, while keeping
	// the fixture address so the ValkeyNode labels still resolve to
	// shard 3.
	src := state.Shards[2]
	src.Nodes[0] = scriptedPrimaryAt(t, server, src.Nodes[0].Id, "10.0.3.1", 6379)

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
