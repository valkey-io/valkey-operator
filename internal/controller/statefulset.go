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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

// createStatefulSet creates a singleton StatefulSet for the ValkeyCluster.
func createStatefulSet(cluster *valkeyiov1alpha1.ValkeyCluster) *appsv1.StatefulSet {
	containers := generateContainersDef(cluster)
	volumes := generateVolumesDef(cluster)
	statefulset := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: cluster.Name + "-",
			Namespace:    cluster.Namespace,
			Labels:       labels(cluster),
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: func(i int32) *int32 { return &i }(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: labels(cluster),
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels(cluster),
				},
				Spec: corev1.PodSpec{
					Containers: containers,
					Volumes:    volumes,
				},
			},
		},
	}
	return statefulset
}
