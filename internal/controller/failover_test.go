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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestFindFailoverShard(t *testing.T) {
	t.Run("primary with synced replica returns shard", func(t *testing.T) {
		state := &valkey.ClusterState{
			Shards: []*valkey.ShardState{
				{
					Id:        "shard-1",
					PrimaryId: "node-1",
					Nodes: []*valkey.NodeState{
						{Address: "10.0.0.1", Id: "node-1", Flags: []string{"master"}},
						{Address: "10.0.0.2", Id: "node-2", Flags: []string{"slave"}, Info: map[string]string{"master_link_status": "up"}},
					},
				},
			},
		}
		shard, replicas := findFailoverShard(state, "10.0.0.1")
		assert.NotNil(t, shard)
		assert.Len(t, replicas, 1)
	})

	t.Run("replica address returns nil", func(t *testing.T) {
		state := &valkey.ClusterState{
			Shards: []*valkey.ShardState{
				{
					Id:        "shard-1",
					PrimaryId: "node-1",
					Nodes: []*valkey.NodeState{
						{Address: "10.0.0.1", Id: "node-1", Flags: []string{"master"}},
						{Address: "10.0.0.2", Id: "node-2", Flags: []string{"slave"}, Info: map[string]string{"master_link_status": "up"}},
					},
				},
			},
		}
		shard, replicas := findFailoverShard(state, "10.0.0.2")
		assert.Nil(t, shard)
		assert.Nil(t, replicas)
	})

	t.Run("primary with no replicas returns nil", func(t *testing.T) {
		state := &valkey.ClusterState{
			Shards: []*valkey.ShardState{
				{
					Id:        "shard-1",
					PrimaryId: "node-1",
					Nodes: []*valkey.NodeState{
						{Address: "10.0.0.1", Id: "node-1", Flags: []string{"master"}},
					},
				},
			},
		}
		shard, replicas := findFailoverShard(state, "10.0.0.1")
		assert.Nil(t, shard)
		assert.Nil(t, replicas)
	})

	t.Run("primary with unsynced replica returns nil", func(t *testing.T) {
		state := &valkey.ClusterState{
			Shards: []*valkey.ShardState{
				{
					Id:        "shard-1",
					PrimaryId: "node-1",
					Nodes: []*valkey.NodeState{
						{Address: "10.0.0.1", Id: "node-1", Flags: []string{"master"}},
						{Address: "10.0.0.2", Id: "node-2", Flags: []string{"slave"}, Info: map[string]string{"master_link_status": "down"}},
					},
				},
			},
		}
		shard, replicas := findFailoverShard(state, "10.0.0.1")
		assert.Nil(t, shard)
		assert.Nil(t, replicas)
	})

	t.Run("unknown address returns nil", func(t *testing.T) {
		state := &valkey.ClusterState{
			Shards: []*valkey.ShardState{
				{
					Id:        "shard-1",
					PrimaryId: "node-1",
					Nodes: []*valkey.NodeState{
						{Address: "10.0.0.1", Id: "node-1", Flags: []string{"master"}},
					},
				},
			},
		}
		shard, replicas := findFailoverShard(state, "10.0.0.99")
		assert.Nil(t, shard)
		assert.Nil(t, replicas)
	})
}

func TestAnyNodeRequiresFailoverAwareRoll(t *testing.T) {
	cluster := &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec:       valkeyiov1alpha1.ValkeyClusterSpec{Shards: 1, Replicas: 0},
	}

	steadyStateNode := func() valkeyiov1alpha1.ValkeyNode {
		n := buildClusterValkeyNode(cluster, 0, 0)
		require.NoError(t, setDesiredWorkloadRevision(n))
		n.Status.PodIP = "10.0.0.1"
		return *n
	}

	t.Run("settled node does not need failover-aware roll", func(t *testing.T) {
		n := steadyStateNode()
		nodes := &valkeyiov1alpha1.ValkeyNodeList{Items: []valkeyiov1alpha1.ValkeyNode{n}}
		live := map[string]string{n.Name: n.Spec.WorkloadRevision}
		assert.False(t, anyNodeRequiresFailoverAwareRoll(cluster, nodes, live))
	})

	t.Run("empty WorkloadRevision backfill when live matches does not need failover-aware roll", func(t *testing.T) {
		n := steadyStateNode()
		authorized := n.Spec.WorkloadRevision
		n.Spec.WorkloadRevision = ""
		nodes := &valkeyiov1alpha1.ValkeyNodeList{Items: []valkeyiov1alpha1.ValkeyNode{n}}
		live := map[string]string{n.Name: authorized}
		assert.False(t, anyNodeRequiresFailoverAwareRoll(cluster, nodes, live))
	})

	t.Run("empty WorkloadRevision with live template mismatch needs failover-aware roll", func(t *testing.T) {
		n := steadyStateNode()
		n.Spec.WorkloadRevision = ""
		nodes := &valkeyiov1alpha1.ValkeyNodeList{Items: []valkeyiov1alpha1.ValkeyNode{n}}
		// Live still on an older template (e.g. ACL hash change before backfill).
		live := map[string]string{n.Name: "old-live-template-hash"}
		assert.True(t, anyNodeRequiresFailoverAwareRoll(cluster, nodes, live))
	})

	t.Run("cluster config change needs failover-aware roll", func(t *testing.T) {
		n := steadyStateNode()
		changed := cluster.DeepCopy()
		changed.Spec.Config = map[string]string{"appendfsync": "always"}
		nodes := &valkeyiov1alpha1.ValkeyNodeList{Items: []valkeyiov1alpha1.ValkeyNode{n}}
		// Live still runs the template rendered before the config change.
		live := map[string]string{n.Name: n.Spec.WorkloadRevision}
		assert.True(t, anyNodeRequiresFailoverAwareRoll(changed, nodes, live))
	})

	t.Run("stale WorkloadRevision with unchanged live template does not need failover-aware roll", func(t *testing.T) {
		// The #401 false-positive class: the stored spec differs in shape only
		// (here: a stale revision string), but the rendered template is unchanged.
		n := steadyStateNode()
		authorized := n.Spec.WorkloadRevision
		n.Spec.WorkloadRevision = "not-the-real-hash"
		nodes := &valkeyiov1alpha1.ValkeyNodeList{Items: []valkeyiov1alpha1.ValkeyNode{n}}
		live := map[string]string{n.Name: authorized}
		assert.False(t, anyNodeRequiresFailoverAwareRoll(cluster, nodes, live))
	})

	t.Run("no live workload does not need failover-aware roll", func(t *testing.T) {
		n := steadyStateNode()
		n.Spec.WorkloadRevision = "not-the-real-hash"
		nodes := &valkeyiov1alpha1.ValkeyNodeList{Items: []valkeyiov1alpha1.ValkeyNode{n}}
		assert.False(t, anyNodeRequiresFailoverAwareRoll(cluster, nodes, nil))
	})
}

func TestNeedsProactiveFailoverForRoll(t *testing.T) {
	base := func() (*valkeyiov1alpha1.ValkeyNode, *valkeyiov1alpha1.ValkeyNode) {
		current := &valkeyiov1alpha1.ValkeyNode{
			Spec:   valkeyiov1alpha1.ValkeyNodeSpec{Image: "valkey:8", WorkloadRevision: "rev-a"},
			Status: valkeyiov1alpha1.ValkeyNodeStatus{PodIP: "10.0.0.1"},
		}
		desired := current.DeepCopy()
		return current, desired
	}

	t.Run("live matches desired template does not need failover", func(t *testing.T) {
		current, desired := base()
		assert.False(t, needsProactiveFailoverForRoll(current, desired, "rev-a"))
	})

	t.Run("stale current spec with unchanged template does not need failover", func(t *testing.T) {
		// The #401 class: the stored spec encodes differently from desired
		// (e.g. bool→*bool conversion) but renders the same template.
		current, desired := base()
		current.Spec.Image = "stale-encoding"
		assert.False(t, needsProactiveFailoverForRoll(current, desired, "rev-a"))
	})

	t.Run("live differs from desired template needs failover", func(t *testing.T) {
		current, desired := base()
		desired.Spec.WorkloadRevision = "rev-b"
		assert.True(t, needsProactiveFailoverForRoll(current, desired, "rev-a"))
	})

	t.Run("no running pod does not need failover", func(t *testing.T) {
		current, desired := base()
		current.Status.PodIP = ""
		desired.Spec.WorkloadRevision = "rev-b"
		assert.False(t, needsProactiveFailoverForRoll(current, desired, "rev-a"))
	})

	t.Run("unknown live template does not need failover", func(t *testing.T) {
		// No live workload means the coming update creates rather than rolls.
		current, desired := base()
		desired.Spec.WorkloadRevision = "rev-b"
		assert.False(t, needsProactiveFailoverForRoll(current, desired, ""))
	})

	t.Run("empty revision backfill when live matches does not need failover", func(t *testing.T) {
		current, desired := base()
		current.Spec.WorkloadRevision = ""
		desired.Spec.WorkloadRevision = "rev-b"
		assert.False(t, needsProactiveFailoverForRoll(current, desired, "rev-b"))
	})

	t.Run("workload type swap needs failover even with identical template", func(t *testing.T) {
		current, desired := base()
		current.Spec.WorkloadType = valkeyiov1alpha1.WorkloadTypeDeployment
		desired.Spec.WorkloadType = valkeyiov1alpha1.WorkloadTypeStatefulSet
		assert.True(t, needsProactiveFailoverForRoll(current, desired, "rev-a"))
	})

	t.Run("defaulted workload type is not a swap", func(t *testing.T) {
		// "" means StatefulSet (CRD default); ""→"StatefulSet" is a spec-shape
		// change, not a workload swap.
		current, desired := base()
		current.Spec.WorkloadType = ""
		desired.Spec.WorkloadType = valkeyiov1alpha1.WorkloadTypeStatefulSet
		assert.False(t, needsProactiveFailoverForRoll(current, desired, "rev-a"))
	})
}
