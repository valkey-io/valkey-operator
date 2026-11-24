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
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// pausedRequeue is how often a paused object is looked at again, so that a
// resume is picked up even if the annotation change itself is missed.
const pausedRequeue = 30 * time.Second

// reconciliationPaused reports whether obj carries the pause annotation with
// the exact value "true". Any other value, or no annotation, means running.
func reconciliationPaused(obj metav1.Object) bool {
	return annotationTrue(obj, valkeyiov1alpha1.AnnotationPauseReconciliation)
}

// syncNodePause sets the pause annotation on every ValkeyNode of the cluster
// while the cluster is paused, and removes it from all of them, including one
// a human set by hand, once the cluster runs again. It is the one write the
// operator makes to a paused cluster's objects: the ValkeyNode controller
// reads its own annotation, so the nodes need not look up their parent.
func (r *ValkeyClusterReconciler) syncNodePause(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, paused bool) error {
	nodes := &valkeyiov1alpha1.ValkeyNodeList{}
	if err := r.List(ctx, nodes, client.InNamespace(cluster.Namespace), client.MatchingLabels{LabelCluster: cluster.Name}); err != nil {
		return err
	}
	for _, node := range ownedValkeyNodes(cluster, nodes) {
		var err error
		if paused {
			err = r.patchNodeAnnotations(ctx, node, map[string]string{valkeyiov1alpha1.AnnotationPauseReconciliation: annotationTrueValue}, nil)
		} else {
			err = r.patchNodeAnnotations(ctx, node, nil, []string{valkeyiov1alpha1.AnnotationPauseReconciliation})
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// reconcilePaused handles a paused cluster: it marks the ValkeyNodes, sets
// the ReconciliationPaused condition with an event on the transition, and
// changes nothing else.
func (r *ValkeyClusterReconciler) reconcilePaused(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) error {
	if err := r.syncNodePause(ctx, cluster, true); err != nil {
		return err
	}
	if meta.IsStatusConditionTrue(cluster.Status.Conditions, valkeyiov1alpha1.ConditionReconciliationPaused) {
		// Rebuild gauge values from saved status after an operator restart.
		updateClusterMetrics(cluster)
		return nil
	}
	logf.FromContext(ctx).Info("Paused reconciliation of ValkeyCluster", "annotation", valkeyiov1alpha1.AnnotationPauseReconciliation)
	r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "ReconciliationPaused", "PauseReconciliation",
		"Reconciliation paused by the %s annotation", valkeyiov1alpha1.AnnotationPauseReconciliation)
	setCondition(cluster, valkeyiov1alpha1.ConditionReconciliationPaused, valkeyiov1alpha1.ReasonPauseAnnotationSet,
		"The "+valkeyiov1alpha1.AnnotationPauseReconciliation+" annotation is set to \"true\"; the operator changes nothing until it is removed",
		metav1.ConditionTrue)
	return r.updateStatus(ctx, cluster, nil)
}

// resumeIfPaused undoes a pause once the annotation is gone: it removes the
// annotation from the ValkeyNodes and clears the condition, with an event.
func (r *ValkeyClusterReconciler) resumeIfPaused(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) (bool, error) {
	if err := r.syncNodePause(ctx, cluster, false); err != nil {
		return false, err
	}
	if meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionReconciliationPaused) == nil {
		return false, nil
	}
	logf.FromContext(ctx).Info("Resumed reconciliation of ValkeyCluster")
	r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "ReconciliationResumed", "PauseReconciliation", "Reconciliation resumed")
	meta.RemoveStatusCondition(&cluster.Status.Conditions, valkeyiov1alpha1.ConditionReconciliationPaused)
	if err := r.updateStatus(ctx, cluster, nil); err != nil {
		return false, err
	}
	return true, nil
}
