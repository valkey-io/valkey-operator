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

	"github.com/stretchr/testify/assert"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

// A drained primary leads its shard in the scraped state but owns no slots:
// the shard is not counted as ready and the primary has no live role yet.
func TestDrainedPrimaryShardIsNotServing(t *testing.T) {
	primary := &valkey.NodeState{Id: "p1", Address: "10.0.1.1", Flags: []string{"master"}}
	replica := &valkey.NodeState{Id: "r1", Address: "10.0.1.2", Flags: []string{"slave"},
		Info: map[string]string{"master_link_status": "up"}}
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{{Id: "s1", PrimaryId: "p1", Nodes: []*valkey.NodeState{primary, replica}}}}

	r := &ValkeyClusterReconciler{}
	cluster := &valkeyiov1alpha1.ValkeyCluster{Spec: valkeyiov1alpha1.ValkeyClusterSpec{Replicas: 1}}
	assert.Equal(t, int32(0), r.countReadyShards(state, cluster))
	assert.Empty(t, liveRoleForAddress(state, "10.0.1.1"))
	assert.Equal(t, RoleReplica, liveRoleForAddress(state, "10.0.1.2"))
}
