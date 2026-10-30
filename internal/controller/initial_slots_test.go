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
	valkeyv1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func slotTestNode(shard, index, ip string) valkeyv1.ValkeyNode {
	return valkeyv1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "n-" + shard + "-" + index,
			Labels: map[string]string{
				LabelShardIndex: shard,
				LabelNodeIndex:  index,
			},
		},
		Status: valkeyv1.ValkeyNodeStatus{PodIP: ip},
	}
}

func slotTestPrimary(addr, known string) *valkey.NodeState {
	return &valkey.NodeState{
		Address:     addr,
		Id:          addr,
		Flags:       []string{"master"},
		ClusterInfo: map[string]string{"cluster_known_nodes": known},
	}
}

func TestInitialSlotPrimariesReady(t *testing.T) {
	cluster := &valkeyv1.ValkeyCluster{
		Spec: valkeyv1.ValkeyClusterSpec{Shards: 2, Replicas: 1},
	}
	nodes := &valkeyv1.ValkeyNodeList{Items: []valkeyv1.ValkeyNode{
		slotTestNode("0", "0", "10.0.0.1"),
		slotTestNode("0", "1", "10.0.0.2"),
		slotTestNode("1", "0", "10.0.1.1"),
		slotTestNode("1", "1", "10.0.1.2"),
	}}
	bothPending := &valkey.ClusterState{PendingNodes: []*valkey.NodeState{
		slotTestPrimary("10.0.0.1", "2"),
		slotTestPrimary("10.0.0.2", "2"),
		slotTestPrimary("10.0.1.1", "2"),
		slotTestPrimary("10.0.1.2", "2"),
	}}
	assert.True(t, initialSlotPrimariesReady(cluster, bothPending, nodes),
		"every primary is scraped and in the cluster")

	late := &valkey.ClusterState{PendingNodes: []*valkey.NodeState{
		slotTestPrimary("10.0.0.1", "2"),
		slotTestPrimary("10.0.0.2", "2"),
	}}
	assert.False(t, initialSlotPrimariesReady(cluster, late, nodes),
		"a primary that has not been scraped must not be skipped")
	assert.Equal(t, "n-1-0", initialSlotWait(cluster, late, nodes),
		"the log names the primary that has not been scraped")

	replicasDown := &valkey.ClusterState{PendingNodes: []*valkey.NodeState{
		slotTestPrimary("10.0.0.1", "2"),
		slotTestPrimary("10.0.1.1", "2"),
	}}
	assert.Equal(t, "", initialSlotWait(cluster, replicasDown, nodes),
		"an unscaped replica must not hold slot assignment")

	isolated := &valkey.ClusterState{PendingNodes: []*valkey.NodeState{
		slotTestPrimary("10.0.0.1", "2"),
		slotTestPrimary("10.0.0.2", "2"),
		slotTestPrimary("10.0.1.1", "1"),
		slotTestPrimary("10.0.1.2", "2"),
	}}
	assert.False(t, initialSlotPrimariesReady(cluster, isolated, nodes),
		"an isolated primary has not joined yet")

	single := &valkeyv1.ValkeyCluster{Spec: valkeyv1.ValkeyClusterSpec{Shards: 1, Replicas: 0}}
	singleNodes := &valkeyv1.ValkeyNodeList{Items: []valkeyv1.ValkeyNode{
		slotTestNode("0", "0", "10.0.0.1"),
	}}
	singleState := &valkey.ClusterState{PendingNodes: []*valkey.NodeState{
		slotTestPrimary("10.0.0.1", "1"),
	}}
	assert.True(t, initialSlotPrimariesReady(single, singleState, singleNodes),
		"a one-node cluster is isolated for good")

	owned := &valkey.ClusterState{
		Shards: []*valkey.ShardState{{
			Id:    "s0",
			Slots: []valkey.SlotsRange{{Start: 0, End: 16383}},
			Nodes: []*valkey.NodeState{
				{Address: "10.0.0.1", Id: "p0", Flags: []string{"master"}, ClusterInfo: map[string]string{"cluster_known_nodes": "3"}},
				{Address: "10.0.0.2", Id: "r0", Flags: []string{"slave"}, ClusterInfo: map[string]string{"cluster_known_nodes": "3"}},
			},
		}},
		PendingNodes: []*valkey.NodeState{
			slotTestPrimary("10.0.1.1", "3"),
			slotTestPrimary("10.0.1.2", "3"),
		},
	}
	assert.True(t, initialSlotPrimariesReady(cluster, owned, nodes),
		"a shard that already owns slots does not block the new primary")

	// The labeled primary was scraped, but it is slotless. The shard's slots
	// belong to a primary that did not answer; only its replica did. That
	// primary is not node-index 0.
	replicaOnly := &valkey.ClusterState{
		Shards: []*valkey.ShardState{{
			Id:        "s0",
			PrimaryId: "promoted-not-scraped",
			Nodes: []*valkey.NodeState{
				{Address: "10.0.0.2", Id: "r0", Flags: []string{"slave"}},
			},
		}},
		PendingNodes: []*valkey.NodeState{
			slotTestPrimary("10.0.0.1", "3"),
			slotTestPrimary("10.0.1.1", "3"),
		},
	}
	assert.Equal(t, "primary of 10.0.0.2", initialSlotWait(cluster, replicaOnly, nodes),
		"a replica-only shard must not make its primary's slots look free")
	assert.False(t, initialSlotPrimariesReady(cluster, replicaOnly, nodes))

	assert.False(t, initialSlotPrimariesReady(cluster, nil, nodes))
}
