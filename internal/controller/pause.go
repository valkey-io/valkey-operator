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
	return obj.GetAnnotations()[valkeyiov1alpha1.AnnotationPauseReconciliation] == "true"
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
	for i := range nodes.Items {
		node := &nodes.Items[i]
		_, has := node.Annotations[valkeyiov1alpha1.AnnotationPauseReconciliation]
		if paused == reconciliationPaused(node) && (paused || !has) {
			continue
		}
		patch := client.MergeFrom(node.DeepCopy())
		if paused {
			upsertAnnotation(node, valkeyiov1alpha1.AnnotationPauseReconciliation, "true")
		} else {
			delete(node.Annotations, valkeyiov1alpha1.AnnotationPauseReconciliation)
		}
		if err := r.Patch(ctx, node, patch); err != nil {
			return client.IgnoreNotFound(err)
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
		return nil
	}
	logf.FromContext(ctx).Info("Paused reconciliation of ValkeyCluster", "annotation", valkeyiov1alpha1.AnnotationPauseReconciliation)
	r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "ReconciliationPaused", "PauseReconciliation",
		"Reconciliation paused by the %s annotation", valkeyiov1alpha1.AnnotationPauseReconciliation)
	setCondition(cluster, valkeyiov1alpha1.ConditionReconciliationPaused, valkeyiov1alpha1.ReasonReconciliationPaused,
		"The "+valkeyiov1alpha1.AnnotationPauseReconciliation+" annotation is set to \"true\"; the operator changes nothing until it is removed",
		metav1.ConditionTrue)
	return r.updateStatus(ctx, cluster, nil)
}

// resumeIfPaused undoes a pause once the annotation is gone: it removes the
// annotation from the ValkeyNodes and clears the condition, with an event.
func (r *ValkeyClusterReconciler) resumeIfPaused(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) error {
	if err := r.syncNodePause(ctx, cluster, false); err != nil {
		return err
	}
	if meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionReconciliationPaused) == nil {
		return nil
	}
	logf.FromContext(ctx).Info("Resumed reconciliation of ValkeyCluster")
	r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "ReconciliationResumed", "PauseReconciliation", "Reconciliation resumed")
	meta.RemoveStatusCondition(&cluster.Status.Conditions, valkeyiov1alpha1.ConditionReconciliationPaused)
	return r.updateStatus(ctx, cluster, nil)
}
