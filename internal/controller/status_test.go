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
	"testing"

	. "github.com/onsi/gomega"
	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
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

func TestApplyConfigurationWarnings(t *testing.T) {
	g := NewWithT(t)

	ctx := context.Background()
	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	recorder := events.NewFakeRecorder(10)
	r := &ValkeyClusterReconciler{Recorder: recorder}
	graceMessage := "spec.terminationGracePeriodSeconds (20s) is below the recommended 30s for cluster-manual-failover-timeout; SIGKILL may interrupt the graceful failover on shutdown"

	g.Expect(cluster.Status.Conditions).To(BeEmpty())

	// Add a warning and ensure the condition is updated
	r.applyConfigurationWarnings(ctx, cluster, []configWarning{{
		reason:  valkeyiov1alpha1.ReasonGracePeriodTooShort,
		message: graceMessage,
	}})

	cond := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionConfigurationWarning)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal(valkeyiov1alpha1.ReasonGracePeriodTooShort))
	g.Expect(cond.Message).To(Equal(graceMessage))
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(<-recorder.Events).To(ContainSubstring(valkeyiov1alpha1.ReasonGracePeriodTooShort))

	// Add another warning and ensure the condition is updated with multiple warnings
	r.applyConfigurationWarnings(ctx, cluster, []configWarning{
		{reason: valkeyiov1alpha1.ReasonGracePeriodTooShort, message: graceMessage},
		{reason: valkeyiov1alpha1.ReasonUnsupportedConfigDirective, message: "directive warning"},
	})

	cond = meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionConfigurationWarning)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal(valkeyiov1alpha1.ReasonMultipleConfigurationWarnings))
	g.Expect(cond.Message).To(Equal(graceMessage + "; directive warning"))
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(<-recorder.Events).To(ContainSubstring(valkeyiov1alpha1.ReasonUnsupportedConfigDirective))

	// Warnings that share the same reason should keep that reason.
	r.applyConfigurationWarnings(ctx, cluster, []configWarning{
		{reason: valkeyiov1alpha1.ReasonUnsupportedConfigDirective, message: "directive warning"},
		{reason: valkeyiov1alpha1.ReasonUnsupportedConfigDirective, message: "another directive warning"},
	})

	cond = meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionConfigurationWarning)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal(valkeyiov1alpha1.ReasonUnsupportedConfigDirective))
	g.Expect(cond.Message).To(Equal("another directive warning; directive warning"))
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))

	// Reapplying the exact same warning set should keep the condition stable.
	before := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionConfigurationWarning)
	r.applyConfigurationWarnings(ctx, cluster, []configWarning{
		{reason: valkeyiov1alpha1.ReasonGracePeriodTooShort, message: graceMessage},
		{reason: valkeyiov1alpha1.ReasonUnsupportedConfigDirective, message: "directive warning"},
	})
	after := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionConfigurationWarning)
	g.Expect(after).NotTo(BeNil())
	g.Expect(after.Reason).To(Equal(before.Reason))
	g.Expect(after.Message).To(Equal(before.Message))

	// remove all warnings and ensure the condition is removed
	r.applyConfigurationWarnings(ctx, cluster, nil)
	g.Expect(meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionConfigurationWarning)).To(BeNil())
}
