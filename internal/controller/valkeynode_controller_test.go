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
	"k8s.io/apimachinery/pkg/api/resource"
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
		statefulSetName := types.NamespacedName{
			Name:      "valkey-" + resourceName,
			Namespace: "default",
		}
		configName := types.NamespacedName{
			Name:      GetServerConfigMapName(resourceName),
			Namespace: "default",
		}
		secretName := types.NamespacedName{
			Name:      getInternalSecretName(resourceName),
			Namespace: "default",
		}
		cleanupManagedPVC := func() {
			pvcName := types.NamespacedName{Name: childName.Name + "-data", Namespace: childName.Namespace}
			pvc := &corev1.PersistentVolumeClaim{}
			if err := k8sClient.Get(ctx, pvcName, pvc); err == nil {
				if len(pvc.Finalizers) > 0 {
					pvc.Finalizers = nil
					Expect(k8sClient.Update(ctx, pvc)).To(Succeed())
				}
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pvc))).To(Succeed())
				Eventually(func() bool {
					return apierrors.IsNotFound(k8sClient.Get(ctx, pvcName, &corev1.PersistentVolumeClaim{}))
				}, 5*time.Second, 100*time.Millisecond).Should(BeTrue())
			}
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
						Labels: map[string]string{
							LabelCluster: resourceName,
						},
					},
					Spec: valkeyiov1alpha1.ValkeyNodeSpec{
						WorkloadType: valkeyiov1alpha1.WorkloadTypeStatefulSet,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
			By("creating the ACL secret")
			secret := &corev1.Secret{}
			err = k8sClient.Get(ctx, secretName, secret)
			if err != nil && apierrors.IsNotFound(err) {
				secret = &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      getInternalSecretName(resourceName),
						Namespace: "default",
					},
					Type: AclSecretType,
				}
				Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			}
		})

		AfterEach(func() {
			// Delete owned resources explicitly — envtest does not run garbage collection.
			cm := &corev1.ConfigMap{}
			if err := k8sClient.Get(ctx, configName, cm); err == nil {
				Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
				Eventually(func() bool {
					return apierrors.IsNotFound(k8sClient.Get(ctx, childName, &corev1.ConfigMap{}))
				}, 5*time.Second, 100*time.Millisecond).Should(BeTrue())
			}
			sts := &appsv1.StatefulSet{}
			if err := k8sClient.Get(ctx, statefulSetName, sts); err == nil {
				Expect(k8sClient.Delete(ctx, sts)).To(Succeed())
				Eventually(func() bool {
					return apierrors.IsNotFound(k8sClient.Get(ctx, childName, &appsv1.StatefulSet{}))
				}, 5*time.Second, 100*time.Millisecond).Should(BeTrue())
			}
			cleanupManagedPVC()

			node := &valkeyiov1alpha1.ValkeyNode{}
			if err := k8sClient.Get(ctx, typeNamespacedName, node); err == nil {
				node.Finalizers = nil
				Expect(k8sClient.Update(ctx, node)).To(Succeed())
				Expect(k8sClient.Delete(ctx, node)).To(Succeed())
				Eventually(func() bool {
					return apierrors.IsNotFound(k8sClient.Get(ctx, typeNamespacedName, &valkeyiov1alpha1.ValkeyNode{}))
				}, 5*time.Second, 100*time.Millisecond).Should(BeTrue())
			}

			secret := &corev1.Secret{}
			if err := k8sClient.Get(ctx, secretName, secret); err == nil {
				Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
				Eventually(func() bool {
					return apierrors.IsNotFound(k8sClient.Get(ctx, secretName, &corev1.Secret{}))
				}, 5*time.Second, 100*time.Millisecond).Should(BeTrue())
			}
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
			Expect(k8sClient.Get(ctx, configName, cm)).To(Succeed())
			Expect(cm.Data).To(HaveKey("valkey.conf"))
			Expect(cm.Data).To(HaveKey("liveness-check.sh"))
			Expect(cm.Data).To(HaveKey("readiness-check.sh"))

			By("verifying the StatefulSet was created with correct labels")
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, statefulSetName, sts)).To(Succeed())
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

		It("should surface pending PVC status before the pod is ready when persistence is enabled", func() {
			node := &valkeyiov1alpha1.ValkeyNode{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, node)).To(Succeed())
			node.Spec.Persistence = &valkeyiov1alpha1.PersistenceSpec{
				Size: resource.MustParse("10Gi"),
			}
			Expect(k8sClient.Update(ctx, node)).To(Succeed())

			r := &ValkeyNodeReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: events.NewFakeRecorder(100),
			}

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			updated := &valkeyiov1alpha1.ValkeyNode{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Status.Ready).To(BeFalse())

			readyCond := testutils.FindCondition(updated.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionReady)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCond.Reason).To(Equal(valkeyiov1alpha1.ValkeyNodeReasonPersistentVolumeClaimPending))
			Expect(readyCond.Message).To(ContainSubstring("PersistentVolumeClaim"))

			pvcCond := testutils.FindCondition(updated.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionPersistentVolumeClaimReady)
			Expect(pvcCond).NotTo(BeNil())
			Expect(pvcCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(pvcCond.Reason).To(Equal(valkeyiov1alpha1.ValkeyNodeReasonPersistentVolumeClaimPending))
			Expect(pvcCond.Message).To(ContainSubstring("PersistentVolumeClaim"))
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

		It("should not update the StatefulSet on a second reconcile when nothing has changed", func() {
			r := &ValkeyNodeReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: events.NewFakeRecorder(100),
			}

			By("first reconcile creates the StatefulSet")
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("capturing ResourceVersion after first reconcile")
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, statefulSetName, sts)).To(Succeed())
			rvAfterFirst := sts.ResourceVersion

			By("second reconcile with no changes")
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying ResourceVersion is unchanged")
			sts2 := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, statefulSetName, sts2)).To(Succeed())
			Expect(sts2.ResourceVersion).To(Equal(rvAfterFirst), "StatefulSet should not be updated when nothing changed")
		})

		It("should create a PVC before reconciling the StatefulSet when persistence is enabled", func() {
			node := &valkeyiov1alpha1.ValkeyNode{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, node)).To(Succeed())
			node.Spec.Persistence = &valkeyiov1alpha1.PersistenceSpec{
				Size: resource.MustParse("10Gi"),
			}
			Expect(k8sClient.Update(ctx, node)).To(Succeed())

			r := &ValkeyNodeReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: events.NewFakeRecorder(100),
			}

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: childName.Name + "-data", Namespace: childName.Namespace}, pvc)).To(Succeed())
			Expect(pvc.Spec.Resources.Requests).To(HaveKey(corev1.ResourceStorage))
			Expect(pvc.Spec.Resources.Requests[corev1.ResourceStorage]).To(Equal(resource.MustParse("10Gi")))
			Expect(pvc.OwnerReferences).To(BeEmpty(), "PVCs should not be tied to ValkeyNode garbage collection")
		})

		It("should add a finalizer when persistence reclaim policy is Delete", func() {
			node := &valkeyiov1alpha1.ValkeyNode{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, node)).To(Succeed())
			node.Spec.Persistence = &valkeyiov1alpha1.PersistenceSpec{
				Size:          resource.MustParse("10Gi"),
				ReclaimPolicy: valkeyiov1alpha1.PersistenceReclaimPolicyDelete,
			}
			Expect(k8sClient.Update(ctx, node)).To(Succeed())

			r := &ValkeyNodeReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: events.NewFakeRecorder(100),
			}

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			updated := &valkeyiov1alpha1.ValkeyNode{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Finalizers).To(ContainElement("valkey.io/persistent-volume-cleanup"))
		})

		It("should delete the managed PVC before allowing ValkeyNode deletion when reclaim policy is Delete", func() {
			node := &valkeyiov1alpha1.ValkeyNode{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, node)).To(Succeed())
			node.Spec.Persistence = &valkeyiov1alpha1.PersistenceSpec{
				Size:          resource.MustParse("10Gi"),
				ReclaimPolicy: valkeyiov1alpha1.PersistenceReclaimPolicyDelete,
			}
			Expect(k8sClient.Update(ctx, node)).To(Succeed())

			r := &ValkeyNodeReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: events.NewFakeRecorder(100),
			}

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			sts := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: childName.Name, Namespace: childName.Namespace},
			}
			Expect(k8sClient.Get(ctx, childName, sts)).To(Succeed())

			Expect(k8sClient.Delete(ctx, node)).To(Succeed())

			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(2 * time.Second))

			Eventually(func() bool {
				pvc := &corev1.PersistentVolumeClaim{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: childName.Name + "-data", Namespace: childName.Namespace}, pvc)
				if apierrors.IsNotFound(err) {
					return true
				}
				return err == nil && pvc.DeletionTimestamp != nil
			}, 5*time.Second, 100*time.Millisecond).Should(BeTrue())

			Eventually(func() bool {
				deletedSTS := &appsv1.StatefulSet{}
				return apierrors.IsNotFound(k8sClient.Get(ctx, childName, deletedSTS))
			}, 5*time.Second, 100*time.Millisecond).Should(BeTrue())

			nodeDuringDelete := &valkeyiov1alpha1.ValkeyNode{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, nodeDuringDelete)).To(Succeed())
			Expect(nodeDuringDelete.Finalizers).To(ContainElement(persistentVolumeCleanupFinalizer))

			pvc := &corev1.PersistentVolumeClaim{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: childName.Name + "-data", Namespace: childName.Namespace}, pvc); err == nil {
				pvc.Finalizers = nil
				Expect(k8sClient.Update(ctx, pvc)).To(Succeed())
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pvc))).To(Succeed())
				Eventually(func() bool {
					return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: childName.Name + "-data", Namespace: childName.Namespace}, &corev1.PersistentVolumeClaim{}))
				}, 5*time.Second, 100*time.Millisecond).Should(BeTrue())
			}

			Eventually(func() bool {
				_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				deletedNode := &valkeyiov1alpha1.ValkeyNode{}
				return apierrors.IsNotFound(k8sClient.Get(ctx, typeNamespacedName, deletedNode))
			}, 5*time.Second, 100*time.Millisecond).Should(BeTrue())

		})

		It("should not fail when persistence size changes before the PVC is bound", func() {
			node := &valkeyiov1alpha1.ValkeyNode{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, node)).To(Succeed())
			node.Spec.Persistence = &valkeyiov1alpha1.PersistenceSpec{
				Size: resource.MustParse("10Gi"),
			}
			Expect(k8sClient.Update(ctx, node)).To(Succeed())

			r := &ValkeyNodeReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: events.NewFakeRecorder(100),
			}

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			node = &valkeyiov1alpha1.ValkeyNode{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, node)).To(Succeed())
			node.Spec.Persistence.Size = resource.MustParse("20Gi")
			Expect(k8sClient.Update(ctx, node)).To(Succeed())

			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: childName.Name + "-data", Namespace: childName.Namespace}, pvc)).To(Succeed())
			Expect(pvc.Spec.Resources.Requests[corev1.ResourceStorage]).To(Equal(resource.MustParse("10Gi")))
		})

		It("should surface resize progress when the PVC is still expanding", func() {
			node := &valkeyiov1alpha1.ValkeyNode{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, node)).To(Succeed())
			node.Spec.Persistence = &valkeyiov1alpha1.PersistenceSpec{
				Size: resource.MustParse("20Gi"),
			}
			Expect(k8sClient.Update(ctx, node)).To(Succeed())

			pvc := buildValkeyNodePVC(node)
			cleanupManagedPVC()
			Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pvc.Name, Namespace: pvc.Namespace}, pvc)).To(Succeed())
			pvc.Status.Phase = corev1.ClaimBound
			pvc.Status.Capacity = corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("10Gi"),
			}
			pvc.Status.AllocatedResourceStatuses = map[corev1.ResourceName]corev1.ClaimResourceStatus{
				corev1.ResourceStorage: corev1.PersistentVolumeClaimNodeResizePending,
			}
			Expect(k8sClient.Status().Update(ctx, pvc)).To(Succeed())

			r := &ValkeyNodeReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: events.NewFakeRecorder(100),
			}

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			updated := &valkeyiov1alpha1.ValkeyNode{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			sizeCond := testutils.FindCondition(updated.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionPersistentVolumeClaimSizeReady)
			Expect(sizeCond).NotTo(BeNil())
			Expect(sizeCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(sizeCond.Reason).To(Equal(valkeyiov1alpha1.ValkeyNodeReasonPersistentVolumeClaimResizePending))
			Expect(sizeCond.Message).To(ContainSubstring("filesystem resize"))
		})

		It("should surface resize failures when the PVC cannot expand further", func() {
			node := &valkeyiov1alpha1.ValkeyNode{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, node)).To(Succeed())
			node.Spec.Persistence = &valkeyiov1alpha1.PersistenceSpec{
				Size: resource.MustParse("20Gi"),
			}
			Expect(k8sClient.Update(ctx, node)).To(Succeed())

			pvc := buildValkeyNodePVC(node)
			cleanupManagedPVC()
			Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pvc.Name, Namespace: pvc.Namespace}, pvc)).To(Succeed())
			pvc.Status.Phase = corev1.ClaimBound
			pvc.Status.Capacity = corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("10Gi"),
			}
			pvc.Status.AllocatedResourceStatuses = map[corev1.ResourceName]corev1.ClaimResourceStatus{
				corev1.ResourceStorage: corev1.PersistentVolumeClaimControllerResizeInfeasible,
			}
			Expect(k8sClient.Status().Update(ctx, pvc)).To(Succeed())

			r := &ValkeyNodeReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: events.NewFakeRecorder(100),
			}

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			updated := &valkeyiov1alpha1.ValkeyNode{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			sizeCond := testutils.FindCondition(updated.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionPersistentVolumeClaimSizeReady)
			Expect(sizeCond).NotTo(BeNil())
			Expect(sizeCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(sizeCond.Reason).To(Equal(valkeyiov1alpha1.ValkeyNodeReasonPersistentVolumeClaimResizeInfeasible))
			Expect(sizeCond.Message).To(ContainSubstring("cannot be satisfied"))
		})
	})

	Context("When WorkloadType is Deployment", func() {
		const resourceName = "test-valkeynode-deploy"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		childName := types.NamespacedName{
			Name:      "valkey-" + resourceName,
			Namespace: "default",
		}
		configName := types.NamespacedName{
			Name:      GetServerConfigMapName(resourceName),
			Namespace: "default",
		}
		secretName := types.NamespacedName{
			Name:      getInternalSecretName(resourceName),
			Namespace: "default",
		}

		BeforeEach(func() {
			node := &valkeyiov1alpha1.ValkeyNode{}
			err := k8sClient.Get(ctx, typeNamespacedName, node)
			if err != nil && apierrors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, &valkeyiov1alpha1.ValkeyNode{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
						Labels: map[string]string{
							LabelCluster: resourceName,
						},
					},
					Spec: valkeyiov1alpha1.ValkeyNodeSpec{
						WorkloadType: valkeyiov1alpha1.WorkloadTypeDeployment,
					},
				})).To(Succeed())
			}
			secret := &corev1.Secret{}
			err = k8sClient.Get(ctx, secretName, secret)
			if err != nil && apierrors.IsNotFound(err) {
				secret = &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: getInternalSecretName(resourceName), Namespace: "default"},
					Type:       AclSecretType,
				}
				Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			}
		})

		AfterEach(func() {
			cm := &corev1.ConfigMap{}
			if err := k8sClient.Get(ctx, configName, cm); err == nil {
				Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
			}
			deploy := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, childName, deploy); err == nil {
				Expect(k8sClient.Delete(ctx, deploy)).To(Succeed())
			}
			node := &valkeyiov1alpha1.ValkeyNode{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, node)).To(Succeed())
			Expect(k8sClient.Delete(ctx, node)).To(Succeed())
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, secretName, secret)).To(Succeed())
			Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
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

		It("should propagate Spec.Image changes to the Deployment on subsequent reconciles", func() {
			r := &ValkeyNodeReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: events.NewFakeRecorder(100),
			}

			By("first reconcile creates the Deployment")
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the initial image is the default")
			initialDeploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, childName, initialDeploy)).To(Succeed())
			Expect(initialDeploy.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(initialDeploy.Spec.Template.Spec.Containers[0].Image).NotTo(Equal("valkey/valkey:8.0.0"))

			By("updating the ValkeyNode image")
			node := &valkeyiov1alpha1.ValkeyNode{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, node)).To(Succeed())
			node.Spec.Image = "valkey/valkey:8.0.0"
			Expect(k8sClient.Update(ctx, node)).To(Succeed())

			By("second reconcile should propagate the image change")
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, childName, deploy)).To(Succeed())
			Expect(deploy.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal("valkey/valkey:8.0.0"))
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

	Context("When persistence is used with Deployment", func() {
		It("should return an error", func() {
			r := &ValkeyNodeReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: events.NewFakeRecorder(100),
			}
			node := &valkeyiov1alpha1.ValkeyNode{
				Spec: valkeyiov1alpha1.ValkeyNodeSpec{
					WorkloadType: valkeyiov1alpha1.WorkloadTypeDeployment,
					Persistence: &valkeyiov1alpha1.PersistenceSpec{
						Size: resource.MustParse("1Gi"),
					},
				},
			}
			Expect(r.ensureWorkload(context.Background(), node)).To(MatchError(ContainSubstring("persistence requires workloadType StatefulSet")))
		})
	})
})

var _ = Describe("isWorkloadRolledOut", func() {
	const ns = "default"
	ctx := context.Background()

	makeReconciler := func() *ValkeyNodeReconciler {
		return &ValkeyNodeReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: events.NewFakeRecorder(100),
			// Leave APIReader nil so the test uses the cached client (simpler for envtest)
		}
	}

	makeNode := func(name string, wt valkeyiov1alpha1.WorkloadType) *valkeyiov1alpha1.ValkeyNode {
		return &valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       valkeyiov1alpha1.ValkeyNodeSpec{WorkloadType: wt},
		}
	}

	Context("StatefulSet workload", func() {
		const nodeName = "isrolled-sts"
		stsName := types.NamespacedName{Name: "valkey-" + nodeName, Namespace: ns}
		node := makeNode(nodeName, valkeyiov1alpha1.WorkloadTypeStatefulSet)

		It("returns false when the StatefulSet does not exist", func() {
			r := makeReconciler()
			rolled, err := r.isWorkloadRolledOut(ctx, node)
			Expect(err).NotTo(HaveOccurred())
			Expect(rolled).To(BeFalse())
		})

		It("returns false when ObservedGeneration < Generation", func() {
			r := makeReconciler()
			replicas := int32(1)
			sts := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: stsName.Name, Namespace: stsName.Namespace},
				Spec: appsv1.StatefulSetSpec{
					Replicas: &replicas,
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": nodeName}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": nodeName}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "valkey/valkey:9.0.0"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, sts)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, sts) }()

			// Leave Status.ObservedGeneration at 0 while Generation > 0
			rolled, err := r.isWorkloadRolledOut(ctx, node)
			Expect(err).NotTo(HaveOccurred())
			Expect(rolled).To(BeFalse())
		})

		It("returns false when CurrentRevision != UpdateRevision", func() {
			r := makeReconciler()
			replicas := int32(1)
			stsName2 := types.NamespacedName{Name: "valkey-" + nodeName + "-rev", Namespace: ns}
			node2 := makeNode(nodeName+"-rev", valkeyiov1alpha1.WorkloadTypeStatefulSet)
			sts := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: stsName2.Name, Namespace: stsName2.Namespace},
				Spec: appsv1.StatefulSetSpec{
					Replicas: &replicas,
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": node2.Name}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": node2.Name}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "valkey/valkey:9.0.0"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, sts)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, sts) }()

			// Re-Get to ensure we have the latest ResourceVersion before status update
			Expect(k8sClient.Get(ctx, stsName2, sts)).To(Succeed())
			sts.Status.ObservedGeneration = sts.Generation
			sts.Status.Replicas = 1
			sts.Status.ReadyReplicas = 1
			sts.Status.CurrentRevision = "old-rev"
			sts.Status.UpdateRevision = "new-rev"
			Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())

			rolled, err := r.isWorkloadRolledOut(ctx, node2)
			Expect(err).NotTo(HaveOccurred())
			Expect(rolled).To(BeFalse())
		})

		It("returns true when fully rolled out", func() {
			r := makeReconciler()
			replicas := int32(1)
			stsName3 := types.NamespacedName{Name: "valkey-" + nodeName + "-done", Namespace: ns}
			node3 := makeNode(nodeName+"-done", valkeyiov1alpha1.WorkloadTypeStatefulSet)
			sts := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: stsName3.Name, Namespace: stsName3.Namespace},
				Spec: appsv1.StatefulSetSpec{
					Replicas: &replicas,
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": node3.Name}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": node3.Name}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "valkey/valkey:9.0.0"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, sts)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, sts) }()

			Expect(k8sClient.Get(ctx, stsName3, sts)).To(Succeed())
			sts.Status.ObservedGeneration = sts.Generation
			sts.Status.Replicas = 1
			sts.Status.ReadyReplicas = 1
			sts.Status.CurrentRevision = "rev-1"
			sts.Status.UpdateRevision = "rev-1"
			Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())

			rolled, err := r.isWorkloadRolledOut(ctx, node3)
			Expect(err).NotTo(HaveOccurred())
			Expect(rolled).To(BeTrue())
		})
	})

	Context("Deployment workload", func() {
		const nodeName = "isrolled-deploy"
		node := makeNode(nodeName, valkeyiov1alpha1.WorkloadTypeDeployment)

		It("returns false when ObservedGeneration < Generation", func() {
			r := makeReconciler()
			replicas := int32(1)
			dep := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "valkey-" + nodeName, Namespace: ns},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": nodeName}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": nodeName}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "valkey/valkey:9.0.0"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, dep) }()

			// Status.ObservedGeneration stays at 0
			rolled, err := r.isWorkloadRolledOut(ctx, node)
			Expect(err).NotTo(HaveOccurred())
			Expect(rolled).To(BeFalse())
		})

		It("returns true when fully rolled out", func() {
			r := makeReconciler()
			replicas := int32(1)
			depName := "valkey-" + nodeName + "-done"
			node2 := makeNode(nodeName+"-done", valkeyiov1alpha1.WorkloadTypeDeployment)
			dep := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: depName, Namespace: ns},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": node2.Name}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": node2.Name}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "valkey/valkey:9.0.0"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, dep) }()

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: depName, Namespace: ns}, dep)).To(Succeed())
			dep.Status.ObservedGeneration = dep.Generation
			dep.Status.Replicas = 1
			dep.Status.UpdatedReplicas = 1
			dep.Status.ReadyReplicas = 1
			Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

			rolled, err := r.isWorkloadRolledOut(ctx, node2)
			Expect(err).NotTo(HaveOccurred())
			Expect(rolled).To(BeTrue())
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
