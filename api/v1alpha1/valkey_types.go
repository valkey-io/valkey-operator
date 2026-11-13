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

// ValkeyACLSpec holds the ACL configuration of a Valkey.
type ValkeyACLSpec struct {
	// Users managed by the operator.
	// +listType=map
	// +listMapKey=name
	// +optional
	Users []ACLUserSpec `json:"users,omitempty"`
}

// ValkeyNetworkingSpec groups how clients and peers reach a Valkey.
type ValkeyNetworkingSpec struct {
	// ClusterDomain is the DNS suffix kubelet publishes Service DNS under (kubelet --cluster-domain).
	// Used when building announce FQDNs and the default TLS ServerName.
	// Must match the cluster. Default cluster.local.
	// +kubebuilder:default="cluster.local"
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*\.?$`
	// +optional
	ClusterDomain string `json:"clusterDomain,omitempty"`

	// TLS configuration for the instance.
	// +optional
	TLS *ValkeyTLSSpec `json:"tls,omitempty"`
}

// ValkeyTLSSpec defines the TLS configuration for a Valkey.
type ValkeyTLSSpec struct {
	// ServerName is the hostname used for TLS verification when the operator connects to a pod by IP.
	// When unset, the operator uses valkey-<name>.<namespace>.svc.<clusterDomain> (default cluster.local).
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="self.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*$')",message="must be a valid DNS-1123 subdomain (lowercase alphanumerics, '-' and '.', starting and ending with an alphanumeric)"
	ServerName string `json:"serverName,omitempty"`

	// Certificates holds the certificate slots used by the instance.
	// +kubebuilder:validation:Required
	Certificates TLSCertificates `json:"certificates"`
}

// ValkeySchedulingSpec groups pod placement for a Valkey's pods.
type ValkeySchedulingSpec struct {
	// Tolerations to apply to the pods.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// NodeSelector to apply to the pods.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Affinity to apply to the pods.
	// Kubernetes ANDs nodeAffinity with NodeSelector rather than one overriding the other.
	// A node must satisfy both for the pod to be scheduled there.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// TopologySpreadConstraints to apply to the pods.
	// +optional
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`

	// PriorityClassName is the name of an existing PriorityClass applied to every pod.
	// It protects them from eviction under resource pressure.
	// +optional
	PriorityClassName string `json:"priorityClassName,omitempty"`
}

// ValkeyPodDisruptionBudgetConfig manages the budget over a Valkey's pods.
type ValkeyPodDisruptionBudgetConfig struct {
	// Mode selects how the operator manages the budget.
	// Managed renders maxUnavailable 1 over this instance's pods.
	// Disabled creates none, and deletes an existing one.
	// +kubebuilder:default=Managed
	// +optional
	Mode ValkeyPDBMode `json:"mode,omitempty"`
}

// FailoverMode selects who promotes a replica when the primary is lost.
// Values may be added in future versions, so clients must handle unknown values.
type FailoverMode string

const (
	// FailoverModeNone means nobody promotes automatically.
	// It covers a standalone instance and manual failover.
	FailoverModeNone FailoverMode = "None"
	// FailoverModeSentinel means a ValkeySentinel quorum is the failover authority.
	FailoverModeSentinel FailoverMode = "Sentinel"
)

// FailoverSpec declares who is responsible for promoting a replica.
//
// It is deliberately self-contained and references no sibling or parent field,
// so a future ValkeyCell can embed the same type for the Valkeys it manages.
//
// The has() guard on mode is defensive: the default fills it before validation runs.
// +kubebuilder:validation:XValidation:rule="!has(self.sentinel) || (has(self.mode) && self.mode == 'Sentinel')",message="failover.sentinel requires failover.mode: Sentinel",fieldPath=".sentinel",reason="FieldValueForbidden"
//
// Once the sentinel block exists it cannot be removed while the mode stays Sentinel.
// The monitorName rules on SentinelFailoverSpec only run when the block is present before
// and after an update. Without this rule, a user could drop the block and add it back
// with a different monitorName, and those rules would never run.
// Leaving Sentinel mode is how a block is dropped on purpose. That path goes through
// deregistration, so coming back later with a new name is allowed.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.sentinel) || !(has(self.mode) && self.mode == 'Sentinel') || has(self.sentinel)",message="failover.sentinel cannot be removed while failover.mode stays Sentinel; set the mode to None first",fieldPath=".sentinel",reason="FieldValueForbidden"
type FailoverSpec struct {
	// Mode selects the failover engine.
	//   None     - nobody promotes automatically.
	//   Sentinel - a ValkeySentinel quorum is the failover authority.
	// The value Operator is reserved and is not accepted yet.
	// +kubebuilder:validation:Enum=None;Sentinel
	// +kubebuilder:default=None
	// +optional
	Mode FailoverMode `json:"mode,omitempty"`

	// Sentinel configures Sentinel-mode failover.
	// Only valid when mode is Sentinel.
	// This block does not by itself cause monitoring:
	// a ValkeySentinel must also select this object.
	// +optional
	Sentinel *SentinelFailoverSpec `json:"sentinel,omitempty"`
}

// SentinelFailoverSpec configures how Sentinel monitors one Valkey.
//
// The rules are transition rules, so they run on updates only, and only while the block
// is present before and after the update. FailoverSpec stops the block from being removed
// while the mode stays Sentinel, so they cannot be skipped that way.
// +kubebuilder:validation:XValidation:rule="has(self.monitorName) == has(oldSelf.monitorName)",message="monitorName cannot be added or removed after the sentinel block is created",fieldPath=".monitorName",reason="FieldValueForbidden"
// +kubebuilder:validation:XValidation:rule="!has(self.monitorName) || !has(oldSelf.monitorName) || self.monitorName == oldSelf.monitorName",message="monitorName is immutable",fieldPath=".monitorName",reason="FieldValueForbidden"
type SentinelFailoverSpec struct {
	// MonitorName is the Sentinel master-name for this instance.
	// It defaults to metadata.name, which Valkey.MonitorName resolves.
	// There is no kubebuilder default, because a materialized default
	// cannot be told apart from a user-set value by the transition rules above.
	// It stays settable so an adopted deployment keeps the master-name
	// its clients already use.
	// +kubebuilder:validation:MinLength=1
	// +optional
	MonitorName string `json:"monitorName,omitempty"`

	// Quorum is the number of Sentinels that must agree the primary is down.
	// It is passed to SENTINEL MONITOR.
	// When unset it defaults to (the selecting ValkeySentinel's replicas / 2) + 1.
	// The default is computed at registration time. Set it explicitly to pin it.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Quorum *int32 `json:"quorum,omitempty"`

	// Config is per-master Sentinel tuning, forwarded as
	// SENTINEL SET <monitorName> <key> <value>.
	// The operator does not validate the values.
	// +optional
	Config map[string]string `json:"config,omitempty"`
}

// MonitoringStatus reports which ValkeySentinel monitors a Valkey.
// Nothing sets it yet. The registration work fills it in.
type MonitoringStatus struct {
	// MonitoredBy lists the ValkeySentinels currently selecting this instance.
	// At most one is permitted. A second selector is rejected and reported as Degraded.
	// +optional
	MonitoredBy []string `json:"monitoredBy,omitempty"`

	// MonitorName is the master-name this instance is registered under.
	// +optional
	MonitorName string `json:"monitorName,omitempty"`

	// Registered is true when every ready pod of the selecting
	// ValkeySentinel reports this monitor.
	// +optional
	Registered bool `json:"registered,omitempty"`

	// Quorum is the quorum the monitor was registered with.
	// +optional
	Quorum int32 `json:"quorum,omitempty"`

	// AvailableQuorum is the number of Sentinels currently reachable for this monitor.
	// +optional
	AvailableQuorum int32 `json:"availableQuorum,omitempty"`
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
// +kubebuilder:validation:XValidation:rule="!(has(self.persistence) && self.workloadType == 'Deployment')",message="persistence requires workloadType StatefulSet",fieldPath=".persistence",reason="FieldValueForbidden"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.persistence) || has(self.persistence)",message="persistence cannot be removed once set",fieldPath=".persistence",reason="FieldValueForbidden"
// +kubebuilder:validation:XValidation:rule="has(oldSelf.persistence) || !has(self.persistence)",message="persistence cannot be added after creation",fieldPath=".persistence",reason="FieldValueForbidden"
// +kubebuilder:validation:XValidation:rule="!has(self.persistence) || !has(oldSelf.persistence) || quantity(self.persistence.size).compareTo(quantity(oldSelf.persistence.size)) >= 0",message="persistence.size may only be expanded",fieldPath=".persistence.size",reason="FieldValueForbidden"
// +kubebuilder:validation:XValidation:rule="!has(self.persistence) || !has(oldSelf.persistence) || ((!has(self.persistence.storageClassName) && !has(oldSelf.persistence.storageClassName)) || (has(self.persistence.storageClassName) && has(oldSelf.persistence.storageClassName) && self.persistence.storageClassName == oldSelf.persistence.storageClassName))",message="persistence.storageClassName is immutable",fieldPath=".persistence.storageClassName",reason="FieldValueForbidden"
type ValkeySpec struct {
	// Replicas is the number of replicas in addition to the primary.
	// Values above 0 are rejected until replication support lands.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// Failover declares how primary failover is performed for this instance.
	// Nothing reads it yet. Sentinel selection and registration use it later.
	// +optional
	Failover *FailoverSpec `json:"failover,omitempty"`

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
	Scheduling *ValkeySchedulingSpec `json:"scheduling,omitempty"`

	// Exporter configures the metrics exporter sidecar.
	// +kubebuilder:default:={enabled:true}
	// +optional
	Exporter ExporterSpec `json:"exporter,omitempty"`

	// WorkloadType picks the ValkeyNode's workload kind. It is immutable.
	// +kubebuilder:default=StatefulSet
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="workloadType is immutable",reason="FieldValueForbidden"
	// +optional
	WorkloadType WorkloadType `json:"workloadType,omitempty"`

	// Persistence defines durable storage propagated to the ValkeyNode.
	// +optional
	Persistence *PersistenceSpec `json:"persistence,omitempty"`

	// ACL configures the Valkey access control the operator manages.
	// The controller does not apply these users yet.
	// +optional
	ACL *ValkeyACLSpec `json:"acl,omitempty"`

	// Containers holds additional containers, or overrides for the default ones.
	// Applied as a strategic merge patch.
	// The name server-init is reserved for the operator-managed init container.
	// +optional
	// +kubebuilder:validation:XValidation:rule="!self.exists(c, c.name == 'server-init')",message="container name 'server-init' is reserved for the operator-managed init container",reason="FieldValueForbidden"
	Containers []corev1.Container `json:"containers,omitempty"`

	// Config holds additional Valkey configuration parameters.
	//
	// Cluster mode directives are rejected.
	// A Valkey always runs with cluster-enabled no.
	// Every cluster- key is therefore inert or actively wrong here.
	// Accepting one would store an unhonourable spec.
	// Every cluster directive except tls-cluster carries the prefix.
	// A prefix test plus tls-cluster therefore covers them all without enumerating each key.
	//
	// The comparison is lowercased because Valkey keys are case-insensitive.
	// Cluster-Enabled has to be caught alongside cluster-enabled.
	//
	// This rejects user input only.
	// The base config still emits cluster-config-file for every node.
	// That keeps the node state file on the writable /data volume.
	// See buildManagedConfig.
	// +kubebuilder:validation:XValidation:rule="self.all(key, !key.lowerAscii().startsWith('cluster-') && key.lowerAscii() != 'tls-cluster')",message="spec.config must not contain cluster- keys or tls-cluster: a Valkey runs standalone, so cluster mode directives are not supported"
	//
	// replicaof and its alias slaveof are rejected too.
	// The operator owns replication topology.
	// A user-set directive would fight the operator's own wiring.
	// +kubebuilder:validation:XValidation:rule="self.all(key, !(key.lowerAscii() in ['replicaof', 'slaveof']))",message="spec.config must not contain replicaof or slaveof: the operator owns replication topology"
	//
	// Keys may contain only letters, digits, '.', '_' and '-'.
	// Valkey trims and unquotes each config line before reading its key, so a key such as " cluster-enabled" or "replicaof" in quotes would pass the rules above.
	// The '.' keeps module configs such as search.<name> valid.
	// +kubebuilder:validation:XValidation:rule="self.all(key, key.matches('^[a-zA-Z0-9._-]+$'))",message="spec.config keys may contain only letters, digits, '.', '_' and '-'. Valkey unquotes and trims keys, so quoted or padded keys are rejected"
	// +optional
	Config map[string]string `json:"config,omitempty"`

	// Networking groups how clients and peers reach the instance.
	// +optional
	Networking *ValkeyNetworkingSpec `json:"networking,omitempty"`

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

	// ServiceAccountName is the ServiceAccount the instance's pods run as.
	// The namespace's default ServiceAccount is used when unset.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// PodMetadata adds labels and annotations to every pod of the instance.
	// They are not part of the workload selector, so they can change at any time, and a change rolls the pods.
	// Keys the operator sets itself are rejected.
	// +optional
	PodMetadata *EmbeddedObjectMetadata `json:"podMetadata,omitempty"`
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

	// Monitoring reports which ValkeySentinel monitors this instance.
	// It stays unset until the registration work fills it in.
	// +optional
	Monitoring *MonitoringStatus `json:"monitoring,omitempty"`

	// SentinelCredentialsSecret locates the _sentinel user's password.
	// It is the only credential a selecting ValkeySentinel reads.
	// It stays unset until the credential work fills it in.
	// +optional
	SentinelCredentialsSecret *corev1.SecretKeySelector `json:"sentinelCredentialsSecret,omitempty"`

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

// ReasonSentinelWithoutReplicas is used with ConditionConfigurationWarning when a
// Valkey declares failover mode Sentinel but has no replicas. Sentinel can track
// the lone primary's liveness, but it has nothing to promote.
const ReasonSentinelWithoutReplicas = "SentinelWithoutReplicas"

// MonitorName resolves the effective Sentinel master-name.
// It is shared so the Valkey and ValkeySentinel controllers cannot disagree.
func (v *Valkey) MonitorName() string {
	if v.Spec.Failover != nil && v.Spec.Failover.Sentinel != nil &&
		v.Spec.Failover.Sentinel.MonitorName != "" {
		return v.Spec.Failover.Sentinel.MonitorName
	}
	return v.Name
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Valkey{}, &ValkeyList{})
		return nil
	})
}
