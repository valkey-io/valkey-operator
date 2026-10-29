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

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestExcessNodesStillServing(t *testing.T) {
	cluster := &valkeyiov1alpha1.ValkeyCluster{
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{Shards: 1},
	}
	nodes := &valkeyiov1alpha1.ValkeyNodeList{
		Items: []valkeyiov1alpha1.ValkeyNode{{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "demo-1-0",
				Labels: map[string]string{LabelShardIndex: "1"},
			},
			Status: valkeyiov1alpha1.ValkeyNodeStatus{PodIP: "10.0.0.3"},
		}},
	}

	viewer := &valkey.NodeState{Id: "abc", Address: "10.0.0.1", ShardId: "shard-0"}
	viewer.SetClusterNodesForTesting(
		"abc 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-8191\n" +
			"ghi 10.0.0.3:6379@16379 master - 0 0 2 connected 8192-16383\n",
	)
	state := &valkey.ClusterState{
		Shards: []*valkey.ShardState{{
			Id:        "shard-0",
			PrimaryId: "abc",
			Nodes:     []*valkey.NodeState{viewer},
			Slots:     []valkey.SlotsRange{{Start: 0, End: 8191}},
		}},
	}

	if !excessNodesStillServing(cluster, nodes, state) {
		t.Fatal("expected the excess primary to still be serving slots")
	}

	nodes.Items[0].Labels[LabelShardIndex] = "0"
	if excessNodesStillServing(cluster, nodes, state) {
		t.Fatal("an in-spec node must not block deletion of other excess nodes")
	}

	nodes.Items[0].Labels[LabelShardIndex] = "1"
	nodes.Items[0].Status.PodIP = "10.9.9.9"
	if excessNodesStillServing(cluster, nodes, state) {
		t.Fatal("a peer table that does not name this pod must not block deletion")
	}
}
