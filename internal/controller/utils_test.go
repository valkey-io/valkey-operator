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
	valkeyv1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestLabels(t *testing.T) {
	testLabels := map[string]string{
		"app": "user-label",
	}
	cluster := &valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-resource",
			Namespace: "default",
			Labels:    testLabels,
		},
	}
	result := labels(cluster)
	if testLabels["app"] != result["app"] {
		t.Errorf("Expected %v, got %v", testLabels["app"], result["app"])
	}
	if result["app.kubernetes.io/name"] != appName {
		t.Errorf("Expected %v, got %v", appName, result["app.kubernetes.io/name"])
	}
	if result["app.kubernetes.io/instance"] != "test-resource" {
		t.Errorf("Expected %v, got %v", "test-resource", result["app.kubernetes.io/instance"])
	}
	result["app.kubernetes.io/component"] = "metrics"
	if result["app.kubernetes.io/component"] != "metrics" {
		t.Errorf("Expected %v, got %v", "metrics", result["app.kubernetes.io/component"])
	}
	result2 := labels(cluster)
	if result2["app.kubernetes.io/component"] != "valkey-cluster" {
		t.Errorf("Expected %v, got %v", "valkey-cluster", result2["app.kubernetes.io/component"])
	}
}

func TestAnnotations(t *testing.T) {
	testAnnotations := map[string]string{
		"app": "user-annotation",
	}
	cluster := &valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-resource",
			Namespace:   "default",
			Annotations: testAnnotations,
		},
	}
	result := annotations(cluster)
	if testAnnotations["app"] != result["app"] {
		t.Errorf("Expected %v, got %v", testAnnotations["app"], result["app"])
	}
}

func TestConfigMapName(t *testing.T) {
	testMapName := "valkey-test-resource"
	result := GetServerConfigMapName("test-resource")
	if result != testMapName {
		t.Errorf("Expected '%v', got '%v'", testMapName, result)
	}
}

func TestNodeRoleAndShard(t *testing.T) {
	nodes := &valkeyv1.ValkeyNodeList{
		Items: []valkeyv1.ValkeyNode{
			{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						LabelShardIndex: "0",
						LabelNodeIndex:  "0",
					},
				},
				Status: valkeyv1.ValkeyNodeStatus{PodIP: "10.0.0.1"},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						LabelShardIndex: "1",
						LabelNodeIndex:  "2",
					},
				},
				Status: valkeyv1.ValkeyNodeStatus{PodIP: "10.0.0.2"},
			},
		},
	}

	role, shard := nodeRoleAndShard("10.0.0.1", nodes)
	assert.Equal(t, RolePrimary, role) // nodeIndex 0 = primary
	assert.Equal(t, 0, shard)

	role, shard = nodeRoleAndShard("10.0.0.2", nodes)
	assert.Equal(t, RoleReplica, role) // nodeIndex 2 = replica
	assert.Equal(t, 1, shard)

	role, shard = nodeRoleAndShard("10.0.0.99", nodes)
	assert.Equal(t, "", role)
	assert.Equal(t, -1, shard)
}

func TestReplicaFirstNodeOrder(t *testing.T) {
	nodes := &valkeyv1.ValkeyNodeList{
		Items: []valkeyv1.ValkeyNode{
			{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						LabelShardIndex: "0",
						LabelNodeIndex:  "0",
					},
				},
				Status: valkeyv1.ValkeyNodeStatus{PodIP: "10.0.0.1"},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						LabelShardIndex: "0",
						LabelNodeIndex:  "1",
					},
				},
				Status: valkeyv1.ValkeyNodeStatus{PodIP: "10.0.0.2"},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						LabelShardIndex: "0",
						LabelNodeIndex:  "2",
					},
				},
				Status: valkeyv1.ValkeyNodeStatus{PodIP: "10.0.0.3"},
			},
		},
	}

	clusterStateWithPrimary := func(primaryID string) *valkey.ClusterState {
		return &valkey.ClusterState{
			Shards: []*valkey.ShardState{
				{
					Id:        "shard-0",
					PrimaryId: primaryID,
					Slots:     []valkey.SlotsRange{{Start: 0, End: 16383}},
					Nodes: []*valkey.NodeState{
						{Address: "10.0.0.1", Id: "node-0"},
						{Address: "10.0.0.2", Id: "node-1"},
						{Address: "10.0.0.3", Id: "node-2"},
					},
				},
			},
		}
	}

	t.Run("nil clusterState returns descending order", func(t *testing.T) {
		order := replicaFirstNodeOrder(0, 3, nodes, nil)
		assert.Equal(t, []int{2, 1, 0}, order)
	})

	t.Run("primary at index 0 is placed last", func(t *testing.T) {
		order := replicaFirstNodeOrder(0, 3, nodes, clusterStateWithPrimary("node-0"))
		assert.Equal(t, []int{1, 2, 0}, order)
	})

	t.Run("primary at non-zero index is placed last (post-failover)", func(t *testing.T) {
		order := replicaFirstNodeOrder(0, 3, nodes, clusterStateWithPrimary("node-1"))
		assert.Equal(t, []int{0, 2, 1}, order)
	})

	t.Run("primary IP not in nodes list returns descending order", func(t *testing.T) {
		state := &valkey.ClusterState{
			Shards: []*valkey.ShardState{
				{
					Id:        "shard-0",
					PrimaryId: "node-x",
					Slots:     []valkey.SlotsRange{{Start: 0, End: 16383}},
					Nodes: []*valkey.NodeState{
						{Address: "10.9.9.9", Id: "node-x"},
					},
				},
			},
		}
		order := replicaFirstNodeOrder(0, 3, nodes, state)
		assert.Equal(t, []int{2, 1, 0}, order)
	})

	t.Run("correct shard is used when multiple shards present", func(t *testing.T) {
		multiShardNodes := &valkeyv1.ValkeyNodeList{
			Items: []valkeyv1.ValkeyNode{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							LabelShardIndex: "0",
							LabelNodeIndex:  "0",
						},
					},
					Status: valkeyv1.ValkeyNodeStatus{PodIP: "10.0.0.1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							LabelShardIndex: "0",
							LabelNodeIndex:  "1",
						},
					},
					Status: valkeyv1.ValkeyNodeStatus{PodIP: "10.0.0.2"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							LabelShardIndex: "1",
							LabelNodeIndex:  "0",
						},
					},
					Status: valkeyv1.ValkeyNodeStatus{PodIP: "10.1.0.1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							LabelShardIndex: "1",
							LabelNodeIndex:  "1",
						},
					},
					Status: valkeyv1.ValkeyNodeStatus{PodIP: "10.1.0.2"},
				},
			},
		}
		// Shard 1's primary is at node-index 1 (post-failover on shard 1).
		// Shard 0's primary is at node-index 0 — but we're asking about shard 1,
		// so shard 0 nodes must not affect the result.
		state := &valkey.ClusterState{
			Shards: []*valkey.ShardState{
				{
					Id:        "shard-0",
					PrimaryId: "s0-node-0",
					Slots:     []valkey.SlotsRange{{Start: 0, End: 8191}},
					Nodes: []*valkey.NodeState{
						{Address: "10.0.0.1", Id: "s0-node-0"},
						{Address: "10.0.0.2", Id: "s0-node-1"},
					},
				},
				{
					Id:        "shard-1",
					PrimaryId: "s1-node-1",
					Slots:     []valkey.SlotsRange{{Start: 8192, End: 16383}},
					Nodes: []*valkey.NodeState{
						{Address: "10.1.0.1", Id: "s1-node-0"},
						{Address: "10.1.0.2", Id: "s1-node-1"},
					},
				},
			},
		}
		// Querying shard 1 with nodesPerShard=2: node-index 1 is the primary,
		// so it is placed last → [0, 1].
		order := replicaFirstNodeOrder(1, 2, multiShardNodes, state)
		assert.Equal(t, []int{0, 1}, order)
	})
}

func TestPrimaryNodeIndexForShard(t *testing.T) {
	nodes := &valkeyv1.ValkeyNodeList{
		Items: []valkeyv1.ValkeyNode{
			{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{LabelShardIndex: "0", LabelNodeIndex: "0"},
				},
				Status: valkeyv1.ValkeyNodeStatus{PodIP: "10.0.0.1"},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{LabelShardIndex: "0", LabelNodeIndex: "1"},
				},
				Status: valkeyv1.ValkeyNodeStatus{PodIP: "10.0.0.2"},
			},
		},
	}

	clusterStateWithPrimary := func(primaryID string) *valkey.ClusterState {
		return &valkey.ClusterState{
			Shards: []*valkey.ShardState{
				{
					Id:        "shard-0",
					PrimaryId: primaryID,
					Slots:     []valkey.SlotsRange{{Start: 0, End: 16383}},
					Nodes: []*valkey.NodeState{
						{Address: "10.0.0.1", Id: "node-0"},
						{Address: "10.0.0.2", Id: "node-1"},
					},
				},
			},
		}
	}

	t.Run("primary at index 0 returns 0", func(t *testing.T) {
		assert.Equal(t, 0, primaryNodeIndexForShard(0, 2, nodes, clusterStateWithPrimary("node-0")))
	})

	t.Run("primary at index 1 returns 1 (post-failover)", func(t *testing.T) {
		assert.Equal(t, 1, primaryNodeIndexForShard(0, 2, nodes, clusterStateWithPrimary("node-1")))
	})

	t.Run("nil clusterState returns -1", func(t *testing.T) {
		assert.Equal(t, -1, primaryNodeIndexForShard(0, 2, nodes, nil))
	})

	t.Run("shard not in topology returns -1", func(t *testing.T) {
		// clusterState has no shards with slots, so findShardPrimary returns ""
		state := &valkey.ClusterState{Shards: []*valkey.ShardState{}}
		assert.Equal(t, -1, primaryNodeIndexForShard(0, 2, nodes, state))
	})

	t.Run("primary IP not in nodes list returns -1", func(t *testing.T) {
		state := &valkey.ClusterState{
			Shards: []*valkey.ShardState{
				{
					Id:        "shard-0",
					PrimaryId: "node-x",
					Slots:     []valkey.SlotsRange{{Start: 0, End: 16383}},
					Nodes:     []*valkey.NodeState{{Address: "10.9.9.9", Id: "node-x"}},
				},
			},
		}
		assert.Equal(t, -1, primaryNodeIndexForShard(0, 2, nodes, state))
	})

	t.Run("primary index out of range returns -1", func(t *testing.T) {
		// nodesPerShard=1 but primary resolves to index 1 — out of range
		assert.Equal(t, -1, primaryNodeIndexForShard(0, 1, nodes, clusterStateWithPrimary("node-1")))
	})
}

func TestBuildClusterValkeyNodeLabels(t *testing.T) {
	cluster := &valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mycluster",
			Namespace: "default",
			Labels: map[string]string{
				"custom-label": "custom-value",
				// Attempt to override a recommended label via the cluster; should be ignored.
				"app.kubernetes.io/name": "should-be-ignored",
			},
		},
	}

	node := buildClusterValkeyNode(cluster, 1, 2)

	// Recommended k8s labels.
	assert.Equal(t, appName, node.Labels["app.kubernetes.io/name"])
	assert.Equal(t, "mycluster", node.Labels["app.kubernetes.io/instance"])
	assert.Equal(t, "valkey-node", node.Labels["app.kubernetes.io/component"])
	assert.Equal(t, appName, node.Labels["app.kubernetes.io/part-of"])
	assert.Equal(t, "valkey-operator", node.Labels["app.kubernetes.io/managed-by"])

	// Operator-specific labels.
	assert.Equal(t, "mycluster", node.Labels[LabelCluster])
	assert.Equal(t, "1", node.Labels[LabelShardIndex])
	assert.Equal(t, "2", node.Labels[LabelNodeIndex])

	// User-defined labels are inherited from the parent cluster.
	assert.Equal(t, "custom-value", node.Labels["custom-label"])

	// Recommended labels cannot be overridden by user-defined cluster labels.
	assert.Equal(t, appName, node.Labels["app.kubernetes.io/name"])
}

func TestBuildClusterValkeyNodeImagePullSecrets(t *testing.T) {
	secrets := []corev1.LocalObjectReference{{Name: "registrycredential"}}
	cluster := &valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "mycluster", Namespace: "default"},
		Spec:       valkeyv1.ValkeyClusterSpec{ImagePullSecrets: secrets},
	}

	node := buildClusterValkeyNode(cluster, 0, 0)
	assert.Equal(t, secrets, node.Spec.ImagePullSecrets, "imagePullSecrets should propagate cluster -> node")

	// Absent on the cluster -> absent on the node.
	bare := buildClusterValkeyNode(&valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c2", Namespace: "default"},
	}, 0, 0)
	assert.Empty(t, bare.Spec.ImagePullSecrets)
}

// TestBuildClusterValkeyNodePodSecurityContext verifies that
// cluster.Spec.PodSecurityContext is propagated to the per-shard
// ValkeyNode so the node controller can render it onto the pod.
func TestBuildClusterValkeyNodePodSecurityContext(t *testing.T) {
	fsGroup := int64(56849)
	psc := &corev1.PodSecurityContext{
		FSGroup:      &fsGroup,
		RunAsNonRoot: boolPtr(true),
	}
	cluster := &valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "mycluster", Namespace: "default"},
		Spec:       valkeyv1.ValkeyClusterSpec{PodSecurityContext: psc},
	}

	node := buildClusterValkeyNode(cluster, 0, 0)
	assert.Equal(t, psc, node.Spec.PodSecurityContext, "podSecurityContext should propagate cluster -> node")

	// Absent on the cluster -> absent on the node.
	bare := buildClusterValkeyNode(&valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c2", Namespace: "default"},
	}, 0, 0)
	assert.Nil(t, bare.Spec.PodSecurityContext)
}

func boolPtr(b bool) *bool { return &b }
