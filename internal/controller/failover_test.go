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
