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

package v1alpha1

import (
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ClusterState represents the high-level state of the ValkeyCluster.
// +kubebuilder:validation:Enum=Initializing;Reconciling;Ready;Degraded;Failed
type ClusterState string

const (
	// ClusterStateInitializing indicates the cluster is being created for the first time.
	ClusterStateInitializing ClusterState = "Initializing"
	// ClusterStateReconciling indicates the cluster is being updated.
	ClusterStateReconciling ClusterState = "Reconciling"
	// ClusterStateReady indicates the cluster is healthy and serving traffic.
	ClusterStateReady ClusterState = "Ready"
	// ClusterStateDegraded indicates the cluster is partially functional.
	ClusterStateDegraded ClusterState = "Degraded"
	// ClusterStateFailed indicates the cluster has failed and cannot recover.
	ClusterStateFailed ClusterState = "Failed"
)

// ClusterStates lists all possible ValkeyCluster states.
var ClusterStates = []ClusterState{
	ClusterStateInitializing,
	ClusterStateReconciling,
	ClusterStateReady,
	ClusterStateDegraded,
	ClusterStateFailed,
}

// PDBMode selects how the operator manages PodDisruptionBudgets for the cluster.
// Additional values may be added in future versions; clients MUST handle unknown
// values gracefully by falling back to default behaviour.
// +kubebuilder:validation:Enum=Cluster;Disabled
type PDBMode string

const (
	// PDBModeCluster manages one cluster-wide PDB with maxUnavailable: 1.
	PDBModeCluster PDBMode = "Cluster"
	// PDBModeDisabled manages no PDB.
	PDBModeDisabled PDBMode = "Disabled"
)

// PodDisruptionBudgetConfig configures operator-managed PodDisruptionBudgets.
type PodDisruptionBudgetConfig struct {
	// Mode selects the PDB strategy. Defaults to Cluster.
	// +kubebuilder:default=Cluster
	// +optional
	Mode PDBMode `json:"mode,omitempty"`
}

// UnmarshalJSON accepts both the current object form and the legacy
// string form ("Managed"/"Disabled"). It exists only so objects still stored
// under the old string form decode instead of failing the whole informer list
// decode during an operator upgrade. Removable at v1beta1.
func (c *PodDisruptionBudgetConfig) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		switch s {
		case "Managed":
			*c = PodDisruptionBudgetConfig{Mode: PDBModeCluster}
		case "Disabled":
			*c = PodDisruptionBudgetConfig{Mode: PDBModeDisabled}
		default:
			*c = PodDisruptionBudgetConfig{Mode: PDBMode(s)}
		}
		return nil
	}
	type alias PodDisruptionBudgetConfig
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*c = PodDisruptionBudgetConfig(a)
	return nil
}

// SchedulingSpec groups pod placement configuration for the cluster's pods.
// These fields are rendered onto each ValkeyNode workload the cluster creates.
type SchedulingSpec struct {
	// Tolerations to apply to the pods
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// NodeSelector to apply to the pods
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Affinity to apply to the pods. Kubernetes ANDs nodeAffinity with
	// NodeSelector rather than one overriding the other: a node must satisfy
	// both for the pod to be scheduled there.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// TopologySpreadConstraints to apply to the pods
	// +optional
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`

	// PriorityClassName is the name of an existing PriorityClass applied to
	// every pod in the cluster, protecting them from eviction under resource
	// pressure. Pod priority is a scheduling concern (preemption, scheduling-queue
	// order), so it lives alongside the other placement fields.
	// +optional
	PriorityClassName string `json:"priorityClassName,omitempty"`

	// Node groups scheduling constraints on the node axis
	// (topologyKey kubernetes.io/hostname). When unset, every node spread is
	// Disabled and no scheduling primitives are emitted.
	// +optional
	Node *NodeScheduling `json:"node,omitempty"`

	// Zone groups scheduling constraints on the zone axis
	// (topologyKey topology.kubernetes.io/zone). When unset, every zone spread is
	// Disabled and no scheduling primitives are emitted.
	// +optional
	Zone *ZoneScheduling `json:"zone,omitempty"`
}

// SpreadMode selects the strength of a spread constraint.
// +kubebuilder:validation:Enum=Disabled;Preferred;Required
type SpreadMode string

const (
	// SpreadDisabled emits no scheduling primitive for the dimension.
	SpreadDisabled SpreadMode = "Disabled"
	// SpreadPreferred is best-effort: it biases placement but can never leave a
	// pod unschedulable.
	SpreadPreferred SpreadMode = "Preferred"
	// SpreadRequired is a hard constraint: a pod that cannot satisfy it stays
	// Pending.
	SpreadRequired SpreadMode = "Required"
)

// SpreadConstraint configures one spread dimension.
type SpreadConstraint struct {
	// Mode selects the strength of the constraint. When unset, the dimension's
	// documented default applies.
	// +optional
	Mode SpreadMode `json:"mode,omitempty"`
}

// NodeSpread controls how the cluster's pods are distributed across nodes
// (topologyKey kubernetes.io/hostname).
type NodeSpread struct {
	// Shard keeps pods of the same shard on distinct nodes, rendered as pod
	// anti-affinity. Defaults to Disabled when unset.
	// +optional
	Shard SpreadConstraint `json:"shard,omitempty"`

	// Primaries balances each shard's node-index-0 pod across nodes, rendered as
	// a topology spread constraint.
	// Defaults to Disabled when unset.
	// +optional
	Primaries SpreadConstraint `json:"primaries,omitempty"`

	// Pods balances all of the cluster's pods across nodes, rendered as a
	// topology spread constraint. Defaults to Disabled when unset. Enabling this
	// alongside an explicit primaries spread of the same strength is rejected
	// (only one topology spread constraint per strength is permitted per node).
	// +optional
	Pods SpreadConstraint `json:"pods,omitempty"`
}

// NodeScheduling groups scheduling constraints on the node axis
// (topologyKey kubernetes.io/hostname).
type NodeScheduling struct {
	// Spread distributes the cluster's pods across nodes.
	// +optional
	Spread NodeSpread `json:"spread,omitempty"`
}

// ZoneSpread controls how the cluster's pods are distributed across zones
// (topologyKey topology.kubernetes.io/zone). Every dimension renders as a
// topology spread constraint — balancing, not anti-affinity — so zone shard
// members may share a zone when the shard is larger than the zone count.
type ZoneSpread struct {
	// Shard balances the pods of each shard across zones, rendered as a topology
	// spread constraint. Defaults to Disabled when unset.
	// +optional
	Shard SpreadConstraint `json:"shard,omitempty"`

	// Primaries balances each shard's node-index-0 pod across zones, rendered as
	// a topology spread constraint. Defaults to Disabled when unset.
	// +optional
	Primaries SpreadConstraint `json:"primaries,omitempty"`

	// Pods balances all of the cluster's pods across zones, rendered as a
	// topology spread constraint. Defaults to Disabled when unset. Enabling this
	// alongside an explicit shard or primaries spread of the same strength is
	// rejected (only one topology spread constraint per strength is permitted per
	// zone).
	// +optional
	Pods SpreadConstraint `json:"pods,omitempty"`
}

// ZoneScheduling groups scheduling constraints on the zone axis
// (topologyKey topology.kubernetes.io/zone).
type ZoneScheduling struct {
	// Spread distributes the cluster's pods across zones.
	// +optional
	Spread ZoneSpread `json:"spread,omitempty"`

	// Pinning assigns every pod a deterministic zone by round-robin over an
	// ordered zone list. Mutually exclusive with any non-Disabled Spread field
	// on this axis: pinning already fixes each pod's zone, so a zone spread
	// constraint can only contradict it. Unset means no pinning.
	// +optional
	Pinning *ZonePinning `json:"pinning,omitempty"`
}

// ZonePinning assigns every pod a deterministic zone. Rendered as a
// topology.kubernetes.io/zone entry in the pod's nodeSelector, which Kubernetes
// ANDs with any affinity the user supplies through the escape hatch.
// +kubebuilder:validation:XValidation:rule="self.zones.all(z, self.zones.exists_one(y, y == z))",message="zone.pinning.zones entries must be unique: a repeated zone silently biases the round-robin"
type ZonePinning struct {
	// Zones is the ordered round-robin sequence. A pod's zone is
	// zones[(shardIndex + nodeIndex) % len(zones)], so each shard's pods walk
	// the list from a different starting point, and while there are at least
	// as many zones as shards, primaries land in distinct zones as a side
	// effect. When replicas+1 exceeds the zone count, some members of a shard
	// necessarily share a zone.
	//
	// Adding shards or replicas never moves an existing pod, because a pod's
	// indices never change. Changing this list would move nearly all of them,
	// so it is immutable while set: remove pinning, reconcile, then re-add it
	// with the new sequence. On a cluster with persistence, note that zonal
	// volumes cannot follow a pod to a new zone.
	//
	// The list is ordered, so it is atomic rather than a set: a set list is
	// unordered under server-side apply, which would scramble the assignment.
	// Uniqueness is enforced by CEL for the same reason.
	//
	// Each entry must be a valid Kubernetes label value, because it is rendered
	// as the value of a topology.kubernetes.io/zone nodeSelector entry. Without
	// that constraint an entry such as "eu-west-1a!" is accepted here and then
	// rejected when the StatefulSet is created, surfacing on the ValkeyNode
	// rather than on the ValkeyCluster the user edited.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`
	// +listType=atomic
	Zones []string `json:"zones"`
}

// ValkeyClusterSpec defines the desired state of ValkeyCluster.
// +kubebuilder:validation:XValidation:rule="!(has(self.persistence) && self.workloadType == 'Deployment')",message="persistence requires workloadType StatefulSet"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.persistence) || has(self.persistence)",message="persistence cannot be removed once set"
// +kubebuilder:validation:XValidation:rule="has(oldSelf.persistence) || !has(self.persistence)",message="persistence cannot be added after creation"
// +kubebuilder:validation:XValidation:rule="!has(self.persistence) || !has(oldSelf.persistence) || quantity(self.persistence.size).compareTo(quantity(oldSelf.persistence.size)) >= 0",message="persistence.size may only be expanded"
// +kubebuilder:validation:XValidation:rule="!has(self.persistence) || !has(oldSelf.persistence) || ((!has(self.persistence.storageClassName) && !has(oldSelf.persistence.storageClassName)) || (has(self.persistence.storageClassName) && has(oldSelf.persistence.storageClassName) && self.persistence.storageClassName == oldSelf.persistence.storageClassName))",message="persistence.storageClassName is immutable"
//
// node.spread: reject primaries and pods both Required (they would render duplicate hostname DoNotSchedule constraints).
// +kubebuilder:validation:XValidation:rule="!has(self.scheduling) || !has(self.scheduling.node) || !has(self.scheduling.node.spread) || !( ((has(self.scheduling.node.spread.primaries) && has(self.scheduling.node.spread.primaries.mode)) ? self.scheduling.node.spread.primaries.mode : 'Disabled') == 'Required' && ((has(self.scheduling.node.spread.pods) && has(self.scheduling.node.spread.pods.mode)) ? self.scheduling.node.spread.pods.mode : 'Disabled') == 'Required' )",message="node.spread.primaries and node.spread.pods cannot both be Required: they render duplicate kubernetes.io/hostname DoNotSchedule topology spread constraints"
//
// node.spread: reject primaries and pods both Preferred (they would render duplicate hostname ScheduleAnyway constraints).
// +kubebuilder:validation:XValidation:rule="!has(self.scheduling) || !has(self.scheduling.node) || !has(self.scheduling.node.spread) || !( ((has(self.scheduling.node.spread.primaries) && has(self.scheduling.node.spread.primaries.mode)) ? self.scheduling.node.spread.primaries.mode : 'Disabled') == 'Preferred' && ((has(self.scheduling.node.spread.pods) && has(self.scheduling.node.spread.pods.mode)) ? self.scheduling.node.spread.pods.mode : 'Disabled') == 'Preferred' )",message="node.spread.primaries and node.spread.pods cannot both be Preferred: they render duplicate kubernetes.io/hostname ScheduleAnyway topology spread constraints (set one to Disabled or Required)"
//
// node.spread: reject a user hostname DoNotSchedule TSC that collides with a Required primaries/pods spread.
// +kubebuilder:validation:XValidation:rule="!has(self.scheduling) || !has(self.scheduling.topologySpreadConstraints) || !self.scheduling.topologySpreadConstraints.exists(c, c.topologyKey == 'kubernetes.io/hostname' && c.whenUnsatisfiable == 'DoNotSchedule') || !has(self.scheduling.node) || !has(self.scheduling.node.spread) || !( ((has(self.scheduling.node.spread.primaries) && has(self.scheduling.node.spread.primaries.mode)) ? self.scheduling.node.spread.primaries.mode : 'Disabled') == 'Required' || ((has(self.scheduling.node.spread.pods) && has(self.scheduling.node.spread.pods.mode)) ? self.scheduling.node.spread.pods.mode : 'Disabled') == 'Required' )",message="a topologySpreadConstraints entry on kubernetes.io/hostname with whenUnsatisfiable DoNotSchedule collides with node.spread.primaries or node.spread.pods set to Required, which render the same hostname DoNotSchedule constraint: set that node.spread mode to Disabled, or remove the passthrough constraint"
//
// node.spread: reject a user hostname ScheduleAnyway TSC that collides with a Preferred primaries/pods spread.
// +kubebuilder:validation:XValidation:rule="!has(self.scheduling) || !has(self.scheduling.topologySpreadConstraints) || !self.scheduling.topologySpreadConstraints.exists(c, c.topologyKey == 'kubernetes.io/hostname' && c.whenUnsatisfiable == 'ScheduleAnyway') || !has(self.scheduling.node) || !has(self.scheduling.node.spread) || !( ((has(self.scheduling.node.spread.primaries) && has(self.scheduling.node.spread.primaries.mode)) ? self.scheduling.node.spread.primaries.mode : 'Disabled') == 'Preferred' || ((has(self.scheduling.node.spread.pods) && has(self.scheduling.node.spread.pods.mode)) ? self.scheduling.node.spread.pods.mode : 'Disabled') == 'Preferred' )",message="a topologySpreadConstraints entry on kubernetes.io/hostname with whenUnsatisfiable ScheduleAnyway collides with node.spread.primaries or node.spread.pods set to Preferred, which render the same hostname ScheduleAnyway constraint: set that node.spread mode to Disabled or Required, or remove the passthrough constraint"
//
// zone.spread: reject more than one of shard/primaries/pods Required (they would render duplicate zone DoNotSchedule constraints).
// +kubebuilder:validation:XValidation:rule="!has(self.scheduling) || !has(self.scheduling.zone) || !has(self.scheduling.zone.spread) || !( (((has(self.scheduling.zone.spread.shard) && has(self.scheduling.zone.spread.shard.mode)) ? self.scheduling.zone.spread.shard.mode : 'Disabled') == 'Required' && ((has(self.scheduling.zone.spread.primaries) && has(self.scheduling.zone.spread.primaries.mode)) ? self.scheduling.zone.spread.primaries.mode : 'Disabled') == 'Required') || (((has(self.scheduling.zone.spread.shard) && has(self.scheduling.zone.spread.shard.mode)) ? self.scheduling.zone.spread.shard.mode : 'Disabled') == 'Required' && ((has(self.scheduling.zone.spread.pods) && has(self.scheduling.zone.spread.pods.mode)) ? self.scheduling.zone.spread.pods.mode : 'Disabled') == 'Required') || (((has(self.scheduling.zone.spread.primaries) && has(self.scheduling.zone.spread.primaries.mode)) ? self.scheduling.zone.spread.primaries.mode : 'Disabled') == 'Required' && ((has(self.scheduling.zone.spread.pods) && has(self.scheduling.zone.spread.pods.mode)) ? self.scheduling.zone.spread.pods.mode : 'Disabled') == 'Required') )",message="at most one of zone.spread.shard, zone.spread.primaries, zone.spread.pods may be Required: they render duplicate topology.kubernetes.io/zone DoNotSchedule topology spread constraints"
//
// zone.spread: reject more than one of shard/primaries/pods Preferred (they would render duplicate zone ScheduleAnyway constraints).
// +kubebuilder:validation:XValidation:rule="!has(self.scheduling) || !has(self.scheduling.zone) || !has(self.scheduling.zone.spread) || !( (((has(self.scheduling.zone.spread.shard) && has(self.scheduling.zone.spread.shard.mode)) ? self.scheduling.zone.spread.shard.mode : 'Disabled') == 'Preferred' && ((has(self.scheduling.zone.spread.primaries) && has(self.scheduling.zone.spread.primaries.mode)) ? self.scheduling.zone.spread.primaries.mode : 'Disabled') == 'Preferred') || (((has(self.scheduling.zone.spread.shard) && has(self.scheduling.zone.spread.shard.mode)) ? self.scheduling.zone.spread.shard.mode : 'Disabled') == 'Preferred' && ((has(self.scheduling.zone.spread.pods) && has(self.scheduling.zone.spread.pods.mode)) ? self.scheduling.zone.spread.pods.mode : 'Disabled') == 'Preferred') || (((has(self.scheduling.zone.spread.primaries) && has(self.scheduling.zone.spread.primaries.mode)) ? self.scheduling.zone.spread.primaries.mode : 'Disabled') == 'Preferred' && ((has(self.scheduling.zone.spread.pods) && has(self.scheduling.zone.spread.pods.mode)) ? self.scheduling.zone.spread.pods.mode : 'Disabled') == 'Preferred') )",message="at most one of zone.spread.shard, zone.spread.primaries, zone.spread.pods may be Preferred: they render duplicate topology.kubernetes.io/zone ScheduleAnyway topology spread constraints (set one to Disabled or Required)"
//
// zone.spread: reject a user zone DoNotSchedule TSC that collides with a Required shard/primaries/pods spread.
// +kubebuilder:validation:XValidation:rule="!has(self.scheduling) || !has(self.scheduling.topologySpreadConstraints) || !self.scheduling.topologySpreadConstraints.exists(c, c.topologyKey == 'topology.kubernetes.io/zone' && c.whenUnsatisfiable == 'DoNotSchedule') || !has(self.scheduling.zone) || !has(self.scheduling.zone.spread) || !( ((has(self.scheduling.zone.spread.shard) && has(self.scheduling.zone.spread.shard.mode)) ? self.scheduling.zone.spread.shard.mode : 'Disabled') == 'Required' || ((has(self.scheduling.zone.spread.primaries) && has(self.scheduling.zone.spread.primaries.mode)) ? self.scheduling.zone.spread.primaries.mode : 'Disabled') == 'Required' || ((has(self.scheduling.zone.spread.pods) && has(self.scheduling.zone.spread.pods.mode)) ? self.scheduling.zone.spread.pods.mode : 'Disabled') == 'Required' )",message="a topologySpreadConstraints entry on topology.kubernetes.io/zone with whenUnsatisfiable DoNotSchedule collides with zone.spread.shard, primaries, or pods set to Required, which render the same zone DoNotSchedule constraint: set that zone.spread mode to Disabled, or remove the passthrough constraint"
//
// zone.spread: reject a user zone ScheduleAnyway TSC that collides with a Preferred shard/primaries/pods spread.
// +kubebuilder:validation:XValidation:rule="!has(self.scheduling) || !has(self.scheduling.topologySpreadConstraints) || !self.scheduling.topologySpreadConstraints.exists(c, c.topologyKey == 'topology.kubernetes.io/zone' && c.whenUnsatisfiable == 'ScheduleAnyway') || !has(self.scheduling.zone) || !has(self.scheduling.zone.spread) || !( ((has(self.scheduling.zone.spread.shard) && has(self.scheduling.zone.spread.shard.mode)) ? self.scheduling.zone.spread.shard.mode : 'Disabled') == 'Preferred' || ((has(self.scheduling.zone.spread.primaries) && has(self.scheduling.zone.spread.primaries.mode)) ? self.scheduling.zone.spread.primaries.mode : 'Disabled') == 'Preferred' || ((has(self.scheduling.zone.spread.pods) && has(self.scheduling.zone.spread.pods.mode)) ? self.scheduling.zone.spread.pods.mode : 'Disabled') == 'Preferred' )",message="a topologySpreadConstraints entry on topology.kubernetes.io/zone with whenUnsatisfiable ScheduleAnyway collides with zone.spread.shard, primaries, or pods set to Preferred, which render the same zone ScheduleAnyway constraint: set that zone.spread mode to Disabled or Required, or remove the passthrough constraint"
//
// zone.pinning: reject pinning combined with any non-Disabled zone spread (pinning already fixes each pod's zone).
// +kubebuilder:validation:XValidation:rule="!has(self.scheduling) || !has(self.scheduling.zone) || !has(self.scheduling.zone.pinning) || !has(self.scheduling.zone.spread) || ( ((has(self.scheduling.zone.spread.shard) && has(self.scheduling.zone.spread.shard.mode)) ? self.scheduling.zone.spread.shard.mode : 'Disabled') == 'Disabled' && ((has(self.scheduling.zone.spread.primaries) && has(self.scheduling.zone.spread.primaries.mode)) ? self.scheduling.zone.spread.primaries.mode : 'Disabled') == 'Disabled' && ((has(self.scheduling.zone.spread.pods) && has(self.scheduling.zone.spread.pods.mode)) ? self.scheduling.zone.spread.pods.mode : 'Disabled') == 'Disabled' )",message="zone.pinning cannot be combined with a non-Disabled zone.spread.shard, primaries, or pods: pinning already assigns every pod a fixed zone, which a zone spread constraint can contradict"
//
// zone.pinning: the zone list is immutable while set (changing it reassigns nearly every pod); removal and re-adding are allowed.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.scheduling) || !has(oldSelf.scheduling.zone) || !has(oldSelf.scheduling.zone.pinning) || !has(self.scheduling) || !has(self.scheduling.zone) || !has(self.scheduling.zone.pinning) || self.scheduling.zone.pinning.zones == oldSelf.scheduling.zone.pinning.zones",message="zone.pinning.zones is immutable while set: changing it reassigns nearly every pod. Remove zone.pinning, reconcile, then re-add it with the new list"
//
// zone.pinning: reject a passthrough nodeSelector that sets the zone key the pinning render owns.
// +kubebuilder:validation:XValidation:rule="!has(self.scheduling) || !has(self.scheduling.zone) || !has(self.scheduling.zone.pinning) || !has(self.scheduling.nodeSelector) || !('topology.kubernetes.io/zone' in self.scheduling.nodeSelector)",message="scheduling.nodeSelector cannot set topology.kubernetes.io/zone while zone.pinning is set: pinning renders that key itself, and the curated value would overwrite yours"
//
// discovery: Hostname announce needs stable StatefulSet pod names.
// +kubebuilder:validation:XValidation:rule="!has(self.networking) || !has(self.networking.discovery) || !has(self.networking.discovery.preferredEndpointType) || self.networking.discovery.preferredEndpointType != 'Hostname' || !has(self.workloadType) || self.workloadType == 'StatefulSet'",message="networking.discovery.preferredEndpointType Hostname requires workloadType StatefulSet (or omit workloadType for the StatefulSet default)"
type ValkeyClusterSpec struct {

	// Override the default Valkey image
	Image string `json:"image,omitempty"`

	// ImagePullSecrets is a list of references to Secrets in the same namespace used for
	// pulling any of the images (Valkey server, metrics exporter, and any additional
	// containers) from private registries.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// The number of shards groups. Each shard group contains one primary and N replicas.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	Shards int32 `json:"shards"`

	// The number of replicas for each shard group.
	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas,omitempty"`

	// Override resource requirements for the Valkey container in each pod
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Scheduling groups pod placement configuration (affinity, node selector,
	// tolerations, topology spread constraints, priority class) for the
	// cluster's pods.
	// +optional
	Scheduling *SchedulingSpec `json:"scheduling,omitempty"`

	// Metrics exporter options
	// +kubebuilder:default:={enabled:true}
	// +optional
	Exporter ExporterSpec `json:"exporter,omitempty"`

	// WorkloadType specifies whether ValkeyNodes create StatefulSets or Deployments.
	// +kubebuilder:default=StatefulSet
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="workloadType is immutable"
	// +optional
	WorkloadType WorkloadType `json:"workloadType,omitempty"`

	// Persistence defines durable storage that is propagated to each ValkeyNode.
	// +optional
	Persistence *PersistenceSpec `json:"persistence,omitempty"`

	// Users, and ACL-related configuration; see valkeyacls_types.go
	// +listType=map
	// +listMapKey=name
	Users []UserAclSpec `json:"users,omitempty"`

	// Additional containers or overrides for existing containers, applied using strategic merge patch
	// +optional
	Containers []corev1.Container `json:"containers,omitempty"`

	// Additional Valkey configuration parameters
	// +optional
	Config map[string]string `json:"config,omitempty"`

	// TerminationGracePeriodSeconds is the pod termination grace period for the
	// Valkey nodes. It must give the graceful CLUSTER FAILOVER triggered on
	// SIGTERM (shutdown-on-sigterm) enough time to hand the shard off to a
	// replica before SIGKILL. When unset, the operator derives a safe default
	// from cluster-manual-failover-timeout. A value below that derived minimum
	// is honoured but reported as a warning.
	// +kubebuilder:validation:Minimum=1
	// +optional
	TerminationGracePeriodSeconds *int64 `json:"terminationGracePeriodSeconds,omitempty"`

	// Networking groups how clients and peers reach cluster nodes (TLS,
	// in-cluster discovery announce, and later external access).
	// +optional
	Networking *NetworkingSpec `json:"networking,omitempty"`

	// PodDisruptionBudget configures the operator-managed PodDisruptionBudget(s)
	// for this cluster. When unset, the operator applies the default (Cluster) mode.
	// +optional
	PodDisruptionBudget *PodDisruptionBudgetConfig `json:"podDisruptionBudget,omitempty"`

	// Override the PodSecurityContext applied to each ValkeyNode pod of the cluster.
	// When set, this overrides the default PodSecurityContext.
	// +optional
	PodSecurityContext *corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`
}

// PreferredEndpointType mirrors valkey's cluster-preferred-endpoint-type directive.
// +kubebuilder:validation:Enum=IP;Hostname
type PreferredEndpointType string

const (
	// PreferredEndpointTypeIP announces pod IPs (default).
	PreferredEndpointTypeIP PreferredEndpointType = "IP"
	// PreferredEndpointTypeHostname announces stable per-pod DNS names under the
	// cluster headless Service.
	PreferredEndpointTypeHostname PreferredEndpointType = "Hostname"
)

// NetworkingSpec groups connectivity configuration for the cluster.
type NetworkingSpec struct {
	// ClusterDomain is the DNS suffix kubelet publishes Service DNS under
	// (kubelet --cluster-domain). Used when building Hostname announce FQDNs.
	// Must match the cluster; the API cannot validate that. Default cluster.local.
	// +kubebuilder:default="cluster.local"
	// +optional
	ClusterDomain string `json:"clusterDomain,omitempty"`

	// Discovery configures in-cluster endpoint announcement after CLUSTER SLOTS.
	// +optional
	Discovery *DiscoverySpec `json:"discovery,omitempty"`

	// TLS configuration for the cluster.
	// +optional
	TLS *TLSSpec `json:"tls,omitempty"`
}

// DiscoverySpec configures how nodes announce themselves for in-cluster clients.
type DiscoverySpec struct {
	// PreferredEndpointType selects IP (default) or Hostname announcement.
	// Hostname uses per-pod DNS under the cluster headless Service
	// (<pod>.<headless>.<namespace>.svc.<clusterDomain>) and requires
	// workloadType StatefulSet (or the default).
	// +kubebuilder:default=IP
	// +optional
	PreferredEndpointType PreferredEndpointType `json:"preferredEndpointType,omitempty"`
}

// TLSSpec defines the TLS configuration for ValkeyCluster.
type TLSSpec struct {
	// Certificates holds the certificate slots used by the cluster.
	// +kubebuilder:validation:Required
	Certificates TLSCertificates `json:"certificates"`
}

// TLSCertificates groups the certificate slots for a ValkeyCluster. Today
// `server` is the only slot; the trust-source, outbound-identity and
// control-plane-identity slots land in later phases of #360.
type TLSCertificates struct {
	// Server is the node identity presented to clients and peers, and the
	// trust root for the cluster. The referenced secret must contain:
	//
	// - `ca.crt`: The certificate authority.
	// - `tls.crt`: The certificate (or a chain).
	// - `tls.key`: The private key to the first certificate in the certificate chain.
	// +kubebuilder:validation:Required
	Server CertificateSource `json:"server"`
}

// GetTLS returns the cluster TLS config from spec.networking.tls, or nil.
func (c *ValkeyCluster) GetTLS() *TLSSpec {
	if c == nil || c.Spec.Networking == nil {
		return nil
	}
	return c.Spec.Networking.TLS
}

// GetPreferredEndpointType returns discovery preferred endpoint type, default IP.
func (c *ValkeyCluster) GetPreferredEndpointType() PreferredEndpointType {
	if c == nil || c.Spec.Networking == nil || c.Spec.Networking.Discovery == nil ||
		c.Spec.Networking.Discovery.PreferredEndpointType == "" {
		return PreferredEndpointTypeIP
	}
	return c.Spec.Networking.Discovery.PreferredEndpointType
}

// GetClusterDomain returns networking.clusterDomain, default cluster.local.
func (c *ValkeyCluster) GetClusterDomain() string {
	if c == nil || c.Spec.Networking == nil || c.Spec.Networking.ClusterDomain == "" {
		return "cluster.local"
	}
	return c.Spec.Networking.ClusterDomain
}

// PrefersHostnameAnnounce reports whether discovery announces hostnames.
func (c *ValkeyCluster) PrefersHostnameAnnounce() bool {
	return c.GetPreferredEndpointType() == PreferredEndpointTypeHostname
}

// CertificateSource references a certificate and its private key. Today the
// only source is a Secret; future sources (cert-manager, operator-generated)
// are added as sibling fields forming a union where exactly one may be set.
type CertificateSource struct {
	// SecretName is the name of the secret.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	SecretName string `json:"secretName"`
}

type ExporterSpec struct {

	// Override the default exporter image
	Image string `json:"image,omitempty"`

	// Override resource requirements for the exporter container in each pod
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Enable or disable the exporter sidecar container
	Enabled bool `json:"enabled,omitempty"`

	// Override the SecurityContext applied to the exporter sidecar container.
	// +optional
	SecurityContext *corev1.SecurityContext `json:"securityContext,omitempty"`

	// Additional cmdline arguments passed to exporter sidecar container.
	// +optional
	Args []string `json:"args,omitempty"`
}

// ValkeyClusterStatus defines the observed state of ValkeyCluster.
type ValkeyClusterStatus struct {
	// State provides a high-level summary of the cluster's current state.
	// +kubebuilder:default=Initializing
	// +optional
	State ClusterState `json:"state,omitempty"`

	// Reason provides a brief machine-readable explanation for the current state.
	// +optional
	Reason string `json:"reason,omitempty"`

	// Message provides human-readable details about the current state.
	// +optional
	Message string `json:"message,omitempty"`

	// Shards represents the number of shards currently formed in the cluster.
	// +kubebuilder:default=0
	// +optional
	Shards int32 `json:"shards,omitempty"`

	// ReadyShards represents the number of shards that are fully healthy.
	// +kubebuilder:default=0
	// +optional
	ReadyShards int32 `json:"readyShards,omitempty"`

	// Conditions represent the current state of the ValkeyCluster resource.
	// Standard condition types:
	// - "Ready": the cluster is fully functional and serving traffic
	// - "Progressing": the cluster is being created, updated, or scaled
	// - "Degraded": the cluster is impaired but may be partially functional
	// Valkey-specific condition types:
	// - "ClusterFormed": all nodes have joined and meet the shard/replica layout
	// - "SlotsAssigned": all 16384 hash slots are assigned to primaries
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

const (
	ConditionReady         = "Ready"
	ConditionProgressing   = "Progressing"
	ConditionDegraded      = "Degraded"
	ConditionClusterFormed = "ClusterFormed"
	ConditionSlotsAssigned = "SlotsAssigned"
	// ConditionConfigurationWarning flags a spec value the operator accepted but
	// considers risky, for example a terminationGracePeriodSeconds too short for
	// graceful failover.
	ConditionConfigurationWarning = "ConfigurationWarning"
	// ConditionTLSEndpointWarning flags TLS with IP announce (including default
	// IP). Non-blocking: Ready may stay True. Prefer Hostname announce with DNS SANs.
	ConditionTLSEndpointWarning = "TLSEndpointWarning"
)

const (
	// Common reasons for conditions
	ReasonInitializing             = "Initializing"
	ReasonReconciling              = "Reconciling"
	ReasonClusterHealthy           = "ClusterHealthy"
	ReasonServiceError             = "ServiceError"
	ReasonConfigMapError           = "ConfigMapError"
	ReasonValkeyNodeError          = "ValkeyNodeError"
	ReasonValkeyNodeListError      = "ValkeyNodeListError"
	ReasonAddingNodes              = "AddingNodes"
	ReasonNodeAddFailed            = "NodeAddFailed"
	ReasonMissingShards            = "MissingShards"
	ReasonMissingReplicas          = "MissingReplicas"
	ReasonReconcileComplete        = "ReconcileComplete"
	ReasonTopologyComplete         = "TopologyComplete"
	ReasonAllSlotsAssigned         = "AllSlotsAssigned"
	ReasonSlotsUnassigned          = "SlotsUnassigned"
	ReasonGracePeriodTooShort      = "GracePeriodTooShort"
	ReasonPrimaryLost              = "PrimaryLost"
	ReasonNoSlots                  = "NoSlotsAvailable"
	ReasonRebalancingSlots         = "RebalancingSlots"
	ReasonRebalanceFailed          = "RebalanceFailed"
	ReasonUsersAclError            = "UsersACLError"
	ReasonUpdatingNodes            = "UpdatingNodes"
	ReasonSystemUsersAclError      = "SystemUsersACLError"
	ReasonPodDisruptionBudgetError = "PodDisruptionBudgetError"
	ReasonPodUnschedulable         = "PodUnschedulable"
	// ReasonTLSWithIPAnnounce is used with ConditionTLSEndpointWarning when TLS
	// is enabled and preferred endpoint type is IP (default or explicit).
	ReasonTLSWithIPAnnounce = "TLSWithIPAnnounce"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// ValkeyCluster is the Schema for the valkeyclusters API
// +kubebuilder:printcolumn:name="State",type="string",JSONPath=".status.state",description="Current state of the cluster"
// +kubebuilder:printcolumn:name="Reason",type="string",JSONPath=".status.reason",description="Reason for current state"
// +kubebuilder:printcolumn:name="ReadyShards",type="integer",JSONPath=".status.readyShards",description="Ready shards",priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time since creation"
type ValkeyCluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ValkeyCluster
	// +required
	Spec ValkeyClusterSpec `json:"spec"`

	// status defines the observed state of ValkeyCluster
	// +kubebuilder:default:={state: "Initializing", readyShards:0}
	// +optional
	Status ValkeyClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ValkeyClusterList contains a list of ValkeyCluster
type ValkeyClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ValkeyCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ValkeyCluster{}, &ValkeyClusterList{})
		return nil
	})
}
