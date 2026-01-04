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
	corev1 "k8s.io/api/core/v1"
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

func TestCreateClusterDeployment_SetsPodAntiAffinity(t *testing.T) {
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

	d := createClusterDeployment(cluster)

	got := d.Spec.Template.Spec.Affinity
	if diff := cmp.Diff(antiAffinity, got, cmpopts.EquateEmpty()); diff != "" {
		t.Fatalf("affinity mismatch (-want +got):\n%s", diff)
	}
}
