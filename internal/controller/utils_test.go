/*
Copyright 2024.

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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	valkeyv1 "valkey.io/valkey-operator/api/v1alpha1"
)

func TestLabels(t *testing.T) {
	testLabels := map[string]string{
		"app": "valkey",
	}
	cluster := &valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-resource",
			Namespace: "default",
			Labels:    testLabels,
		},
	}
	result := labels(cluster, -1, "")
	if testLabels["app"] != result["app"] {
		t.Errorf("Expected %v, got %v", testLabels["app"], result["app"])
	}
	if result["app.kubernetes.io/name"] != "valkey" {
		t.Errorf("Expected %v, got %v", "valkey", result["app.kubernetes.io/name"])
	}
	if result["app.kubernetes.io/instance"] != "test-resource" {
		t.Errorf("Expected %v, got %v", "test-resource", result["app.kubernetes.io/instance"])
	}
	result["app.kubernetes.io/component"] = "metrics"
	if result["app.kubernetes.io/component"] != "metrics" {
		t.Errorf("Expected %v, got %v", "metrics", result["app.kubernetes.io/component"])
	}
	result2 := labels(cluster, -1, "")
	if result2["app.kubernetes.io/component"] != "valkey-cluster" {
		t.Errorf("Expected %v, got %v", "valkey-cluster", result2["app.kubernetes.io/component"])
	}

	if result["valkey.io/salt"] != "" {
		t.Errorf("Expected empty salt label, got %v", result["valkey.io/salt"])
	}
	if result["valkey.io/shard"] != "" {
		t.Errorf("Expected empty shard label, got %v", result["valkey.io/shard"])
	}

	result3 := labels(cluster, 2, "XYZ")
	if result3["valkey.io/salt"] != "XYZ" {
		t.Errorf("Expected salt label XYZ, got %v", result3["valkey.io/salt"])
	}
	if result3["valkey.io/shard"] != "2" {
		t.Errorf("Expected shard label 2, got %v", result3["valkey.io/shard"])
	}
}

func TestL(t *testing.T) {
	cluster := &valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-resource",
			Namespace: "default",
		},
	}
	result := l(cluster)
	if result["app.kubernetes.io/name"] != "valkey" {
		t.Errorf("Expected %v, got %v", "valkey", result["app.kubernetes.io/name"])
	}
	if result["app.kubernetes.io/instance"] != "test-resource" {
		t.Errorf("Expected %v, got %v", "test-resource", result["app.kubernetes.io/instance"])
	}
	if result["app.kubernetes.io/component"] != "valkey-cluster" {
		t.Errorf("Expected %v, got %v", "valkey-cluster", result["app.kubernetes.io/component"])
	}
}

func TestAnnotations(t *testing.T) {
	testAnnotations := map[string]string{
		"app": "valkey",
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
