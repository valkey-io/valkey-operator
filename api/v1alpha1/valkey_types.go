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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ValkeyPDBMode selects how the operator manages the PodDisruptionBudget.
// +kubebuilder:validation:Enum=Managed;Disabled
type ValkeyPDBMode string

const (
	// ValkeyPDBModeManaged means the operator owns the budget.
	ValkeyPDBModeManaged ValkeyPDBMode = "Managed"
	// ValkeyPDBModeDisabled means no budget is created, and an existing one is deleted.
	ValkeyPDBModeDisabled ValkeyPDBMode = "Disabled"
)

// ValkeyState represents the high-level state of a Valkey.
// It uses the same vocabulary as ClusterState so both kinds read alike.
// +kubebuilder:validation:Enum=Initializing;Reconciling;Ready;Degraded;Failed
type ValkeyState string

const (
	// ValkeyStateInitializing indicates the instance is being created for the first time.
	ValkeyStateInitializing ValkeyState = "Initializing"
	// ValkeyStateReconciling indicates the instance is being updated.
	ValkeyStateReconciling ValkeyState = "Reconciling"
	// ValkeyStateReady indicates the instance is healthy and serving traffic.
	ValkeyStateReady ValkeyState = "Ready"
	// ValkeyStateDegraded indicates the instance is partially functional.
	ValkeyStateDegraded ValkeyState = "Degraded"
	// ValkeyStateFailed indicates the instance has failed and cannot recover.
	ValkeyStateFailed ValkeyState = "Failed"
)

// ValkeyStates lists all possible Valkey states.
var ValkeyStates = []ValkeyState{
	ValkeyStateInitializing,
	ValkeyStateReconciling,
	ValkeyStateReady,
	ValkeyStateDegraded,
	ValkeyStateFailed,
}

// ValkeyPodDisruptionBudgetConfig manages the budget over a Valkey's pods.
//
// This is a separate type from the ValkeyCluster config of the same shape.
// The two kinds can then grow different fields independently.
//
// It also drops two things from that type.
// The Cluster mode value, which describes nothing outside a cluster.
// The legacy UnmarshalJSON, which only serves pre-existing stored objects.
type ValkeyPodDisruptionBudgetConfig struct {
	// Mode selects how the operator manages the budget.
	// Managed renders maxUnavailable 1 over this instance's pods.
	// Disabled creates none, and deletes an existing one.
	// +kubebuilder:default=Managed
	// +optional
	Mode ValkeyPDBMode `json:"mode,omitempty"`
}

// ValkeySpec defines the desired state of Valkey.
//
// Replication is not implemented yet, so spec.replicas is pinned to 0 for now.
// The restriction is relaxed when replication lands, which is a backwards compatible change.
// Adding it later would not be.
//
// The has() guard is required to let users omit the field.
// case: no replicas key --> admitted
// case: replicas: 0 --> admitted
// case: replicas: 2 --> rejected
// +kubebuilder:validation:XValidation:rule="!has(self.replicas) || self.replicas == 0",message="spec.replicas must be 0: replication is not implemented yet, only standalone Valkey is supported"
//
// Persistence rules are copied from ValkeyClusterSpec so both kinds behave alike.
// +kubebuilder:validation:XValidation:rule="!(has(self.persistence) && self.workloadType == 'Deployment')",message="persistence requires workloadType StatefulSet"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.persistence) || has(self.persistence)",message="persistence cannot be removed once set"
// +kubebuilder:validation:XValidation:rule="has(oldSelf.persistence) || !has(self.persistence)",message="persistence cannot be added after creation"
// +kubebuilder:validation:XValidation:rule="!has(self.persistence) || !has(oldSelf.persistence) || quantity(self.persistence.size).compareTo(quantity(oldSelf.persistence.size)) >= 0",message="persistence.size may only be expanded"
// +kubebuilder:validation:XValidation:rule="!has(self.persistence) || !has(oldSelf.persistence) || ((!has(self.persistence.storageClassName) && !has(oldSelf.persistence.storageClassName)) || (has(self.persistence.storageClassName) && has(oldSelf.persistence.storageClassName) && self.persistence.storageClassName == oldSelf.persistence.storageClassName))",message="persistence.storageClassName is immutable"
type ValkeySpec struct {
	// Replicas is the number of replicas in addition to the primary.
	// Values above 0 are rejected until replication support lands.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// Image overrides the default Valkey image.
	// +optional
	Image string `json:"image,omitempty"`

	// ImagePullSecrets references Secrets in this namespace.
	// They are used to pull the pod's images from private registries.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// Resources overrides the resource requirements for the Valkey container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Scheduling groups pod placement configuration for the instance's pods.
	// +optional
	Scheduling *SchedulingSpec `json:"scheduling,omitempty"`

	// Exporter configures the metrics exporter sidecar.
	// +kubebuilder:default:={enabled:true}
	// +optional
	Exporter ExporterSpec `json:"exporter,omitempty"`

	// WorkloadType picks the ValkeyNode's workload kind. It is immutable.
	// +kubebuilder:default=StatefulSet
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="workloadType is immutable"
	// +optional
	WorkloadType WorkloadType `json:"workloadType,omitempty"`

	// Persistence defines durable storage propagated to the ValkeyNode.
	// +optional
	Persistence *PersistenceSpec `json:"persistence,omitempty"`

	// Users holds ACL user definitions. See valkeyacls_types.go.
	// +listType=map
	// +listMapKey=name
	// +optional
	Users []UserAclSpec `json:"users,omitempty"`

	// Containers holds additional containers, or overrides for the default ones.
	// Applied as a strategic merge patch.
	// +optional
	Containers []corev1.Container `json:"containers,omitempty"`

	// Config holds additional Valkey configuration parameters.
	//
	// Cluster mode directives are rejected.
	// A Valkey always runs with cluster-enabled no.
	// Every cluster- key is therefore inert or actively wrong here.
	// Accepting one would store an unhonourable spec.
	// Every cluster directive carries the prefix.
	// A prefix test therefore covers them all without enumerating each key.
	//
	// The comparison is lowercased because Valkey keys are case-insensitive.
	// Cluster-Enabled has to be caught alongside cluster-enabled.
	//
	// This rejects user input only.
	// The base config still emits cluster-config-file for every node.
	// That keeps the node state file on the writable /data volume.
	// See buildManagedConfig.
	// +kubebuilder:validation:XValidation:rule="self.all(key, !key.lowerAscii().startsWith('cluster-'))",message="spec.config must not contain cluster- keys: a Valkey runs standalone, so cluster mode directives are not supported"
	// +optional
	Config map[string]string `json:"config,omitempty"`

	// Networking groups how clients and peers reach the instance.
	// +optional
	Networking *NetworkingSpec `json:"networking,omitempty"`

	// PodDisruptionBudget configures the budget over this instance's pods.
	// No budget is created while spec.replicas is 0, whatever the mode.
	// A single-pod budget either allows evicting it or blocks drains.
	// +optional
	PodDisruptionBudget *ValkeyPodDisruptionBudgetConfig `json:"podDisruptionBudget,omitempty"`

	// PodSecurityContext overrides the PodSecurityContext applied to the pod.
	// +optional
	PodSecurityContext *corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`

	// TerminationGracePeriodSeconds is the pod termination grace period.
	// +kubebuilder:validation:Minimum=1
	// +optional
	TerminationGracePeriodSeconds *int64 `json:"terminationGracePeriodSeconds,omitempty"`
}

// ValkeyStatus defines the observed state of Valkey.
type ValkeyStatus struct {
	// State provides a high-level summary of the instance's current state.
	// +kubebuilder:default=Initializing
	// +optional
	State ValkeyState `json:"state,omitempty"`

	// Reason provides a brief machine-readable explanation for the current state.
	// +optional
	Reason string `json:"reason,omitempty"`

	// Message provides human-readable details about the current state.
	// +optional
	Message string `json:"message,omitempty"`

	// Primary is the name of the ValkeyNode currently serving as primary.
	// +optional
	Primary string `json:"primary,omitempty"`

	// Replicas counts this instance's ValkeyNodes, excluding the primary.
	// +kubebuilder:default=0
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// ReadyReplicas is the number of those ValkeyNodes reporting ready.
	// +kubebuilder:default=0
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// ObservedGeneration is the most recent spec generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the current state of the Valkey resource.
	// Standard condition types:
	// - "Ready": the instance is fully functional and serving traffic
	// - "Progressing": the instance is being created or updated
	// - "Degraded": the instance is impaired but may be partially functional
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Valkey is the Schema for the valkeys API.
//
// The name is bounded because child resource names are derived from it.
// Every derived name is held to a 63 character DNS label.
// That is stricter than some child types need: Secret names allow 253.
// One rule for every child keeps the limit simple.
// It also leaves room for derived names used as labels or volume names.
//
// The longest derived name is the Secret "internal-<name>-system-passwords".
// Its 26 fixed characters leave 37 for the name.
//
// The limit is in place from the start.
// Tightening it later would reject objects that already exist.
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 37",message="metadata.name must be at most 37 characters, because child resource names are derived from it"
// +kubebuilder:printcolumn:name="State",type="string",JSONPath=".status.state",description="Current state of the instance"
// +kubebuilder:printcolumn:name="Reason",type="string",JSONPath=".status.reason",description="Reason for current state"
// +kubebuilder:printcolumn:name="Primary",type="string",JSONPath=".status.primary",description="ValkeyNode currently serving as primary"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time since creation"
type Valkey struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Valkey
	// +required
	Spec ValkeySpec `json:"spec"`

	// status defines the observed state of Valkey
	// +kubebuilder:default:={state: "Initializing", replicas:0, readyReplicas:0}
	// +optional
	Status ValkeyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ValkeyList contains a list of Valkey.
type ValkeyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Valkey `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Valkey{}, &ValkeyList{})
		return nil
	})
}
