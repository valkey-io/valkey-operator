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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	valkeyv1 "valkey.io/valkey-operator/api/v1alpha1"
)

func TestCreateClusterDeployment(t *testing.T) {
	cluster := &valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mycluster",
		},
		Spec: valkeyv1.ValkeyClusterSpec{
			Image: "container:version",
		},
	}
	d := createClusterDeployment(cluster)
	if d.Name != "" {
		t.Errorf("Expected empty name field, got %v", d.Name)
	}
	if d.GenerateName != "mycluster-" {
		t.Errorf("Expected %v, got %v", "mycluster-", d.GenerateName)
	}
	if *d.Spec.Replicas != 1 {
		t.Errorf("Expected %v, got %v", 1, d.Spec.Replicas)
	}
	if len(d.Spec.Template.Spec.Containers) != 1 {
		t.Errorf("Expected %v, got %v", 1, len(d.Spec.Template.Spec.Containers))
	}
	if d.Spec.Template.Spec.Containers[0].Image != "container:version" {
		t.Errorf("Expected %v, got %v", "container:version", d.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestCreateClusterDeploymentWithOnlyResourceRequests(t *testing.T) {
	resourceReqs := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceRequestsMemory: resource.MustParse("100Mi"),
			corev1.ResourceRequestsCPU:    resource.MustParse("100m"),
		},
	}
	cluster := &valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mycluster",
		},
		Spec: valkeyv1.ValkeyClusterSpec{
			Image:     "container:version",
			Resources: resourceReqs,
		},
	}
	d := createClusterDeployment(cluster)
	if len(d.Spec.Template.Spec.Containers[0].Resources.Requests) != 2 {
		t.Errorf("Expected %v, got %v", 2, len(d.Spec.Template.Spec.Containers[0].Resources.Requests))
	}
}

func TestCreateClusterDeploymentWithOnlyResourceLimits(t *testing.T) {
	resourceReqs := corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceLimitsMemory: resource.MustParse("100Mi"),
			corev1.ResourceLimitsCPU:    resource.MustParse("100m"),
		},
	}
	cluster := &valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mycluster",
		},
		Spec: valkeyv1.ValkeyClusterSpec{
			Image:     "container:version",
			Resources: resourceReqs,
		},
	}
	d := createClusterDeployment(cluster)
	if len(d.Spec.Template.Spec.Containers[0].Resources.Limits) != 2 {
		t.Errorf("Expected %v, got %v", 2, len(d.Spec.Template.Spec.Containers[0].Resources.Limits))
	}
}

func TestCreateClusterDeploymentWithBothResourceRequestsAndLimits(t *testing.T) {
	resourceReqs := corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceLimitsMemory: resource.MustParse("500Mi"),
			corev1.ResourceLimitsCPU:    resource.MustParse("200m"),
		},
		Requests: corev1.ResourceList{
			corev1.ResourceRequestsMemory: resource.MustParse("100Mi"),
			corev1.ResourceRequestsCPU:    resource.MustParse("100m"),
		},
	}
	cluster := &valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mycluster",
		},
		Spec: valkeyv1.ValkeyClusterSpec{
			Image:     "container:version",
			Resources: resourceReqs,
		},
	}
	d := createClusterDeployment(cluster)
	if len(d.Spec.Template.Spec.Containers[0].Resources.Requests) != 2 {
		t.Errorf("Expected %v, got %v", 2, len(d.Spec.Template.Spec.Containers[0].Resources.Requests))
	}
	if len(d.Spec.Template.Spec.Containers[0].Resources.Limits) != 2 {
		t.Errorf("Expected %v, got %v", 2, len(d.Spec.Template.Spec.Containers[0].Resources.Limits))
	}

}
