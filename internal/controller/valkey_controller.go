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
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// ValkeyReconciler reconciles a standalone Valkey: one primary, no replicas.
type ValkeyReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  events.EventRecorder
}

// +kubebuilder:rbac:groups=valkey.io,resources=valkeys,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=valkey.io,resources=valkeys/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=valkey.io,resources=valkeys/finalizers,verbs=update
// +kubebuilder:rbac:groups=valkey.io,resources=valkeynodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile builds the children of a Valkey and derives its status from the single ValkeyNode.
// Children are garbage collected through owner references, so there is no finalizer.
func (r *ValkeyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.V(1).Info("reconcile...")

	v := &valkeyiov1alpha1.Valkey{}
	if err := r.Get(ctx, req.NamespacedName, v); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Reconciling during deletion would recreate the children being deleted.
	if !v.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	rc := &resourceClient{Client: r.Client, APIReader: r.APIReader, Scheme: r.Scheme, Recorder: r.Recorder}
	labels := valkeyLabels(v)

	if err := upsertHeadlessService(ctx, rc, v, v.Name, v.Namespace, labels); err != nil {
		return r.failChild(ctx, v, valkeyiov1alpha1.ReasonServiceError, err)
	}

	if err := reconcilePDB(ctx, rc, v, v.Name, v.Namespace, labels, v.Spec.PodDisruptionBudgetEnabled()); err != nil {
		return r.failChild(ctx, v, valkeyiov1alpha1.ReasonPodDisruptionBudgetError, err)
	}

	if err := reconcileUsersAcl(ctx, rc, v, v.Name, v.Namespace, labels, v.Spec.Users, v.Spec.ExporterEnabled()); err != nil {
		return r.failChild(ctx, v, valkeyiov1alpha1.ReasonUsersAclError, err)
	}

	if err := upsertServerConfigMap(ctx, rc, v, v.Name, v.Namespace, labels, buildValkeyServerConfig(v)); err != nil {
		return r.failChild(ctx, v, valkeyiov1alpha1.ReasonConfigMapError, err)
	}

	node, err := reconcileStandaloneNode(ctx, rc, v)
	if err != nil {
		return r.failChild(ctx, v, valkeyiov1alpha1.ReasonValkeyNodeError, err)
	}

	v.Status.Primary = node.Name
	if node.Status.Ready {
		setConditionOn(&v.Status.Conditions, v.Generation, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonInstanceHealthy, "Instance is healthy", metav1.ConditionTrue)
		setConditionOn(&v.Status.Conditions, v.Generation, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonReconcileComplete, "No changes needed", metav1.ConditionFalse)
	} else {
		msg := fmt.Sprintf("Waiting for ValkeyNode %s to become ready", node.Name)
		setConditionOn(&v.Status.Conditions, v.Generation, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonReconciling, msg, metav1.ConditionFalse)
		setConditionOn(&v.Status.Conditions, v.Generation, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonReconciling, msg, metav1.ConditionTrue)
	}

	// A node whose ACL the operator cannot apply leaves the declared users out of Valkey.
	// The instance would otherwise read as healthy with an ACL that is not in effect.
	if c := meta.FindStatusCondition(node.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionACLApplied); c != nil && c.Status == metav1.ConditionFalse && c.Reason == valkeyiov1alpha1.ValkeyNodeReasonApplyFailed {
		setConditionOn(&v.Status.Conditions, v.Generation, valkeyiov1alpha1.ConditionDegraded, valkeyiov1alpha1.ReasonACLApplyFailed, fmt.Sprintf("ACL not applied on %s", node.Name), metav1.ConditionTrue)
	} else {
		meta.RemoveStatusCondition(&v.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)
	}

	// The node's status changes requeue this Valkey through Owns, so no periodic requeue is needed.
	if err := r.updateStatus(ctx, v); err != nil {
		return ctrl.Result{}, err
	}
	log.V(1).Info("reconcile done")
	return ctrl.Result{}, nil
}

// failChild records a child resource error as Ready=False and returns the error for a retry.
func (r *ValkeyReconciler) failChild(ctx context.Context, v *valkeyiov1alpha1.Valkey, reason string, err error) (ctrl.Result, error) {
	setConditionOn(&v.Status.Conditions, v.Generation, valkeyiov1alpha1.ConditionReady, reason, err.Error(), metav1.ConditionFalse)
	_ = r.updateStatus(ctx, v)
	return ctrl.Result{}, err
}

// updateStatus writes the in-memory conditions and derived state to the Valkey status.
// The state priority matches the ValkeyCluster controller: Degraded > Ready > Progressing > Failed.
func (r *ValkeyReconciler) updateStatus(ctx context.Context, v *valkeyiov1alpha1.Valkey) error {
	log := logf.FromContext(ctx)
	// Fetch the latest version to avoid conflicts from a stale resourceVersion.
	current := &valkeyiov1alpha1.Valkey{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(v), current); err != nil {
		return err
	}
	patchBase := current.DeepCopy()
	patch := client.MergeFrom(patchBase)

	current.Status.Conditions = v.Status.Conditions
	current.Status.Primary = v.Status.Primary
	// Replicas exclude the primary, and a standalone Valkey has none.
	current.Status.Replicas = 0
	current.Status.ReadyReplicas = 0
	current.Status.ObservedGeneration = v.Generation

	readyCondition := meta.FindStatusCondition(current.Status.Conditions, valkeyiov1alpha1.ConditionReady)
	progressingCondition := meta.FindStatusCondition(current.Status.Conditions, valkeyiov1alpha1.ConditionProgressing)
	degradedCondition := meta.FindStatusCondition(current.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)

	switch {
	case degradedCondition != nil && degradedCondition.Status == metav1.ConditionTrue:
		current.Status.State = valkeyiov1alpha1.ValkeyStateDegraded
		current.Status.Reason = degradedCondition.Reason
		current.Status.Message = degradedCondition.Message
	case readyCondition != nil && readyCondition.Status == metav1.ConditionTrue:
		current.Status.State = valkeyiov1alpha1.ValkeyStateReady
		current.Status.Reason = readyCondition.Reason
		current.Status.Message = readyCondition.Message
	case progressingCondition != nil && progressingCondition.Status == metav1.ConditionTrue:
		current.Status.State = valkeyiov1alpha1.ValkeyStateReconciling
		current.Status.Reason = progressingCondition.Reason
		current.Status.Message = progressingCondition.Message
	case readyCondition != nil && readyCondition.Status == metav1.ConditionFalse:
		current.Status.State = valkeyiov1alpha1.ValkeyStateFailed
		current.Status.Reason = readyCondition.Reason
		current.Status.Message = readyCondition.Message
	}

	if !reflect.DeepEqual(patchBase.Status, current.Status) {
		if err := r.Status().Patch(ctx, current, patch); err != nil {
			if !apierrors.IsNotFound(err) {
				log.Error(err, statusUpdateFailedMsg)
			}
			return err
		}
		log.V(1).Info("status updated", "state", current.Status.State, "reason", current.Status.Reason)
	}

	v.Status = current.Status
	v.ResourceVersion = current.ResourceVersion
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ValkeyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.APIReader = mgr.GetAPIReader()
	return ctrl.NewControllerManagedBy(mgr).
		For(&valkeyiov1alpha1.Valkey{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&valkeyiov1alpha1.ValkeyNode{}).
		Owns(&corev1.Secret{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Named("valkey").
		Complete(r)
}
