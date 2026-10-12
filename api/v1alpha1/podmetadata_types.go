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

// LabelValue is a Kubernetes label value. It is a named type so the CRD schema
// can bound its length, which keeps the CEL rules within the cost budget.
// +kubebuilder:validation:MaxLength=63
// +kubebuilder:validation:Pattern=`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`
type LabelValue string

// PodMetadata holds labels and annotations added to Valkey pods. The key and
// value rules mirror the API server's pod metadata checks, using self.matches()
// because Kubernetes 1.31 has no CEL format library. MaxProperties only bounds
// the CEL cost estimate.
type PodMetadata struct {
	// Labels added to the pods.
	// +kubebuilder:validation:MaxProperties=1024
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^([a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?([A-Za-z0-9][-A-Za-z0-9_.]{0,61})?[A-Za-z0-9]$') && (!k.contains('/') || k.indexOf('/') <= 253))",message="label keys must be an optional DNS subdomain prefix (lowercase, at most 253 characters) and '/', followed by a name of at most 63 alphanumerics, '-', '_' or '.', starting and ending with an alphanumeric"
	// +optional
	Labels map[string]LabelValue `json:"labels,omitempty"`

	// Annotations added to the pods. Values are not restricted.
	// +kubebuilder:validation:MaxProperties=1024
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^([A-Za-z0-9]([-A-Za-z0-9]*[A-Za-z0-9])?([.][A-Za-z0-9]([-A-Za-z0-9]*[A-Za-z0-9])?)*/)?([A-Za-z0-9][-A-Za-z0-9_.]{0,61})?[A-Za-z0-9]$') && (!k.contains('/') || k.indexOf('/') <= 253))",message="annotation keys must be an optional DNS subdomain prefix (at most 253 characters) and '/', followed by a name of at most 63 alphanumerics, '-', '_' or '.', starting and ending with an alphanumeric"
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}
