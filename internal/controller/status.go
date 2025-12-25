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
package controller

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

// setCondition is a helper to set a condition with ObservedGeneration
func setCondition(cluster *valkeyiov1alpha1.ValkeyCluster, condType, reason, message string, status metav1.ConditionStatus) {
	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: cluster.Generation,
	})
}

// statusChanged compares two statuses and returns true if they differ (ignoring LastTransitionTime)
func statusChanged(old, new valkeyiov1alpha1.ValkeyClusterStatus) bool {
	// Compare summary fields
	if old.State != new.State || old.Reason != new.Reason || old.Message != new.Message || old.Shards != new.Shards || old.ReadyShards != new.ReadyShards {
		return true
	}
	// Compare conditions (ignoring LastTransitionTime)
	if len(old.Conditions) != len(new.Conditions) {
		return true
	}
	for _, newCond := range new.Conditions {
		oldCond := meta.FindStatusCondition(old.Conditions, newCond.Type)
		if oldCond == nil {
			return true
		}
		// Compare everything except LastTransitionTime
		if oldCond.Status != newCond.Status || oldCond.Reason != newCond.Reason || oldCond.Message != newCond.Message || oldCond.ObservedGeneration != newCond.ObservedGeneration {
			return true
		}
	}
	return false
}
