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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

const (
	// allowWorkloadRevisionAnnotation is set by the ValkeyCluster controller on a
	// ValkeyNode to permit exactly one rolling workload template update. Value is
	// either the desired pod-template roll hash, or allowWorkloadRevisionAny for
	// Spec-driven rolls where the cluster does not know the hash yet.
	allowWorkloadRevisionAnnotation = "valkey.io/allow-workload-revision"

	// desiredWorkloadRevisionAnnotation is set by the ValkeyNode controller when
	// a cluster-owned node has a pod-template drift but no permit yet. Value is
	// the desired template roll hash the cluster controller should grant.
	desiredWorkloadRevisionAnnotation = "valkey.io/desired-workload-revision"

	// allowWorkloadRevisionAny permits one rolling update for whatever the node
	// controller currently builds as desired (used after a ValkeyNode Spec change).
	allowWorkloadRevisionAny = "*"
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

// workloadPermitAllows returns true when the node annotation permits applying
// a rolling template update for desiredHash.
func workloadPermitAllows(node *valkeyiov1alpha1.ValkeyNode, desiredHash string) bool {
	if node.Annotations == nil {
		return false
	}
	permit := node.Annotations[allowWorkloadRevisionAnnotation]
	return permit == allowWorkloadRevisionAny || (desiredHash != "" && permit == desiredHash)
}

// nodeNeedsWorkloadPermit reports that the node is waiting for the cluster to
// grant allow-workload-revision (desired hash known or drift condition set, and
// no matching permit yet).
func nodeNeedsWorkloadPermit(node *valkeyiov1alpha1.ValkeyNode) bool {
	desired := ""
	if node.Annotations != nil {
		desired = node.Annotations[desiredWorkloadRevisionAnnotation]
	}
	if desired != "" {
		return !workloadPermitAllows(node, desired)
	}
	// Condition can appear a beat before the annotation is written.
	return meta.IsStatusConditionTrue(node.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionWorkloadDrift)
}

// nodeHasInFlightWorkloadRoll reports a permit is outstanding (granted and not
// yet fully cleared after the pod has rolled).
func nodeHasInFlightWorkloadRoll(node *valkeyiov1alpha1.ValkeyNode) bool {
	if node.Annotations == nil {
		return false
	}
	return node.Annotations[allowWorkloadRevisionAnnotation] != ""
}

// nodeHasPendingWorkloadDrift is true when the node still needs a permit or has
// an in-flight permitted roll. Used to scrape cluster state for failover.
func nodeHasPendingWorkloadDrift(node *valkeyiov1alpha1.ValkeyNode) bool {
	return nodeNeedsWorkloadPermit(node) || nodeHasInFlightWorkloadRoll(node)
}

// anyNodeRequiresWorkloadRoll is true when any listed node is waiting on a
// workload template permit or has an in-flight permitted roll.
func anyNodeRequiresWorkloadRoll(nodeList *valkeyiov1alpha1.ValkeyNodeList) bool {
	if nodeList == nil {
		return false
	}
	for i := range nodeList.Items {
		if nodeHasPendingWorkloadDrift(&nodeList.Items[i]) {
			return true
		}
	}
	return false
}

// anyNodeHasInFlightWorkloadRoll reports whether any node currently holds a roll permit.
func anyNodeHasInFlightWorkloadRoll(nodeList *valkeyiov1alpha1.ValkeyNodeList) bool {
	if nodeList == nil {
		return false
	}
	for i := range nodeList.Items {
		if nodeHasInFlightWorkloadRoll(&nodeList.Items[i]) {
			return true
		}
	}
	return false
}

// setNodeAnnotation ensures annotations map exists and sets key to value.
// Returns true if the annotation changed.
func setNodeAnnotation(node *valkeyiov1alpha1.ValkeyNode, key, value string) bool {
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	if node.Annotations[key] == value {
		return false
	}
	node.Annotations[key] = value
	return true
}

// clearNodeAnnotation removes key if present. Returns true if removed.
func clearNodeAnnotation(node *valkeyiov1alpha1.ValkeyNode, key string) bool {
	if node.Annotations == nil {
		return false
	}
	if _, ok := node.Annotations[key]; !ok {
		return false
	}
	delete(node.Annotations, key)
	if len(node.Annotations) == 0 {
		node.Annotations = nil
	}
	return true
}
