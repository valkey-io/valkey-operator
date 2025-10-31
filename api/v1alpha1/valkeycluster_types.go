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

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ValkeyClusterSpec defines the desired state of ValkeyCluster
type ValkeyClusterSpec struct {

	// Override the default Valkey image
	Image string `json:"image,omitempty"`

	// Exporter Configuration options
	// +optional
	Exporter ExporterSpec `json:"exporter,omitempty"`

	// The number of shards groups. Each shard group contains one primary and N replicas.
	// +kubebuilder:validation:Minimum=1
	Shards int32 `json:"shards,omitempty"`

	// The number of replicas for each shard group.
	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas,omitempty"`

	// Override resource requirements for the Valkey container in each pod
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Tolerations to apply to the pods
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// NodeSelector to apply to the pods
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Affinity to apply to the pods, overrides NodeSelector if set
	// Some basic anti-affinity rules will be applied by default to spread pods across nodes and zones
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`
}

type ExporterSpec struct {
	// Override the default Exporter image
	Image string `json:"image,omitempty"`

	// Override resource requirements for the Valkey exporter container in each pod
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Enable or disable the exporter sidecar container
	// +kubebuilder:default=true
	Enabled bool `json:"enabled,omitempty"`
}

// ValkeyClusterStatus defines the observed state of ValkeyCluster.
type ValkeyClusterStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the ValkeyCluster resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vkc

// ValkeyCluster is the Schema for the valkeyclusters API
// +kubebuilder:printcolumn:name="Ready",type="integer",JSONPath=".status.conditions[?(@.type=='Available')].status",description="Number of Ready Shard Groups"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time since creation of ValkeyCluster"
// +kubebuilder:printcolumn:name="Shards",type="integer",JSONPath=".spec.shards",description="Number of Shard Groups"
// +kubebuilder:printcolumn:name="Replicas",type="integer",JSONPath=".spec.replicas",description="Number of Replicas per Shard Group"
type ValkeyCluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of ValkeyCluster
	// +required
	Spec ValkeyClusterSpec `json:"spec"`

	// status defines the observed state of ValkeyCluster
	// +optional
	Status ValkeyClusterStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// ValkeyClusterList contains a list of ValkeyCluster
type ValkeyClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ValkeyCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ValkeyCluster{}, &ValkeyClusterList{})
}
