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

	// Affinity to apply to the pods, overrides NodeSelector if set.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// TopologySpreadConstraints defines pod topology spread constraints applied
	// to the ValkeyNode workloads. The operator augments these constraints with
	// shard-aware selectors so pods from the same shard can be spread across the
	// configured topology domain.
	// +optional
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`

	// PriorityClassName is the name of an existing PriorityClass applied to
	// every pod in the cluster, protecting them from eviction under resource
	// pressure. Pod priority is a scheduling concern (preemption, scheduling-queue
	// order), so it lives alongside the other placement fields.
	// +optional
	PriorityClassName string `json:"priorityClassName,omitempty"`
}

// ValkeyClusterSpec defines the desired state of ValkeyCluster.
// +kubebuilder:validation:XValidation:rule="!(has(self.persistence) && self.workloadType == 'Deployment')",message="persistence requires workloadType StatefulSet"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.persistence) || has(self.persistence)",message="persistence cannot be removed once set"
// +kubebuilder:validation:XValidation:rule="has(oldSelf.persistence) || !has(self.persistence)",message="persistence cannot be added after creation"
// +kubebuilder:validation:XValidation:rule="!has(self.persistence) || !has(oldSelf.persistence) || quantity(self.persistence.size).compareTo(quantity(oldSelf.persistence.size)) >= 0",message="persistence.size may only be expanded"
// +kubebuilder:validation:XValidation:rule="!has(self.persistence) || !has(oldSelf.persistence) || ((!has(self.persistence.storageClassName) && !has(oldSelf.persistence.storageClassName)) || (has(self.persistence.storageClassName) && has(oldSelf.persistence.storageClassName) && self.persistence.storageClassName == oldSelf.persistence.storageClassName))",message="persistence.storageClassName is immutable"
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

	// TLS configuration for the cluster
	// +optional
	TLS *TLSConfig `json:"tls,omitempty"`

	// PodDisruptionBudget configures the operator-managed PodDisruptionBudget(s)
	// for this cluster. When unset, the operator applies the default (Cluster) mode.
	// +optional
	PodDisruptionBudget *PodDisruptionBudgetConfig `json:"podDisruptionBudget,omitempty"`

	// Override the PodSecurityContext applied to each ValkeyNode pod of the cluster.
	// When set, this overrides the default PodSecurityContext.
	// +optional
	PodSecurityContext *corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`

	// Networking configures how nodes advertise themselves in the cluster.
	// +optional
	Networking *NetworkingSpec `json:"networking,omitempty"`
}

// PreferredEndpointType mirrors valkey's cluster-preferred-endpoint-type directive.
// +kubebuilder:validation:Enum=IP;Hostname
type PreferredEndpointType string

const (
	PreferredEndpointTypeIP       PreferredEndpointType = "IP"
	PreferredEndpointTypeHostname PreferredEndpointType = "Hostname"
)

// NetworkingSpec configures cluster endpoint announcement.
type NetworkingSpec struct {
	// PreferredEndpointType selects IP (default) or Hostname announcement.
	// Use Hostname with TLS clients that verify SNI against DNS names.
	// +kubebuilder:default=IP
	// +optional
	PreferredEndpointType PreferredEndpointType `json:"preferredEndpointType,omitempty"`
}

// TLSConfig defines the TLS configuration for ValkeyCluster.
type TLSConfig struct {
	// Certificate is a reference to a Kubernetes secret that contains the certificate and private key for enabling TLS.
	// The referenced secret should contain the following:
	//
	// - `ca.crt`: The certificate authority.
	// - `tls.crt`: The certificate (or a chain).
	// - `tls.key`: The private key to the first certificate in the certificate chain.
	Certificate CertificateRef `json:"certificate,omitempty"`
}

// CertificateRef defines the certificate reference for ValkeyCluster.
type CertificateRef struct {
	// SecretName is the name of the secret.
	SecretName string `json:"secretName,omitempty"`
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
