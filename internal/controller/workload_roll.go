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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// podTemplateRollHash returns a stable hash of the pod template fields that
// cause a StatefulSet/Deployment rolling update when they change.
func podTemplateRollHash(tmpl corev1.PodTemplateSpec) string {
	normalized := normalizePodTemplate(tmpl)
	data, err := json.Marshal(normalized)
	if err != nil {
		// json.Marshal on corev1 types is not expected to fail; fall back so
		// callers still get a deterministic-enough gate key.
		sum := sha256.Sum256([]byte(err.Error()))
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// podTemplateWouldRoll reports whether replacing live with desired would
// change the pod template in a way that replaces pods.
func podTemplateWouldRoll(live, desired corev1.PodTemplateSpec) bool {
	return !equality.Semantic.DeepEqual(normalizePodTemplate(live), normalizePodTemplate(desired))
}

// normalizePodTemplate strips volatile metadata so comparisons and hashes are stable.
func normalizePodTemplate(tmpl corev1.PodTemplateSpec) corev1.PodTemplateSpec {
	out := tmpl.DeepCopy()
	out.ObjectMeta = metav1.ObjectMeta{
		Labels:      out.Labels,
		Annotations: out.Annotations,
	}
	return *out
}

// isClusterOwned reports whether the ValkeyNode is controlled by a ValkeyCluster.
func isClusterOwned(node *valkeyiov1alpha1.ValkeyNode) bool {
	for _, ref := range node.OwnerReferences {
		if ref.Kind != "ValkeyCluster" {
			continue
		}
		if ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

// computeWorkloadRevision builds the pod template for the node (using the same
// builders as the ValkeyNode controller) and returns its roll hash. When
// aclSecret is non-nil, template annotations match ensureStatefulSet/Deployment.
func computeWorkloadRevision(node *valkeyiov1alpha1.ValkeyNode, aclSecret *corev1.Secret) (string, error) {
	tmpl, err := buildNodePodTemplate(node, aclSecret)
	if err != nil {
		return "", err
	}
	return podTemplateRollHash(tmpl), nil
}

// buildNodePodTemplate returns the desired pod template for a ValkeyNode.
func buildNodePodTemplate(node *valkeyiov1alpha1.ValkeyNode, aclSecret *corev1.Secret) (corev1.PodTemplateSpec, error) {
	switch node.Spec.WorkloadType {
	case valkeyiov1alpha1.WorkloadTypeDeployment:
		dep, err := buildValkeyNodeDeployment(node)
		if err != nil {
			return corev1.PodTemplateSpec{}, err
		}
		if aclSecret != nil {
			dep.Spec.Template.Annotations = buildPodTemplateAnnotations(node, aclSecret)
		}
		return dep.Spec.Template, nil
	default:
		sts, err := buildValkeyNodeStatefulSet(node)
		if err != nil {
			return corev1.PodTemplateSpec{}, err
		}
		if aclSecret != nil {
			sts.Spec.Template.Annotations = buildPodTemplateAnnotations(node, aclSecret)
		}
		return sts.Spec.Template, nil
	}
}

// workloadRevisionAllows reports whether Spec.WorkloadRevision authorizes applying
// a rolling template whose hash is desiredHash.
func workloadRevisionAllows(node *valkeyiov1alpha1.ValkeyNode, desiredHash string) bool {
	return desiredHash != "" && node.Spec.WorkloadRevision == desiredHash
}

// setDesiredWorkloadRevision sets Spec.WorkloadRevision from the built template.
func setDesiredWorkloadRevision(node *valkeyiov1alpha1.ValkeyNode, aclSecret *corev1.Secret) error {
	rev, err := computeWorkloadRevision(node, aclSecret)
	if err != nil {
		return fmt.Errorf("compute workload revision for %s: %w", node.Name, err)
	}
	node.Spec.WorkloadRevision = rev
	return nil
}
