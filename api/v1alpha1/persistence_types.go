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

import "k8s.io/apimachinery/pkg/api/resource"

// PersistenceSpec defines persistent storage for a Valkey node.
type PersistenceSpec struct {
	// Size is the requested PVC size.
	Size resource.Quantity `json:"size"`

	// StorageClassName is the storage class to use for the PVC.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
}
