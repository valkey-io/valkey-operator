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
	"slices"
	"strconv"
	"strings"
	"time"

	vclient "github.com/valkey-io/valkey-go"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

// sentinelHealthyRequeue refreshes the monitor view while nothing else triggers a reconcile.
const sentinelHealthyRequeue = 30 * time.Second

// sentinelObservation is what one ready Sentinel pod reports about its monitors.
type sentinelObservation struct {
	monitors []valkeyiov1alpha1.SentinelMonitorStatus
	// failoverInProgress is true when Sentinel is failing any monitor over right now.
	failoverInProgress bool
}

// ValkeySentinelReconciler reconciles a ValkeySentinel: the Sentinel pods, their Service, ConfigMap and PDB.
// It does not register monitors yet, so it issues no SENTINEL write commands.
type ValkeySentinelReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  events.EventRecorder

	// observeFunc reads the monitor view from the Sentinel pod at podIP.
	// Tests replace it, because envtest runs no Sentinel.
	observeFunc func(ctx context.Context, s *valkeyiov1alpha1.ValkeySentinel, podIP string) (sentinelObservation, error)
}

// +kubebuilder:rbac:groups=valkey.io,resources=valkeysentinels,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=valkey.io,resources=valkeysentinels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=valkey.io,resources=valkeysentinels/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile builds the Sentinel children and rolls pods on an old revision one at a time.
// Children are garbage collected through owner references, so there is no finalizer.
func (r *ValkeySentinelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.V(1).Info("reconcile...")

	s := &valkeyiov1alpha1.ValkeySentinel{}
	if err := r.Get(ctx, req.NamespacedName, s); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Reconciling during deletion would recreate the children being deleted.
	if !s.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	rc := &resourceClient{Client: r.Client, APIReader: r.APIReader, Scheme: r.Scheme, Recorder: r.Recorder}
	name := sentinelResourceName(s)

	if err := r.upsertService(ctx, s); err != nil {
		return r.failChild(ctx, s, valkeyiov1alpha1.ReasonServiceError, err)
	}
	if err := reconcilePDB(ctx, rc, s, name, s.Namespace, sentinelLabels(s), sentinelSelector(s), s.Spec.PodDisruptionBudgetEnabled()); err != nil {
		return r.failChild(ctx, s, valkeyiov1alpha1.ReasonPodDisruptionBudgetError, err)
	}
	cm, warnings := buildSentinelConfigMap(s)
	if err := r.upsertConfigMap(ctx, s, cm); err != nil {
		return r.failChild(ctx, s, valkeyiov1alpha1.ReasonConfigMapError, err)
	}
	sts, err := r.upsertStatefulSet(ctx, s, sentinelConfigHash(cm))
	if err != nil {
		return r.failChild(ctx, s, valkeyiov1alpha1.ReasonStatefulSetError, err)
	}

	if s.Spec.Replicas%2 == 0 {
		warnings = append(warnings, configWarning{
			reason:  valkeyiov1alpha1.ReasonEvenSentinelReplicas,
			message: fmt.Sprintf("spec.replicas %d is even: it needs the same majority as %d and adds a failure domain without adding authority", s.Spec.Replicas, s.Spec.Replicas-1),
		})
	}
	applyConfigurationWarningsOn(ctx, r.Recorder, s, &s.Status.Conditions, s.Generation, "ReconcileValkeySentinel", warnings)

	pods, err := r.listPods(ctx, s)
	if err != nil {
		return ctrl.Result{}, err
	}
	obs, observed := r.observe(ctx, s, pods)
	if observed {
		s.Status.Monitors = obs.monitors
	}

	rolling, blockedMsg, err := r.rollOutdatedPod(ctx, s, sts, pods, obs, observed)
	if err != nil {
		return ctrl.Result{}, err
	}

	r.setConditions(s, sts, rolling, blockedMsg)
	if err := r.updateStatus(ctx, s, sts); err != nil {
		return ctrl.Result{}, err
	}
	log.V(1).Info("reconcile done")
	if rolling {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	return ctrl.Result{RequeueAfter: sentinelHealthyRequeue}, nil
}

// failChild records a child resource error as Ready=False and Progressing=False, and returns the error for a retry.
// Clearing Progressing keeps a stale "waiting" state from hiding the new error.
func (r *ValkeySentinelReconciler) failChild(ctx context.Context, s *valkeyiov1alpha1.ValkeySentinel, reason string, err error) (ctrl.Result, error) {
	setConditionOn(&s.Status.Conditions, s.Generation, valkeyiov1alpha1.ConditionReady, reason, err.Error(), metav1.ConditionFalse)
	setConditionOn(&s.Status.Conditions, s.Generation, valkeyiov1alpha1.ConditionProgressing, reason, err.Error(), metav1.ConditionFalse)
	_ = r.updateStatus(ctx, s, nil)
	return ctrl.Result{}, err
}

func (r *ValkeySentinelReconciler) upsertService(ctx context.Context, s *valkeyiov1alpha1.ValkeySentinel) error {
	desired := buildSentinelService(s)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = desired.Labels
		svc.Spec.Type = desired.Spec.Type
		// ClusterIP is immutable after creation; preserve the existing value on updates.
		if svc.Spec.ClusterIP == "" {
			svc.Spec.ClusterIP = desired.Spec.ClusterIP
		}
		svc.Spec.PublishNotReadyAddresses = desired.Spec.PublishNotReadyAddresses
		svc.Spec.Selector = desired.Spec.Selector
		svc.Spec.Ports = desired.Spec.Ports
		return controllerutil.SetControllerReference(s, svc, r.Scheme)
	})
	return err
}

func (r *ValkeySentinelReconciler) upsertConfigMap(ctx context.Context, s *valkeyiov1alpha1.ValkeySentinel, desired *corev1.ConfigMap) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = desired.Labels
		cm.Data = desired.Data
		return controllerutil.SetControllerReference(s, cm, r.Scheme)
	})
	return err
}

func (r *ValkeySentinelReconciler) upsertStatefulSet(ctx context.Context, s *valkeyiov1alpha1.ValkeySentinel, configHash string) (*appsv1.StatefulSet, error) {
	desired, err := buildSentinelStatefulSet(s, configHash)
	if err != nil {
		return nil, err
	}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		sts.Labels = desired.Labels
		sts.Spec = desired.Spec
		return controllerutil.SetControllerReference(s, sts, r.Scheme)
	})
	if err != nil {
		r.Recorder.Eventf(s, sts, corev1.EventTypeWarning, "StatefulSetFailed", "ReconcileStatefulSet", "Failed to reconcile StatefulSet: %v", err)
		return nil, err
	}
	if result == controllerutil.OperationResultCreated {
		r.Recorder.Eventf(s, sts, corev1.EventTypeNormal, "StatefulSetCreated", "CreateStatefulSet", "Created Sentinel StatefulSet")
	}
	return sts, nil
}

func (r *ValkeySentinelReconciler) listPods(ctx context.Context, s *valkeyiov1alpha1.ValkeySentinel) ([]corev1.Pod, error) {
	list := &corev1.PodList{}
	if err := r.List(ctx, list, client.InNamespace(s.Namespace), client.MatchingLabels(sentinelSelector(s))); err != nil {
		return nil, err
	}
	pods := list.Items
	slices.SortFunc(pods, func(a, b corev1.Pod) int { return podOrdinal(a.Name) - podOrdinal(b.Name) })
	return pods, nil
}

// podOrdinal returns the StatefulSet ordinal at the end of a pod name, or -1.
func podOrdinal(name string) int {
	i := strings.LastIndexByte(name, '-')
	if i < 0 {
		return -1
	}
	n, err := strconv.Atoi(name[i+1:])
	if err != nil {
		return -1
	}
	return n
}

// sentinelPodServing reports whether a Sentinel pod is ready, has an IP, and is not being deleted.
func sentinelPodServing(pod *corev1.Pod) bool {
	return pod.DeletionTimestamp.IsZero() && pod.Status.PodIP != "" && podReady(pod)
}

// observe reads the monitor view from the lowest-ordinal ready pod.
// The lowest ordinal is the pod a roll replaces last, so its view is the most stable during a roll.
// observed is false when no pod is ready or the read failed; the caller then keeps the previous status.
func (r *ValkeySentinelReconciler) observe(ctx context.Context, s *valkeyiov1alpha1.ValkeySentinel, pods []corev1.Pod) (sentinelObservation, bool) {
	idx := slices.IndexFunc(pods, func(p corev1.Pod) bool { return sentinelPodServing(&p) })
	if idx < 0 {
		return sentinelObservation{}, false
	}
	observeFn := r.observeFunc
	if observeFn == nil {
		observeFn = r.observeLive
	}
	obs, err := observeFn(ctx, s, pods[idx].Status.PodIP)
	if err != nil {
		logf.FromContext(ctx).Info("could not read Sentinel monitors", "pod", pods[idx].Name, "error", err.Error())
		return sentinelObservation{}, false
	}
	return obs, true
}

// observeLive dials one Sentinel pod and reads SENTINEL MASTERS, then CKQUORUM for each monitor.
// Sentinel has no authentication in this phase, so the dial uses no credentials.
func (r *ValkeySentinelReconciler) observeLive(ctx context.Context, s *valkeyiov1alpha1.ValkeySentinel, podIP string) (sentinelObservation, error) {
	cfg := connConfig{}
	if t := s.GetTLS(); t != nil {
		tlsCfg, _, err := getTLSConfig(ctx, r.APIReader, t.Certificates.Server.SecretName, sentinelTLSServerName(s), s.Namespace, false)
		if err != nil {
			return sentinelObservation{}, fmt.Errorf("TLS config: %w", err)
		}
		cfg.tls = tlsCfg
	}
	// A status read uses one short-lived connection outside the shared pool.
	c, err := vclient.NewClient(clientOption(fmt.Sprintf("%s:%d", podIP, valkey.SentinelPort), cfg))
	if err != nil {
		return sentinelObservation{}, err
	}
	defer c.Close()

	masters, err := valkey.SentinelMasters(ctx, c)
	if err != nil {
		return sentinelObservation{}, err
	}
	obs := sentinelObservation{}
	for _, m := range masters {
		ok, _, err := valkey.SentinelCKQuorum(ctx, c, m.Name)
		if err != nil {
			return sentinelObservation{}, err
		}
		obs.failoverInProgress = obs.failoverInProgress || m.FailoverInProgress()
		obs.monitors = append(obs.monitors, valkeyiov1alpha1.SentinelMonitorStatus{
			Name:           m.Name,
			PrimaryAddress: m.Address(),
			Quorum:         int32(m.Quorum),
			OK:             ok,
			Replicas:       int32(m.NumReplicas),
		})
	}
	return obs, nil
}

// rollOutdatedPod deletes at most one pod still on an old StatefulSet revision.
// It waits until every pod is ready, and blocks while any monitor lacks quorum or is failing over.
// Replacing a Sentinel in either state could leave a primary with no failover authority.
// rolling is true while any pod is outdated; blockedMsg explains a held roll.
func (r *ValkeySentinelReconciler) rollOutdatedPod(ctx context.Context, s *valkeyiov1alpha1.ValkeySentinel, sts *appsv1.StatefulSet, pods []corev1.Pod, obs sentinelObservation, observed bool) (rolling bool, blockedMsg string, err error) {
	update := sts.Status.UpdateRevision
	if update == "" || sts.Status.ObservedGeneration < sts.Generation {
		return false, "", nil
	}
	var outdated []corev1.Pod
	for _, p := range pods {
		if p.Labels[appsv1.ControllerRevisionHashLabelKey] != update {
			outdated = append(outdated, p)
		}
	}
	if len(outdated) == 0 {
		return false, "", nil
	}
	if int32(len(pods)) < s.Spec.Replicas || slices.ContainsFunc(pods, func(p corev1.Pod) bool { return !sentinelPodServing(&p) }) {
		return true, "", nil
	}
	if len(obs.monitors) > 0 {
		if !observed {
			return true, "cannot read the monitor view to confirm quorum", nil
		}
		if obs.failoverInProgress {
			return true, "a failover is in progress", nil
		}
		if i := slices.IndexFunc(obs.monitors, func(m valkeyiov1alpha1.SentinelMonitorStatus) bool { return !m.OK }); i >= 0 {
			return true, fmt.Sprintf("monitor %s has no quorum", obs.monitors[i].Name), nil
		}
	}
	// Replace the highest ordinal first, so the observing pod (the lowest ready ordinal) is replaced last.
	target := outdated[len(outdated)-1]
	if err := r.Delete(ctx, &target); err != nil && !apierrors.IsNotFound(err) {
		return true, "", err
	}
	r.Recorder.Eventf(s, &target, corev1.EventTypeNormal, "SentinelPodRolled", "RollSentinel", "Deleted %s to apply revision %s", target.Name, update)
	return true, "", nil
}

// setConditions derives Ready, Progressing and Degraded from the StatefulSet and the monitor view.
func (r *ValkeySentinelReconciler) setConditions(s *valkeyiov1alpha1.ValkeySentinel, sts *appsv1.StatefulSet, rolling bool, blockedMsg string) {
	set := func(condType, reason, msg string, status metav1.ConditionStatus) {
		setConditionOn(&s.Status.Conditions, s.Generation, condType, reason, msg, status)
	}
	ready := sts.Status.ReadyReplicas
	switch {
	case blockedMsg != "":
		set(valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonRollBlocked, "Roll held: "+blockedMsg, metav1.ConditionFalse)
		set(valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonRollBlocked, "Roll held: "+blockedMsg, metav1.ConditionTrue)
	case rolling:
		set(valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonRollingSentinels, "Replacing pods on an old revision one at a time", metav1.ConditionFalse)
		set(valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonRollingSentinels, "Replacing pods on an old revision one at a time", metav1.ConditionTrue)
	case ready < s.Spec.Replicas:
		msg := fmt.Sprintf("%d of %d Sentinel pods ready", ready, s.Spec.Replicas)
		set(valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonWaitingForSentinels, msg, metav1.ConditionFalse)
		set(valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonWaitingForSentinels, msg, metav1.ConditionTrue)
	default:
		set(valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonSentinelsReady, "All Sentinel pods are ready", metav1.ConditionTrue)
		set(valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonReconcileComplete, "No changes needed", metav1.ConditionFalse)
	}

	var noQuorum []string
	for _, m := range s.Status.Monitors {
		if !m.OK {
			noQuorum = append(noQuorum, m.Name)
		}
	}
	if len(noQuorum) > 0 {
		set(valkeyiov1alpha1.ConditionDegraded, valkeyiov1alpha1.ReasonQuorumUnavailable, "No quorum for "+strings.Join(noQuorum, ", "), metav1.ConditionTrue)
	} else {
		meta.RemoveStatusCondition(&s.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)
	}
}

// updateStatus writes the in-memory conditions and derived state to the ValkeySentinel status.
// The state priority matches the other controllers: Degraded > Ready > Progressing > Failed.
// sts is nil when reconcile failed before the StatefulSet step.
func (r *ValkeySentinelReconciler) updateStatus(ctx context.Context, s *valkeyiov1alpha1.ValkeySentinel, sts *appsv1.StatefulSet) error {
	log := logf.FromContext(ctx)
	current := &valkeyiov1alpha1.ValkeySentinel{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(s), current); err != nil {
		return err
	}
	patchBase := current.DeepCopy()
	patch := client.MergeFrom(patchBase)

	current.Status.Conditions = s.Status.Conditions
	current.Status.Monitors = s.Status.Monitors
	current.Status.ObservedGeneration = s.Generation
	if sts != nil {
		current.Status.Replicas = sts.Status.Replicas
		current.Status.ReadyReplicas = sts.Status.ReadyReplicas
	}

	readyCondition := meta.FindStatusCondition(current.Status.Conditions, valkeyiov1alpha1.ConditionReady)
	progressingCondition := meta.FindStatusCondition(current.Status.Conditions, valkeyiov1alpha1.ConditionProgressing)
	degradedCondition := meta.FindStatusCondition(current.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)

	switch {
	case degradedCondition != nil && degradedCondition.Status == metav1.ConditionTrue:
		current.Status.State = valkeyiov1alpha1.ValkeySentinelStateDegraded
		current.Status.Reason = degradedCondition.Reason
		current.Status.Message = degradedCondition.Message
	case readyCondition != nil && readyCondition.Status == metav1.ConditionTrue:
		current.Status.State = valkeyiov1alpha1.ValkeySentinelStateReady
		current.Status.Reason = readyCondition.Reason
		current.Status.Message = readyCondition.Message
	case progressingCondition != nil && progressingCondition.Status == metav1.ConditionTrue:
		current.Status.State = valkeyiov1alpha1.ValkeySentinelStateReconciling
		current.Status.Reason = progressingCondition.Reason
		current.Status.Message = progressingCondition.Message
	case readyCondition != nil && readyCondition.Status == metav1.ConditionFalse:
		current.Status.State = valkeyiov1alpha1.ValkeySentinelStateFailed
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

	s.Status = current.Status
	s.ResourceVersion = current.ResourceVersion
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ValkeySentinelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.APIReader = mgr.GetAPIReader()
	return ctrl.NewControllerManagedBy(mgr).
		For(&valkeyiov1alpha1.ValkeySentinel{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Named("valkeysentinel").
		Complete(r)
}
