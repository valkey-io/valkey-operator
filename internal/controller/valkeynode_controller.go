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
	"strings"
	"time"

	vclient "github.com/valkey-io/valkey-go"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

// ValkeyNodeReconciler reconciles a ValkeyNode object
type ValkeyNodeReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=valkey.io,resources=valkeynodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=valkey.io,resources=valkeynodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=valkey.io,resources=valkeynodes/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
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

	if err := r.ensureConfigMap(ctx, node); err != nil {
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

// ensureStatefulSet creates or updates the StatefulSet for the ValkeyNode.
func (r *ValkeyNodeReconciler) ensureStatefulSet(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) error {
	desired := buildValkeyNodeStatefulSet(node)
	if err := controllerutil.SetControllerReference(node, desired, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, desired); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return r.Update(ctx, desired)
		}
		return err
	}
	return nil
}

// ensureDeployment creates or updates the Deployment for the ValkeyNode.
func (r *ValkeyNodeReconciler) ensureDeployment(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) error {
	desired := buildValkeyNodeDeployment(node)
	if err := controllerutil.SetControllerReference(node, desired, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, desired); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return r.Update(ctx, desired)
		}
		return err
	}
	return nil
}

// ensureConfigMap creates or updates the ConfigMap for the ValkeyNode, containing
// the valkey.conf and probe scripts. If ScriptsConfigMapName is set, the ConfigMap
// is assumed to be managed externally and this step is skipped.
func (r *ValkeyNodeReconciler) ensureConfigMap(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) error {
	if node.Spec.ScriptsConfigMapName != "" {
		// ConfigMap is provided externally (e.g. by ValkeyCluster), skip creation.
		return nil
	}
	desired, err := buildValkeyNodeConfigMap(node)
	if err != nil {
		return err
	}
	if err := controllerutil.SetControllerReference(node, desired, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, desired); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return r.Update(ctx, desired)
		}
		return err
	}
	return nil
}

// updateStatus updates the ValkeyNode status based on Pod state.
func (r *ValkeyNodeReconciler) updateStatus(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) error {
	log := logf.FromContext(ctx)

	current := &valkeyiov1alpha1.ValkeyNode{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(node), current); err != nil {
		return err
	}

	// Snapshot status before mutations so we can skip the write if nothing changed.
	previous := current.Status.DeepCopy()

	pod, err := r.getPod(ctx, node)
	if err != nil {
		return err
	}

	if pod == nil {
		current.Status.Ready = false
		current.Status.PodName = ""
		current.Status.PodIP = ""
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type:               valkeyiov1alpha1.ValkeyNodeConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             valkeyiov1alpha1.ValkeyNodeReasonPodNotReady,
			Message:            "Pod does not exist yet",
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

		current.Status.Ready = podReady
		if podReady {
			current.Status.Role = getValkeyRole(ctx, pod.Status.PodIP, DefaultPort)
			meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
				Type:               valkeyiov1alpha1.ValkeyNodeConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             valkeyiov1alpha1.ValkeyNodeReasonPodRunning,
				Message:            "Pod is running and ready",
				ObservedGeneration: current.Generation,
			})
		} else {
			meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
				Type:               valkeyiov1alpha1.ValkeyNodeConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             valkeyiov1alpha1.ValkeyNodeReasonPodNotReady,
				Message:            "Pod is not ready",
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
func getValkeyRole(ctx context.Context, podIP string, port int) string {
	c, err := vclient.NewClient(vclient.ClientOption{
		InitAddress:       []string{fmt.Sprintf("%s:%d", podIP, port)},
		ForceSingleClient: true,
	})
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
	return ctrl.NewControllerManagedBy(mgr).
		For(&valkeyiov1alpha1.ValkeyNode{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Named("valkeynode").
		Complete(r)
}
