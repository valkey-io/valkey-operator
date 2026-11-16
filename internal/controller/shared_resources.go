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

package controller

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// resourceClient carries the controller-runtime handles every child resource builder needs.
// It lets the builders be free functions shared by the ValkeyCluster and Valkey reconcilers.
// Each reconciler builds one from its own fields and passes the owner object separately per call.
type resourceClient struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  events.EventRecorder
}

// setConditionOn sets a condition with the given ObservedGeneration on any conditions slice.
// It is the kind-agnostic core of setCondition.
func setConditionOn(conditions *[]metav1.Condition, generation int64, condType, reason, message string, status metav1.ConditionStatus) {
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}
