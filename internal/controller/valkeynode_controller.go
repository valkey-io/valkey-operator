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
	"k8s.io/apimachinery/pkg/api/equality"
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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
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
	// LoadACL reloads the mounted aclfile into the running server.
	LoadACL(ctx context.Context) error
	// UserNames returns the names of the users the server currently has.
	UserNames(ctx context.Context) ([]string, error)
	// UserPasswordHashes returns the user's currently configured password
	// hashes, sorted and deduplicated. An unknown user yields an empty slice
	// rather than an error, so a user that has not been loaded yet reads as out
	// of sync.
	UserPasswordHashes(ctx context.Context, username string) ([]string, error)
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

func (rc *realValkeyConfigClient) LoadACL(ctx context.Context) error {
	if err := rc.client.Do(ctx, rc.client.B().AclLoad().Build()).Error(); err != nil {
		return fmt.Errorf("ACL LOAD: %w", err)
	}
	return nil
}

func (rc *realValkeyConfigClient) UserNames(ctx context.Context) ([]string, error) {
	users, err := rc.client.Do(ctx, rc.client.B().AclUsers().Build()).AsStrSlice()
	if err != nil {
		return nil, fmt.Errorf("ACL USERS: %w", err)
	}
	return users, nil
}

func (rc *realValkeyConfigClient) UserPasswordHashes(ctx context.Context, username string) ([]string, error) {
	m, err := rc.client.Do(ctx, rc.client.B().AclGetuser().Username(username).Build()).AsMap()
	if err != nil {
		if vclient.IsValkeyNil(err) {
			// User is not defined on the server yet.
			return []string{}, nil
		}
		return nil, fmt.Errorf("ACL GETUSER %s: %w", username, err)
	}
	passwords, ok := m["passwords"]
	if !ok {
		return []string{}, nil
	}
	hashes, err := passwords.AsStrSlice()
	if err != nil {
		return nil, fmt.Errorf("ACL GETUSER %s passwords: %w", username, err)
	}
	// Valkey keeps passwords as a set, but normalize anyway so both sides of
	// the comparison are in the same shape.
	return normalizeHashes(hashes), nil
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
	// RoleEvents carries reconcile triggers from the RolePoller, which detects
	// role changes that produce no Kubernetes event (a failover between two
	// healthy pods). Events are triggers only: the reconcile re-resolves the role
	// from the node's own connection, so this controller stays the sole writer of
	// Status.Role. Leave nil to run without the poller; the backstop requeue then
	// bounds staleness on its own.
	RoleEvents <-chan event.GenericEvent
	// resolveRoleFunc, when set, overrides how resolveRole reads a node's live
	// replication role. Tests set it to a fake (envtest has no running Valkey
	// server); when nil, resolveRole connects to the pod directly.
	resolveRoleFunc func(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) string
}

// +kubebuilder:rbac:groups=valkey.io,resources=valkeynodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=valkey.io,resources=valkeynodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=valkey.io,resources=valkeynodes/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
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

	// Re-read after ensureWorkload may have written WorkloadRollPending status.
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !node.Status.Ready {
		log.V(1).Info("ValkeyNode not ready, requeuing")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Apply live config before WorkloadRollPending requeue so a node waiting on
	// Spec.WorkloadRevision can still clear LiveConfigApplied and not block
	// the cluster controller on an unrelated condition.
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

	// Apply the ACL live too, before the WorkloadRollPending requeue: ACL is no
	// longer part of the pod template (it does not enter Spec.WorkloadRevision),
	// so a node waiting on a roll must still pick up ACL edits without one.
	aclSynced, err := r.applyLiveACL(ctx, node)
	if err != nil {
		log.Error(err, "failed to apply live ACL")
		r.Recorder.Eventf(node, nil, corev1.EventTypeWarning, "LiveACLApplyFailed", "ApplyLiveACL", "Failed to apply live ACL: %v", err)
		if condErr := r.setACLCondition(ctx, node, metav1.ConditionFalse, "ApplyFailed", err.Error()); condErr != nil {
			log.Error(condErr, "failed to set ACLApplied condition")
		}
		return ctrl.Result{}, err
	}
	if !aclSynced {
		// The reload ran, but the mounted aclfile had not caught up with the
		// Secret, so it loaded stale content. Report that rather than claiming
		// the desired passwords are live, and reload again on the requeue.
		log.V(1).Info("desired ACL passwords not live yet, waiting for the aclfile volume to propagate")
		if condErr := r.setACLCondition(ctx, node, metav1.ConditionFalse, "PendingPropagation",
			"Waiting for the mounted aclfile to reflect the desired ACL"); condErr != nil {
			log.Error(condErr, "failed to set ACLApplied condition")
			return ctrl.Result{}, condErr
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if condErr := r.setACLCondition(ctx, node, metav1.ConditionTrue, "Applied",
		"Desired ACL passwords are live"); condErr != nil {
		log.Error(condErr, "failed to set ACLApplied condition")
		return ctrl.Result{}, condErr
	}

	// Waiting for Spec.WorkloadRevision: rely on watches when the cluster advances
	// Spec, with a long backoff so waiters do not spam the API.
	if meta.IsStatusConditionTrue(node.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionWorkloadRollPending) {
		log.V(1).Info("ValkeyNode awaiting Spec.WorkloadRevision, requeuing", "name", node.Name)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
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

// setACLCondition sets the ACLApplied condition, reporting whether the ACL the
// cluster controller wrote to the mounted Secret is live on the server.
func (r *ValkeyNodeReconciler) setACLCondition(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode, status metav1.ConditionStatus, reason, message string) error {
	current := &valkeyiov1alpha1.ValkeyNode{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(node), current); err != nil {
		return fmt.Errorf("get ValkeyNode: %w", err)
	}
	patchBase := current.DeepCopy()
	if !meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type:               valkeyiov1alpha1.ValkeyNodeConditionACLApplied,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: current.Generation,
	}) {
		return nil
	}
	if err := r.Status().Patch(ctx, current, client.MergeFrom(patchBase)); err != nil {
		return fmt.Errorf("patch ACLApplied condition: %w", err)
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

// buildPodTemplateAnnotations assembles the annotations stamped on the pod
// template, and therefore the ones that feed the WorkloadRevision roll hash.
// Only the server-config hash lives here: a config change that is not
// live-settable still needs a rolling restart to take effect. The ACL hash is
// deliberately absent. ACL edits are applied to the running server live by the
// ValkeyNode reconciler (see applyLiveACL), so they must not enter the
// WorkloadRevision and roll the pods.
func buildPodTemplateAnnotations(node *valkeyiov1alpha1.ValkeyNode) map[string]string {
	annotations := map[string]string{}
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
	desired.Spec.Template.Annotations = buildPodTemplateAnnotations(node)

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      desired.Name,
			Namespace: desired.Namespace,
		},
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(sts), sts); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		// Create path: no live pod to protect.
		sts = desired.DeepCopy()
		if err := controllerutil.SetControllerReference(node, sts, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, sts); err != nil {
			return err
		}
		log.V(1).Info("created StatefulSet", "name", sts.Name)
		return r.clearWorkloadRollPending(ctx, node)
	}

	desiredHash := podTemplateRollHash(desired.Spec.Template)
	// Heal live drift whenever templates differ; do not skip on a stale
	// last-applied annotation (that can hide real STS edits).
	if !podTemplateWouldRoll(sts.Spec.Template, desired.Spec.Template) {
		if err := r.syncStatefulSetWithoutRoll(ctx, node, sts, desired); err != nil {
			return err
		}
		return r.clearWorkloadRollPending(ctx, node)
	}

	allowed, err := r.gateRollingWorkloadUpdate(ctx, node, desiredHash)
	if err != nil {
		return err
	}
	if !allowed {
		log.V(1).Info("deferring StatefulSet template update until Spec.WorkloadRevision matches",
			"name", sts.Name, "desiredHash", desiredHash, "specRevision", node.Spec.WorkloadRevision)
		return nil
	}

	sts.Labels = desired.Labels
	sts.Spec = desired.Spec
	if err := controllerutil.SetControllerReference(node, sts, r.Scheme); err != nil {
		return err
	}
	if err := r.Update(ctx, sts); err != nil {
		return err
	}
	log.Info("updated StatefulSet pod template", "name", sts.Name, "desiredHash", desiredHash)
	r.Recorder.Eventf(node, nil, corev1.EventTypeNormal, "WorkloadRollApplied", "ApplyWorkloadRoll",
		"Applied pod template update (hash %s)", desiredHash)
	return r.clearWorkloadRollPending(ctx, node)
}

// ensureDeployment creates or updates the Deployment for the ValkeyNode.
func (r *ValkeyNodeReconciler) ensureDeployment(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) error {
	log := logf.FromContext(ctx)
	desired, err := buildValkeyNodeDeployment(node)
	if err != nil {
		return err
	}
	desired.Spec.Template.Annotations = buildPodTemplateAnnotations(node)

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      desired.Name,
			Namespace: desired.Namespace,
		},
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(dep), dep); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		dep = desired.DeepCopy()
		if err := controllerutil.SetControllerReference(node, dep, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, dep); err != nil {
			return err
		}
		log.V(1).Info("created Deployment", "name", dep.Name)
		return r.clearWorkloadRollPending(ctx, node)
	}

	desiredHash := podTemplateRollHash(desired.Spec.Template)
	if !podTemplateWouldRoll(dep.Spec.Template, desired.Spec.Template) {
		if err := r.syncDeploymentWithoutRoll(ctx, node, dep, desired); err != nil {
			return err
		}
		return r.clearWorkloadRollPending(ctx, node)
	}

	allowed, err := r.gateRollingWorkloadUpdate(ctx, node, desiredHash)
	if err != nil {
		return err
	}
	if !allowed {
		log.V(1).Info("deferring Deployment template update until Spec.WorkloadRevision matches",
			"name", dep.Name, "desiredHash", desiredHash, "specRevision", node.Spec.WorkloadRevision)
		return nil
	}

	dep.Labels = desired.Labels
	dep.Spec = desired.Spec
	if err := controllerutil.SetControllerReference(node, dep, r.Scheme); err != nil {
		return err
	}
	if err := r.Update(ctx, dep); err != nil {
		return err
	}
	log.Info("updated Deployment pod template", "name", dep.Name, "desiredHash", desiredHash)
	r.Recorder.Eventf(node, nil, corev1.EventTypeNormal, "WorkloadRollApplied", "ApplyWorkloadRoll",
		"Applied pod template update (hash %s)", desiredHash)
	return r.clearWorkloadRollPending(ctx, node)
}

// gateRollingWorkloadUpdate decides whether a rolling pod-template update may be
// applied. Standalone nodes always apply. Cluster-owned nodes apply only when
// Spec.WorkloadRevision matches the desired template hash (set by ValkeyCluster).
func (r *ValkeyNodeReconciler) gateRollingWorkloadUpdate(
	ctx context.Context,
	node *valkeyiov1alpha1.ValkeyNode,
	desiredHash string,
) (bool, error) {
	if !isClusterOwned(node) {
		return true, nil
	}
	// Fresh read: cluster may have advanced Spec.WorkloadRevision after this reconcile started.
	fresh := &valkeyiov1alpha1.ValkeyNode{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(node), fresh); err != nil {
		return false, err
	}
	node.Spec = fresh.Spec
	if workloadRevisionAllows(fresh, desiredHash) {
		return true, nil
	}
	transitioned, err := r.markWorkloadRollPending(ctx, node, desiredHash)
	if err != nil {
		return false, err
	}
	if transitioned {
		r.Recorder.Eventf(node, nil, corev1.EventTypeNormal, "WorkloadRollDeferred", "GateWorkloadRoll",
			"Deferred pod template update (hash %s); waiting for Spec.WorkloadRevision", desiredHash)
	}
	return false, nil
}

// markWorkloadRollPending sets WorkloadRollPending=True. Returns true when newly transitioned.
func (r *ValkeyNodeReconciler) markWorkloadRollPending(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode, desiredHash string) (bool, error) {
	current := &valkeyiov1alpha1.ValkeyNode{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(node), current); err != nil {
		return false, err
	}
	alreadyPending := meta.IsStatusConditionTrue(current.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionWorkloadRollPending)
	patchBase := current.DeepCopy()
	changed := meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type:               valkeyiov1alpha1.ValkeyNodeConditionWorkloadRollPending,
		Status:             metav1.ConditionTrue,
		Reason:             valkeyiov1alpha1.ValkeyNodeReasonAwaitingWorkloadRevision,
		Message:            fmt.Sprintf("desired workload template hash %s awaits Spec.WorkloadRevision (have %q)", desiredHash, current.Spec.WorkloadRevision),
		ObservedGeneration: current.Generation,
	})
	if changed {
		if err := r.Status().Patch(ctx, current, client.MergeFrom(patchBase)); err != nil {
			return false, fmt.Errorf("patch WorkloadRollPending condition: %w", err)
		}
	}
	return !alreadyPending && changed, nil
}

func (r *ValkeyNodeReconciler) clearWorkloadRollPending(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) error {
	current := &valkeyiov1alpha1.ValkeyNode{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(node), current); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !meta.IsStatusConditionTrue(current.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionWorkloadRollPending) {
		return nil
	}
	patchBase := current.DeepCopy()
	if meta.RemoveStatusCondition(&current.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionWorkloadRollPending) {
		if err := r.Status().Patch(ctx, current, client.MergeFrom(patchBase)); err != nil {
			return fmt.Errorf("clear WorkloadRollPending condition: %w", err)
		}
	}
	return nil
}

// syncStatefulSetWithoutRoll updates labels, owner, and Spec when the pod
// template would not roll.
func (r *ValkeyNodeReconciler) syncStatefulSetWithoutRoll(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode, sts *appsv1.StatefulSet, desired *appsv1.StatefulSet) error {
	before := sts.DeepCopy()
	sts.Labels = desired.Labels
	sts.Spec = desired.Spec
	if err := controllerutil.SetControllerReference(node, sts, r.Scheme); err != nil {
		return err
	}
	return r.maybeUpdateWorkloadWithoutRoll(ctx, sts, before.Labels, sts.Labels, before.Spec, sts.Spec, before.OwnerReferences, sts.OwnerReferences, "StatefulSet", sts.Name)
}

// syncDeploymentWithoutRoll updates labels, owner, and Spec when the pod
// template would not roll.
func (r *ValkeyNodeReconciler) syncDeploymentWithoutRoll(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode, dep *appsv1.Deployment, desired *appsv1.Deployment) error {
	before := dep.DeepCopy()
	dep.Labels = desired.Labels
	dep.Spec = desired.Spec
	if err := controllerutil.SetControllerReference(node, dep, r.Scheme); err != nil {
		return err
	}
	return r.maybeUpdateWorkloadWithoutRoll(ctx, dep, before.Labels, dep.Labels, before.Spec, dep.Spec, before.OwnerReferences, dep.OwnerReferences, "Deployment", dep.Name)
}

func (r *ValkeyNodeReconciler) maybeUpdateWorkloadWithoutRoll(
	ctx context.Context,
	obj client.Object,
	beforeLabels, afterLabels map[string]string,
	beforeSpec, afterSpec any,
	beforeOwners, afterOwners []metav1.OwnerReference,
	kind, name string,
) error {
	log := logf.FromContext(ctx)
	if equality.Semantic.DeepEqual(beforeLabels, afterLabels) &&
		equality.Semantic.DeepEqual(beforeSpec, afterSpec) &&
		equality.Semantic.DeepEqual(beforeOwners, afterOwners) {
		log.V(1).Info(kind+" already matches desired (no pod template roll)", "name", name)
		return nil
	}
	if err := r.Update(ctx, obj); err != nil {
		return err
	}
	log.V(1).Info("synced "+kind+" without pod template roll", "name", name)
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

		// If the pod is ready, also verify the workload rollout has completed.
		// The old pod may still be running (and ready) while the StatefulSet is rolling
		// to a new spec; we must not report Ready=true until the rollout is done so the
		// ValkeyCluster controller waits before advancing to the next node.
		ready := podReady(pod)
		rolledOut := ready
		if ready {
			rolled, err := r.isWorkloadRolledOut(ctx, node)
			if err != nil {
				return false, err
			}
			rolledOut = rolled
		}

		current.Status.Ready = ready && rolledOut

		// Role follows the pod, not the rollout: a pod that is serving has a
		// resolvable role even while its workload rolls, and blanking it there
		// would drop a live primary from role-based selection for the whole roll.
		// Only an unready pod (nothing to ask) clears it.
		if ready {
			if role := r.resolveRole(ctx, current); role != "" {
				current.Status.Role = role
			} else {
				// Transient INFO failure on a ready pod: keep the last-known
				// role rather than blanking a healthy node, and signal a short
				// requeue so we retry sooner than the backstop.
				roleResolveFailed = true
			}
		} else {
			current.Status.Role = ""
		}

		if current.Status.Ready {
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
// A slot-less myself,master therefore resolves to replica in two states: the
// restarting replica above, and the primary of a freshly created shard before
// CLUSTER ADDSLOTSRANGE. The second is a deliberate tradeoff — the two are
// indistinguishable from the node's own CLUSTER NODES output, and publishing a
// restarting replica as primary is the more damaging error. A new shard's
// primary is therefore reported as replica until it owns slots. This matches
// the cluster-wide view, where GetClusterState holds slot-less masters in
// PendingNodes rather than assigning them a shard primary.
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
	ctrlBuilder := ctrl.NewControllerManagedBy(mgr).
		For(&valkeyiov1alpha1.ValkeyNode{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.podToValkeyNode),
			builder.WithPredicates(valkeyPodPredicate()),
		).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.aclSecretToNodes)).
		Named("valkeynode")
	if r.RoleEvents != nil {
		// The poller pushes the ValkeyNode itself, so the event maps straight to
		// its own reconcile request. Repeat events for one node while a reconcile
		// is already queued collapse into a single pass.
		ctrlBuilder = ctrlBuilder.WatchesRawSource(source.Channel(r.RoleEvents, &handler.EnqueueRequestForObject{}))
	}
	return ctrlBuilder.Complete(r)
}

// aclSecretToNodes maps a changed internal ACL Secret to reconcile requests for
// every ValkeyNode that mounts it. ACL edits no longer roll the pods, so this
// watch is what keeps the live path prompt: a Secret update enqueues the nodes,
// whose reconcile reloads the ACL into the running server (see applyLiveACL)
// instead of waiting for the periodic resync. The Secret type gate keeps the
// controller from listing nodes on every unrelated Secret event.
func (r *ValkeyNodeReconciler) aclSecretToNodes(ctx context.Context, obj client.Object) []reconcile.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok || secret.Type != AclSecretType {
		return nil
	}
	var nodes valkeyiov1alpha1.ValkeyNodeList
	if err := r.List(ctx, &nodes, client.InNamespace(secret.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range nodes.Items {
		if nodes.Items[i].Spec.UsersACLSecretName == secret.GetName() {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
				Name:      nodes.Items[i].Name,
				Namespace: nodes.Items[i].Namespace,
			}})
		}
	}
	return reqs
}
