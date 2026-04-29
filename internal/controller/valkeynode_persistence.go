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
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

const persistentVolumeCleanupFinalizer = "valkey.io/persistent-volume-cleanup"

func persistenceReclaimPolicy(node *valkeyiov1alpha1.ValkeyNode) valkeyiov1alpha1.PersistenceReclaimPolicy {
	if node.Spec.Persistence == nil || node.Spec.Persistence.ReclaimPolicy == "" {
		return valkeyiov1alpha1.PersistenceReclaimPolicyRetain
	}
	return node.Spec.Persistence.ReclaimPolicy
}

func (r *ValkeyNodeReconciler) reconcilePersistenceFinalizer(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) (bool, error) {
	shouldHaveFinalizer := node.Spec.Persistence != nil && persistenceReclaimPolicy(node) == valkeyiov1alpha1.PersistenceReclaimPolicyDelete
	hasFinalizer := controllerutil.ContainsFinalizer(node, persistentVolumeCleanupFinalizer)

	switch {
	case shouldHaveFinalizer && !hasFinalizer:
		controllerutil.AddFinalizer(node, persistentVolumeCleanupFinalizer)
		return true, r.Update(ctx, node)
	case !shouldHaveFinalizer && hasFinalizer:
		controllerutil.RemoveFinalizer(node, persistentVolumeCleanupFinalizer)
		return true, r.Update(ctx, node)
	default:
		return false, nil
	}
}

func (r *ValkeyNodeReconciler) reconcileDeletion(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(node, persistentVolumeCleanupFinalizer) {
		return ctrl.Result{}, nil
	}

	if node.Spec.Persistence == nil || persistenceReclaimPolicy(node) != valkeyiov1alpha1.PersistenceReclaimPolicyDelete {
		controllerutil.RemoveFinalizer(node, persistentVolumeCleanupFinalizer)
		return ctrl.Result{}, r.Update(ctx, node)
	}

	if err := r.deleteWorkload(ctx, node); err != nil {
		return ctrl.Result{}, err
	}

	pod, err := r.getPod(ctx, node)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pod != nil {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	pvc, err := r.getPersistentVolumeClaim(ctx, node)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pvc != nil {
		if pvc.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	controllerutil.RemoveFinalizer(node, persistentVolumeCleanupFinalizer)
	return ctrl.Result{}, r.Update(ctx, node)
}

func (r *ValkeyNodeReconciler) ensurePersistentVolumeClaim(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) error {
	log := logf.FromContext(ctx)
	if node.Spec.Persistence == nil {
		return nil
	}

	desired := buildValkeyNodePVC(node)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      desired.Name,
			Namespace: desired.Namespace,
		},
	}

	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, pvc, func() error {
		pvc.Labels = desired.Labels

		if pvc.CreationTimestamp.IsZero() {
			pvc.Spec = desired.Spec
			return nil
		}

		// Only patch storage requests after the claim is bound. Kubernetes
		// rejects storage request mutations on pending/unbound claims.
		if pvc.Status.Phase != corev1.ClaimBound {
			return nil
		}

		if pvc.Spec.Resources.Requests == nil {
			pvc.Spec.Resources.Requests = corev1.ResourceList{}
		}

		currentRequest, hasCurrent := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		desiredRequest := desired.Spec.Resources.Requests[corev1.ResourceStorage]
		if hasCurrent && desiredRequest.Cmp(currentRequest) < 0 {
			log.V(1).Info("ignoring PVC shrink request", "name", pvc.Name, "current", currentRequest.String(), "desired", desiredRequest.String())
			return nil
		}
		pvc.Spec.Resources.Requests[corev1.ResourceStorage] = desiredRequest
		return nil
	})
	if err != nil {
		return err
	}

	log.V(1).Info("reconciled PersistentVolumeClaim", "result", result, "name", pvc.Name)
	return nil
}

func (r *ValkeyNodeReconciler) getPersistentVolumeClaim(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) (*corev1.PersistentVolumeClaim, error) {
	if node.Spec.Persistence == nil {
		return nil, nil
	}

	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{Name: valkeyNodePVCName(node), Namespace: node.Namespace}, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return pvc, nil
}

func pvcStatusCondition(pvc *corev1.PersistentVolumeClaim) (metav1.ConditionStatus, string, string) {
	if pvc == nil {
		return metav1.ConditionFalse,
			valkeyiov1alpha1.ValkeyNodeReasonPersistentVolumeClaimPending,
			"PersistentVolumeClaim does not exist yet"
	}
	if pvc.Status.Phase != corev1.ClaimBound {
		phase := pvc.Status.Phase
		if phase == "" {
			phase = corev1.ClaimPending
		}
		return metav1.ConditionFalse,
			valkeyiov1alpha1.ValkeyNodeReasonPersistentVolumeClaimPending,
			fmt.Sprintf("PersistentVolumeClaim %s is %s", pvc.Name, phase)
	}
	return metav1.ConditionTrue,
		valkeyiov1alpha1.ValkeyNodeReasonPersistentVolumeClaimBound,
		fmt.Sprintf("PersistentVolumeClaim %s is bound", pvc.Name)
}

func pvcSizeStatusCondition(node *valkeyiov1alpha1.ValkeyNode, pvc *corev1.PersistentVolumeClaim) (metav1.ConditionStatus, string, string) {
	if node.Spec.Persistence == nil {
		return metav1.ConditionTrue, "", ""
	}
	if pvc == nil {
		return metav1.ConditionFalse,
			valkeyiov1alpha1.ValkeyNodeReasonPersistentVolumeClaimResizePending,
			"PersistentVolumeClaim does not exist yet"
	}
	if pvc.Status.Phase != corev1.ClaimBound {
		phase := pvc.Status.Phase
		if phase == "" {
			phase = corev1.ClaimPending
		}
		return metav1.ConditionFalse,
			valkeyiov1alpha1.ValkeyNodeReasonPersistentVolumeClaimResizePending,
			fmt.Sprintf("PersistentVolumeClaim %s is %s before size reconciliation can complete", pvc.Name, phase)
	}

	for _, cond := range pvc.Status.Conditions {
		switch cond.Type {
		case corev1.PersistentVolumeClaimControllerResizeError, corev1.PersistentVolumeClaimNodeResizeError:
			msg := cond.Message
			if msg == "" {
				msg = fmt.Sprintf("PersistentVolumeClaim %s resize failed", pvc.Name)
			}
			return metav1.ConditionFalse, valkeyiov1alpha1.ValkeyNodeReasonPersistentVolumeClaimResizeInfeasible, msg
		case corev1.PersistentVolumeClaimResizing:
			msg := cond.Message
			if msg == "" {
				msg = fmt.Sprintf("PersistentVolumeClaim %s resize is in progress", pvc.Name)
			}
			return metav1.ConditionFalse, valkeyiov1alpha1.ValkeyNodeReasonPersistentVolumeClaimResizeInProgress, msg
		case corev1.PersistentVolumeClaimFileSystemResizePending:
			msg := cond.Message
			if msg == "" {
				msg = fmt.Sprintf("PersistentVolumeClaim %s is waiting for filesystem resize", pvc.Name)
			}
			return metav1.ConditionFalse, valkeyiov1alpha1.ValkeyNodeReasonPersistentVolumeClaimResizePending, msg
		}
	}

	switch pvc.Status.AllocatedResourceStatuses[corev1.ResourceStorage] {
	case corev1.PersistentVolumeClaimControllerResizeInfeasible, corev1.PersistentVolumeClaimNodeResizeInfeasible:
		return metav1.ConditionFalse,
			valkeyiov1alpha1.ValkeyNodeReasonPersistentVolumeClaimResizeInfeasible,
			fmt.Sprintf("PersistentVolumeClaim %s resize cannot be satisfied", pvc.Name)
	case corev1.PersistentVolumeClaimControllerResizeInProgress, corev1.PersistentVolumeClaimNodeResizeInProgress:
		return metav1.ConditionFalse,
			valkeyiov1alpha1.ValkeyNodeReasonPersistentVolumeClaimResizeInProgress,
			fmt.Sprintf("PersistentVolumeClaim %s resize is in progress", pvc.Name)
	case corev1.PersistentVolumeClaimNodeResizePending:
		return metav1.ConditionFalse,
			valkeyiov1alpha1.ValkeyNodeReasonPersistentVolumeClaimResizePending,
			fmt.Sprintf("PersistentVolumeClaim %s is waiting for node-side filesystem resize", pvc.Name)
	}

	currentCapacity, ok := pvc.Status.Capacity[corev1.ResourceStorage]
	if !ok {
		return metav1.ConditionFalse,
			valkeyiov1alpha1.ValkeyNodeReasonPersistentVolumeClaimResizePending,
			fmt.Sprintf("PersistentVolumeClaim %s has no reported storage capacity yet", pvc.Name)
	}
	if currentCapacity.Cmp(node.Spec.Persistence.Size) < 0 {
		return metav1.ConditionFalse,
			valkeyiov1alpha1.ValkeyNodeReasonPersistentVolumeClaimResizePending,
			fmt.Sprintf("PersistentVolumeClaim %s requested %s but current capacity is %s", pvc.Name, node.Spec.Persistence.Size.String(), currentCapacity.String())
	}

	return metav1.ConditionTrue,
		valkeyiov1alpha1.ValkeyNodeReasonPersistentVolumeClaimSizeSatisfied,
		fmt.Sprintf("PersistentVolumeClaim %s satisfies the requested size %s", pvc.Name, node.Spec.Persistence.Size.String())
}

func (r *ValkeyNodeReconciler) deleteWorkload(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) error {
	key := client.ObjectKey{Name: valkeyNodeResourceName(node), Namespace: node.Namespace}
	switch node.Spec.WorkloadType {
	case valkeyiov1alpha1.WorkloadTypeStatefulSet:
		sts := &appsv1.StatefulSet{}
		if err := r.Get(ctx, key, sts); err != nil {
			return client.IgnoreNotFound(err)
		}
		return client.IgnoreNotFound(r.Delete(ctx, sts))
	case valkeyiov1alpha1.WorkloadTypeDeployment:
		dep := &appsv1.Deployment{}
		if err := r.Get(ctx, key, dep); err != nil {
			return client.IgnoreNotFound(err)
		}
		return client.IgnoreNotFound(r.Delete(ctx, dep))
	default:
		return nil
	}
}
