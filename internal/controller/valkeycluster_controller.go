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
	"fmt"
	"slices"
	"strings"
	"time"

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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
	"valkey.io/valkey-operator/internal/valkey"
)

const (
	DefaultPort           = 6379
	DefaultClusterBusPort = 16379
	DefaultImage          = "valkey/valkey:9.0.0"
	DefaultExporterImage  = "oliver006/redis_exporter:v1.80.0"
	DefaultExporterPort   = 9121

	// Error messages
	statusUpdateFailedMsg = "failed to update status"
)

// ValkeyClusterReconciler reconciles a ValkeyCluster object
type ValkeyClusterReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

//go:embed scripts/*
var scripts embed.FS

// +kubebuilder:rbac:groups=valkey.io,resources=valkeyclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=valkey.io,resources=valkeyclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=valkey.io,resources=valkeyclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="apps",resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile is the main reconciliation loop. On each invocation it drives the
// cluster one step closer to the desired state described by the ValkeyCluster
// spec. The pipeline runs in the following order:
//
//  1. Ensure the headless Service exists (upsertService).
//  2. Ensure the ConfigMap with valkey.conf and health-check scripts exists
//     (upsertConfigMap).
//  3. Ensure one Deployment per (shard, node) pair exists, each named
//     deterministically (e.g. mycluster-0-0) (upsertDeployments).
//  4. List all pods and build the Valkey cluster state by connecting to each
//     node and scraping CLUSTER INFO / CLUSTER NODES.
//  5. Forget stale nodes that no longer have a backing pod.
//  6. Phase 1 – MEET: batch-introduce all isolated pending nodes to the
//     cluster via CLUSTER MEET. Requeue to let gossip propagate.
//  7. Phase 2 – Assign slots: batch-assign hash-slot ranges to all
//     primary-labeled pending nodes via CLUSTER ADDSLOTSRANGE.
//  8. Phase 3 – Replicate: batch-attach all replica-labeled pending nodes
//     to their matching primaries via CLUSTER REPLICATE.
//  9. Verify that the expected number of shards and replicas exist.
//  10. Verify that all 16384 hash slots are assigned.
//  11. If everything is healthy, mark the cluster Ready and requeue after 30s
//     for periodic health checks.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.1/pkg/reconcile
func (r *ValkeyClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.V(1).Info("reconcile...")

	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := r.upsertService(ctx, cluster); err != nil {
		setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonServiceError, err.Error(), metav1.ConditionFalse)
		_ = r.updateStatus(ctx, cluster, nil)
		return ctrl.Result{}, err
	}

	if err := r.reconcileUsersAcl(ctx, cluster); err != nil {
		setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonUsersAclError, err.Error(), metav1.ConditionFalse)
		return ctrl.Result{}, err
	}

	if err := r.upsertConfigMap(ctx, cluster); err != nil {
		setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonConfigMapError, err.Error(), metav1.ConditionFalse)
		_ = r.updateStatus(ctx, cluster, nil)
		return ctrl.Result{}, err
	}

	if err := r.upsertDeployments(ctx, cluster); err != nil {
		setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonDeploymentError, err.Error(), metav1.ConditionFalse)
		_ = r.updateStatus(ctx, cluster, nil)
		return ctrl.Result{}, err
	}

	// Get all pods and their current Valkey Cluster state
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(cluster.Namespace), client.MatchingLabels(labels(cluster))); err != nil {
		log.Error(err, "failed to list Pods")
		setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonPodListError, err.Error(), metav1.ConditionFalse)
		_ = r.updateStatus(ctx, cluster, nil)
		return ctrl.Result{}, err
	}
	state := r.getValkeyClusterState(ctx, pods)
	defer state.CloseClients()

	// Check if we need to forget stale non-existing nodes
	r.forgetStaleNodes(ctx, cluster, state, pods)

	// --- Phase 1: MEET all isolated nodes in one batch ---
	// A node with cluster_known_nodes <= 1 hasn't been introduced to the
	// cluster yet. CLUSTER MEET is idempotent and has no ordering
	// dependencies, so we issue it for every isolated pending node in a
	// single reconcile pass. Phase 2 refuses to assign slots to isolated
	// nodes, so every node is guaranteed to pass through here first.
	// After MEET, we requeue to let gossip propagate before proceeding
	// to slot assignment or replication.
	{
		met, err := r.meetIsolatedNodes(ctx, cluster, state)
		if err != nil {
			setCondition(cluster, valkeyiov1alpha1.ConditionDegraded, valkeyiov1alpha1.ReasonNodeAddFailed, err.Error(), metav1.ConditionTrue)
			_ = r.updateStatus(ctx, cluster, state)
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		if met > 0 {
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "ClusterMeetBatch", "ClusterMeet", "Introduced %d isolated node(s) to the cluster", met)
			setCondition(cluster, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonAddingNodes, "Introducing nodes to cluster", metav1.ConditionTrue)
			setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonReconciling, "Cluster is Reconciling", metav1.ConditionFalse)
			_ = r.updateStatus(ctx, cluster, state)
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
	}

	// --- Phase 2: Assign slots to all primary-labeled pending nodes ---
	// Slot assignment (CLUSTER ADDSLOTSRANGE) makes a pending node a
	// slot-owning primary. During scale-out (no unassigned slots), this
	// is a no-op; new primaries stay in PendingNodes until the rebalancer
	// migrates slots to them.
	if len(state.PendingNodes) > 0 {
		assigned, err := r.assignSlotsToPendingPrimaries(ctx, cluster, state, pods)
		if err != nil {
			setCondition(cluster, valkeyiov1alpha1.ConditionDegraded, valkeyiov1alpha1.ReasonNodeAddFailed, err.Error(), metav1.ConditionTrue)
			_ = r.updateStatus(ctx, cluster, state)
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		if assigned > 0 {
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "PrimariesCreated", "AssignSlots", "Assigned slots to %d new primary node(s)", assigned)
			setCondition(cluster, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonAddingNodes, "Assigning slots to primaries", metav1.ConditionTrue)
			setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonReconciling, "Cluster is Reconciling", metav1.ConditionFalse)
			_ = r.updateStatus(ctx, cluster, state)
			// Requeue so that the next reconcile sees the newly-created shards
			// in state.Shards, which replicas need for their lookup.
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
	}

	// --- Phase 3: REPLICATE all replica-labeled pending nodes ---
	// By this point all currently known primaries have slots and appear in state.Shards.
	// CLUSTER REPLICATE for different replicas targets different primaries,
	// so they can all be issued in one pass.
	if len(state.PendingNodes) > 0 {
		replicated, err := r.replicatePendingReplicas(ctx, cluster, state, pods)
		if err != nil {
			setCondition(cluster, valkeyiov1alpha1.ConditionDegraded, valkeyiov1alpha1.ReasonNodeAddFailed, err.Error(), metav1.ConditionTrue)
			_ = r.updateStatus(ctx, cluster, state)
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		if replicated > 0 {
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "ReplicasAttached", "CreateReplica", "Attached %d replica node(s)", replicated)
			setCondition(cluster, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonAddingNodes, "Attaching replicas", metav1.ConditionTrue)
			setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonReconciling, "Cluster is Reconciling", metav1.ConditionFalse)
			_ = r.updateStatus(ctx, cluster, state)
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
	}

	// Build the effective shard list: state.Shards plus any pending primaries
	// that are scale-out leaders. During scale-out, GetClusterState places
	// new slot-less primaries in PendingNodes because it can't distinguish
	// them from unreplicated replicas. We use pod labels to identify them
	// and include them as empty shards for health checks and rebalancing.
	allShards := effectiveShards(state, pods)

	// Check cluster status
	if len(allShards) < int(cluster.Spec.Shards) {
		log.V(1).Info("missing shards, requeue..")
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "WaitingForShards", "CheckShards", "%d of %d shards exist", len(allShards), cluster.Spec.Shards)
		setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonMissingShards, "Waiting for all shards to be created", metav1.ConditionFalse)
		setCondition(cluster, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonReconciling, "Creating shards", metav1.ConditionTrue)
		setCondition(cluster, valkeyiov1alpha1.ConditionClusterFormed, valkeyiov1alpha1.ReasonMissingShards, "Waiting for shards", metav1.ConditionFalse)
		_ = r.updateStatus(ctx, cluster, state)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	for _, shard := range allShards {
		if countSlots(shard.Slots) == 0 {
			continue
		}
		if len(shard.Nodes) < (1 + int(cluster.Spec.Replicas)) {
			log.V(1).Info("missing replicas, requeue..")
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "WaitingForReplicas", "CheckReplicas", "Shard has %d of %d nodes", len(shard.Nodes), 1+int(cluster.Spec.Replicas))
			setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonMissingReplicas, "Waiting for all replicas to be created", metav1.ConditionFalse)
			setCondition(cluster, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonReconciling, "Creating replicas", metav1.ConditionTrue)
			setCondition(cluster, valkeyiov1alpha1.ConditionClusterFormed, valkeyiov1alpha1.ReasonMissingReplicas, "Waiting for replicas", metav1.ConditionFalse)
			_ = r.updateStatus(ctx, cluster, state)
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
	}

	// Check if all slots are assigned
	unassignedSlots := state.GetUnassignedSlots()
	allSlotsAssigned := len(unassignedSlots) == 0
	if !allSlotsAssigned {
		log.V(1).Info("slots are not assigned, requeue..", "unassignedSlots", unassignedSlots)
		setCondition(cluster, valkeyiov1alpha1.ConditionSlotsAssigned, valkeyiov1alpha1.ReasonSlotsUnassigned, "Waiting for slots to be assigned", metav1.ConditionFalse)
		setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonReconciling, "Waiting for all slots to be assigned", metav1.ConditionFalse)
		setCondition(cluster, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonReconciling, "Waiting for slots to be assigned", metav1.ConditionTrue)
		setCondition(cluster, valkeyiov1alpha1.ConditionClusterFormed, valkeyiov1alpha1.ReasonSlotsUnassigned, "Waiting for slots to be assigned", metav1.ConditionFalse)
		_ = r.updateStatus(ctx, cluster, state)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// Check that all replicas have their replication link up (master_link_status:up).
	// before marking the cluster Ready, we need to make sure all replicas are in sync with their primary.
	for _, shard := range allShards {
		for _, node := range shard.Nodes {
			if !node.IsReplicationInSync() {
				log.V(1).Info("replica not yet in sync, requeue..", "address", node.Address)
				setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonReconciling, "Waiting for replicas to sync with primary", metav1.ConditionFalse)
				setCondition(cluster, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonReconciling, "Waiting for replica sync", metav1.ConditionTrue)
				_ = r.updateStatus(ctx, cluster, state)
				return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
			}
		}
	}

	// Rebalance slots across primaries if needed (scale-out).
	rebalanced, err := r.rebalanceSlots(ctx, cluster, allShards)
	if err != nil {
		log.Error(err, "slot rebalancing failed")
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "SlotRebalanceFailed", "RebalanceSlots", "Slot rebalancing failed: %v", err)
		setCondition(cluster, valkeyiov1alpha1.ConditionDegraded, valkeyiov1alpha1.ReasonRebalanceFailed, err.Error(), metav1.ConditionTrue)
		setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonReconciling, "Cluster is Reconciling", metav1.ConditionFalse)
		setCondition(cluster, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonRebalancingSlots, "Rebalancing slots across primaries", metav1.ConditionTrue)
		_ = r.updateStatus(ctx, cluster, state)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	if rebalanced {
		// Clear any stale Degraded condition from earlier phases (e.g.
		// NodeAddFailed set when replicas couldn't attach to primaries
		// that hadn't received slots yet during scale-out).
		meta.RemoveStatusCondition(&cluster.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)
		setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonReconciling, "Cluster is Reconciling", metav1.ConditionFalse)
		setCondition(cluster, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonRebalancingSlots, "Rebalancing slots across primaries", metav1.ConditionTrue)
		_ = r.updateStatus(ctx, cluster, state)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// Cluster is healthy - set all positive conditions
	r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "ClusterReady", "ReconcileCluster", "Cluster ready with %d shards and %d replicas", cluster.Spec.Shards, cluster.Spec.Replicas)
	setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonClusterHealthy, "Cluster is healthy", metav1.ConditionTrue)
	setCondition(cluster, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonReconcileComplete, "No changes needed", metav1.ConditionFalse)
	meta.RemoveStatusCondition(&cluster.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)
	setCondition(cluster, valkeyiov1alpha1.ConditionClusterFormed, valkeyiov1alpha1.ReasonTopologyComplete, "All nodes joined cluster", metav1.ConditionTrue)
	setCondition(cluster, valkeyiov1alpha1.ConditionSlotsAssigned, valkeyiov1alpha1.ReasonAllSlotsAssigned, "All slots assigned", metav1.ConditionTrue)

	if err := r.updateStatus(ctx, cluster, state); err != nil {
		log.Error(err, statusUpdateFailedMsg)
		return ctrl.Result{}, err
	}

	log.V(1).Info("reconcile done")
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
				r.Recorder.Eventf(cluster, svc, corev1.EventTypeWarning, "ServiceUpdateFailed", "UpdateService", "Failed to update Service: %v", err)
				return err
			}
		} else {
			return err
		}
	} else {
		r.Recorder.Eventf(cluster, svc, corev1.EventTypeNormal, "ServiceCreated", "CreateService", "Created headless Service")
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
cluster-node-timeout 2000
aclfile /config/users/users.acl`,
		},
	}
	if err := controllerutil.SetControllerReference(cluster, cm, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, cm); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if err := r.Update(ctx, cm); err != nil {
				r.Recorder.Eventf(cluster, cm, corev1.EventTypeWarning, "ConfigMapUpdateFailed", "UpdateConfigMap", "Failed to update ConfigMap: %v", err)
				return err
			}
		} else {
			r.Recorder.Eventf(cluster, cm, corev1.EventTypeWarning, "ConfigMapCreationFailed", "CreateConfigMap", "Failed to create ConfigMap: %v", err)
			return err
		}
	} else {
		r.Recorder.Eventf(cluster, cm, corev1.EventTypeNormal, "ConfigMapCreated", "CreateConfigMap", "Created ConfigMap with configuration")
	}
	return nil
}

// upsertDeployments ensures every (shard, nodeIndex) pair has a Deployment.
// Each Deployment manages exactly one Pod (Replicas=1) and is named
// deterministically:
//
//	<cluster>-<N>-<M>
//
// where N is the shard index and M is the node index (0 = initial primary,
// 1+ = replicas). Because the names are deterministic, the function is
// idempotent: it tries to create each Deployment and silently ignores
// AlreadyExists errors.
//
// For a 3-shard cluster with 2 replicas per shard, this produces 9 Deployments:
//
//	mycluster-0-0, mycluster-0-1, mycluster-0-2,
//	mycluster-1-0, mycluster-1-1, mycluster-1-2,
//	mycluster-2-0, mycluster-2-1, mycluster-2-2.
func (r *ValkeyClusterReconciler) upsertDeployments(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) error {
	log := logf.FromContext(ctx)

	nodesPerShard := 1 + int(cluster.Spec.Replicas)
	created := 0
	expected := int(cluster.Spec.Shards) * nodesPerShard
	for shard := range int(cluster.Spec.Shards) {
		for ni := range nodesPerShard {
			if err := r.ensureDeployment(ctx, cluster, shard, ni, expected, &created); err != nil {
				return err
			}
		}
	}
	if created > 0 {
		log.V(1).Info("created deployments", "count", created)
	}

	// TODO: update existing Deployments when the spec changes (e.g. image upgrade).

	return nil
}

// ensureDeployment creates a single Deployment if it doesn't already exist.
func (r *ValkeyClusterReconciler) ensureDeployment(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, shard int, nodeIndex int, expected int, created *int) error {
	deployment := createClusterDeployment(cluster, shard, nodeIndex)
	if err := controllerutil.SetControllerReference(cluster, deployment, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, deployment); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		r.Recorder.Eventf(cluster, deployment, corev1.EventTypeWarning, "DeploymentCreationFailed", "CreateDeployment", "Failed to create deployment: %v", err)
		return err
	}
	*created++
	r.Recorder.Eventf(cluster, deployment, corev1.EventTypeNormal, "DeploymentCreated", "CreateDeployment", "Created deployment for shard %d node %d (%d of %d)", shard, nodeIndex, *created, expected)
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

// findMeetTarget picks the best node to MEET all isolated nodes against.
// Priority: (1) a non-isolated shard primary — already has slots, so gossip
// will propagate slot info; (2) a non-isolated pending node from a previous
// MEET batch; (3) the first isolated node as a bootstrap seed when every
// single node is isolated (fresh bootstrap, first reconcile).
func findMeetTarget(state *valkey.ClusterState, isolated []*valkey.NodeState) *valkey.NodeState {
	for _, shard := range state.Shards {
		if p := shard.GetPrimaryNode(); p != nil && !p.IsIsolated() {
			return p
		}
	}
	for _, node := range state.PendingNodes {
		if !node.IsIsolated() {
			return node
		}
	}
	return isolated[0]
}

// meetIsolatedNodes issues CLUSTER MEET for every isolated pending node
// (cluster_known_nodes <= 1). Phase 2 (assignSlotsToPendingPrimaries)
// refuses to assign slots to isolated nodes, so every node is guaranteed
// to pass through this function before receiving slots or replicating.
//
// MEET is idempotent and has no ordering dependencies, so all isolated nodes
// can be introduced in a single pass. We pick a single "meet target" node
// and MEET all others against it:
//
//   - If any non-isolated node exists (shard primary with cluster_known_nodes
//     > 1, or a pending node from a previous MEET batch), use it as the
//     target so new nodes join the existing cluster.
//   - If all nodes are isolated (fresh bootstrap), use the first isolated
//     node as a bootstrap seed. All others MEET this seed, and gossip
//     propagates the full topology from there.
//
// Returns the number of nodes that were MEET'd.
func (r *ValkeyClusterReconciler) meetIsolatedNodes(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState) (int, error) {
	log := logf.FromContext(ctx)

	var isolated []*valkey.NodeState
	for _, node := range state.PendingNodes {
		if node.IsIsolated() {
			isolated = append(isolated, node)
		}
	}
	if len(isolated) == 0 {
		return 0, nil
	}

	// Find a well-connected node to MEET all isolated nodes against.
	// Falls back to an isolated node as a bootstrap seed if every node
	// is isolated (fresh bootstrap).
	meetTarget := findMeetTarget(state, isolated)
	if meetTarget == isolated[0] {
		isolated = isolated[1:]
	}

	met := 0
	for _, node := range isolated {
		// Bidirectional MEET: the isolated node MEETs the target, AND the
		// target MEETs the isolated node. Bidirectional MEET avoids the
		// fragmentation problem where one-way MEET + slow gossip creates
		// subclusters: by having the target actively reach out, the new
		// node is guaranteed to appear in the target's node table
		// immediately, not after gossip propagation.
		log.V(1).Info("meet node", "node", node.Address, "target", meetTarget.Address)
		if err := node.Client.Do(ctx, node.Client.B().ClusterMeet().Ip(meetTarget.Address).Port(int64(meetTarget.Port)).Build()).Error(); err != nil {
			log.Error(err, "CLUSTER MEET failed", "from", node.Address, "to", meetTarget.Address)
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "ClusterMeetFailed", "ClusterMeet", "CLUSTER MEET %v -> %v failed: %v", node.Address, meetTarget.Address, err)
			return met, err
		}
		if err := meetTarget.Client.Do(ctx, meetTarget.Client.B().ClusterMeet().Ip(node.Address).Port(int64(node.Port)).Build()).Error(); err != nil {
			log.Error(err, "CLUSTER MEET failed", "from", meetTarget.Address, "to", node.Address)
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "ClusterMeetFailed", "ClusterMeet", "CLUSTER MEET %v -> %v failed: %v", meetTarget.Address, node.Address, err)
			return met, err
		}
		met++
	}
	return met, nil
}

// assignSlotsToPendingPrimaries assigns hash-slot ranges to non-isolated
// pending nodes whose pod labels indicate they are primaries (node index 0
// within their shard). During scale-out (no unassigned slots available) it
// returns 0 — new primaries stay in PendingNodes for the rebalancer to handle.
// Isolated nodes (cluster_known_nodes <= 1) are skipped; they must be MEET'd
// first in Phase 1.
func (r *ValkeyClusterReconciler) assignSlotsToPendingPrimaries(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState, pods *corev1.PodList) (int, error) {
	log := logf.FromContext(ctx)
	shardsRequired := int(cluster.Spec.Shards)

	// Collect primary pending nodes (node index 0 = primary), skipping:
	//  - isolated nodes (cluster_known_nodes <= 1): they haven't been
	//    MEET'd yet and must go through Phase 1 first. Without this guard
	//    a single pod that starts before its peers would get slots while
	//    isolated, creating a disconnected shard primary.
	//  - post-failover replacements whose shard already has a live primary.
	primaries := make([]*valkey.NodeState, 0, len(state.PendingNodes))
	for _, node := range state.PendingNodes {
		if node.IsIsolated() {
			continue
		}
		role, shardIndex := podRoleAndShard(node.Address, pods)
		if role != RolePrimary {
			continue
		}
		if shardExistsInTopology(state, shardIndex, pods) {
			log.V(1).Info("skipping slot assignment; shard already exists in topology (post-failover)",
				"shardIndex", shardIndex, "node", node.Address)
			continue
		}
		primaries = append(primaries, node)
	}
	if len(primaries) == 0 {
		return 0, nil
	}

	slots := state.GetUnassignedSlots()
	if len(slots) == 0 {
		// Scale-out: all slots already assigned to existing primaries.
		// New primaries stay in PendingNodes; the rebalancer will find
		// them and migrate slots to them.
		log.V(1).Info("no unassigned slots; new primaries will receive slots via rebalance", "count", len(primaries))
		return 0, nil
	}

	assigned := 0
	shardsExists := len(state.Shards)
	for _, node := range primaries {
		if len(slots) == 0 {
			break
		}

		slotStart := slots[0].Start
		numSlotsPerShard := valkey.TotalSlots / shardsRequired
		shardIdx := shardsExists + assigned
		// Distribute the remainder evenly: the first (TotalSlots % shardsRequired)
		// shards each get one extra slot.
		if shardIdx < valkey.TotalSlots%shardsRequired {
			numSlotsPerShard++
		}
		slotEnd := slotStart + numSlotsPerShard - 1

		log.V(1).Info("assign slots to primary", "node", node.Address, "slotStart", slotStart, "slotEnd", slotEnd)
		if err := node.Client.Do(ctx, node.Client.B().ClusterAddslotsrange().StartSlotEndSlot().StartSlotEndSlot(int64(slotStart), int64(slotEnd)).Build()).Error(); err != nil {
			log.Error(err, "CLUSTER ADDSLOTSRANGE failed", "node", node.Address, "slotStart", slotStart, "slotEnd", slotEnd)
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "SlotAssignmentFailed", "AssignSlots", "Failed to assign slots %d-%d to %v: %v", slotStart, slotEnd, node.Address, err)
			return assigned, err
		}
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "PrimaryCreated", "CreatePrimary", "Created primary %v with slots %d-%d", node.Address, slotStart, slotEnd)
		assigned++

		// Update the unassigned slots for the next iteration by removing
		// the range we just assigned.
		var remainingSlots []valkey.SlotsRange
		for _, s := range slots {
			remainingSlots = append(remainingSlots, valkey.SubtractSlotsRange(s, valkey.SlotsRange{Start: slotStart, End: slotEnd})...)
		}
		slots = remainingSlots
	}
	return assigned, nil
}

// errPrimaryNotReady is returned by replicateToShardPrimary when the
// primary for a shard hasn't received slots yet (e.g. during scale-out,
// while the rebalancer is still migrating slots), or when gossip hasn't
// propagated the primary's node ID to the replica yet. This is not a
// fatal error — the replica will be retried on a future reconcile.
var errPrimaryNotReady = errors.New("primary not yet in cluster state (awaiting rebalance)")

// replicatePendingReplicas issues CLUSTER REPLICATE for all pending nodes
// whose pod labels indicate they are replicas (node index 1+), as well as
// post-failover replacement primaries (node index 0 whose shard already has
// a live primary). During initial creation all primaries should already
// have slots, but during scale-out new primaries may still be empty while the
// rebalancer migrates slots to them. Replicas whose primary isn't ready yet
// are silently skipped and retried on the next reconcile.
// Returns the number of replicas that were attached.
func (r *ValkeyClusterReconciler) replicatePendingReplicas(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState, pods *corev1.PodList) (int, error) {
	log := logf.FromContext(ctx)
	replicated := 0

	for _, node := range state.PendingNodes {
		role, shardIndex := podRoleAndShard(node.Address, pods)
		// Normal replicas (node-index >= 1) always need replication.
		// Post-failover replacements (node-index=0 but shard already has
		// a live primary) also need replication — they were skipped by
		// assignSlotsToPendingPrimaries.
		if role == RolePrimary {
			if !shardExistsInTopology(state, shardIndex, pods) {
				continue // genuine new primary, handled by assignSlotsToPendingPrimaries
			}
			log.V(1).Info("post-failover: attaching replacement node as replica",
				"shardIndex", shardIndex, "node", node.Address)
		}

		if err := r.replicateToShardPrimary(ctx, cluster, state, node, shardIndex, pods); err != nil {
			if errors.Is(err, errPrimaryNotReady) {
				log.V(1).Info("skipping replica; primary not ready yet", "node", node.Address, "shard", shardIndex)
				continue
			}
			log.Error(err, "failed to replicate pending node", "node", node.Address, "shard", shardIndex)
			return replicated, err
		}
		replicated++
	}
	return replicated, nil
}

// replicateToShardPrimary issues CLUSTER REPLICATE to attach this node as a
// replica of the primary in the same shard.
//
// The primary is found by scanning all pods in the shard against the live
// Valkey topology (via findShardPrimary). This handles both the normal case
// (node-index=0 is the primary) and the post-failover case (a promoted
// replica is the primary). If no primary is found (e.g. the primary pod
// hasn't started yet or hasn't joined the cluster), we return an error and
// the reconciler retries on the next cycle.
func (r *ValkeyClusterReconciler) replicateToShardPrimary(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState, node *valkey.NodeState, shardIndex int, pods *corev1.PodList) error {
	log := logf.FromContext(ctx)

	// Find the actual primary for this shard by scanning all shard pods
	// against the live Valkey topology. This handles both the normal case
	// (node-index=0 is the primary) and the post-failover case (a promoted
	// replica is the primary).
	primaryNodeId, primaryIP := findShardPrimary(state, shardIndex, pods)
	if primaryNodeId == "" {
		return fmt.Errorf("shard %d: %w", shardIndex, errPrimaryNotReady)
	}

	log.V(1).Info("add a new replica", "primary IP", primaryIP, "primary Id", primaryNodeId, "replica address", node.Address, "shardIndex", shardIndex)
	if err := node.Client.Do(ctx, node.Client.B().ClusterReplicate().NodeId(primaryNodeId).Build()).Error(); err != nil {
		// "Unknown node" means gossip hasn't propagated the primary's ID to
		// this replica yet. This is transient and will resolve on the next
		// reconcile once gossip catches up — treat it as retriable.
		if strings.Contains(err.Error(), "Unknown node") {
			log.V(1).Info("replica does not yet know primary (gossip pending); will retry", "replica", node.Address, "primaryId", primaryNodeId)
			return fmt.Errorf("shard %d: %w", shardIndex, errPrimaryNotReady)
		}
		log.Error(err, "command failed: CLUSTER REPLICATE", "nodeId", primaryNodeId)
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "ReplicaCreationFailed", "CreateReplica", "Failed to create replica: %v", err)
		return err
	}
	r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "ReplicaCreated", "CreateReplica", "Created replica for primary %v (shard %d)", primaryNodeId, shardIndex)
	return nil
}

// Check each cluster node and forget stale nodes (noaddr or status fail)
func (r *ValkeyClusterReconciler) forgetStaleNodes(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState, pods *corev1.PodList) {
	log := logf.FromContext(ctx)
	for _, shard := range state.Shards {
		for _, node := range shard.Nodes {
			// Get known nodes that are failing.
			for _, failing := range node.GetFailingNodes() {
				idx := slices.IndexFunc(pods.Items, func(p corev1.Pod) bool { return p.Status.PodIP == failing.Address })
				if idx == -1 {
					// Could not find a pod with the address of a failing node. Lets forget this node.
					log.V(1).Info("forget a failing node", "address", failing.Address, "Id", failing.Id)
					if err := node.Client.Do(ctx, node.Client.B().ClusterForget().NodeId(failing.Id).Build()).Error(); err != nil {
						log.Error(err, "command failed: CLUSTER FORGET")
						r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "NodeForgetFailed", "ForgetNode", "Failed to forget node: %v", err)
					} else {
						r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "StaleNodeForgotten", "ForgetNode", "Forgot stale node %v", failing.Address)
					}
				}

			}
		}
	}
}

// updateStatus updates the status with the current conditions and computes the Valkey Cluster state
func (r *ValkeyClusterReconciler) updateStatus(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState) error {
	log := logf.FromContext(ctx)
	// Fetch current status to compare
	current := &valkeyiov1alpha1.ValkeyCluster{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(cluster), current); err != nil {
		return err
	}
	// Update shard counts
	if state != nil {
		cluster.Status.ReadyShards = r.countReadyShards(state, cluster)
		cluster.Status.Shards = int32(len(state.Shards))
	}
	// compute Valkey Cluster state from conditions (priority order: Degraded > Ready > Progressing > Failed)
	readyCondition := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionReady)
	progressingCondition := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionProgressing)
	degradedCondition := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)

	switch {
	case degradedCondition != nil && degradedCondition.Status == metav1.ConditionTrue:
		cluster.Status.State = valkeyiov1alpha1.ClusterStateDegraded
		cluster.Status.Reason = degradedCondition.Reason
		cluster.Status.Message = degradedCondition.Message
	case readyCondition != nil && readyCondition.Status == metav1.ConditionTrue:
		cluster.Status.State = valkeyiov1alpha1.ClusterStateReady
		cluster.Status.Reason = readyCondition.Reason
		cluster.Status.Message = readyCondition.Message
	case progressingCondition != nil && progressingCondition.Status == metav1.ConditionTrue:
		cluster.Status.State = valkeyiov1alpha1.ClusterStateReconciling
		cluster.Status.Reason = progressingCondition.Reason
		cluster.Status.Message = progressingCondition.Message
	case readyCondition != nil && readyCondition.Status == metav1.ConditionFalse:
		cluster.Status.State = valkeyiov1alpha1.ClusterStateFailed
		cluster.Status.Reason = readyCondition.Reason
		cluster.Status.Message = readyCondition.Message
	}

	// Only update if status has changed
	if statusChanged(current.Status, cluster.Status) {
		if err := r.Status().Update(ctx, cluster); err != nil {
			log.Error(err, statusUpdateFailedMsg)
			return err
		}
		log.V(1).Info("status updated", "state", cluster.Status.State, "reason", cluster.Status.Reason)
	} else {
		log.V(2).Info("status unchanged, skipping update")
	}
	return nil
}

// countReadyShards counts shards that have all required nodes, are healthy,
// and have all replicas in sync with their primary.
func (r *ValkeyClusterReconciler) countReadyShards(state *valkey.ClusterState, cluster *valkeyiov1alpha1.ValkeyCluster) int32 {
	var readyCount int32 = 0
	requiredNodes := 1 + int(cluster.Spec.Replicas)
	for _, shard := range state.Shards {
		if len(shard.Nodes) < requiredNodes || shard.GetPrimaryNode() == nil {
			continue
		}
		// Check if all nodes in this shard are healthy and in sync
		allHealthy := true
		for _, node := range shard.Nodes {
			if slices.Contains(node.Flags, "fail") || slices.Contains(node.Flags, "pfail") {
				allHealthy = false
				break
			}
			if !node.IsReplicationInSync() {
				allHealthy = false
				break
			}
		}
		if allHealthy {
			readyCount++
		}
	}
	return readyCount
}

// effectiveShards returns state.Shards plus any pending primaries that are
// scale-out leaders (slot-less Valkey masters labeled as RolePrimary).
func effectiveShards(state *valkey.ClusterState, pods *corev1.PodList) []*valkey.ShardState {
	shards := append([]*valkey.ShardState(nil), state.Shards...)
	for _, node := range state.PendingNodes {
		role, _ := podRoleAndShard(node.Address, pods)
		if role == RolePrimary {
			shards = append(shards, &valkey.ShardState{
				Id:        node.ShardId,
				Nodes:     []*valkey.NodeState{node},
				PrimaryId: node.Id,
			})
		}
	}
	return shards
}

// SetupWithManager sets up the controller with the Manager.
func (r *ValkeyClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&valkeyiov1alpha1.ValkeyCluster{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&appsv1.Deployment{}).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.findReferencedClusters),
		).
		Named("valkeycluster").
		Complete(r)
}
