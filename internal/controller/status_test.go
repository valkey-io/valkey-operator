package controller

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

func TestSetCondition(t *testing.T) {
	g := NewWithT(t)

	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	cluster.Generation = 1

	// 1. Add a new condition
	setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonReconciling, "reconciling", metav1.ConditionFalse)

	g.Expect(cluster.Status.Conditions).To(HaveLen(1))
	readyCond := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionReady)
	g.Expect(readyCond).NotTo(BeNil())
	g.Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(readyCond.Reason).To(Equal(valkeyiov1alpha1.ReasonReconciling))
	g.Expect(readyCond.ObservedGeneration).To(Equal(int64(1)))

	// 2. Update an existing condition
	cluster.Generation = 2
	setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonClusterHealthy, "healthy", metav1.ConditionTrue)
	g.Expect(cluster.Status.Conditions).To(HaveLen(1))
	readyCond = meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionReady)
	g.Expect(readyCond).NotTo(BeNil())
	g.Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(readyCond.Reason).To(Equal(valkeyiov1alpha1.ReasonClusterHealthy))
	g.Expect(readyCond.ObservedGeneration).To(Equal(int64(2)))

	// 3. Add a different condition
	setCondition(cluster, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonAddingNodes, "adding nodes", metav1.ConditionTrue)
	g.Expect(cluster.Status.Conditions).To(HaveLen(2))
	progressingCond := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionProgressing)
	g.Expect(progressingCond).NotTo(BeNil())
	g.Expect(progressingCond.Status).To(Equal(metav1.ConditionTrue))
}

func TestStatusChanged(t *testing.T) {
	g := NewWithT(t)
	now := metav1.Now()
	later := metav1.NewTime(now.Add(1 * time.Minute))

	baseStatus := func() valkeyiov1alpha1.ValkeyClusterStatus {
		return valkeyiov1alpha1.ValkeyClusterStatus{
			State:       valkeyiov1alpha1.ClusterStateReady,
			Reason:      valkeyiov1alpha1.ReasonClusterHealthy,
			ReadyShards: 3,
			Shards:      3,
			Conditions: []metav1.Condition{
				{Type: valkeyiov1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "R1", Message: "M1", ObservedGeneration: 1, LastTransitionTime: now},
				{Type: valkeyiov1alpha1.ConditionProgressing, Status: metav1.ConditionFalse, Reason: "R2", Message: "M2", ObservedGeneration: 1, LastTransitionTime: now},
			},
		}
	}

	t.Run("should return false for identical statuses", func(t *testing.T) {
		oldStatus := baseStatus()
		newStatus := baseStatus()
		g.Expect(statusChanged(oldStatus, newStatus)).To(BeFalse())
	})

	t.Run("should return true if State differs", func(t *testing.T) {
		oldStatus := baseStatus()
		newStatus := baseStatus()
		newStatus.State = valkeyiov1alpha1.ClusterStateDegraded
		g.Expect(statusChanged(oldStatus, newStatus)).To(BeTrue())
	})

	t.Run("should return true if Reason differs", func(t *testing.T) {
		oldStatus := baseStatus()
		newStatus := baseStatus()
		newStatus.Reason = "NewReason"
		g.Expect(statusChanged(oldStatus, newStatus)).To(BeTrue())
	})

	t.Run("should return true if number of conditions differs", func(t *testing.T) {
		oldStatus := baseStatus()
		newStatus := baseStatus()
		newStatus.Conditions = newStatus.Conditions[:1] // Remove one condition
		g.Expect(statusChanged(oldStatus, newStatus)).To(BeTrue())
	})

	t.Run("should return true if a condition's status differs", func(t *testing.T) {
		oldStatus := baseStatus()
		newStatus := baseStatus()
		newStatus.Conditions[0].Status = metav1.ConditionFalse
		g.Expect(statusChanged(oldStatus, newStatus)).To(BeTrue())
	})

	t.Run("should return true if a condition's reason differs", func(t *testing.T) {
		oldStatus := baseStatus()
		newStatus := baseStatus()
		newStatus.Conditions[0].Reason = "NewReason"
		g.Expect(statusChanged(oldStatus, newStatus)).To(BeTrue())
	})

	t.Run("should return true if observed generation differs", func(t *testing.T) {
		oldStatus := baseStatus()
		newStatus := baseStatus()
		newStatus.Conditions[0].ObservedGeneration = 2
		g.Expect(statusChanged(oldStatus, newStatus)).To(BeTrue())
	})

	t.Run("should return false if only LastTransitionTime differs", func(t *testing.T) {
		oldStatus := baseStatus()
		newStatus := baseStatus()
		newStatus.Conditions[0].LastTransitionTime = later
		g.Expect(statusChanged(oldStatus, newStatus)).To(BeFalse())
	})
}
