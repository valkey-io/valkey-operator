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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
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
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="apps",resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="apps",resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="storage.k8s.io",resources=storageclasses,verbs=get;list;watch
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
//  6. For every pending node (primary with no slots and cluster_known_nodes
//     <= 1), introduce it to the cluster. Only one pending node is processed
//     per reconcile to allow gossip to propagate before the next step.
//  7. Verify that the expected number of shards and replicas exist.
//  8. Verify that all 16384 hash slots are assigned.
//  9. If everything is healthy, mark the cluster Ready and requeue after 30s
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

	if err := r.upsertConfigMap(ctx, cluster); err != nil {
		setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonConfigMapError, err.Error(), metav1.ConditionFalse)
		_ = r.updateStatus(ctx, cluster, nil)
		return ctrl.Result{}, err
	}

	// Use StatefulSets if persistent storage is enabled, otherwise use Deployments
	if cluster.Spec.Storage.Enabled {
		if err := r.upsertStatefulSets(ctx, cluster); err != nil {
			setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonDeploymentError, err.Error(), metav1.ConditionFalse)
			_ = r.updateStatus(ctx, cluster, nil)
			return ctrl.Result{}, err
		}

		// Handle volume expansion if storage size has changed
		if err := r.handleVolumeExpansion(ctx, cluster); err != nil {
			log.Error(err, "volume expansion failed")
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "VolumeExpansionFailed", "VolumeExpansion", "Volume expansion failed: %v", err)
			// Don't return error - allow reconciliation to continue
			// The expansion will be retried on the next reconciliation
		}

		// Check if volume expansion is in progress
		expansionComplete, err := r.checkVolumeExpansionStatus(ctx, cluster)
		if err != nil {
			log.Error(err, "failed to check volume expansion status")
		} else if !expansionComplete {
			log.V(1).Info("volume expansion in progress, will recheck")
			setCondition(cluster, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonVolumeExpanding, "Volume expansion in progress", metav1.ConditionTrue)
			_ = r.updateStatus(ctx, cluster, nil)
			// Requeue more frequently to monitor expansion progress
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
	} else {
		if err := r.upsertDeployments(ctx, cluster); err != nil {
			setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonDeploymentError, err.Error(), metav1.ConditionFalse)
			_ = r.updateStatus(ctx, cluster, nil)
			return ctrl.Result{}, err
		}
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

	// Process one pending node per reconcile. A "pending" node is a Valkey
	// node with no slots assigned (see clusterstate.go). We handle only one at
	// a time so that gossip has a chance to propagate the topology change to
	// all members before the next node is introduced. After addValkeyNode
	// returns, we requeue after 2 seconds.
	//
	// We prioritize node-index 0 (primary) over higher indices (replicas).
	// This is important because replicateToShardPrimary needs the primary
	// to already be in state.Shards (i.e. have slots assigned). If we
	// processed a replica first, its primary might still be in PendingNodes
	// and the lookup would fail.
	if len(state.PendingNodes) > 0 {
		node := pickPendingNode(state.PendingNodes, pods)
		log.V(1).Info("adding node", "address", node.Address, "Id", node.Id)
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "NodeAdding", "AddNode", "Adding node %v to cluster", node.Address)
		setCondition(cluster, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonAddingNodes, "Adding nodes to cluster", metav1.ConditionTrue)
		setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonReconciling, "Cluster is Reconciling", metav1.ConditionFalse)
		setCondition(cluster, valkeyiov1alpha1.ConditionSlotsAssigned, valkeyiov1alpha1.ReasonSlotsUnassigned, "Assigning slots to nodes", metav1.ConditionFalse)
		_ = r.updateStatus(ctx, cluster, state)
		if err := r.addValkeyNode(ctx, cluster, state, node, pods); err != nil {
			log.Error(err, "unable to add cluster node")
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "NodeAddFailed", "AddNode", "Failed to add node: %v", err)
			setCondition(cluster, valkeyiov1alpha1.ConditionDegraded, valkeyiov1alpha1.ReasonNodeAddFailed, err.Error(), metav1.ConditionTrue)
			_ = r.updateStatus(ctx, cluster, state)
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "NodeAdded", "AddNode", "Node %v joined cluster", node.Address)
		// Let the added node stabilize, and refetch the cluster state.
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// Check cluster status
	if len(state.Shards) < int(cluster.Spec.Shards) {
		log.V(1).Info("missing shards, requeue..")
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "WaitingForShards", "CheckShards", "%d of %d shards exist", len(state.Shards), cluster.Spec.Shards)
		setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonMissingShards, "Waiting for all shards to be created", metav1.ConditionFalse)
		setCondition(cluster, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonReconciling, "Creating shards", metav1.ConditionTrue)
		setCondition(cluster, valkeyiov1alpha1.ConditionClusterFormed, valkeyiov1alpha1.ReasonMissingShards, "Waiting for shards", metav1.ConditionFalse)
		_ = r.updateStatus(ctx, cluster, state)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	for _, shard := range state.Shards {
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
dir /data
appendonly yes
appendfilename "appendonly.aof"
appendfsync everysec`,
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

// Create Valkey instances using StatefulSets with persistent volumes
func (r *ValkeyClusterReconciler) upsertStatefulSets(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) error {
	log := logf.FromContext(ctx)

	existing := &appsv1.StatefulSetList{}
	if err := r.List(ctx, existing, client.InNamespace(cluster.Namespace), client.MatchingLabels(labels(cluster))); err != nil {
		log.Error(err, "failed to list StatefulSets")
		return err
	}

	expected := int(cluster.Spec.Shards * (1 + cluster.Spec.Replicas))

	// Create missing statefulsets
	for i := len(existing.Items); i < expected; i++ {
		statefulSet := createClusterStatefulSet(cluster)
		if err := controllerutil.SetControllerReference(cluster, statefulSet, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, statefulSet); err != nil {
			r.Recorder.Eventf(cluster, statefulSet, corev1.EventTypeWarning, "StatefulSetCreationFailed", "CreateStatefulSet", "Failed to create statefulset: %v", err)
			return err
		}
		r.Recorder.Eventf(cluster, statefulSet, corev1.EventTypeNormal, "StatefulSetCreated", "CreateStatefulSet", "Created statefulset %d of %d", i+1, expected)
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

// addValkeyNode introduces a pending Valkey node into the cluster. The node's
// intended role is derived from its pod labels (e.g. shard-index=0, node-index=0
// for the initial primary, shard-index=1, node-index=1 for a replica), which
// was set at Deployment-creation time by upsertDeployments. This removes all
// guesswork from the reconciler:
//
//  1. MEET: if the node is isolated (cluster_known_nodes <= 1), introduce it
//     to existing cluster members via CLUSTER MEET and return. The next
//     reconcile will proceed once gossip propagates.
//
//  2. PRIMARY: if node index is 0, assign the next available slot range via
//     CLUSTER ADDSLOTSRANGE.
//
//  3. REPLICA: if node index is >= 1, find the node-0 (primary) pod for the
//     same shard, look up its Valkey node ID, and issue CLUSTER REPLICATE.
func (r *ValkeyClusterReconciler) addValkeyNode(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState, node *valkey.NodeState, pods *corev1.PodList) error {
	log := logf.FromContext(ctx)

	// --- Step 1: MEET if this node is isolated ---
	// A freshly-started Valkey node only knows itself (cluster_known_nodes=1).
	// Before we can assign slots or replicate, the node must be introduced to
	// at least one existing cluster member via CLUSTER MEET. We MEET every
	// known primary so that the new node learns the full topology through
	// gossip rather than depending on a single point of contact. After the
	// MEET we return immediately; the next reconcile will see
	// cluster_known_nodes > 1 and proceed to Step 2 or 3.
	if sval, ok := node.ClusterInfo["cluster_known_nodes"]; ok {
		if val, err := strconv.Atoi(sval); err == nil {
			if val <= 1 && len(state.Shards) > 0 {
				for _, shard := range state.Shards {
					primary := shard.GetPrimaryNode()
					if primary == nil {
						continue
					}
					log.V(1).Info("meet other node", "this node", node.Address, "other node", primary.Address)
					if err = node.Client.Do(ctx, node.Client.B().ClusterMeet().Ip(primary.Address).Port(int64(primary.Port)).Build()).Error(); err != nil {
						log.Error(err, "command failed: CLUSTER MEET", "from", node.Address, "to", primary.Address)
						r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "ClusterMeetFailed", "ClusterMeet", "CLUSTER MEET failed: %v", err)
						return err
					}
					r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "ClusterMeet", "ClusterMeet", "Node %v met node %v", node.Address, primary.Address)
				}
				return nil
			}
		}
	}

	// --- Resolve pod labels for this node ---
	role, shardIndex := podRoleAndShard(node.Address, pods)

	// --- Step 2: PRIMARY – assign slots ---
	if role == RolePrimary {
		return r.assignSlotsToNewPrimary(ctx, cluster, state, node)
	}

	// --- Step 3: REPLICA – replicate to the matching primary ---
	if role == RoleReplica {
		return r.replicateToShardPrimary(ctx, cluster, state, node, shardIndex, pods)
	}

	return errors.New("cannot determine node role from pod name")
}

// assignSlotsToNewPrimary assigns the next available hash-slot range to a
// node, promoting it to a slot-bearing primary.
//
// Slot range calculation:
//
//	Each shard gets TotalSlots/shardsRequired slots (integer division).
//	The last shard absorbs any remainder so that exactly 16384 slots are
//	covered. For example, with 3 shards: shard 0 gets 0-5460, shard 1 gets
//	5461-10921, and shard 2 gets 10922-16383.
//
// We only assign a contiguous range from the first unassigned gap. The last
// shard is special-cased: if it is the final shard to create, it takes
// everything that remains (slots[0].End) to avoid rounding issues.
func (r *ValkeyClusterReconciler) assignSlotsToNewPrimary(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState, node *valkey.NodeState) error {
	log := logf.FromContext(ctx)
	shardsRequired := int(cluster.Spec.Shards)
	shardsExists := len(state.Shards)

	slots := state.GetUnassignedSlots()
	if len(slots) == 0 {
		log.Error(nil, "no unassigned slots available for new shard")
		setCondition(cluster, valkeyiov1alpha1.ConditionDegraded, valkeyiov1alpha1.ReasonNoSlots, "No unassigned slots available for new shard", metav1.ConditionTrue)
		return errors.New("no slots range to assign")
	}

	// Compute the slot range for this shard.
	slotStart := slots[0].Start
	slotEnd := slotStart + (valkey.TotalSlots / shardsRequired) - 1
	if shardsRequired-shardsExists == 1 {
		// Last shard: absorb remaining slots to cover all 16384.
		if len(slots) != 1 {
			return errors.New("assigning multiple ranges to shard not yet supported")
		}
		slotEnd = slots[0].End
	}

	log.V(1).Info("add a new primary", "slotStart", slotStart, "slotEnd", slotEnd)
	if err := node.Client.Do(ctx, node.Client.B().ClusterAddslotsrange().StartSlotEndSlot().StartSlotEndSlot(int64(slotStart), int64(slotEnd)).Build()).Error(); err != nil {
		log.Error(err, "command failed: CLUSTER ADDSLOTSRANGE", "slotStart", slotStart, "slotEnd", slotEnd)
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "SlotAssignmentFailed", "AssignSlots", "Failed to assign slots: %v", err)
		return err
	}
	r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "PrimaryCreated", "CreatePrimary", "Created primary with slots %d-%d", slotStart, slotEnd)
	return nil
}

// replicateToShardPrimary issues CLUSTER REPLICATE to attach this node as a
// replica of the primary in the same shard.
//
// The lookup happens in two stages because Kubernetes and Valkey have
// independent identity systems:
//
//  1. Kubernetes side: find the node-0 (primary) pod for the same shard by
//     matching pod labels (shard-index=N, node-index=0). This
//     gives us the IP.
//
//  2. Valkey side: scan the cluster state for a primary node whose address
//     matches that IP. This gives us the Valkey node ID required by
//     CLUSTER REPLICATE.
//
// If either lookup fails (e.g. the primary pod hasn't started yet, or its
// Valkey process hasn't joined the cluster), we return an error and the
// reconciler retries on the next cycle.
func (r *ValkeyClusterReconciler) replicateToShardPrimary(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState, node *valkey.NodeState, shardIndex int, pods *corev1.PodList) error {
	log := logf.FromContext(ctx)

	// Stage 1: Kubernetes lookup — find the node-0 (primary) pod by name prefix.
	primaryIP := primaryPodIP(pods, shardIndex)
	if primaryIP == "" {
		return errors.New("primary pod not found for shard " + strconv.Itoa(shardIndex))
	}

	// Stage 2: Valkey lookup — translate pod IP to Valkey node ID.
	var primaryNodeId string
	for _, shard := range state.Shards {
		primary := shard.GetPrimaryNode()
		if primary != nil && primary.Address == primaryIP {
			primaryNodeId = primary.Id
			break
		}
	}
	if primaryNodeId == "" {
		return errors.New("primary Valkey node not found in cluster state for shard " + strconv.Itoa(shardIndex))
	}

	log.V(1).Info("add a new replica", "primary IP", primaryIP, "primary Id", primaryNodeId, "replica address", node.Address, "shardIndex", shardIndex)
	if err := node.Client.Do(ctx, node.Client.B().ClusterReplicate().NodeId(primaryNodeId).Build()).Error(); err != nil {
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

// countReadyShards counts shards that have all required nodes and are healthy
func (r *ValkeyClusterReconciler) countReadyShards(state *valkey.ClusterState, cluster *valkeyiov1alpha1.ValkeyCluster) int32 {
	var readyCount int32 = 0
	requiredNodes := 1 + int(cluster.Spec.Replicas)
	for _, shard := range state.Shards {
		if len(shard.Nodes) < requiredNodes || shard.GetPrimaryNode() == nil {
			continue
		}
		// Check if all nodes in this shard are healthy
		allHealthy := true
		for _, node := range shard.Nodes {
			if slices.Contains(node.Flags, "fail") || slices.Contains(node.Flags, "pfail") {
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
