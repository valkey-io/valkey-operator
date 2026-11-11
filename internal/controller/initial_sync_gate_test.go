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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

// gatePrimary is a scraped primary at address. slots is its owned range in
// CLUSTER NODES form, or empty for a primary with no slots.
func gatePrimary(address, slots string, info map[string]string) *valkey.NodeState {
	n := &valkey.NodeState{Id: "id-" + address, Address: address, Flags: []string{"myself", "master"}, Info: info}
	line := "id-" + address + " " + address + ":6379@16379 myself,master - 0 0 1 connected"
	if slots != "" {
		line += " " + slots
	}
	n.SetClusterNodesForTesting(line + "\n")
	return n
}

// gateReplica is a scraped replica at address with the given INFO fields.
func gateReplica(address string, info map[string]string) *valkey.NodeState {
	n := &valkey.NodeState{Id: "id-" + address, Address: address, Flags: []string{"myself", "slave"}, Info: info}
	n.SetClusterNodesForTesting("id-" + address + " " + address + ":6379@16379 myself,slave id-p 0 0 1 connected\n")
	return n
}

func TestInitialSyncVerdict(t *testing.T) {
	for _, tt := range []struct {
		name     string
		node     *valkey.NodeState
		newShard bool
		want     gateVerdict
		reason   string
	}{
		{"missed by the scrape", nil, false, gateKeep, ""},
		{"primary that owns slots", gatePrimary("10.0.0.1", "0-16383", nil), false, gateSet, "OwnsSlots"},
		{"primary loading its dataset", gatePrimary("10.0.0.1", "0-16383", map[string]string{"loading": "1"}), false, gateClear, "Loading"},
		{"primary with no slots in an existing shard", gatePrimary("10.0.0.1", "", nil), false, gateKeep, ""},
		{"primary of a new shard", gatePrimary("10.0.0.1", "", nil), true, gateSet, "NewShardPrimary"},
		{"replica that finished its full sync", gateReplica("10.0.0.2", map[string]string{"master_link_status": "up", "master_sync_in_progress": "0"}), false, gateSet, "ReplicaSynced"},
		{"replica receiving a full sync", gateReplica("10.0.0.2", map[string]string{"master_link_status": "down", "master_sync_in_progress": "1"}), false, gateClear, "FullSyncInProgress"},
		{"replica whose link dropped", gateReplica("10.0.0.2", map[string]string{"master_link_status": "down", "master_sync_in_progress": "0"}), false, gateKeep, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			verdict, reason, _ := initialSyncVerdict(tt.node, tt.newShard)
			assert.Equal(t, tt.want, verdict)
			assert.Equal(t, tt.reason, reason)
		})
	}
}

// gateMember is a ValkeyNode the cluster controller built, at podIP.
func gateMember(shard, index, podIP, podName string) valkeyiov1alpha1.ValkeyNode {
	return valkeyiov1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "c-" + shard + "-" + index,
			Namespace: "ns",
			Labels:    map[string]string{LabelCluster: "c", LabelShardIndex: shard, LabelNodeIndex: index},
		},
		Status: valkeyiov1alpha1.ValkeyNodeStatus{PodIP: podIP, PodName: podName},
	}
}

func TestNewShardPrimaries(t *testing.T) {
	serving := &valkey.ShardState{Nodes: []*valkey.NodeState{{Address: "10.0.0.1"}, {Address: "10.0.0.2"}}}
	nodes := &valkeyiov1alpha1.ValkeyNodeList{Items: []valkeyiov1alpha1.ValkeyNode{
		gateMember("0", "0", "10.0.0.1", "p0"),
		gateMember("0", "1", "10.0.0.2", "p1"),
		gateMember("1", "0", "10.0.0.3", "p2"),
		gateMember("1", "1", "10.0.0.4", "p3"),
	}}

	state := &valkey.ClusterState{
		Shards:       []*valkey.ShardState{serving},
		PendingNodes: []*valkey.NodeState{{Address: "10.0.0.3"}, {Address: "10.0.0.4"}},
	}
	assert.Equal(t, map[string]bool{"10.0.0.3": true}, newShardPrimaries(state, nodes),
		"only the node-index 0 member of the shard that is not in the topology")

	state.PendingNodes = []*valkey.NodeState{{Address: "10.0.0.3"}}
	assert.Empty(t, newShardPrimaries(state, nodes), "a member the scrape missed may be what puts the shard in the topology")

	// A replacement node-index 0 pod whose shard is served by its promoted replica.
	state = &valkey.ClusterState{
		Shards:       []*valkey.ShardState{{Nodes: []*valkey.NodeState{{Address: "10.0.0.2"}}}, {Nodes: []*valkey.NodeState{{Address: "10.0.0.3"}, {Address: "10.0.0.4"}}}},
		PendingNodes: []*valkey.NodeState{{Address: "10.0.0.1"}},
	}
	assert.Empty(t, newShardPrimaries(state, nodes))
}

// gatePod is a member pod that lists initialSyncGate, with the gate's current
// status when recorded.
func gatePod(name string, gate *corev1.ConditionStatus) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: corev1.PodSpec{
			Containers:     []corev1.Container{{Name: "server", Image: DefaultImage}},
			ReadinessGates: []corev1.PodReadinessGate{{ConditionType: initialSyncGate}},
		},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.ContainersReady, Status: corev1.ConditionTrue}}},
	}
	if gate != nil {
		pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{Type: initialSyncGate, Status: *gate})
	}
	return pod
}

func TestSyncInitialSyncGates(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	isTrue := corev1.ConditionTrue

	oldPod := gatePod("old", nil)
	oldPod.Spec.ReadinessGates = nil
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1.Pod{}).
		WithObjects(gatePod("primary", nil), gatePod("syncing", nil), gatePod("dropped", &isTrue), oldPod).Build()
	r := &ValkeyClusterReconciler{Client: c, Scheme: scheme}
	nodes := &valkeyiov1alpha1.ValkeyNodeList{Items: []valkeyiov1alpha1.ValkeyNode{
		gateMember("0", "0", "10.0.0.1", "primary"),
		gateMember("0", "1", "10.0.0.2", "syncing"),
		gateMember("0", "2", "10.0.0.3", "dropped"),
		gateMember("1", "0", "10.0.0.4", "old"),
	}}
	shard := &valkey.ShardState{Nodes: []*valkey.NodeState{
		gatePrimary("10.0.0.1", "0-16383", nil),
		gateReplica("10.0.0.2", map[string]string{"master_link_status": "down", "master_sync_in_progress": "1"}),
		gateReplica("10.0.0.3", map[string]string{"master_link_status": "down", "master_sync_in_progress": "0"}),
	}}
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{shard}, PendingNodes: []*valkey.NodeState{gatePrimary("10.0.0.4", "", nil)}}

	gate := func(name string) *corev1.PodCondition {
		t.Helper()
		pod := &corev1.Pod{}
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: name}, pod))
		for i := range pod.Status.Conditions {
			if pod.Status.Conditions[i].Type == initialSyncGate {
				return &pod.Status.Conditions[i]
			}
		}
		return nil
	}

	gated, err := r.syncInitialSyncGates(context.Background(), nodes, state)
	require.NoError(t, err)
	assert.True(t, gated, "the syncing replica is still gated")
	require.NotNil(t, gate("primary"))
	assert.Equal(t, corev1.ConditionTrue, gate("primary").Status)
	assert.Equal(t, "OwnsSlots", gate("primary").Reason)
	require.NotNil(t, gate("syncing"))
	assert.Equal(t, corev1.ConditionFalse, gate("syncing").Status)
	assert.Equal(t, "FullSyncInProgress", gate("syncing").Reason)
	assert.Equal(t, corev1.ConditionTrue, gate("dropped").Status, "a dropped link keeps the gate")
	assert.Nil(t, gate("old"), "a pod without the gate in its spec is left alone")

	shard.Nodes[1] = gateReplica("10.0.0.2", map[string]string{"master_link_status": "up", "master_sync_in_progress": "0"})
	gated, err = r.syncInitialSyncGates(context.Background(), nodes, state)
	require.NoError(t, err)
	assert.False(t, gated)
	assert.Equal(t, corev1.ConditionTrue, gate("syncing").Status)
	assert.Equal(t, "ReplicaSynced", gate("syncing").Reason)
	containersReady := false
	pod := &corev1.Pod{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "syncing"}, pod))
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.ContainersReady && cond.Status == corev1.ConditionTrue {
			containersReady = true
		}
	}
	assert.True(t, containersReady, "the patch leaves the kubelet's conditions alone")
}
