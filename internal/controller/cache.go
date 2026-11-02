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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ManagedCacheByObject is the manager cache scope. Pods and ConfigMaps the
// operator creates have to carry app.kubernetes.io/managed-by=valkey-operator
// or a later Get treats them as missing.
func ManagedCacheByObject() map[client.Object]cache.ByObject {
	managedBy := k8slabels.SelectorFromSet(k8slabels.Set{
		"app.kubernetes.io/managed-by": "valkey-operator",
	})
	return map[client.Object]cache.ByObject{
		&corev1.Service{}:               {Label: managedBy},
		&appsv1.StatefulSet{}:           {Label: managedBy},
		&appsv1.Deployment{}:            {Label: managedBy},
		&corev1.ConfigMap{}:             {Label: managedBy},
		&corev1.Secret{}:                {Label: managedBy},
		&corev1.PersistentVolumeClaim{}: {Label: managedBy},
		&policyv1.PodDisruptionBudget{}: {Label: managedBy},
		// Pod is not explicitly watched but is cached due to r.List calls
		// in the ValkeyNode controller.
		&corev1.Pod{}: {Label: managedBy},
	}
}
