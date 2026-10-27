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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

// podAt returns a pod created at the given second with the given IP, deleted
// when terminating is set.
func podAt(name string, created int, ip string, terminating bool) corev1.Pod {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(time.Unix(int64(created), 0))},
		Status:     corev1.PodStatus{PodIP: ip},
	}
	if terminating {
		pod.DeletionTimestamp = &metav1.Time{Time: time.Unix(int64(created)+30, 0)}
	}
	return pod
}

func TestPickPods(t *testing.T) {
	t.Run("the replacement is current and the old pod is terminating, whatever the list order", func(t *testing.T) {
		for _, pods := range [][]corev1.Pod{
			{podAt("old", 100, "10.0.0.1", true), podAt("new", 200, "10.0.0.2", false)},
			{podAt("new", 200, "10.0.0.2", false), podAt("old", 100, "10.0.0.1", true)},
		} {
			current, terminating := pickPods(pods)
			require.NotNil(t, current)
			assert.Equal(t, "new", current.Name)
			assert.Equal(t, []string{"10.0.0.1"}, terminatingPodIPs(terminating))
		}
	})

	t.Run("a replacement deleted in its turn joins the terminating pods, newest first", func(t *testing.T) {
		// The original primary keeps serving through its preStop while the
		// first replacement is deleted too and a second one starts. Both
		// terminating pods stay visible, so the primary is still scraped.
		current, terminating := pickPods([]corev1.Pod{
			podAt("old", 100, "10.0.0.1", true), podAt("first", 200, "10.0.0.2", true), podAt("second", 300, "10.0.0.3", false),
		})
		assert.Equal(t, "second", current.Name)
		assert.Equal(t, []string{"10.0.0.2", "10.0.0.1"}, terminatingPodIPs(terminating))
	})

	t.Run("a terminating pod on its own is current", func(t *testing.T) {
		current, terminating := pickPods([]corev1.Pod{podAt("old", 100, "10.0.0.1", true)})
		require.NotNil(t, current)
		assert.Equal(t, "old", current.Name)
		assert.Empty(t, terminating)
	})

	t.Run("the newest of two live pods is current and the other is not terminating", func(t *testing.T) {
		current, terminating := pickPods([]corev1.Pod{podAt("a", 100, "10.0.0.1", false), podAt("b", 200, "10.0.0.2", false)})
		assert.Equal(t, "b", current.Name)
		assert.Empty(t, terminating)
	})

	t.Run("a terminating pod without an IP is nothing to dial", func(t *testing.T) {
		current, terminating := pickPods([]corev1.Pod{podAt("old", 100, "", true), podAt("new", 200, "10.0.0.2", false)})
		assert.Equal(t, "new", current.Name)
		assert.Empty(t, terminating)
	})

	t.Run("no pod", func(t *testing.T) {
		current, terminating := pickPods(nil)
		assert.Nil(t, current)
		assert.Empty(t, terminating)
	})
}

// A terminating primary that still serves beside its replacement stays the
// shard's primary for the cluster controller: it is scraped, found, and the
// replacements are attached to it rather than handed its slots.
func TestTerminatingPodIsStillTheShardPrimary(t *testing.T) {
	nodes := &valkeyiov1alpha1.ValkeyNodeList{Items: []valkeyiov1alpha1.ValkeyNode{
		{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{LabelShardIndex: "0", LabelNodeIndex: "0"}},
			Status: valkeyiov1alpha1.ValkeyNodeStatus{PodIP: "10.0.0.11", TerminatingPodIPs: []string{"10.0.0.21", "10.0.0.1"}}},
		{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{LabelShardIndex: "0", LabelNodeIndex: "1"}},
			Status: valkeyiov1alpha1.ValkeyNodeStatus{PodIP: "10.0.0.12"}},
	}}
	state := &valkey.ClusterState{
		Shards: []*valkey.ShardState{{Id: "s0", PrimaryId: "p0", Slots: []valkey.SlotsRange{{Start: 0, End: 16383}},
			Nodes: []*valkey.NodeState{{Id: "p0", Address: "10.0.0.1", Flags: []string{"master"}}}}},
		PendingNodes: []*valkey.NodeState{
			{Id: "n11", Address: "10.0.0.11", Flags: []string{"master"}},
			{Id: "n12", Address: "10.0.0.12", Flags: []string{"master"}},
		},
	}

	assert.ElementsMatch(t, []string{"10.0.0.11", "10.0.0.21", "10.0.0.1", "10.0.0.12"}, nodeAddresses(nodes))

	id, ip := findShardPrimary(state, 0, nodes)
	assert.Equal(t, "p0", id)
	assert.Equal(t, "10.0.0.1", ip, "the address to reach the primary at is the terminating pod's, even behind a newer one")
	assert.True(t, shardExistsInTopology(state, 0, nodes))
	assert.Equal(t, 0, nodeIndexForAddress("10.0.0.1", nodes))
	role, shard := nodeRoleAndShard("10.0.0.1", nodes)
	assert.Equal(t, RolePrimary, role)
	assert.Equal(t, 0, shard)
	assert.True(t, hasNodeWithPodIP(nodes.Items, "10.0.0.1"), "a failing entry for the terminating pod is not forgotten while it serves")
	assert.False(t, hasNodeWithPodIP(nodes.Items, ""))
}
