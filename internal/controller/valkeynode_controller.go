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
	"slices"
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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

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
// Status conditions are mutated in memory and flushed by a single deferred patch.
func (r *ValkeyNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	log := logf.FromContext(ctx)
	log.V(1).Info("reconciling ValkeyNode")

	node := &valkeyiov1alpha1.ValkeyNode{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !node.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, node)
	}

	// Snapshot for deferred status patch.
	patchBase := client.MergeFrom(node.DeepCopy())

	defer func() {
		if err := r.Status().Patch(ctx, node, patchBase); err != nil {
			log.Error(err, "failed to patch ValkeyNode status")
			if retErr == nil {
				retErr = err
			}
		}
	}()

	setCondition := func(condType string, status metav1.ConditionStatus, reason, message string) {
		meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: node.Generation,
		})
	}

	removeCondition := func(condType string) {
		meta.RemoveStatusCondition(&node.Status.Conditions, condType)
	}

	if requeue, err := r.reconcilePersistenceFinalizer(ctx, node); err != nil {
		return ctrl.Result{}, err
	} else if requeue {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	if err := r.ensureConfigMap(ctx, node); err != nil {
		setCondition(valkeyiov1alpha1.ValkeyNodeConditionReady, metav1.ConditionFalse, "ConfigMapError", err.Error())
		node.Status.Ready = false
		return ctrl.Result{}, err
	}
	if err := r.ensurePersistentVolumeClaim(ctx, node); err != nil {
		setCondition(valkeyiov1alpha1.ValkeyNodeConditionReady, metav1.ConditionFalse, "PersistentVolumeClaimError", err.Error())
		node.Status.Ready = false
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
		setCondition(valkeyiov1alpha1.ValkeyNodeConditionReady, metav1.ConditionFalse, workloadReason, err.Error())
		node.Status.Ready = false
		return ctrl.Result{}, err
	}

	// Update status from workload and pod state.
	node.Status.ObservedGeneration = node.Generation

	pvc, err := r.getPersistentVolumeClaim(ctx, node)
	if err != nil {
		return ctrl.Result{}, err
	}
	if node.Spec.Persistence != nil {
		pvcStatus, pvcReason, pvcMessage := pvcStatusCondition(pvc)
		setCondition(valkeyiov1alpha1.ValkeyNodeConditionPersistentVolumeClaimReady, pvcStatus, pvcReason, pvcMessage)
		pvcSizeStatus, pvcSizeReason, pvcSizeMessage := pvcSizeStatusCondition(node, pvc)
		setCondition(valkeyiov1alpha1.ValkeyNodeConditionPersistentVolumeClaimSizeReady, pvcSizeStatus, pvcSizeReason, pvcSizeMessage)
	} else {
		removeCondition(valkeyiov1alpha1.ValkeyNodeConditionPersistentVolumeClaimReady)
		removeCondition(valkeyiov1alpha1.ValkeyNodeConditionPersistentVolumeClaimSizeReady)
	}

	pod, err := r.getPod(ctx, node)
	if err != nil {
		return ctrl.Result{}, err
	}

	if pod == nil {
		node.Status.Ready = false
		node.Status.PodName = ""
		node.Status.PodIP = ""
		reason := valkeyiov1alpha1.ValkeyNodeReasonPodNotReady
		message := "Pod does not exist yet"
		if node.Spec.Persistence != nil {
			_, reason, message = pvcStatusCondition(pvc)
		}
		setCondition(valkeyiov1alpha1.ValkeyNodeConditionReady, metav1.ConditionFalse, reason, message)
	} else {
		node.Status.PodName = pod.Name
		node.Status.PodIP = pod.Status.PodIP

		podReady := false
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				podReady = true
				break
			}
		}

		if podReady {
			rolled, err := r.isWorkloadRolledOut(ctx, node)
			if err != nil {
				return ctrl.Result{}, err
			}
			podReady = rolled
		}

		node.Status.Ready = podReady
		if podReady {
			node.Status.Role = r.getValkeyRole(ctx, node)
			setCondition(valkeyiov1alpha1.ValkeyNodeConditionReady, metav1.ConditionTrue, valkeyiov1alpha1.ValkeyNodeReasonPodRunning, "Pod is running and ready")
		} else {
			reason := valkeyiov1alpha1.ValkeyNodeReasonPodNotReady
			message := "Pod is not ready"
			if node.Spec.Persistence != nil {
				if pvcStatus, pvcReason, pvcMessage := pvcStatusCondition(pvc); pvcStatus != metav1.ConditionTrue {
					reason = pvcReason
					message = pvcMessage
				}
			}
			setCondition(valkeyiov1alpha1.ValkeyNodeConditionReady, metav1.ConditionFalse, reason, message)
		}
	}

	if !node.Status.Ready {
		log.V(1).Info("ValkeyNode not ready, requeuing")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	applied, err := r.applyLiveConfig(ctx, node)
	if err != nil {
		log.Error(err, "failed to apply live config")
		r.Recorder.Eventf(node, nil, corev1.EventTypeWarning, "LiveConfigApplyFailed", "ApplyLiveConfig", "Failed to apply live config: %v", err)
		setCondition(valkeyiov1alpha1.ValkeyNodeConditionLiveConfigApplied, metav1.ConditionFalse, "ApplyFailed", err.Error())
		return ctrl.Result{}, err
	}
	if applied {
		setCondition(valkeyiov1alpha1.ValkeyNodeConditionLiveConfigApplied, metav1.ConditionTrue, "Applied", "Live config applied")
	} else {
		removeCondition(valkeyiov1alpha1.ValkeyNodeConditionLiveConfigApplied)
	}

	log.V(1).Info("ValkeyNode reconciliation complete")
	return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
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
// when available). Shared by getValkeyRole and the live-config client.
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

// getValkeyRole connects to a Valkey pod and returns its replication role
// ("primary" or "replica"). Returns an empty string if the role cannot be determined.
func (r *ValkeyNodeReconciler) getValkeyRole(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) string {
	c, err := vclient.NewClient(r.buildNodeClientOption(ctx, node))
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
	if r.newConfigClient == nil {
		r.newConfigClient = realConfigClient
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&valkeyiov1alpha1.ValkeyNode{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Named("valkeynode").
		Complete(r)
}
