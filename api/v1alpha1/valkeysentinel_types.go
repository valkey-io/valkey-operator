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

// SentinelPDBMode selects how the operator manages the PodDisruptionBudget over Sentinel pods.
// +kubebuilder:validation:Enum=Managed;Disabled
type SentinelPDBMode string

const (
	// SentinelPDBModeManaged means the operator owns the budget.
	SentinelPDBModeManaged SentinelPDBMode = "Managed"
	// SentinelPDBModeDisabled means no budget is created, and an existing one is deleted.
	SentinelPDBModeDisabled SentinelPDBMode = "Disabled"
)

// ValkeySentinelState represents the high-level state of a ValkeySentinel.
// It uses the same vocabulary as ValkeyState so the kinds read alike.
// +kubebuilder:validation:Enum=Initializing;Reconciling;Ready;Degraded;Failed
type ValkeySentinelState string

const (
	// ValkeySentinelStateInitializing indicates the quorum is being created for the first time.
	ValkeySentinelStateInitializing ValkeySentinelState = "Initializing"
	// ValkeySentinelStateReconciling indicates the quorum is being updated.
	ValkeySentinelStateReconciling ValkeySentinelState = "Reconciling"
	// ValkeySentinelStateReady indicates every Sentinel pod is ready.
	ValkeySentinelStateReady ValkeySentinelState = "Ready"
	// ValkeySentinelStateDegraded indicates the quorum is running but impaired.
	ValkeySentinelStateDegraded ValkeySentinelState = "Degraded"
	// ValkeySentinelStateFailed indicates the quorum has failed and cannot recover.
	ValkeySentinelStateFailed ValkeySentinelState = "Failed"
)

// Condition reasons specific to ValkeySentinel.
const (
	// ReasonSentinelsReady means every Sentinel pod is ready on the current revision.
	ReasonSentinelsReady = "SentinelsReady"
	// ReasonWaitingForSentinels means some Sentinel pods are not ready yet.
	ReasonWaitingForSentinels = "WaitingForSentinels"
	// ReasonRollingSentinels means pods on an old revision are being replaced one at a time.
	ReasonRollingSentinels = "RollingSentinels"
	// ReasonRollBlocked means a pending roll is held because replacing a pod now could break quorum.
	ReasonRollBlocked = "RollBlocked"
	// ReasonStatefulSetError means the StatefulSet could not be created or updated.
	ReasonStatefulSetError = "StatefulSetError"
	// ReasonQuorumUnavailable means SENTINEL CKQUORUM failed for at least one monitor.
	ReasonQuorumUnavailable = "QuorumUnavailable"
	// ReasonEvenSentinelReplicas warns that an even replica count adds a failure domain without lowering the majority.
	ReasonEvenSentinelReplicas = "EvenSentinelReplicas"
)

// SentinelNetworkingSpec groups how clients and peers reach a ValkeySentinel.
type SentinelNetworkingSpec struct {
	// ClusterDomain is the DNS suffix kubelet publishes Service DNS under (kubelet --cluster-domain).
	// Sentinel announces and resolves pod FQDNs built from it.
	// Must match the cluster. Default cluster.local.
	// +kubebuilder:default="cluster.local"
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*\.?$`
	// +optional
	ClusterDomain string `json:"clusterDomain,omitempty"`

	// TLS configuration for the Sentinel port.
	// +optional
	TLS *SentinelTLSSpec `json:"tls,omitempty"`
}

// SentinelTLSSpec defines the TLS configuration for a ValkeySentinel.
type SentinelTLSSpec struct {
	// ServerName is the hostname used for TLS verification when the operator connects to a Sentinel pod by IP.
	// When unset, the operator uses valkey-sentinel-<name>.<namespace>.svc.<clusterDomain>.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="!format.dns1123Subdomain().validate(self).hasValue()",message="must be a valid DNS-1123 subdomain (lowercase alphanumerics, '-' and '.', starting and ending with an alphanumeric)"
	ServerName string `json:"serverName,omitempty"`

	// Certificates holds the certificate slots used by the Sentinel pods.
	// +kubebuilder:validation:Required
	Certificates TLSCertificates `json:"certificates"`
}

// SentinelSchedulingSpec groups pod placement for a ValkeySentinel's pods.
type SentinelSchedulingSpec struct {
	// Tolerations to apply to the pods.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// NodeSelector to apply to the pods.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Affinity to apply to the pods.
	// When it sets no podAntiAffinity, the operator adds a preferred anti-affinity over kubernetes.io/hostname.
	// That keeps one node failure from taking out more than one Sentinel where the cluster has room.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// TopologySpreadConstraints to apply to the pods.
	// +optional
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`

	// PriorityClassName is the name of an existing PriorityClass applied to every pod.
	// +optional
	PriorityClassName string `json:"priorityClassName,omitempty"`
}

// SentinelPodDisruptionBudgetConfig manages the budget over a ValkeySentinel's pods.
type SentinelPodDisruptionBudgetConfig struct {
	// Mode selects how the operator manages the budget.
	// Managed renders maxUnavailable 1 over this quorum's pods.
	// Disabled creates none, and deletes an existing one.
	// +kubebuilder:default=Managed
	// +optional
	Mode SentinelPDBMode `json:"mode,omitempty"`
}

// ValkeySentinelSpec defines a Sentinel quorum.
type ValkeySentinelSpec struct {
	// Replicas is the number of Sentinel pods.
	// Fewer than 3 cannot keep a majority through the loss of one pod.
	// An even count raises a ConfigurationWarning rather than being rejected.
	// It adds a failure domain without lowering the majority threshold.
	// +kubebuilder:validation:Minimum=3
	// +kubebuilder:default=3
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// Image overrides the default Valkey image.
	// Sentinel runs from the same image as the data nodes.
	// +optional
	Image string `json:"image,omitempty"`

	// ImagePullSecrets references Secrets in this namespace used to pull the pod's images.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// Resources overrides the resource requirements for the Sentinel container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Scheduling groups pod placement configuration for the Sentinel pods.
	// +optional
	Scheduling *SentinelSchedulingSpec `json:"scheduling,omitempty"`

	// Containers holds additional containers, or overrides for the default one.
	// Applied as a strategic merge patch.
	// +optional
	Containers []corev1.Container `json:"containers,omitempty"`

	// Config holds process-global sentinel.conf directives, written to the file as is.
	// A key is the directive and the value its arguments, for example "sentinel deny-scripts-reconfig": "yes".
	// Per-monitor settings do not belong here.
	// Operator-owned directives win over user keys, and a collision raises a ConfigurationWarning.
	//
	// Monitor registration is owned by the operator, so the monitor directive is rejected.
	// +kubebuilder:validation:XValidation:rule="self.all(key, key.lowerAscii() != 'sentinel monitor')",message="spec.config must not contain sentinel monitor: the operator registers monitors"
	// +optional
	Config map[string]string `json:"config,omitempty"`

	// Networking groups how clients and peers reach the Sentinel pods.
	// +optional
	Networking *SentinelNetworkingSpec `json:"networking,omitempty"`

	// PodDisruptionBudget configures the budget over the Sentinel pods.
	// +optional
	PodDisruptionBudget *SentinelPodDisruptionBudgetConfig `json:"podDisruptionBudget,omitempty"`

	// PodSecurityContext overrides the PodSecurityContext applied to the pods.
	// +optional
	PodSecurityContext *corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`
}

// SentinelMonitorStatus is the live view of one monitor, read from SENTINEL MASTERS.
type SentinelMonitorStatus struct {
	// Name is the monitor (master) name.
	Name string `json:"name"`

	// Valkey is the Valkey the monitor belongs to.
	// It stays empty until the operator registers monitors.
	// +optional
	Valkey string `json:"valkey,omitempty"`

	// PrimaryAddress is the primary Sentinel currently holds for this monitor.
	// +optional
	PrimaryAddress string `json:"primaryAddress,omitempty"`

	// Quorum is the number of Sentinels that must agree the primary is down.
	// +optional
	Quorum int32 `json:"quorum,omitempty"`

	// OK is the result of SENTINEL CKQUORUM.
	// +optional
	OK bool `json:"ok,omitempty"`

	// Replicas is the replica count Sentinel itself discovered.
	// It can lag or differ from Valkey.status.readyReplicas.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`
}

// ValkeySentinelStatus defines the observed state of ValkeySentinel.
type ValkeySentinelStatus struct {
	// State provides a high-level summary of the quorum's current state.
	// +kubebuilder:default=Initializing
	// +optional
	State ValkeySentinelState `json:"state,omitempty"`

	// Reason provides a brief machine-readable explanation for the current state.
	// +optional
	Reason string `json:"reason,omitempty"`

	// Message provides human-readable details about the current state.
	// +optional
	Message string `json:"message,omitempty"`

	// Replicas is the number of Sentinel pods the StatefulSet runs.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// ReadyReplicas is the number of Sentinel pods reporting ready.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// Monitors is the live view from SENTINEL MASTERS on a ready pod.
	// +listType=map
	// +listMapKey=name
	// +optional
	Monitors []SentinelMonitorStatus `json:"monitors,omitempty"`

	// ObservedGeneration is the most recent spec generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the current state of the ValkeySentinel resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vks
//
// Child resource names are derived from metadata.name.
// The StatefulSet "valkey-sentinel-<name>" is the binding limit.
// Its pods carry a controller-revision-hash label of "<statefulset>-<10 character hash>", which must fit 63 characters.
// That leaves 63 - 11 - 16 = 36 characters for the name.

// ValkeySentinel is the Schema for the valkeysentinels API.
// It runs a Sentinel quorum as a StatefulSet.
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 36",message="metadata.name must be at most 36 characters, because child resource names are derived from it"
// +kubebuilder:validation:XValidation:rule="!self.metadata.name.contains('.')",message="metadata.name must not contain '.', because child Service names are derived from it"
// +kubebuilder:printcolumn:name="State",type="string",JSONPath=".status.state",description="Current state of the quorum"
// +kubebuilder:printcolumn:name="Reason",type="string",JSONPath=".status.reason",description="Reason for current state"
// +kubebuilder:printcolumn:name="Ready",type="integer",JSONPath=".status.readyReplicas",description="Ready Sentinel pods"
// +kubebuilder:printcolumn:name="Replicas",type="integer",JSONPath=".spec.replicas",description="Desired Sentinel pods",priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time since creation"
type ValkeySentinel struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ValkeySentinel
	// +required
	Spec ValkeySentinelSpec `json:"spec"`

	// status defines the observed state of ValkeySentinel
	// +kubebuilder:default:={state: "Initializing"}
	// +optional
	Status ValkeySentinelStatus `json:"status,omitzero"`
}

// GetTLS returns the TLS config from spec.networking.tls, or nil.
func (s *ValkeySentinel) GetTLS() *SentinelTLSSpec {
	if s == nil || s.Spec.Networking == nil {
		return nil
	}
	return s.Spec.Networking.TLS
}

// GetClusterDomain returns networking.clusterDomain, default cluster.local.
func (s *ValkeySentinel) GetClusterDomain() string {
	if s == nil || s.Spec.Networking == nil || s.Spec.Networking.ClusterDomain == "" {
		return DefaultClusterDomain
	}
	return s.Spec.Networking.ClusterDomain
}

// PodDisruptionBudgetEnabled reports whether the operator should keep a budget over the pods.
// Mode defaults to Managed.
func (s ValkeySentinelSpec) PodDisruptionBudgetEnabled() bool {
	return s.PodDisruptionBudget == nil || s.PodDisruptionBudget.Mode != SentinelPDBModeDisabled
}

// +kubebuilder:object:root=true

// ValkeySentinelList contains a list of ValkeySentinel.
type ValkeySentinelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ValkeySentinel `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ValkeySentinel{}, &ValkeySentinelList{})
		return nil
	})
}
