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
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

var _ = Describe("reconcilePodDisruptionBudget", func() {
	var (
		ctx        context.Context
		reconciler *ValkeyClusterReconciler
		cluster    *valkeyiov1alpha1.ValkeyCluster
		pdbKey     types.NamespacedName
	)

	BeforeEach(func() {
		ctx = context.Background()
		reconciler = &ValkeyClusterReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: events.NewFakeRecorder(100),
		}
		cluster = &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pdb-test-cluster",
				Namespace: "default",
			},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:   1,
				Replicas: 0,
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		pdbKey = types.NamespacedName{Name: pdbName(cluster), Namespace: cluster.Namespace}

		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, cluster)
			pdb := &policyv1.PodDisruptionBudget{}
			if err := k8sClient.Get(ctx, pdbKey, pdb); err == nil {
				_ = k8sClient.Delete(ctx, pdb)
			}
		})
	})

	Context("when PodDisruptionBudget is unset (default Cluster)", func() {
		It("creates a PDB with maxUnavailable: 1 and the correct selector", func() {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			pdb := &policyv1.PodDisruptionBudget{}
			Expect(k8sClient.Get(ctx, pdbKey, pdb)).To(Succeed())

			maxUnavailable := intstr.FromInt32(1)
			Expect(pdb.Spec.MaxUnavailable).To(Equal(&maxUnavailable))
			Expect(pdb.Spec.Selector.MatchLabels).To(HaveKeyWithValue(LabelCluster, cluster.Name))
		})

		It("recreates the PDB if it is externally deleted", func() {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			pdb := &policyv1.PodDisruptionBudget{}
			Expect(k8sClient.Get(ctx, pdbKey, pdb)).To(Succeed())
			Expect(k8sClient.Delete(ctx, pdb)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, pdbKey, pdb)).To(Succeed())
		})

		It("does not update the PDB on repeated reconciles when nothing changed", func() {
			Expect(reconciler.reconcilePodDisruptionBudget(ctx, cluster)).To(Succeed())

			pdb := &policyv1.PodDisruptionBudget{}
			Expect(k8sClient.Get(ctx, pdbKey, pdb)).To(Succeed())
			createdResourceVersion := pdb.ResourceVersion

			// The PDB mutate assigns fields individually (it never replaces the
			// whole spec), so anything the API server defaults on the stored
			// object — e.g. unhealthyPodEvictionPolicy on newer clusters — must
			// survive and repeated reconciles must be no-ops (#315).
			for range 3 {
				Expect(reconciler.reconcilePodDisruptionBudget(ctx, cluster)).To(Succeed())
			}
			Expect(k8sClient.Get(ctx, pdbKey, pdb)).To(Succeed())
			Expect(pdb.ResourceVersion).To(Equal(createdResourceVersion),
				"a reconcile with no changes must not write the PDB")
		})
	})

	Context("when PodDisruptionBudget mode is Cluster", func() {
		It("creates a PDB with maxUnavailable: 1 and the correct selector", func() {
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}, cluster)).To(Succeed())
			cluster.Spec.PodDisruptionBudget = &valkeyiov1alpha1.PodDisruptionBudgetConfig{
				Mode: valkeyiov1alpha1.PDBModeCluster,
			}
			Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			pdb := &policyv1.PodDisruptionBudget{}
			Expect(k8sClient.Get(ctx, pdbKey, pdb)).To(Succeed())

			maxUnavailable := intstr.FromInt32(1)
			Expect(pdb.Spec.MaxUnavailable).To(Equal(&maxUnavailable))
			Expect(pdb.Spec.Selector.MatchLabels).To(HaveKeyWithValue(LabelCluster, cluster.Name))
		})
	})

	Context("when PodDisruptionBudget is Disabled", func() {
		It("does not create a PDB", func() {
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}, cluster)).To(Succeed())
			cluster.Spec.PodDisruptionBudget = &valkeyiov1alpha1.PodDisruptionBudgetConfig{
				Mode: valkeyiov1alpha1.PDBModeDisabled,
			}
			Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			pdb := &policyv1.PodDisruptionBudget{}
			err = k8sClient.Get(ctx, pdbKey, pdb)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("deletes an existing PDB when switched to Disabled", func() {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			pdb := &policyv1.PodDisruptionBudget{}
			Expect(k8sClient.Get(ctx, pdbKey, pdb)).To(Succeed())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}, cluster)).To(Succeed())
			cluster.Spec.PodDisruptionBudget = &valkeyiov1alpha1.PodDisruptionBudgetConfig{
				Mode: valkeyiov1alpha1.PDBModeDisabled,
			}
			Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, pdbKey, pdb)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})
	})
})
