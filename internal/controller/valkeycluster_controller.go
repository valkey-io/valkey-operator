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
	"embed"
	"errors"
	"slices"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
	"valkey.io/valkey-operator/internal/valkey"
)

const (
	DefaultPort           = 6379
	DefaultClusterBusPort = 16379
	DefaultImage          = "valkey/valkey:9.0.0"
)

// ValkeyClusterReconciler reconciles a ValkeyCluster object
type ValkeyClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//go:embed scripts/*
var scripts embed.FS

// +kubebuilder:rbac:groups=valkey.io,resources=valkeyclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=valkey.io,resources=valkeyclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=valkey.io,resources=valkeyclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="apps",resources=deployments,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/reconcile
func (r *ValkeyClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.V(1).Info("Reconcile...")

	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := r.upsertService(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.upsertConfigMap(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.upsertDeployments(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}

	// Get all pods and their current Valkey Cluster state
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(cluster.Namespace), client.MatchingLabels(labels(cluster))); err != nil {
		log.Error(err, "Failed to list pods")
		return ctrl.Result{}, err
	}
	state := r.getValkeyClusterState(ctx, pods)
	defer state.CloseClients()

	// Check if we need to forget stale nonexiting nodes
	r.forgetStaleNodes(ctx, state, pods)

	// Add new nodes
	if len(state.PendingNodes) > 0 {
		node := state.PendingNodes[0]
		log.V(1).Info("Adding node", "address", node.Address, "Id", node.Id)
		if err := r.addValkeyNode(ctx, cluster, state, node); err != nil {
			log.Error(err, "Unable to add cluster node")
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		// Let the added node stabilize, and refetch the cluster state.
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// Check cluster status
	if len(state.Shards) < int(cluster.Spec.Shards) {
		log.V(1).Info("Missing shards, requeue..")
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	for _, shard := range state.Shards {
		if len(shard.Nodes) < (1 + int(cluster.Spec.Replicas)) {
			log.V(1).Info("Missing replicas, requeue..")
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
	}

	log.V(1).Info("Reconcile done")
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// Create or update a headless service (client connects to pods directly)
func (r *ValkeyClusterReconciler) upsertService(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
			Labels:    labels(cluster),
		},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "None",
			Selector:  labels(cluster),
			Ports: []corev1.ServicePort{
				{
					Name: "valkey",
					Port: DefaultPort,
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(cluster, svc, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, svc); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if err := r.Update(ctx, svc); err != nil {
				return err
			}
		} else {
			return err
		}
	}
	return nil
}

// Create or update a basic valkey.conf
func (r *ValkeyClusterReconciler) upsertConfigMap(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) error {
	readiness, err := scripts.ReadFile("scripts/readiness-check.sh")
	if err != nil {
		return err
	}
	liveness, err := scripts.ReadFile("scripts/liveness-check.sh")
	if err != nil {
		return err
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
			Labels:    labels(cluster),
		},
		Data: map[string]string{
			"readiness-check.sh": string(readiness),
			"liveness-check.sh":  string(liveness),
			"valkey.conf": `
cluster-enabled yes
protected-mode no
cluster-node-timeout 2000`,
		},
	}
	if err := controllerutil.SetControllerReference(cluster, cm, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, cm); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if err := r.Update(ctx, cm); err != nil {
				return err
			}
		} else {
			return err
		}
	}
	return nil
}

// Create Valkey instances, one Deployment and Pod each
func (r *ValkeyClusterReconciler) upsertDeployments(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) error {
	existing := &appsv1.DeploymentList{}
	if err := r.List(ctx, existing, client.InNamespace(cluster.Namespace)); err != nil {
		return err
	}

	replicas := int(cluster.Spec.Shards * (1 + cluster.Spec.Replicas))

	// Create missing deployments
	for i := len(existing.Items); i < replicas; i++ {
		deployment := createClusterDeployment(cluster)
		if err := controllerutil.SetControllerReference(cluster, deployment, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, deployment); err != nil {
			return err
		}
	}

	// TODO: update existing

	return nil
}

func (r *ValkeyClusterReconciler) getValkeyClusterState(ctx context.Context, pods *corev1.PodList) *valkey.ClusterState {
	// Create a list of addresses to possible Valkey nodes
	ips := []string{}
	for _, pod := range pods.Items {
		if pod.Status.PodIP == "" {
			continue
		}
		ips = append(ips, pod.Status.PodIP)
	}

	// Get current state of the Valkey cluster
	return valkey.GetClusterState(ctx, ips, DefaultPort)
}

func (r *ValkeyClusterReconciler) addValkeyNode(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState, node *valkey.NodeState) error {
	log := logf.FromContext(ctx)

	shardsExists := len(state.Shards)
	shardsRequired := int(cluster.Spec.Shards)
	replicasRequired := int(cluster.Spec.Replicas)

	// Meet other nodes in shards
	if sval, ok := node.ClusterInfo["cluster_known_nodes"]; ok {
		if val, err := strconv.Atoi(sval); err == nil {
			if val <= 1 && len(state.Shards) > 0 {
				// This node does not know any other nodes.
				for _, shard := range state.Shards {
					primary := shard.GetPrimaryNode()
					if primary == nil {
						continue
					}
					log.V(1).Info("Meet other node", "this node", node.Address, "other node", primary.Address)
					if err = node.Client.Do(ctx, node.Client.B().ClusterMeet().Ip(primary.Address).Port(int64(primary.Port)).Build()).Error(); err != nil {
						log.Error(err, "Command failed: CLUSTER MEET", "from", node.Address, "to", primary.Address)
						return err
					}
				}
				return nil
			}
		}
	}

	// Add a new primary when more shards are expected.
	if shardsExists < shardsRequired {
		slots := state.GetUnassignedSlots()
		if len(slots) == 0 {
			return errors.New("no slots range to assign")
		}

		// Assign unbalanced slot ranges for now, i.e.
		// the last range contains more slots.
		slotStart := slots[0].Start
		slotEnd := slotStart + (16384 / shardsRequired) - 1
		if shardsRequired-shardsExists == 1 {
			if len(slots) != 1 {
				return errors.New("assigning multiple ranges to shard not yet supported")
			}
			slotEnd = slots[0].End
		}

		log.V(1).Info("Add a new primary", "slotStart", slotStart, "slotEnd", slotEnd)

		if err := node.Client.Do(ctx, node.Client.B().ClusterAddslotsrange().StartSlotEndSlot().StartSlotEndSlot(int64(slotStart), int64(slotEnd)).Build()).Error(); err != nil {
			log.Error(err, "Command failed: CLUSTER ADDSLOTSRANGE", "slotStart", slotStart, "slotEnd", slotEnd)
			return err
		}
		return nil
	}

	// Add a new replica when primary is ok
	for _, shard := range state.Shards {
		if len(shard.Nodes) < (1 + replicasRequired) {
			primary := shard.GetPrimaryNode()
			if primary == nil {
				log.Error(nil, "Primary lost in shard", "Shard Id", shard.Id)
				continue
			}

			log.V(1).Info("Add a new replica", "primary address", primary.Address, "primary Id", primary.Id, "replica address", node.Address)

			if err := node.Client.Do(ctx, node.Client.B().ClusterReplicate().NodeId(primary.Id).Build()).Error(); err != nil {
				log.Error(err, "Command failed: CLUSTER REPLICATE", "nodeId", primary.Id)
				return err
			}
			return nil
		}
	}
	return errors.New("node not added")
}

// Check each cluster node and forget stale nodes (noaddr or status fail)
func (r *ValkeyClusterReconciler) forgetStaleNodes(ctx context.Context, state *valkey.ClusterState, pods *corev1.PodList) {
	log := logf.FromContext(ctx)
	for _, shard := range state.Shards {
		for _, node := range shard.Nodes {
			// Get known nodes that are failing.
			for _, failing := range node.GetFailingNodes() {
				idx := slices.IndexFunc(pods.Items, func(p corev1.Pod) bool { return p.Status.PodIP == failing.Address })
				if idx == -1 {
					// Could not find a pod with the address of a failing node. Lets forget this node.
					log.V(1).Info("Forget a failing node", "address", failing.Address, "Id", failing.Id)
					if err := node.Client.Do(ctx, node.Client.B().ClusterForget().NodeId(failing.Id).Build()).Error(); err != nil {
						log.Error(err, "Command failed: CLUSTER FORGET")
					}
				}

			}
		}
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ValkeyClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&valkeyiov1alpha1.ValkeyCluster{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&appsv1.Deployment{}).
		Named("valkeycluster").
		Complete(r)
}
