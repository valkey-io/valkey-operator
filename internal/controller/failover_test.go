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

func TestFindShardForAddress(t *testing.T) {
	state := &valkey.ClusterState{
		Shards: []*valkey.ShardState{
			{
				Id:        "shard-1",
				PrimaryId: "node-1",
				Nodes: []*valkey.NodeState{
					{Address: "10.0.0.1", Id: "node-1", Flags: []string{"master", "myself"}},
					{Address: "10.0.0.2", Id: "node-2", Flags: []string{"slave"}},
				},
			},
			{
				Id:        "shard-2",
				PrimaryId: "node-3",
				Nodes: []*valkey.NodeState{
					{Address: "10.0.0.3", Id: "node-3", Flags: []string{"master"}},
				},
			},
		},
	}

	t.Run("found in first shard", func(t *testing.T) {
		shard := findShardForAddress(state, "10.0.0.1")
		assert.NotNil(t, shard)
		assert.Equal(t, "shard-1", shard.Id)
	})

	t.Run("found replica in first shard", func(t *testing.T) {
		shard := findShardForAddress(state, "10.0.0.2")
		assert.NotNil(t, shard)
		assert.Equal(t, "shard-1", shard.Id)
	})

	t.Run("found in second shard", func(t *testing.T) {
		shard := findShardForAddress(state, "10.0.0.3")
		assert.NotNil(t, shard)
		assert.Equal(t, "shard-2", shard.Id)
	})

	t.Run("not found", func(t *testing.T) {
		shard := findShardForAddress(state, "10.0.0.99")
		assert.Nil(t, shard)
	})
}

func TestShouldFailoverBeforeUpdate(t *testing.T) {
	t.Run("primary with synced replica", func(t *testing.T) {
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
		assert.True(t, shouldFailoverBeforeUpdate(state, "10.0.0.1"))
	})

	t.Run("replica address - should not failover", func(t *testing.T) {
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
		assert.False(t, shouldFailoverBeforeUpdate(state, "10.0.0.2"))
	})

	t.Run("primary with no replicas", func(t *testing.T) {
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
		assert.False(t, shouldFailoverBeforeUpdate(state, "10.0.0.1"))
	})

	t.Run("primary with unsynced replica", func(t *testing.T) {
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
		assert.False(t, shouldFailoverBeforeUpdate(state, "10.0.0.1"))
	})

	t.Run("unknown address", func(t *testing.T) {
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
		assert.False(t, shouldFailoverBeforeUpdate(state, "10.0.0.99"))
	})
}

func TestShouldTakeover(t *testing.T) {
	t.Run("1 master with failing primary", func(t *testing.T) {
		state := &valkey.ClusterState{
			Shards: []*valkey.ShardState{
				{
					Id:        "shard-1",
					PrimaryId: "node-1",
					Nodes: []*valkey.NodeState{
						{Address: "10.0.0.1", Id: "node-1", Flags: []string{"master", "fail"}},
						{Address: "10.0.0.2", Id: "node-2", Flags: []string{"slave"}},
					},
				},
			},
		}
		ok, reason := shouldTakeover(state, state.Shards[0])
		assert.True(t, ok)
		assert.Equal(t, "InsufficientQuorum", reason)
	})

	t.Run("2 masters with pfail primary", func(t *testing.T) {
		state := &valkey.ClusterState{
			Shards: []*valkey.ShardState{
				{
					Id:        "shard-1",
					PrimaryId: "node-1",
					Nodes: []*valkey.NodeState{
						{Address: "10.0.0.1", Id: "node-1", Flags: []string{"master", "pfail"}},
						{Address: "10.0.0.2", Id: "node-2", Flags: []string{"slave"}},
					},
				},
				{
					Id:        "shard-2",
					PrimaryId: "node-3",
					Nodes: []*valkey.NodeState{
						{Address: "10.0.0.3", Id: "node-3", Flags: []string{"master"}},
					},
				},
			},
		}
		ok, reason := shouldTakeover(state, state.Shards[0])
		assert.True(t, ok)
		assert.Equal(t, "InsufficientQuorum", reason)
	})

	t.Run("3 masters with failing primary - quorum possible", func(t *testing.T) {
		state := &valkey.ClusterState{
			Shards: []*valkey.ShardState{
				{
					Id:        "shard-1",
					PrimaryId: "node-1",
					Nodes: []*valkey.NodeState{
						{Address: "10.0.0.1", Id: "node-1", Flags: []string{"master", "fail"}},
						{Address: "10.0.0.2", Id: "node-2", Flags: []string{"slave"}},
					},
				},
				{
					Id:        "shard-2",
					PrimaryId: "node-3",
					Nodes: []*valkey.NodeState{
						{Address: "10.0.0.3", Id: "node-3", Flags: []string{"master"}},
					},
				},
				{
					Id:        "shard-3",
					PrimaryId: "node-5",
					Nodes: []*valkey.NodeState{
						{Address: "10.0.0.5", Id: "node-5", Flags: []string{"master"}},
					},
				},
			},
		}
		ok, reason := shouldTakeover(state, state.Shards[0])
		assert.False(t, ok)
		assert.Empty(t, reason)
	})

	t.Run("non-failing primary", func(t *testing.T) {
		state := &valkey.ClusterState{
			Shards: []*valkey.ShardState{
				{
					Id:        "shard-1",
					PrimaryId: "node-1",
					Nodes: []*valkey.NodeState{
						{Address: "10.0.0.1", Id: "node-1", Flags: []string{"master"}},
						{Address: "10.0.0.2", Id: "node-2", Flags: []string{"slave"}},
					},
				},
			},
		}
		ok, reason := shouldTakeover(state, state.Shards[0])
		assert.False(t, ok)
		assert.Empty(t, reason)
	})
}

func TestSpecEqual(t *testing.T) {
	t.Run("identical specs", func(t *testing.T) {
		a := valkeyiov1alpha1.ValkeyNodeSpec{Image: "valkey:8.1"}
		b := valkeyiov1alpha1.ValkeyNodeSpec{Image: "valkey:8.1"}
		assert.True(t, specEqual(a, b))
	})

	t.Run("different image", func(t *testing.T) {
		a := valkeyiov1alpha1.ValkeyNodeSpec{Image: "valkey:8.1"}
		b := valkeyiov1alpha1.ValkeyNodeSpec{Image: "valkey:8.2"}
		assert.False(t, specEqual(a, b))
	})

	t.Run("zero values equal", func(t *testing.T) {
		a := valkeyiov1alpha1.ValkeyNodeSpec{}
		b := valkeyiov1alpha1.ValkeyNodeSpec{}
		assert.True(t, specEqual(a, b))
	})
}

func TestCountTotalMasters(t *testing.T) {
	t.Run("empty cluster", func(t *testing.T) {
		state := &valkey.ClusterState{Shards: []*valkey.ShardState{}}
		assert.Equal(t, 0, countTotalMasters(state))
	})

	t.Run("three shards", func(t *testing.T) {
		state := &valkey.ClusterState{
			Shards: []*valkey.ShardState{
				{Id: "shard-1"},
				{Id: "shard-2"},
				{Id: "shard-3"},
			},
		}
		assert.Equal(t, 3, countTotalMasters(state))
	})
}
