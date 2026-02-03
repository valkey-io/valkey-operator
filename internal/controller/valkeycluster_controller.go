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
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
	"valkey.io/valkey-operator/internal/valkey"
)

const (
	DefaultPort                         = 6379
	DefaultClusterBusPort               = 16379
	DefaultImage                        = "valkey/valkey:9.0.0"
	DefaultExporterImage                = "oliver006/redis_exporter:v1.80.0"
	DefaultExporterPort                 = 9121
	ExternalAccessLabelKey              = "valkey.io/external-access"
	ExternalAccessLabelValue            = "nodeport"

	ShardIndexLabelKey   = "valkey.io/shard-index"
	ReplicaIndexLabelKey = "valkey.io/replica-index"
	RoleLabelKey         = "valkey.io/role"

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
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="apps",resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="apps",resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.0/pkg/reconcile
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

	if err := r.upsertStatefulSet(ctx, cluster); err != nil {
		setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonDeploymentError, err.Error(), metav1.ConditionFalse)
		_ = r.updateStatus(ctx, cluster, nil)
		return ctrl.Result{}, err
	}

	if cluster.Spec.AllowExternalAccess {
		if err := r.upsertNodePortServices(ctx, cluster); err != nil {
			setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonServiceError, err.Error(), metav1.ConditionFalse)
			_ = r.updateStatus(ctx, cluster, nil)
			return ctrl.Result{}, err
		}
	} else {
		if err := r.deleteNodePortServices(ctx, cluster); err != nil {
			setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonServiceError, err.Error(), metav1.ConditionFalse)
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

	r.syncPodRoleLabels(ctx, cluster, state, pods)

	// Check if we need to forget stale non-existing nodes
	r.forgetStaleNodes(ctx, cluster, state, pods)

	// Add new nodes
	if len(state.PendingNodes) > 0 {
		node := state.PendingNodes[0]
		log.V(1).Info("adding node", "address", node.Address, "Id", node.Id)
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "NodeAdding", "AddNode", "Adding node %v to cluster", node.Address)
		setCondition(cluster, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonAddingNodes, "Adding nodes to cluster", metav1.ConditionTrue)
		setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonReconciling, "Cluster is Reconciling", metav1.ConditionFalse)
		setCondition(cluster, valkeyiov1alpha1.ConditionSlotsAssigned, valkeyiov1alpha1.ReasonSlotsUnassigned, "Assigning slots to nodes", metav1.ConditionFalse)
		_ = r.updateStatus(ctx, cluster, state)
		if err := r.addValkeyNode(ctx, cluster, state, node); err != nil {
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
	startValkey, err := scripts.ReadFile("scripts/start-valkey.sh")
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
			"start-valkey.sh":    string(startValkey),
			"valkey.conf": `
cluster-enabled yes
protected-mode no
cluster-node-timeout 2000
include /data/announce.conf`,
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


// Create Valkey instances with a single StatefulSet
func (r *ValkeyClusterReconciler) upsertStatefulSet(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) error {
	log := logf.FromContext(ctx)

	existing := &appsv1.StatefulSetList{}
	if err := r.List(ctx, existing, client.InNamespace(cluster.Namespace), client.MatchingLabels(labels(cluster))); err != nil {
		log.Error(err, "failed to list StatefulSets")
		return err
	}

	expected := int(cluster.Spec.Shards * (1 + cluster.Spec.Replicas))

	if len(existing.Items) == 0 {
		statefulSet := createClusterStatefulSet(cluster, int32(expected))
		if err := controllerutil.SetControllerReference(cluster, statefulSet, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, statefulSet); err != nil {
			r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "StatefulSetCreationFailed", "Failed to create StatefulSet: %v", err)
			return err
		}
		r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "StatefulSetCreated", "Created StatefulSet with %d replicas", expected)
		return nil
	}

	// Update existing StatefulSet replica count if needed
	statefulSet := &existing.Items[0]
	updated := false
	if statefulSet.Spec.Replicas == nil || *statefulSet.Spec.Replicas != int32(expected) {
		statefulSet.Spec.Replicas = func(i int32) *int32 { return &i }(int32(expected))
		updated = true
	}
	if updated {
		if err := r.Update(ctx, statefulSet); err != nil {
			r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "StatefulSetUpdateFailed", "Failed to update StatefulSet: %v", err)
			return err
		}
	}

	return nil
}

// Create or update a NodePort service for each pod.
func (r *ValkeyClusterReconciler) upsertNodePortServices(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) error {
	log := logf.FromContext(ctx)

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(cluster.Namespace), client.MatchingLabels(labels(cluster))); err != nil {
		log.Error(err, "failed to list Pods")
		return err
	}

	services := &corev1.ServiceList{}
	if err := r.List(ctx, services); err != nil {
		log.Error(err, "failed to list Services")
		return err
	}

	externalServices := map[string]*corev1.Service{}
	for i := range services.Items {
		service := &services.Items[i]
		if service.Namespace == cluster.Namespace {
			externalServices[service.Name] = service
		}
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		svcName := pod.Name + "-nodeport"
		var nodePort int32
		var busNodePort int32
		existingSvc, hasExistingService := externalServices[svcName]

		ordinal, err := parsePodOrdinal(pod.Name)
		if err != nil {
			r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "ServiceCreationFailed", "Failed to parse pod ordinal: %v", err)
			return err
		}
		base := int32(30000)
		nodePort = base + int32(ordinal*2)
		busNodePort = base + int32(ordinal*2+1)

		desiredPorts := []corev1.ServicePort{
			{
				Name:       "valkey",
				Port:       DefaultPort,
				TargetPort: intstr.FromInt(DefaultPort),
				NodePort:   nodePort,
			},
			{
				Name:       "cluster-bus",
				Port:       DefaultClusterBusPort,
				TargetPort: intstr.FromInt(DefaultClusterBusPort),
				NodePort:   busNodePort,
			},
		}

		if hasExistingService {
			existingSvc.Spec.Type = corev1.ServiceTypeNodePort
			existingSvc.Spec.Selector = map[string]string{"statefulset.kubernetes.io/pod-name": pod.Name}
			existingSvc.Spec.Ports = desiredPorts
			if existingSvc.Labels == nil {
				existingSvc.Labels = map[string]string{}
			}
			for key, value := range labels(cluster) {
				existingSvc.Labels[key] = value
			}
			existingSvc.Labels[ExternalAccessLabelKey] = ExternalAccessLabelValue
			if err := controllerutil.SetControllerReference(cluster, existingSvc, r.Scheme); err != nil {
				return err
			}
			if err := r.Update(ctx, existingSvc); err != nil {
				r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "ServiceUpdateFailed", "Failed to update external Service: %v", err)
				return err
			}
			for _, port := range existingSvc.Spec.Ports {
				switch port.Name {
				case "valkey":
					nodePort = port.NodePort
				case "cluster-bus":
					busNodePort = port.NodePort
				}
			}
		} else {
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      svcName,
					Namespace: cluster.Namespace,
					Labels:    labels(cluster),
				},
				Spec: corev1.ServiceSpec{
					Type:     corev1.ServiceTypeNodePort,
					Selector: map[string]string{"statefulset.kubernetes.io/pod-name": pod.Name},
					Ports:    desiredPorts,
				},
			}
			svc.Labels[ExternalAccessLabelKey] = ExternalAccessLabelValue
			if err := controllerutil.SetControllerReference(cluster, svc, r.Scheme); err != nil {
				return err
			}
			if err := r.Create(ctx, svc); err != nil {
				if !apierrors.IsAlreadyExists(err) {
					r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "ServiceCreationFailed", "Failed to create external Service: %v", err)
					return err
				}
			} else {
				r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "ServiceCreated", "Created NodePort Service %s", svcName)
			}
		}

	}
	return nil
}

func (r *ValkeyClusterReconciler) deleteNodePortServices(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) error {
	log := logf.FromContext(ctx)
	services := &corev1.ServiceList{}
	if err := r.List(ctx, services, client.InNamespace(cluster.Namespace), client.MatchingLabels(labels(cluster))); err != nil {
		log.Error(err, "failed to list Services")
		return err
	}
	for i := range services.Items {
		service := &services.Items[i]
		if service.Labels[ExternalAccessLabelKey] != ExternalAccessLabelValue {
			continue
		}
		if err := r.Delete(ctx, service); err != nil && !apierrors.IsNotFound(err) {
			r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "ServiceDeletionFailed", "Failed to delete external Service: %v", err)
			return err
		}
	}
	return nil
}

func parsePodOrdinal(podName string) (int, error) {
	lastDash := strings.LastIndex(podName, "-")
	if lastDash == -1 || lastDash == len(podName)-1 {
		return 0, errors.New("pod name missing ordinal suffix")
	}
	ordinal, err := strconv.Atoi(podName[lastDash+1:])
	if err != nil {
		return 0, err
	}
	return ordinal, nil
}

func (r *ValkeyClusterReconciler) syncPodRoleLabels(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState, pods *corev1.PodList) {
	if state == nil {
		return
	}

	podsByIP := map[string]*corev1.Pod{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.PodIP == "" {
			continue
		}
		podsByIP[pod.Status.PodIP] = pod
	}

	type shardEntry struct {
		shard *valkey.ShardState
		key   int
		id    string
	}
	shardEntries := make([]shardEntry, 0, len(state.Shards))
	for _, shard := range state.Shards {
		key := 1<<31 - 1
		if len(shard.Slots) > 0 {
			key = shard.Slots[0].Start
		}
		shardEntries = append(shardEntries, shardEntry{shard: shard, key: key, id: shard.Id})
	}
	sort.Slice(shardEntries, func(i, j int) bool {
		if shardEntries[i].key != shardEntries[j].key {
			return shardEntries[i].key < shardEntries[j].key
		}
		return shardEntries[i].id < shardEntries[j].id
	})

	shardIndexByID := map[string]int{}
	for i, entry := range shardEntries {
		shardIndexByID[entry.id] = i
	}

	for _, entry := range shardEntries {
		shard := entry.shard
		nodes := make([]*valkey.NodeState, 0, len(shard.Nodes))
		nodes = append(nodes, shard.Nodes...)
		sort.Slice(nodes, func(i, j int) bool {
			if nodes[i].IsPrimary() != nodes[j].IsPrimary() {
				return nodes[i].IsPrimary()
			}
			return nodes[i].Id < nodes[j].Id
		})
		for idx, node := range nodes {
			pod, ok := podsByIP[node.Address]
			if !ok {
				continue
			}
			labelsUpdated := false
			if pod.Labels == nil {
				pod.Labels = map[string]string{}
				labelsUpdated = true
			}
			shardIndex := shardIndexByID[shard.Id]
			role := "replica"
			if node.IsPrimary() {
				role = "primary"
			}
			if pod.Labels[ShardIndexLabelKey] != strconv.Itoa(shardIndex) {
				pod.Labels[ShardIndexLabelKey] = strconv.Itoa(shardIndex)
				labelsUpdated = true
			}
			if pod.Labels[ReplicaIndexLabelKey] != strconv.Itoa(idx) {
				pod.Labels[ReplicaIndexLabelKey] = strconv.Itoa(idx)
				labelsUpdated = true
			}
			if pod.Labels[RoleLabelKey] != role {
				pod.Labels[RoleLabelKey] = role
				labelsUpdated = true
			}
			if labelsUpdated {
				if err := r.Update(ctx, pod); err != nil {
					r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "PodUpdateFailed", "Failed to update pod labels: %v", err)
					return
				}
			}
		}
	}
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

	// Add a new primary when more shards are expected.
	if shardsExists < shardsRequired {
		slots := state.GetUnassignedSlots()
		if len(slots) == 0 {
			log.Error(nil, "no unassigned slots available for new shard")
			setCondition(cluster, valkeyiov1alpha1.ConditionDegraded, valkeyiov1alpha1.ReasonNoSlots, "No unassigned slots available for new shard", metav1.ConditionTrue)
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

		log.V(1).Info("add a new primary", "slotStart", slotStart, "slotEnd", slotEnd)

		if err := node.Client.Do(ctx, node.Client.B().ClusterAddslotsrange().StartSlotEndSlot().StartSlotEndSlot(int64(slotStart), int64(slotEnd)).Build()).Error(); err != nil {
			log.Error(err, "command failed: CLUSTER ADDSLOTSRANGE", "slotStart", slotStart, "slotEnd", slotEnd)
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "SlotAssignmentFailed", "AssignSlots", "Failed to assign slots: %v", err)
			return err
		}
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "PrimaryCreated", "CreatePrimary", "Created primary with slots %d-%d", slotStart, slotEnd)
		return nil
	}

	// Add a new replica when primary is ok
	for _, shard := range state.Shards {
		if len(shard.Nodes) < (1 + replicasRequired) {
			primary := shard.GetPrimaryNode()
			if primary == nil {
				log.Error(nil, "primary lost in shard", "Shard Id", shard.Id)
				r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "PrimaryLost", "CheckPrimary", "Primary lost in shard %v", shard.Id)
				setCondition(cluster, valkeyiov1alpha1.ConditionDegraded, valkeyiov1alpha1.ReasonPrimaryLost, "Primary lost in one or more shards", metav1.ConditionTrue)
				// Cannot add replica without a primary - return error to trigger degraded state.
				return errors.New("primary lost in shard, cannot add replica")
			}

			log.V(1).Info("add a new replica", "primary address", primary.Address, "primary Id", primary.Id, "replica address", node.Address)

			if err := node.Client.Do(ctx, node.Client.B().ClusterReplicate().NodeId(primary.Id).Build()).Error(); err != nil {
				log.Error(err, "command failed: CLUSTER REPLICATE", "nodeId", primary.Id)
				r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "ReplicaCreationFailed", "CreateReplica", "Failed to create replica: %v", err)
				return err
			}
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "ReplicaCreated", "CreateReplica", "Created replica for primary %v", primary.Id)
			return nil
		}
	}
	return errors.New("node not added")
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
