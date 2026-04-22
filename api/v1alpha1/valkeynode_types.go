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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WorkloadType specifies the type of workload to create for the ValkeyNode.
// +kubebuilder:validation:Enum=StatefulSet;Deployment
type WorkloadType string

const (
	// WorkloadTypeStatefulSet creates a ValkeyNode that will be a StatefulSet.
	WorkloadTypeStatefulSet WorkloadType = "StatefulSet"
	// WorkloadTypeDeployment creates a ValkeyNode that will be a Deployment.
	WorkloadTypeDeployment WorkloadType = "Deployment"
)

// ValkeyNodeSpec defines the desired state of ValkeyNode.
// +kubebuilder:validation:XValidation:rule="!(has(self.persistence) && self.workloadType == 'Deployment')",message="persistence requires workloadType StatefulSet"
type ValkeyNodeSpec struct {
	// Image is the Valkey container image to use.
	// +optional
	Image string `json:"image,omitempty"`

	// WorkloadType specifies whether to create a StatefulSet or Deployment.
	// This field is immutable after creation.
	// +kubebuilder:default=StatefulSet
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="workloadType is immutable"
	// +optional
	WorkloadType WorkloadType `json:"workloadType,omitempty"`

	// Persistence defines durable storage for the ValkeyNode.
	// +optional
	Persistence *PersistenceSpec `json:"persistence,omitempty"`

	// Resources defines the resource requirements for the Valkey container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// NodeSelector defines the node selection constraints.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Affinity defines the pod affinity/anti-affinity rules.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// Tolerations defines the pod tolerations.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Exporter defines the metrics exporter sidecar configuration.
	// +optional
	Exporter ExporterSpec `json:"exporter,omitempty"`

	// ServerConfigMapName specifies the name of the ConfigMap that contains the
	// scripts, and Valkey config for the ValkeyNode.
	// +optional
	ServerConfigMapName string `json:"serverConfigMapName,omitempty"`

	// UsersACLSecretName is the name of the Secret containing the ACL user
	// file. When set, mounts a users-acl volume from this Secret so the
	// container can load aclfile /config/users/users.acl.
	// +optional
	UsersACLSecretName string `json:"usersACLSecretName,omitempty"`

	// Containers allows patching the default containers via a strategic merge
	// patch. Existing containers (e.g. "server", "metrics-exporter") are merged
	// by name; any container name not present in the defaults is appended.
	// +optional
	Containers []corev1.Container `json:"containers,omitempty"`

	// TLS configuration for the node
	// +optional
	TLS *TLSConfig `json:"tls,omitempty"`
}

// ValkeyNodeStatus defines the observed state of ValkeyNode.
type ValkeyNodeStatus struct {
	// ObservedGeneration is the most recent generation observed by the controller.
	// It corresponds to the ValkeyNode's generation, which is updated on mutation
	// of the spec field.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Ready indicates whether the ValkeyNode is ready to serve traffic.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// PodName is the name of the pod created by the workload.
	// +optional
	PodName string `json:"podName,omitempty"`

	// PodIP is the IP address of the pod.
	// +optional
	PodIP string `json:"podIP,omitempty"`

	// Role is the Valkey replication role of this node (primary or replica).
	// +optional
	Role string `json:"role,omitempty"`

	// Conditions represent the current state of the ValkeyNode.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

const (
	// ValkeyNodeConditionReady indicates the ValkeyNode is ready.
	ValkeyNodeConditionReady = "Ready"
)

const (
	// ValkeyNodeReasonPodRunning indicates the pod is running.
	ValkeyNodeReasonPodRunning = "PodRunning"
	// ValkeyNodeReasonPodNotReady indicates the pod is not ready.
	ValkeyNodeReasonPodNotReady = "PodNotReady"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// ValkeyNode is the Schema for the valkeynodes API.
// ValkeyNode is an internal CRD. Users should not create ValkeyNodes directly.
// +kubebuilder:printcolumn:name="Ready",type="boolean",JSONPath=".status.ready",description="Whether the node is ready"
// +kubebuilder:printcolumn:name="Role",type="string",JSONPath=".status.role",description="Valkey replication role"
// +kubebuilder:printcolumn:name="Pod",type="string",JSONPath=".status.podName",description="Pod name"
// +kubebuilder:printcolumn:name="IP",type="string",JSONPath=".status.podIP",description="Pod IP",priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time since creation"
type ValkeyNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec ValkeyNodeSpec `json:"spec"`

	// +optional
	Status ValkeyNodeStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ValkeyNodeList contains a list of ValkeyNode.
type ValkeyNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ValkeyNode `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ValkeyNode{}, &ValkeyNodeList{})
}
