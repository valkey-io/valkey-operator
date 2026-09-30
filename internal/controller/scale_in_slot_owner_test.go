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
	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
		PrimaryId: id,
		Slots:     slots,
		Nodes:     []*valkey.NodeState{{Id: id, Address: fmt.Sprintf("10.0.0.%d", shard)}},
	}
}

func TestExcessShardOwnsSlots(t *testing.T) {
	nodes := &valkeyiov1alpha1.ValkeyNodeList{}
	for i := range 4 {
		nodes.Items = append(nodes.Items, *primaryNode(i))
	}

	// Scaling 2 -> 4, shard 3 got slots before shard 2 joined, then the spec
	// dropped to 3: three shards, matching the spec, but shard 3 owns data.
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{
		primaryShard(0, valkey.SlotsRange{Start: 0, End: 5460}),
		primaryShard(1, valkey.SlotsRange{Start: 5461, End: 10922}),
		primaryShard(3, valkey.SlotsRange{Start: 10923, End: 16383}),
	}}
	assert.True(t, excessShardOwnsSlots(state, nodes, 3))

	state.Shards[2] = primaryShard(3)
	assert.False(t, excessShardOwnsSlots(state, nodes, 3))
}

func TestDeleteExcessValkeyNodesKeepsSlotOwners(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	inSpec, owner, drained := primaryNode(0), primaryNode(3), primaryNode(2)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(inSpec, owner, drained).Build()
	recorder := events.NewFakeRecorder(8)
	r := &ValkeyClusterReconciler{Client: c, Scheme: scheme, Recorder: recorder}
	cluster := &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
		Spec:       valkeyiov1alpha1.ValkeyClusterSpec{Shards: 2},
	}
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{
		primaryShard(0, valkey.SlotsRange{Start: 0, End: 8191}),
		primaryShard(3, valkey.SlotsRange{Start: 8192, End: 16383}),
		primaryShard(2),
	}}

	deleted, err := r.deleteExcessValkeyNodes(context.Background(), cluster, state)
	require.NoError(t, err)
	assert.True(t, deleted)

	exists := func(n *valkeyiov1alpha1.ValkeyNode) bool {
		return !apierrors.IsNotFound(c.Get(context.Background(), client.ObjectKeyFromObject(n), &valkeyiov1alpha1.ValkeyNode{}))
	}
	assert.True(t, exists(inSpec))
	assert.True(t, exists(owner), "excess node that owns slots must be kept")
	assert.False(t, exists(drained))
	close(recorder.Events)
	var got []string
	for e := range recorder.Events {
		got = append(got, e)
	}
	assert.Contains(t, got, "Warning ScaleInBlocked Excess ValkeyNode c-3-0 still owns slots; waiting for drain")
}
