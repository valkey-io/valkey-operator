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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	valkeyv1 "valkey.io/valkey-operator/api/v1alpha1"
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

func TestGenerateValkeyConfig_WithStorage(t *testing.T) {
	cluster := &valkeyv1.ValkeyCluster{
		Spec: valkeyv1.ValkeyClusterSpec{
			Storage: &valkeyv1.StorageSpec{
				Size: resource.MustParse("1Gi"),
			},
		},
	}

	config := generateValkeyConfig(cluster)
	assert.Contains(t, config, "dir /data")
	assert.Contains(t, config, "appendonly yes")
}

func TestGenerateValkeyNodeConfig_WithStorage(t *testing.T) {
	node := &valkeyv1.ValkeyNode{
		Spec: valkeyv1.ValkeyNodeSpec{
			Storage: &valkeyv1.StorageSpec{
				Size: resource.MustParse("1Gi"),
			},
		},
	}

	config := generateValkeyNodeConfig(node)
	assert.Contains(t, config, "dir /data")
	assert.Contains(t, config, "appendonly yes")
}
