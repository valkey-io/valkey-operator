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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
	testutils "valkey.io/valkey-operator/test/utils"
)

var _ = Describe("ValkeyCluster Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		valkeycluster := &valkeyiov1alpha1.ValkeyCluster{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind ValkeyCluster")
			err := k8sClient.Get(ctx, typeNamespacedName, valkeycluster)
			if err != nil && errors.IsNotFound(err) {
				resource := &valkeyiov1alpha1.ValkeyCluster{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: valkeyiov1alpha1.ValkeyClusterSpec{
						Shards:   3,
						Replicas: 1,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &valkeyiov1alpha1.ValkeyCluster{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance ValkeyCluster")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &ValkeyClusterReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Check status conditions
			updatedValkeyCluster := &valkeyiov1alpha1.ValkeyCluster{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updatedValkeyCluster)).To(Succeed())
			Expect(updatedValkeyCluster.Status.Conditions).ToNot(BeEmpty())

			// Verify that the Ready condition is set to False initially
			readyCondition := testutils.FindCondition(updatedValkeyCluster.Status.Conditions, valkeyiov1alpha1.ConditionReady)
			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))

			// Verify that the Progressing condition is set to True
			progressingCondition := testutils.FindCondition(updatedValkeyCluster.Status.Conditions, valkeyiov1alpha1.ConditionProgressing)
			Expect(progressingCondition).NotTo(BeNil())
			Expect(progressingCondition.Status).To(Equal(metav1.ConditionTrue))
		})
	})
})

var _ = Describe("updateStatus", func() {
	var (
		cluster *valkeyiov1alpha1.ValkeyCluster
		r       *ValkeyClusterReconciler
		ctx     context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		cluster = &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "default",
			},
		}
		// In a real scenario, the reconciler would be created with a real client,
		// but for this focused unit test, we can use a fake client if needed,
		// or pass nil if the tested function doesn't use the client.
		// For updateStatus, we need a client to Get the current object.
		// The envtest client is used here.
		r = &ValkeyClusterReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}

		// Create the cluster object in the fake client
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
	})

	It("should set state to Ready when Ready condition is True", func() {
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:   valkeyiov1alpha1.ConditionReady,
			Status: metav1.ConditionTrue,
			Reason: valkeyiov1alpha1.ReasonClusterHealthy,
		})

		err := r.updateStatus(ctx, cluster, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(cluster.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
		Expect(cluster.Status.Reason).To(Equal(valkeyiov1alpha1.ReasonClusterHealthy))
	})

	It("should set state to Degraded when Degraded condition is True", func() {
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:   valkeyiov1alpha1.ConditionDegraded,
			Status: metav1.ConditionTrue,
			Reason: valkeyiov1alpha1.ReasonNodeAddFailed,
		})
		// A degraded cluster can still be progressing
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:   valkeyiov1alpha1.ConditionProgressing,
			Status: metav1.ConditionTrue,
			Reason: valkeyiov1alpha1.ReasonAddingNodes,
		})

		err := r.updateStatus(ctx, cluster, nil)
		Expect(err).NotTo(HaveOccurred())
		// Degraded takes precedence over Progressing
		Expect(cluster.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateDegraded))
		Expect(cluster.Status.Reason).To(Equal(valkeyiov1alpha1.ReasonNodeAddFailed))
	})

	It("should set state to Reconciling when Progressing is True and shards > 0", func() {
		// First set the cluster to Initializing
		cluster.Status.State = valkeyiov1alpha1.ClusterStateInitializing
		cluster.Status.Shards = 1 // Simulate that the cluster is not new
		Expect(k8sClient.Status().Update(ctx, cluster)).To(Succeed())

		// Now set the Progressing condition
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:   valkeyiov1alpha1.ConditionProgressing,
			Status: metav1.ConditionTrue,
			Reason: valkeyiov1alpha1.ReasonReconciling,
		})

		err := r.updateStatus(ctx, cluster, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(cluster.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReconciling))
		Expect(cluster.Status.Reason).To(Equal(valkeyiov1alpha1.ReasonReconciling))
	})

	It("should set state to Initializing when Progressing is True and shards = 0", func() {
		cluster.Status.Shards = 0 // This is a new cluster
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:   valkeyiov1alpha1.ConditionProgressing,
			Status: metav1.ConditionTrue,
			Reason: valkeyiov1alpha1.ReasonInitializing,
		})

		err := r.updateStatus(ctx, cluster, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(cluster.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateInitializing))
		Expect(cluster.Status.Reason).To(Equal(valkeyiov1alpha1.ReasonInitializing))
	})
})
