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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
	"valkey.io/valkey-operator/internal/valkey"
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

func TestNodeRequiresRoll(t *testing.T) {
	t.Run("different spec with pod IP requires roll", func(t *testing.T) {
		current := &valkeyiov1alpha1.ValkeyNode{}
		current.Spec.Image = "valkey:8.1"
		current.Status.PodIP = "10.0.0.1"
		desired := &valkeyiov1alpha1.ValkeyNode{}
		desired.Spec.Image = "valkey:8.2"
		assert.True(t, nodeRequiresRoll(current, desired))
	})

	t.Run("same spec does not require roll", func(t *testing.T) {
		current := &valkeyiov1alpha1.ValkeyNode{}
		current.Spec.Image = "valkey:8.1"
		current.Status.PodIP = "10.0.0.1"
		desired := &valkeyiov1alpha1.ValkeyNode{}
		desired.Spec.Image = "valkey:8.1"
		assert.False(t, nodeRequiresRoll(current, desired))
	})

	t.Run("different spec but no pod IP does not require roll", func(t *testing.T) {
		current := &valkeyiov1alpha1.ValkeyNode{}
		current.Spec.Image = "valkey:8.1"
		desired := &valkeyiov1alpha1.ValkeyNode{}
		desired.Spec.Image = "valkey:8.2"
		assert.False(t, nodeRequiresRoll(current, desired))
	})
}

func TestNodeRequiresRollIgnoresConfig(t *testing.T) {
	withConfig := func(policy string) *valkeyiov1alpha1.ValkeyNode {
		return &valkeyiov1alpha1.ValkeyNode{
			Spec: valkeyiov1alpha1.ValkeyNodeSpec{
				Image:            "valkey:8",
				ServerConfigHash: "abc",
				Config:           map[string]string{"maxmemory-policy": policy},
			},
		}
	}

	t.Run("does not require a roll when only Config differs", func(t *testing.T) {
		current := withConfig("allkeys-lru")
		current.Status.PodIP = "10.0.0.1"
		desired := withConfig("volatile-lru")
		assert.False(t, nodeRequiresRoll(current, desired))
	})

	t.Run("requires a roll when a non-Config spec field differs", func(t *testing.T) {
		current := &valkeyiov1alpha1.ValkeyNode{
			Spec:   valkeyiov1alpha1.ValkeyNodeSpec{Image: "valkey:8"},
			Status: valkeyiov1alpha1.ValkeyNodeStatus{PodIP: "10.0.0.1"},
		}
		desired := &valkeyiov1alpha1.ValkeyNode{
			Spec: valkeyiov1alpha1.ValkeyNodeSpec{Image: "valkey:9"},
		}
		assert.True(t, nodeRequiresRoll(current, desired))
	})
}

func TestAnyNodeRequiresRoll(t *testing.T) {
	cluster := &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec:       valkeyiov1alpha1.ValkeyClusterSpec{Shards: 1, Replicas: 0},
	}
	const configHash = "abc123"

	// steadyStateNode returns the node the cluster would build for (0,0) in a
	// settled cluster: matching spec, the current config hash, and a pod IP.
	steadyStateNode := func() valkeyiov1alpha1.ValkeyNode {
		n := buildClusterValkeyNode(cluster, 0, 0)
		n.Spec.ServerConfigHash = configHash
		n.Status.PodIP = "10.0.0.1"
		return *n
	}

	t.Run("settled node with matching config hash does not require roll", func(t *testing.T) {
		nodes := &valkeyiov1alpha1.ValkeyNodeList{Items: []valkeyiov1alpha1.ValkeyNode{steadyStateNode()}}
		assert.False(t, anyNodeRequiresRoll(cluster, nodes, configHash))
	})

	t.Run("node with stale config hash requires roll", func(t *testing.T) {
		n := steadyStateNode()
		n.Spec.ServerConfigHash = "stale"
		nodes := &valkeyiov1alpha1.ValkeyNodeList{Items: []valkeyiov1alpha1.ValkeyNode{n}}
		assert.True(t, anyNodeRequiresRoll(cluster, nodes, configHash))
	})
}

func TestHighestOffsetReplica(t *testing.T) {
	replica := func(addr, offset string) *valkey.NodeState {
		info := map[string]string{}
		if offset != "" {
			info["slave_repl_offset"] = offset
		}
		return &valkey.NodeState{Address: addr, Info: info}
	}

	t.Run("picks the replica with the greatest offset", func(t *testing.T) {
		replicas := []*valkey.NodeState{replica("a", "100"), replica("b", "300"), replica("c", "200")}
		assert.Equal(t, "b", highestOffsetReplica(replicas).Address)
	})

	t.Run("a replica with no offset sorts last", func(t *testing.T) {
		replicas := []*valkey.NodeState{replica("a", ""), replica("b", "5")}
		assert.Equal(t, "b", highestOffsetReplica(replicas).Address)
	})

	t.Run("ties keep discovery order", func(t *testing.T) {
		replicas := []*valkey.NodeState{replica("a", "10"), replica("b", "10")}
		assert.Equal(t, "a", highestOffsetReplica(replicas).Address)
	})

	t.Run("a single replica is returned", func(t *testing.T) {
		replicas := []*valkey.NodeState{replica("only", "42")}
		assert.Equal(t, "only", highestOffsetReplica(replicas).Address)
	})
}
