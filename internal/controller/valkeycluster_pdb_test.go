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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
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
		pdbKey = types.NamespacedName{Name: pdbName(cluster, 0), Namespace: cluster.Namespace}

		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, cluster)
			pdb := &policyv1.PodDisruptionBudget{}
			if err := k8sClient.Get(ctx, pdbKey, pdb); err == nil {
				_ = k8sClient.Delete(ctx, pdb)
			}
		})
	})

	Context("when PodDisruptionBudget is Managed (default)", func() {
		It("creates a PDB with maxUnavailable: 1 and the correct selector", func() {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			pdb := &policyv1.PodDisruptionBudget{}
			Expect(k8sClient.Get(ctx, pdbKey, pdb)).To(Succeed())

			minAvailable := intstr.FromInt32(1)
			Expect(pdb.Spec.MinAvailable).To(Equal(&minAvailable))
			Expect(pdb.Spec.Selector.MatchLabels).To(HaveKeyWithValue(LabelCluster, cluster.Name))
			Expect(pdb.Spec.Selector.MatchLabels).To(HaveKeyWithValue(LabelShardIndex, "0"))
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
	})

	Context("when PodDisruptionBudget is Disabled", func() {
		It("does not create a PDB", func() {
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}, cluster)).To(Succeed())
			cluster.Spec.PodDisruptionBudget = valkeyiov1alpha1.PDBPolicyDisabled
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
			cluster.Spec.PodDisruptionBudget = valkeyiov1alpha1.PDBPolicyDisabled
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

	Context("when old-style PDB exists (without shard index)", func() {
		It("deletes the old-style PDB and creates a new per-shard PDB", func() {
			// Create an old-style PDB manually with controller reference
			oldPdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourcePrefix + cluster.Name,
					Namespace: cluster.Namespace,
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: func() *intstr.IntOrString { v := intstr.FromInt32(1); return &v }(),
				},
			}
			Expect(controllerutil.SetControllerReference(cluster, oldPdb, reconciler.Scheme)).To(Succeed())
			Expect(k8sClient.Create(ctx, oldPdb)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Old-style PDB should be deleted
			err = k8sClient.Get(ctx, types.NamespacedName{Name: resourcePrefix + cluster.Name, Namespace: cluster.Namespace}, oldPdb)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsNotFound(err)).To(BeTrue())

			// New per-shard PDB should exist
			newPdb := &policyv1.PodDisruptionBudget{}
			Expect(k8sClient.Get(ctx, pdbKey, newPdb)).To(Succeed())
			Expect(newPdb.Spec.Selector.MatchLabels).To(HaveKeyWithValue(LabelShardIndex, "0"))
		})
	})
})
