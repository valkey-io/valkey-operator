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
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	vclient "github.com/valkey-io/valkey-go"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

const (
	// valkeyInfoRolePrefix is the key prefix in the INFO replication output.
	valkeyInfoRolePrefix = "role:"
)

// valkeyConfigClient is the subset of Valkey operations the ValkeyNode
// controller needs to apply config live. An interface so tests can inject a
// fake (envtest has no running Valkey server).
type valkeyConfigClient interface {
	SetConfig(ctx context.Context, params map[string]string) error
	Close()
}

// realValkeyConfigClient applies CONFIG SET over a real valkey-go connection.
type realValkeyConfigClient struct {
	client vclient.Client
}

func (rc *realValkeyConfigClient) SetConfig(ctx context.Context, params map[string]string) error {
	cmd := rc.client.B().ConfigSet().ParameterValue()
	for _, param := range slices.Sorted(maps.Keys(params)) {
		cmd = cmd.ParameterValue(param, params[param])
	}
	if err := rc.client.Do(ctx, cmd.Build()).Error(); err != nil {
		return fmt.Errorf("CONFIG SET: %w", err)
	}
	return nil
}

func (rc *realValkeyConfigClient) Close() { rc.client.Close() }

// realConfigClient opens a real Valkey connection to the node's pod.
func realConfigClient(ctx context.Context, r *ValkeyNodeReconciler, node *valkeyiov1alpha1.ValkeyNode) (valkeyConfigClient, error) {
	c, err := vclient.NewClient(r.buildNodeClientOption(ctx, node))
	if err != nil {
		return nil, err
	}
	return &realValkeyConfigClient{client: c}, nil
}

// ValkeyNodeReconciler reconciles a ValkeyNode object
type ValkeyNodeReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Recorder  events.EventRecorder
	APIReader client.Reader
	// newConfigClient opens a Valkey client to a node's pod for live config
	// application. SetupWithManager defaults it to realConfigClient; tests
	// override it with a fake.
	newConfigClient func(ctx context.Context, r *ValkeyNodeReconciler, node *valkeyiov1alpha1.ValkeyNode) (valkeyConfigClient, error)
	// resolveRoleFunc, when set, overrides how resolveRole reads a node's live
	// replication role. Tests set it to a fake (envtest has no running Valkey
	// server); when nil, resolveRole connects to the pod directly.
	resolveRoleFunc func(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) string
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
		r.setReadyCondition(ctx, node, "ConfigMapError", err.Error())
		return ctrl.Result{}, err
	}
	if err := r.ensurePersistentVolumeClaim(ctx, node); err != nil {
		r.setReadyCondition(ctx, node, "PersistentVolumeClaimError", err.Error())
		return ctrl.Result{}, err
	}

	if err := r.ensureWorkload(ctx, node); err != nil {
		workloadReason := "WorkloadError"
		switch node.Spec.WorkloadType {
		case valkeyiov1alpha1.WorkloadTypeStatefulSet:
			workloadReason = "StatefulSetError"
		case valkeyiov1alpha1.WorkloadTypeDeployment:
			workloadReason = "DeploymentError"
		}
		r.setReadyCondition(ctx, node, workloadReason, err.Error())
		return ctrl.Result{}, err
	}

	roleResolveFailed, err := r.updateStatus(ctx, node)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !node.Status.Ready {
		log.V(1).Info("ValkeyNode not ready, requeuing")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	applied, err := r.applyLiveConfig(ctx, node)
	if err != nil {
		log.Error(err, "failed to apply live config")
		r.Recorder.Eventf(node, nil, corev1.EventTypeWarning, "LiveConfigApplyFailed", "ApplyLiveConfig", "Failed to apply live config: %v", err)
		if condErr := r.setLiveConfigCondition(ctx, node, metav1.ConditionFalse, "ApplyFailed", err.Error()); condErr != nil {
			log.Error(condErr, "failed to set LiveConfigApplied condition")
		}
		return ctrl.Result{}, err
	}
	if applied {
		if condErr := r.setLiveConfigCondition(ctx, node, metav1.ConditionTrue, "Applied", "Live config applied"); condErr != nil {
			log.Error(condErr, "failed to set LiveConfigApplied condition")
			return ctrl.Result{}, condErr
		}
	} else {
		// Nothing to apply (no allowlisted keys in spec.config). Clear any
		// stale condition so it reverts to absent, which the cluster
		// controller treats the same as True. Without this, a prior False
		// (e.g. from a CONFIG SET failure) would persist after the offending
		// key is removed and block cluster progress indefinitely.
		if condErr := r.clearLiveConfigCondition(ctx, node); condErr != nil {
			log.Error(condErr, "failed to clear LiveConfigApplied condition")
			return ctrl.Result{}, condErr
		}
	}

	log.V(1).Info("ValkeyNode reconciliation complete")
	// Backstop requeue: a self-heal net, not the primary mechanism (events do
	// the real work). Retry sooner when a ready pod's role could not be resolved.
	requeueAfter := 30 * time.Second
	if roleResolveFailed {
		requeueAfter = 10 * time.Second
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *ValkeyNodeReconciler) setLiveConfigCondition(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode, status metav1.ConditionStatus, reason, message string) error {
	current := &valkeyiov1alpha1.ValkeyNode{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(node), current); err != nil {
		return fmt.Errorf("get ValkeyNode: %w", err)
	}
	patchBase := current.DeepCopy()
	if !meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type:               valkeyiov1alpha1.ValkeyNodeConditionLiveConfigApplied,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: current.Generation,
	}) {
		return nil
	}
	if err := r.Status().Patch(ctx, current, client.MergeFrom(patchBase)); err != nil {
		return fmt.Errorf("patch LiveConfigApplied condition: %w", err)
	}
	return nil
}

// clearLiveConfigCondition removes the LiveConfigApplied condition if present.
// An absent condition is treated as True by the cluster controller, so this is
// the correct resting state when there are no allowlisted keys to apply. It
// no-ops (no patch) when the condition is already absent.
func (r *ValkeyNodeReconciler) clearLiveConfigCondition(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) error {
	current := &valkeyiov1alpha1.ValkeyNode{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(node), current); err != nil {
		return fmt.Errorf("get ValkeyNode: %w", err)
	}
	patchBase := current.DeepCopy()
	if !meta.RemoveStatusCondition(&current.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionLiveConfigApplied) {
		return nil
	}
	if err := r.Status().Patch(ctx, current, client.MergeFrom(patchBase)); err != nil {
		return fmt.Errorf("patch LiveConfigApplied condition: %w", err)
	}
	return nil
}

// setReadyCondition sets the Ready condition to False on the ValkeyNode status
// so that errors from early reconcile stages (ConfigMap, PVC, workload creation)
// are visible on the resource.
func (r *ValkeyNodeReconciler) setReadyCondition(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode, reason, message string) {
	log := logf.FromContext(ctx)
	current := &valkeyiov1alpha1.ValkeyNode{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(node), current); err != nil {
		log.Error(err, "failed to get ValkeyNode for status update")
		return
	}
	patchBase := current.DeepCopy()
	if !meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type:               valkeyiov1alpha1.ValkeyNodeConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: current.Generation,
	}) {
		return
	}
	current.Status.Ready = false
	if err := r.Status().Patch(ctx, current, client.MergeFrom(patchBase)); err != nil {
		log.Error(err, "failed to patch ValkeyNode Ready condition")
	}
}

func (r *ValkeyNodeReconciler) ensureWorkload(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) error {
	switch node.Spec.WorkloadType {
	case valkeyiov1alpha1.WorkloadTypeStatefulSet:
		return r.ensureStatefulSet(ctx, node)
	case valkeyiov1alpha1.WorkloadTypeDeployment:
		return r.ensureDeployment(ctx, node)
	default:
		return fmt.Errorf("unsupported workload type: %q", node.Spec.WorkloadType)
	}
}

// buildPodTemplateAnnotations assembles the annotations that must be present on
// the pod template spec to trigger rolling updates when the ACL secret or the
// server config changes.
func buildPodTemplateAnnotations(node *valkeyiov1alpha1.ValkeyNode, aclSecret *corev1.Secret) map[string]string {
	annotations := map[string]string{
		hashAnnotationKey: aclSecret.Annotations[hashAnnotationKey],
	}
	if node.Spec.ServerConfigHash != "" {
		annotations[configHashKey] = node.Spec.ServerConfigHash
	}
	return annotations
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
		sts.Spec.Template.Annotations = buildPodTemplateAnnotations(node, aclSecret)
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
		dep.Spec.Template.Annotations = buildPodTemplateAnnotations(node, aclSecret)
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
func (r *ValkeyNodeReconciler) updateStatus(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) (roleResolveFailed bool, err error) {
	log := logf.FromContext(ctx)

	current := &valkeyiov1alpha1.ValkeyNode{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(node), current); err != nil {
		return false, err
	}

	// Snapshot for patch base before mutations.
	patchBase := current.DeepCopy()
	patch := client.MergeFrom(patchBase)

	// Always stamp the observed generation so ValkeyCluster can detect
	// whether the controller has processed the latest spec.
	current.Status.ObservedGeneration = current.Generation

	pvc, err := r.getPersistentVolumeClaim(ctx, node)
	if err != nil {
		return false, err
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
		return false, err
	}

	if pod == nil {
		current.Status.Ready = false
		current.Status.PodName = ""
		current.Status.PodIP = ""
		current.Status.Role = ""
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
				return false, err
			}
			podReady = rolled
		}

		current.Status.Ready = podReady
		if podReady {
			if role := r.resolveRole(ctx, current); role != "" {
				current.Status.Role = role
			} else {
				// Transient INFO failure on a ready pod: keep the last-known
				// role rather than blanking a healthy node, and signal a short
				// requeue so we retry sooner than the backstop.
				roleResolveFailed = true
			}
			meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
				Type:               valkeyiov1alpha1.ValkeyNodeConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             valkeyiov1alpha1.ValkeyNodeReasonPodRunning,
				Message:            "Pod is running and ready",
				ObservedGeneration: current.Generation,
			})
		} else {
			current.Status.Role = ""
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

	if !reflect.DeepEqual(patchBase.Status, current.Status) {
		if err := r.Status().Patch(ctx, current, patch); err != nil {
			log.Error(err, "failed to update ValkeyNode status")
			return false, err
		}
		log.V(1).Info("status updated", "ready", current.Status.Ready, "role", current.Status.Role)
	} else {
		log.V(2).Info("status unchanged, skipping update")
	}

	// Sync status fields back to the caller's object so Reconcile uses the
	// values just written: Ready gates the requeue, PodIP is used by applyLiveConfig.
	node.Status.Ready = current.Status.Ready
	node.Status.PodIP = current.Status.PodIP

	return roleResolveFailed, nil
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

// buildNodeClientOption builds the valkey-go client option for connecting to a
// node's pod, on a best-effort basis (TLS and operator credentials are applied
// when available). Shared by resolveRole and the live-config client.
func (r *ValkeyNodeReconciler) buildNodeClientOption(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) vclient.ClientOption {
	var tlsConfig *tls.Config
	if node.Spec.TLS != nil && node.Spec.TLS.Certificate.SecretName != "" {
		secretName := node.Spec.TLS.Certificate.SecretName
		serverName := ""
		if clusterName, ok := node.Labels[LabelCluster]; ok {
			serverName = fmt.Sprintf("%s.%s.svc.cluster.local", headlessServiceName(clusterName), node.Namespace)
		}
		if cfg, err := getTLSConfig(ctx, r.APIReader, secretName, serverName, node.Namespace); err == nil {
			tlsConfig = cfg
		}
	}

	var username, operatorPassword string
	if clusterName, ok := node.Labels[LabelCluster]; ok {
		operatorPassword, _ = fetchSystemUserPassword(ctx, operatorUser, r.Client, clusterName, node.Namespace)
		if operatorPassword != "" {
			username = operatorUser
		}
	}

	return vclient.ClientOption{
		InitAddress:       []string{fmt.Sprintf("%s:%d", node.Status.PodIP, DefaultPort)},
		ForceSingleClient: true,
		TLSConfig:         tlsConfig,
		Username:          username,
		Password:          operatorPassword,
	}
}

// resolveRole returns the node's live replication role ("primary" or "replica"),
// or "" if it cannot be determined. Tests inject resolveRoleFunc to bypass the
// live Valkey connection (envtest has no running Valkey server).
//
// In cluster mode the role is derived from slot ownership (CLUSTER NODES), not
// from INFO replication: a just-restarted replica reports role:master for a few
// seconds before it re-establishes replication with its primary, whereas it
// never owns hash slots. A primary always owns slots. Standalone nodes (cluster
// disabled) have no slots, so they fall back to INFO replication directly.
func (r *ValkeyNodeReconciler) resolveRole(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) string {
	if r.resolveRoleFunc != nil {
		return r.resolveRoleFunc(ctx, node)
	}

	log := logf.FromContext(ctx)
	c, err := vclient.NewClient(r.buildNodeClientOption(ctx, node))
	if err != nil {
		log.Error(err, "failed to create valkey client")
		return ""
	}
	defer c.Close()

	info, err := c.Do(ctx, c.B().Info().Build()).ToString()
	if err != nil {
		log.Error(err, "failed to get info")
		return ""
	}

	if !parseClusterEnabled(info) {
		return parseValkeyRole(info)
	}

	nodes, err := c.Do(ctx, c.B().ClusterNodes().Build()).ToString()
	if err != nil {
		log.Error(err, "failed to get cluster nodes")
		return ""
	}
	return parseClusterNodesRole(nodes)
}

// applyLiveConfig applies the live-settable subset of the node's desired config
// via CONFIG SET. Returns (true, nil) when CONFIG SET succeeds, (false, nil)
// when there is nothing to apply, and (false, err) on failure.
func (r *ValkeyNodeReconciler) applyLiveConfig(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) (bool, error) {
	params := liveConfigToApply(node.Spec.Config)
	if len(params) == 0 {
		return false, nil
	}

	c, err := r.newConfigClient(ctx, r, node)
	if err != nil {
		return false, err
	}
	defer c.Close()

	if err := c.SetConfig(ctx, params); err != nil {
		return false, err
	}
	return true, nil
}

// parseClusterEnabled reports whether the node runs in cluster mode, from the
// "cluster_enabled:1" line of the INFO Cluster section.
func parseClusterEnabled(info string) bool {
	for line := range strings.SplitSeq(info, "\n") {
		if strings.TrimSpace(line) == "cluster_enabled:1" {
			return true
		}
	}
	return false
}

// parseClusterNodesRole determines a node's role from the "myself" line of
// CLUSTER NODES output by slot ownership: a node that owns at least one slot is
// the primary of its shard, otherwise it is a replica. This is reliable across a
// replica restart, where INFO replication (and the myself,master flag)
// transiently report master before replication re-establishes, but no slots are
// ever owned. Returns "" if there is no myself line.
//
// CLUSTER NODES line layout:
// <id> <ip:port@cport> <flags> <master> <ping> <pong> <epoch> <link-state> <slot>...
func parseClusterNodesRole(clusterNodes string) string {
	for line := range strings.SplitSeq(clusterNodes, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		flags := strings.Split(fields[2], ",")
		if !slices.Contains(flags, "myself") {
			continue
		}
		if slices.Contains(flags, "master") && len(fields) > 8 {
			return RolePrimary
		}
		return RoleReplica
	}
	return ""
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

// podToValkeyNode maps a Pod to a reconcile request for its owning ValkeyNode,
// reconstructing the node name from the pod's cluster/shard/node-index labels.
// Returns no request when the labels are absent or non-numeric, so non-valkey
// pods (already excluded by the managed-by cache scope) are ignored.
func (r *ValkeyNodeReconciler) podToValkeyNode(_ context.Context, obj client.Object) []ctrl.Request {
	labels := obj.GetLabels()
	cluster := labels[LabelCluster]
	shardStr := labels[LabelShardIndex]
	nodeStr := labels[LabelNodeIndex]
	if cluster == "" || shardStr == "" || nodeStr == "" {
		return nil
	}
	shardIndex, err := strconv.Atoi(shardStr)
	if err != nil {
		return nil
	}
	nodeIndex, err := strconv.Atoi(nodeStr)
	if err != nil {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{
		Name:      valkeyNodeName(cluster, shardIndex, nodeIndex),
		Namespace: obj.GetNamespace(),
	}}}
}

// podReady reports whether the pod's PodReady condition is True.
func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// valkeyPodPredicate admits only pod transitions that can affect a node's role
// or readiness: a PodReady flip, a PodIP change, or a phase change. Create and
// delete pass through (unset predicate funcs default to true). Everything else
// — the frequent kubelet status churn — is dropped to avoid a reconcile storm.
func valkeyPodPredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPod, ok1 := e.ObjectOld.(*corev1.Pod)
			newPod, ok2 := e.ObjectNew.(*corev1.Pod)
			if !ok1 || !ok2 {
				return false
			}
			return podReady(oldPod) != podReady(newPod) ||
				oldPod.Status.PodIP != newPod.Status.PodIP ||
				oldPod.Status.Phase != newPod.Status.Phase
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ValkeyNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.APIReader = mgr.GetAPIReader()
	if r.newConfigClient == nil {
		r.newConfigClient = realConfigClient
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&valkeyiov1alpha1.ValkeyNode{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.podToValkeyNode),
			builder.WithPredicates(valkeyPodPredicate()),
		).
		Named("valkeynode").
		Complete(r)
}
