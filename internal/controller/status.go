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
	"context"
	"slices"
	"strings"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
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

func removeConditionIfReason(conditions *[]metav1.Condition, condType, reason string) {
	condition := meta.FindStatusCondition(*conditions, condType)
	if condition != nil && condition.Reason == reason {
		meta.RemoveStatusCondition(conditions, condType)
	}
}

// configWarning is a entry that represents a configuration warning with a reason and message.
type configWarning struct {
	reason  string
	message string
}

// applyConfigurationWarnings applies configuration warnings to the ValkeyCluster status.
// It updates the ConditionConfigurationWarning condition based on the provided warnings.
// If there are no warnings, it removes the condition. If there are warnings,
// it sorts them, constructs a message, and sets the condition with the reason and message.
func (r *ValkeyClusterReconciler) applyConfigurationWarnings(cluster *valkeyiov1alpha1.ValkeyCluster, warnings []configWarning) {
	if len(warnings) == 0 {
		meta.RemoveStatusCondition(&cluster.Status.Conditions, valkeyiov1alpha1.ConditionConfigurationWarning)
		return
	}

	existing := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionConfigurationWarning)
	reported := ""
	if existing != nil && existing.Status == metav1.ConditionTrue {
		reported = existing.Message
	}

	slices.SortStableFunc(warnings, func(a, b configWarning) int {
		if a.reason != b.reason {
			return strings.Compare(a.reason, b.reason)
		}
		if a.message != b.message {
			return strings.Compare(a.message, b.message)
		}
		return 0
	})

	messages := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		messages = append(messages, warning.message)
		if strings.Contains(reported, warning.message) {
			continue
		}
		logf.FromContext(context.Background()).Info("configuration warning", "reason", warning.reason, "detail", warning.message)
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, warning.reason, "ReconcileValkeyCluster", "%s", warning.message)
	}

	reason := warnings[0].reason
	if len(warnings) > 1 {
		reason = valkeyiov1alpha1.ReasonMultipleConfigurationWarnings
	}
	setCondition(cluster, valkeyiov1alpha1.ConditionConfigurationWarning, reason, strings.Join(messages, "; "), metav1.ConditionTrue)
}
