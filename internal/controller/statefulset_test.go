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

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	valkeyv1 "valkey.io/valkey-operator/api/v1alpha1"
)

func TestCreateClusterStatefulSet(t *testing.T) {
	cluster := &valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mycluster",
		},
		Spec: valkeyv1.ValkeyClusterSpec{
			Image: "container:version",
		},
	}
	s := createClusterStatefulSet(cluster, 3, "mycluster-0-0")
	if s.Name != "mycluster-0-0" {
		t.Errorf("Expected %v, got %v", "mycluster-0-0", s.Name)
	}
	if s.Spec.ServiceName != "mycluster" {
		t.Errorf("Expected %v, got %v", "mycluster", s.Spec.ServiceName)
	}
	if *s.Spec.Replicas != 3 {
		t.Errorf("Expected %v, got %v", 3, s.Spec.Replicas)
	}
	if len(s.Spec.Template.Spec.Containers) != 1 {
		t.Errorf("Expected %v, got %v", 1, len(s.Spec.Template.Spec.Containers))
	}
	if s.Spec.Template.Spec.Containers[0].Image != "container:version" {
		t.Errorf("Expected %v, got %v", "container:version", s.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestCreateClusterStatefulSet_SetsPodAntiAffinity(t *testing.T) {
	antiAffinity := &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
				{
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/instance": "mycluster",
						},
					},
					TopologyKey: "kubernetes.io/hostname",
				},
			},
		},
	}
	cluster := &valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "mycluster"},
		Spec: valkeyv1.ValkeyClusterSpec{
			Image:    "container:version",
			Affinity: antiAffinity,
		},
	}

	s := createClusterStatefulSet(cluster, 1, "mycluster-0-0")

	got := s.Spec.Template.Spec.Affinity
	if diff := cmp.Diff(antiAffinity, got, cmpopts.EquateEmpty()); diff != "" {
		t.Fatalf("affinity mismatch (-want +got):\n%s", diff)
	}
}

func TestCreateClusterStatefulSet_SetsNodeSelector(t *testing.T) {
	nodeSelector := map[string]string{
		"disktype": "ssd",
	}
	cluster := &valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "mycluster"},
		Spec: valkeyv1.ValkeyClusterSpec{
			Image:        "container:version",
			NodeSelector: nodeSelector,
		},
	}

	s := createClusterStatefulSet(cluster, 1, "mycluster-0-0")

	assert.Equal(t, nodeSelector, s.Spec.Template.Spec.NodeSelector, "node selector should match spec")
}
