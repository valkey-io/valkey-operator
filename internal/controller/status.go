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
