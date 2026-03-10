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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
	testutils "valkey.io/valkey-operator/test/utils"
)

var _ = Describe("ValkeyNode Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-valkeynode"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		childName := types.NamespacedName{
			Name:      "valkey-" + resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			By("creating the custom resource for the Kind ValkeyNode")
			node := &valkeyiov1alpha1.ValkeyNode{}
			err := k8sClient.Get(ctx, typeNamespacedName, node)
			if err != nil && apierrors.IsNotFound(err) {
				resource := &valkeyiov1alpha1.ValkeyNode{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: valkeyiov1alpha1.ValkeyNodeSpec{
						WorkloadType: valkeyiov1alpha1.WorkloadTypeStatefulSet,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// Delete owned resources explicitly — envtest does not run garbage collection.
			cm := &corev1.ConfigMap{}
			if err := k8sClient.Get(ctx, childName, cm); err == nil {
				Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
			}
			sts := &appsv1.StatefulSet{}
			if err := k8sClient.Get(ctx, childName, sts); err == nil {
				Expect(k8sClient.Delete(ctx, sts)).To(Succeed())
			}

			node := &valkeyiov1alpha1.ValkeyNode{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, node)).To(Succeed())
			Expect(k8sClient.Delete(ctx, node)).To(Succeed())
		})

		It("should create a ConfigMap and StatefulSet on first reconcile", func() {
			By("Reconciling the created resource")
			r := &ValkeyNodeReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: events.NewFakeRecorder(100),
			}

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the ConfigMap was created with probe scripts")
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, childName, cm)).To(Succeed())
			Expect(cm.Data).To(HaveKey("valkey.conf"))
			Expect(cm.Data).To(HaveKey("liveness-check.sh"))
			Expect(cm.Data).To(HaveKey("readiness-check.sh"))

			By("verifying the StatefulSet was created with correct labels")
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, childName, sts)).To(Succeed())
			Expect(sts.Spec.Template.Labels).To(HaveKeyWithValue("app.kubernetes.io/instance", resourceName))
			Expect(sts.Spec.Template.Labels).To(HaveKeyWithValue("app.kubernetes.io/component", "valkey-node"))
		})

		It("should set Ready=false with PodNotReady condition when no pod exists", func() {
			By("Reconciling the created resource")
			r := &ValkeyNodeReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: events.NewFakeRecorder(100),
			}

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("checking the ValkeyNode status reflects no running pod")
			updated := &valkeyiov1alpha1.ValkeyNode{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())

			Expect(updated.Status.Ready).To(BeFalse())
			Expect(updated.Status.PodName).To(BeEmpty())

			readyCond := testutils.FindCondition(updated.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionReady)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCond.Reason).To(Equal(valkeyiov1alpha1.ValkeyNodeReasonPodNotReady))
		})

		It("should requeue after 10 seconds when node is not ready", func() {
			r := &ValkeyNodeReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: events.NewFakeRecorder(100),
			}

			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(10 * time.Second))
		})
	})

	Context("When WorkloadType is Deployment", func() {
		const resourceName = "test-valkeynode-deploy"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: "default"}
		childName := types.NamespacedName{Name: "valkey-" + resourceName, Namespace: "default"}

		BeforeEach(func() {
			node := &valkeyiov1alpha1.ValkeyNode{}
			err := k8sClient.Get(ctx, typeNamespacedName, node)
			if err != nil && apierrors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, &valkeyiov1alpha1.ValkeyNode{
					ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: "default"},
					Spec: valkeyiov1alpha1.ValkeyNodeSpec{
						WorkloadType: valkeyiov1alpha1.WorkloadTypeDeployment,
					},
				})).To(Succeed())
			}
		})

		AfterEach(func() {
			cm := &corev1.ConfigMap{}
			if err := k8sClient.Get(ctx, childName, cm); err == nil {
				Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
			}
			deploy := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, childName, deploy); err == nil {
				Expect(k8sClient.Delete(ctx, deploy)).To(Succeed())
			}
			node := &valkeyiov1alpha1.ValkeyNode{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, node)).To(Succeed())
			Expect(k8sClient.Delete(ctx, node)).To(Succeed())
		})

		It("should create a Deployment and no StatefulSet", func() {
			r := &ValkeyNodeReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: events.NewFakeRecorder(100),
			}

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying a Deployment was created with correct labels")
			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, childName, deploy)).To(Succeed())
			Expect(deploy.Spec.Template.Labels).To(HaveKeyWithValue("app.kubernetes.io/instance", resourceName))

			By("verifying no StatefulSet was created")
			sts := &appsv1.StatefulSet{}
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, childName, sts))).To(BeTrue())
		})
	})

	Context("When WorkloadType is unsupported", func() {
		It("should return an error", func() {
			r := &ValkeyNodeReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: events.NewFakeRecorder(100),
			}
			node := &valkeyiov1alpha1.ValkeyNode{
				Spec: valkeyiov1alpha1.ValkeyNodeSpec{WorkloadType: "DaemonSet"},
			}
			Expect(r.ensureWorkload(context.Background(), node)).To(MatchError(ContainSubstring("unsupported workload type")))
		})
	})
})

var _ = Describe("ValkeyNode updateStatus", func() {
	var (
		node *valkeyiov1alpha1.ValkeyNode
		r    *ValkeyNodeReconciler
		ctx  context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		node = &valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "updatestatus-test",
				Namespace: "default",
			},
			Spec: valkeyiov1alpha1.ValkeyNodeSpec{
				WorkloadType: valkeyiov1alpha1.WorkloadTypeStatefulSet,
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		r = &ValkeyNodeReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: events.NewFakeRecorder(100),
		}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, node)).To(Succeed())
	})

	It("should set Ready=false with PodNotReady condition when no pod exists", func() {
		Expect(r.updateStatus(ctx, node)).To(Succeed())

		updated := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(node), updated)).To(Succeed())

		Expect(updated.Status.Ready).To(BeFalse())
		readyCond := testutils.FindCondition(updated.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(readyCond.Reason).To(Equal(valkeyiov1alpha1.ValkeyNodeReasonPodNotReady))
	})

	It("should not write status when nothing has changed between calls", func() {
		By("calling updateStatus to establish baseline status")
		Expect(r.updateStatus(ctx, node)).To(Succeed())

		updated := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(node), updated)).To(Succeed())
		rvAfterFirst := updated.ResourceVersion

		By("calling updateStatus again with no state change")
		Expect(r.updateStatus(ctx, node)).To(Succeed())

		updated2 := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(node), updated2)).To(Succeed())
		Expect(updated2.ResourceVersion).To(Equal(rvAfterFirst), "status write should be skipped when nothing changed")
	})
})
