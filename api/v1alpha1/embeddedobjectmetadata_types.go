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

// A named type lets the CRD schema bound label values, which keeps the
// EmbeddedObjectMetadata CEL rules within the cost budget.

// LabelValue is a label value: at most 63 characters, empty or alphanumerics,
// '-', '_' and '.', starting and ending with an alphanumeric.
// +kubebuilder:validation:MaxLength=63
// +kubebuilder:validation:Pattern=`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`
type LabelValue string

// EmbeddedObjectMetadata holds labels and annotations for objects the operator
// creates. The key and value rules mirror the API server's metadata checks,
// using self.matches() because Kubernetes 1.31 has no CEL format library.
// MaxProperties only bounds the CEL cost estimate. Annotation sizes are counted
// in characters, not bytes: size(bytes()) exceeds the CEL cost budget unless
// values and counts are capped. 262059 is 256 KiB minus the operator's
// valkey.io/config-hash annotation (85 bytes).
type EmbeddedObjectMetadata struct {
	// Labels added to the object. Keys under valkey.io/ and the
	// app.kubernetes.io/ name, instance, component, part-of and managed-by keys
	// are reserved for the operator.
	// +kubebuilder:validation:MaxProperties=1024
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^([a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?([A-Za-z0-9][-A-Za-z0-9_.]{0,61})?[A-Za-z0-9]$') && (!k.contains('/') || k.indexOf('/') <= 253))",message="label keys must be an optional DNS subdomain prefix (lowercase, at most 253 characters) and '/', followed by a name of at most 63 alphanumerics, '-', '_' or '.', starting and ending with an alphanumeric"
	// +kubebuilder:validation:XValidation:rule="self.all(k, !k.startsWith('valkey.io/') && !(k in ['app.kubernetes.io/name', 'app.kubernetes.io/instance', 'app.kubernetes.io/component', 'app.kubernetes.io/part-of', 'app.kubernetes.io/managed-by']))",message="label keys under valkey.io/ and app.kubernetes.io/ name, instance, component, part-of and managed-by are reserved for the operator"
	// +optional
	Labels map[string]LabelValue `json:"labels,omitempty"`

	// Annotations added to the object. Values have no format rule, but keys and
	// values together are limited to 256 KiB less the operator's own annotation.
	// Keys under valkey.io/ are reserved for the operator.
	// +kubebuilder:validation:MaxProperties=1024
	// +kubebuilder:validation:XValidation:rule="self.map(k, size(k) + size(self[k])).sum() <= 262059",message="annotations must total at most 262059 characters (keys plus values): 256 KiB less the operator's valkey.io/config-hash annotation"
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^([A-Za-z0-9]([-A-Za-z0-9]*[A-Za-z0-9])?([.][A-Za-z0-9]([-A-Za-z0-9]*[A-Za-z0-9])?)*/)?([A-Za-z0-9][-A-Za-z0-9_.]{0,61})?[A-Za-z0-9]$') && (!k.contains('/') || k.indexOf('/') <= 253))",message="annotation keys must be an optional DNS subdomain prefix (at most 253 characters) and '/', followed by a name of at most 63 alphanumerics, '-', '_' or '.', starting and ending with an alphanumeric"
	// +kubebuilder:validation:XValidation:rule="self.all(k, !k.startsWith('valkey.io/'))",message="annotation keys under valkey.io/ are reserved for the operator"
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}
