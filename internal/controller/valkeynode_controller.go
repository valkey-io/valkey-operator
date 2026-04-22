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
	"crypto/tls"
	"fmt"
	"strings"
	"time"

	vclient "github.com/valkey-io/valkey-go"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

const (
	// valkeyInfoRolePrefix is the key prefix in the INFO replication output.
	valkeyInfoRolePrefix             = "role:"
	persistentVolumeCleanupFinalizer = "valkey.io/persistent-volume-cleanup"
)

// ValkeyNodeReconciler reconciles a ValkeyNode object
type ValkeyNodeReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Recorder  events.EventRecorder
	APIReader client.Reader
}

// +kubebuilder:rbac:groups=valkey.io,resources=valkeynodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=valkey.io,resources=valkeynodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=valkey.io,resources=valkeynodes/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="apps",resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="apps",resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile moves the current state of the ValkeyNode closer to the desired state.
func (r *ValkeyNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.V(1).Info("reconciling ValkeyNode")

	node := &valkeyiov1alpha1.ValkeyNode{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !node.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, node)
	}
	if requeue, err := r.reconcilePersistenceFinalizer(ctx, node); err != nil {
		return ctrl.Result{}, err
	} else if requeue {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if err := r.ensureConfigMap(ctx, node); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensurePersistentVolumeClaim(ctx, node); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.ensureWorkload(ctx, node); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.updateStatus(ctx, node); err != nil {
		return ctrl.Result{}, err
	}

	if !node.Status.Ready {
		log.V(1).Info("ValkeyNode not ready, requeuing")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	log.V(1).Info("ValkeyNode reconciliation complete")
	// Requeue after 60 seconds to check on the ValkeyNode role.
	return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
}

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

func (r *ValkeyNodeReconciler) ensureWorkload(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) error {
	if node.Spec.Persistence != nil && node.Spec.WorkloadType == valkeyiov1alpha1.WorkloadTypeDeployment {
		return fmt.Errorf("persistence requires workloadType StatefulSet")
	}
	switch node.Spec.WorkloadType {
	case valkeyiov1alpha1.WorkloadTypeStatefulSet:
		return r.ensureStatefulSet(ctx, node)
	case valkeyiov1alpha1.WorkloadTypeDeployment:
		return r.ensureDeployment(ctx, node)
	default:
		return fmt.Errorf("unsupported workload type: %q", node.Spec.WorkloadType)
	}
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

// ensureStatefulSet creates or updates the StatefulSet for the ValkeyNode.
func (r *ValkeyNodeReconciler) ensureStatefulSet(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) error {
	log := logf.FromContext(ctx)
	desired, err := buildValkeyNodeStatefulSet(node)
	if err != nil {
		return err
	}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      desired.Name,
			Namespace: desired.Namespace,
		},
	}
	log.V(1).Info("getting internal secret", "node-labels", desired.Labels)
	aclSecretName := getInternalSecretName(desired.Labels[LabelCluster])
	aclSecret := &corev1.Secret{}
	err = r.Get(ctx, types.NamespacedName{
		Name:      aclSecretName,
		Namespace: desired.Namespace,
	}, aclSecret)
	if err != nil {
		return err
	}
	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		sts.Labels = desired.Labels
		sts.Spec = desired.Spec
		sts.Spec.Template.Annotations = map[string]string{
			hashAnnotationKey: aclSecret.Annotations[hashAnnotationKey],
		}
		return controllerutil.SetControllerReference(node, sts, r.Scheme)
	})
	if err != nil {
		return err
	}
	log.V(1).Info("reconciled StatefulSet", "result", result, "name", sts.Name)
	return nil
}

// ensureDeployment creates or updates the Deployment for the ValkeyNode.
func (r *ValkeyNodeReconciler) ensureDeployment(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) error {
	log := logf.FromContext(ctx)
	desired, err := buildValkeyNodeDeployment(node)
	if err != nil {
		return err
	}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      desired.Name,
			Namespace: desired.Namespace,
		},
	}
	log.V(1).Info("getting internal secret", "node-labels", desired.Labels)
	aclSecretName := getInternalSecretName(desired.Labels[LabelCluster])
	aclSecret := &corev1.Secret{}
	err = r.Get(ctx, types.NamespacedName{
		Name:      aclSecretName,
		Namespace: desired.Namespace,
	}, aclSecret)
	if err != nil {
		return err
	}

	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = desired.Labels
		dep.Spec = desired.Spec
		dep.Spec.Template.Annotations = map[string]string{
			hashAnnotationKey: aclSecret.Annotations[hashAnnotationKey],
		}
		return controllerutil.SetControllerReference(node, dep, r.Scheme)
	})
	if err != nil {
		return err
	}
	log.V(1).Info("reconciled Deployment", "result", result, "name", dep.Name)
	return nil
}

// ensureConfigMap creates or updates the ConfigMap for the ValkeyNode.
// If ServerConfigMapName is set, the ConfigMap is assumed to
// be managed externally and this step is skipped.
func (r *ValkeyNodeReconciler) ensureConfigMap(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) error {
	log := logf.FromContext(ctx)
	if node.Spec.ServerConfigMapName != "" {
		// ConfigMap is provided externally (e.g. by ValkeyCluster), skip creation.
		return nil
	}
	desired, err := buildValkeyNodeConfigMap(node)
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      desired.Name,
			Namespace: desired.Namespace,
		},
	}
	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = desired.Labels
		cm.Data = desired.Data
		return controllerutil.SetControllerReference(node, cm, r.Scheme)
	})
	if err != nil {
		return err
	}
	log.V(1).Info("reconciled ConfigMap", "result", result, "name", cm.Name)
	return nil
}

// updateStatus updates the ValkeyNode status based on workload and Pod state.
func (r *ValkeyNodeReconciler) updateStatus(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) error {
	log := logf.FromContext(ctx)

	current := &valkeyiov1alpha1.ValkeyNode{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(node), current); err != nil {
		return err
	}

	// Snapshot status before mutations so we can skip the write if nothing changed.
	previous := current.Status.DeepCopy()

	// Always stamp the observed generation so ValkeyCluster can detect
	// whether the controller has processed the latest spec.
	current.Status.ObservedGeneration = current.Generation

	pvc, err := r.getPersistentVolumeClaim(ctx, node)
	if err != nil {
		return err
	}
	if node.Spec.Persistence != nil {
		pvcStatus, pvcReason, pvcMessage := pvcStatusCondition(pvc)
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type:               valkeyiov1alpha1.ValkeyNodeConditionPersistentVolumeClaimReady,
			Status:             pvcStatus,
			Reason:             pvcReason,
			Message:            pvcMessage,
			ObservedGeneration: current.Generation,
		})
		pvcSizeStatus, pvcSizeReason, pvcSizeMessage := pvcSizeStatusCondition(current, pvc)
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type:               valkeyiov1alpha1.ValkeyNodeConditionPersistentVolumeClaimSizeReady,
			Status:             pvcSizeStatus,
			Reason:             pvcSizeReason,
			Message:            pvcSizeMessage,
			ObservedGeneration: current.Generation,
		})
	} else {
		meta.RemoveStatusCondition(&current.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionPersistentVolumeClaimReady)
		meta.RemoveStatusCondition(&current.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionPersistentVolumeClaimSizeReady)
	}

	pod, err := r.getPod(ctx, node)
	if err != nil {
		return err
	}

	if pod == nil {
		current.Status.Ready = false
		current.Status.PodName = ""
		current.Status.PodIP = ""
		reason := valkeyiov1alpha1.ValkeyNodeReasonPodNotReady
		message := "Pod does not exist yet"
		if node.Spec.Persistence != nil {
			_, reason, message = pvcStatusCondition(pvc)
		}
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type:               valkeyiov1alpha1.ValkeyNodeConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: current.Generation,
		})
	} else {
		current.Status.PodName = pod.Name
		current.Status.PodIP = pod.Status.PodIP

		podReady := false
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				podReady = true
				break
			}
		}

		// If the pod appears ready, also verify the workload rollout has completed.
		// The old pod may still be running (and ready) while the StatefulSet is rolling
		// to a new spec; we must not report Ready=true until the rollout is done so the
		// ValkeyCluster controller waits before advancing to the next node.
		if podReady {
			rolled, err := r.isWorkloadRolledOut(ctx, node)
			if err != nil {
				return err
			}
			podReady = rolled
		}

		current.Status.Ready = podReady
		if podReady {
			current.Status.Role = r.getValkeyRole(ctx, current)
			meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
				Type:               valkeyiov1alpha1.ValkeyNodeConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             valkeyiov1alpha1.ValkeyNodeReasonPodRunning,
				Message:            "Pod is running and ready",
				ObservedGeneration: current.Generation,
			})
		} else {
			reason := valkeyiov1alpha1.ValkeyNodeReasonPodNotReady
			message := "Pod is not ready"
			if node.Spec.Persistence != nil {
				if pvcStatus, pvcReason, pvcMessage := pvcStatusCondition(pvc); pvcStatus != metav1.ConditionTrue {
					reason = pvcReason
					message = pvcMessage
				}
			}
			meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
				Type:               valkeyiov1alpha1.ValkeyNodeConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             reason,
				Message:            message,
				ObservedGeneration: current.Generation,
			})
		}
	}

	if nodeStatusChanged(*previous, current.Status) {
		if err := r.Status().Update(ctx, current); err != nil {
			log.Error(err, "failed to update ValkeyNode status")
			return err
		}
		log.V(1).Info("status updated", "ready", current.Status.Ready, "role", current.Status.Role)
	} else {
		log.V(2).Info("status unchanged, skipping update")
	}

	// Sync Ready back to the caller's object so the requeue check in Reconcile
	// reflects the status we just wrote.
	node.Status.Ready = current.Status.Ready

	return nil
}

// isWorkloadRolledOut returns true if the workload (StatefulSet or Deployment)
// has fully rolled out to the current spec — all pods are on the latest revision
// and ready. The pod's own Ready condition is not sufficient: the old pod may
// still be running while the StatefulSet/Deployment is rolling to a new spec.
//
// The check uses two gates for StatefulSets:
//  1. status.observedGeneration >= metadata.generation — the STS controller has
//     processed the latest spec (and computed the new updateRevision).
//  2. status.currentRevision == status.updateRevision — all pods are on the
//     new revision (the rolling update has completed).
func (r *ValkeyNodeReconciler) isWorkloadRolledOut(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) (bool, error) {
	// Use APIReader (direct API server read) when available so we always see the
	// latest metadata.generation, bypassing the informer cache. Without this, the
	// same reconcile that patches the STS spec would read a stale cached object
	// where ObservedGeneration == Generation (both old) and
	// currentRevision == updateRevision (both old), causing isWorkloadRolledOut
	// to incorrectly return true before the STS controller has processed the change.
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}

	switch node.Spec.WorkloadType {
	case valkeyiov1alpha1.WorkloadTypeStatefulSet:
		sts := &appsv1.StatefulSet{}
		if err := reader.Get(ctx, client.ObjectKey{Name: valkeyNodeResourceName(node), Namespace: node.Namespace}, sts); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		// Gate 1: STS controller hasn't processed the latest spec change yet.
		if sts.Status.ObservedGeneration < sts.Generation {
			return false, nil
		}
		// Gate 2: rolling update not yet complete.
		return sts.Status.CurrentRevision == sts.Status.UpdateRevision && sts.Status.ReadyReplicas >= 1, nil
	case valkeyiov1alpha1.WorkloadTypeDeployment:
		dep := &appsv1.Deployment{}
		if err := reader.Get(ctx, client.ObjectKey{Name: valkeyNodeResourceName(node), Namespace: node.Namespace}, dep); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		if dep.Status.ObservedGeneration < dep.Generation {
			return false, nil
		}
		replicas := int32(1)
		if dep.Spec.Replicas != nil {
			replicas = *dep.Spec.Replicas
		}
		return dep.Status.UpdatedReplicas >= replicas && dep.Status.ReadyReplicas >= replicas, nil
	default:
		return false, nil
	}
}

// getPod returns the pod for a ValkeyNode by listing with label selector.
func (r *ValkeyNodeReconciler) getPod(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) (*corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(node.Namespace),
		client.MatchingLabels(valkeyNodeLabels(node))); err != nil {
		return nil, fmt.Errorf("listing pods for ValkeyNode %s: %w", node.Name, err)
	}
	if len(podList.Items) > 0 {
		return &podList.Items[0], nil
	}
	return nil, nil
}

// getValkeyRole connects to a Valkey pod and returns its replication role
// ("primary" or "replica"). Returns an empty string if the role cannot be determined.
func (r *ValkeyNodeReconciler) getValkeyRole(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) string {
	var tlsConfig *tls.Config
	if node.Spec.TLS != nil && node.Spec.TLS.Certificate.SecretName != "" {
		secretName := node.Spec.TLS.Certificate.SecretName
		serverName := ""
		if clusterName, ok := node.Labels[LabelCluster]; ok {
			serverName = fmt.Sprintf("%s.%s.svc.cluster.local", clusterName, node.Namespace)
		}

		cfg, err := getTLSConfig(ctx, r.Client, secretName, serverName, node.Namespace)
		if err == nil {
			tlsConfig = cfg
		}
	}

	opt := vclient.ClientOption{
		InitAddress:       []string{fmt.Sprintf("%s:%d", node.Status.PodIP, DefaultPort)},
		ForceSingleClient: true, // Don't connect to another cluster node.
		TLSConfig:         tlsConfig,
	}

	c, err := vclient.NewClient(opt)
	if err != nil {
		return ""
	}
	defer c.Close()

	info, err := c.Do(ctx, c.B().Info().Section("replication").Build()).ToString()
	if err != nil {
		return ""
	}

	return parseValkeyRole(info)
}

// parseValkeyRole extracts the replication role from the output of INFO replication,
// mapping Valkey's internal terms ("master"/"slave") to user-friendly ones ("primary"/"replica").
func parseValkeyRole(info string) string {
	for line := range strings.SplitSeq(info, "\n") {
		line = strings.TrimSpace(line)
		if value, ok := strings.CutPrefix(line, valkeyInfoRolePrefix); ok {
			switch value {
			case RoleMaster:
				return RolePrimary
			case RoleSlave:
				return RoleReplica
			}
		}
	}
	return ""
}

// SetupWithManager sets up the controller with the Manager.
func (r *ValkeyNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.APIReader = mgr.GetAPIReader()
	return ctrl.NewControllerManagedBy(mgr).
		For(&valkeyiov1alpha1.ValkeyNode{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Named("valkeynode").
		Complete(r)
}
