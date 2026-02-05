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
	"maps"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	valkeyv1 "valkey.io/valkey-operator/api/v1alpha1"
)

const appName = "valkey"

// Labels returns a copy of user defined labels including recommended:
// https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/
func labels(cluster *valkeyv1.ValkeyCluster, extraLabels ...map[string]string) map[string]string {
	if cluster.Labels == nil {
		cluster.Labels = make(map[string]string)
	}
	l := maps.Clone(cluster.Labels)
	l["app.kubernetes.io/name"] = appName
	l["app.kubernetes.io/instance"] = cluster.Name
	l["app.kubernetes.io/component"] = "valkey-cluster"
	l["app.kubernetes.io/part-of"] = appName
	l["app.kubernetes.io/managed-by"] = "valkey-operator"

	// Copy extra labels into main map, overriding duplicates
	for _, e := range extraLabels {
		maps.Copy(l, e)
	}

	return l
}

// Annotations returns a copy of user defined annotations.
func annotations(cluster *valkeyv1.ValkeyCluster) map[string]string {
	return maps.Clone(cluster.Annotations)
}

// This function takes a K8S object reference (eg: pod, secret, configmap, etc),
// and a map of annotations to add to, or replace existing, within the object.
// Returns true if the annotation was added, or updated
func upsertAnnotation(o metav1.Object, key string, val string) bool {

	updated := false

	// Get current annotations
	annotations := o.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}

	// If not found, insert, or update
	if orig := annotations[key]; orig != val {

		updated = true
		annotations[key] = val

		// Set annotations
		o.SetAnnotations(annotations)
	}

	return updated
}
