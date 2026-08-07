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
	"testing"

	. "github.com/onsi/gomega"
	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
