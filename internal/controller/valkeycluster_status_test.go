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
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

var _ = Describe("ValkeyCluster updateStatus", func() {
	ctx := context.Background()

	newReconciler := func() *ValkeyClusterReconciler {
		return &ValkeyClusterReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(), Recorder: events.NewFakeRecorder(10)}
	}

	// create makes a cluster whose status says Degraded=UsersACLUnresolved,
	// and returns it as a reconcile would have read it.
	create := func(name string) *valkeyiov1alpha1.ValkeyCluster {
		GinkgoHelper()
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       valkeyiov1alpha1.ValkeyClusterSpec{Shards: 1},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })
		setCondition(cluster, valkeyiov1alpha1.ConditionDegraded, valkeyiov1alpha1.ReasonUsersACLUnresolved, "users ACL unresolved", metav1.ConditionTrue)
		cluster.Status.State = valkeyiov1alpha1.ClusterStateDegraded
		Expect(k8sClient.Status().Update(ctx, cluster)).To(Succeed())
		read := &valkeyiov1alpha1.ValkeyCluster{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), read)).To(Succeed())
		return read
	}

	It("does not put back conditions a later reconcile removed", func() {
		stale := create("status-stale-reconcile")
		key := client.ObjectKeyFromObject(stale)

		By("a later reconcile resolving the ACL")
		later := stale.DeepCopy()
		meta.RemoveStatusCondition(&later.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)
		setCondition(later, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonClusterHealthy, "healthy", metav1.ConditionTrue)
		Expect(newReconciler().updateStatus(ctx, later, nil)).To(Succeed())

		By("a reconcile that started from the earlier read writing its status")
		setCondition(stale, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonReconciling, "reconciling", metav1.ConditionTrue)
		err := newReconciler().updateStatus(ctx, stale, nil)
		Expect(apierrors.IsConflict(err)).To(BeTrue(), "got %v", err)

		got := &valkeyiov1alpha1.ValkeyCluster{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(meta.FindStatusCondition(got.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)).To(BeNil())
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, valkeyiov1alpha1.ConditionReady)).To(BeTrue())
		Expect(got.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
	})

	It("writes repeatedly within one reconcile", func() {
		cluster := create("status-same-reconcile")
		key := client.ObjectKeyFromObject(cluster)
		r := newReconciler()

		meta.RemoveStatusCondition(&cluster.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)
		setCondition(cluster, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonReconciling, "reconciling", metav1.ConditionTrue)
		Expect(r.updateStatus(ctx, cluster, nil)).To(Succeed())

		setCondition(cluster, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonReconciling, "reconciling", metav1.ConditionFalse)
		setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonClusterHealthy, "healthy", metav1.ConditionTrue)
		Expect(r.updateStatus(ctx, cluster, nil)).To(Succeed())

		got := &valkeyiov1alpha1.ValkeyCluster{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, valkeyiov1alpha1.ConditionReady)).To(BeTrue())
		Expect(meta.FindStatusCondition(got.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)).To(BeNil())
		Expect(got.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
	})
})
