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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

func pdbName(cluster *valkeyiov1alpha1.ValkeyCluster) string {
	return resourcePrefix + cluster.Name
}

func (r *ValkeyClusterReconciler) reconcilePodDisruptionBudget(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) error {
	if cluster.Spec.PodDisruptionBudget == valkeyiov1alpha1.PDBPolicyDisabled {
		pdb := &policyv1.PodDisruptionBudget{}
		err := r.Get(ctx, client.ObjectKey{Name: pdbName(cluster), Namespace: cluster.Namespace}, pdb)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("getting PDB for deletion: %w", err)
		}
		if err := r.Delete(ctx, pdb); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting PDB: %w", err)
		}
		r.Recorder.Eventf(cluster, pdb, corev1.EventTypeNormal, "PodDisruptionBudgetDeleted", "DeletePodDisruptionBudget", "Deleted PodDisruptionBudget")
		return nil
	}

	maxUnavailable := intstr.FromInt32(1)
	existing := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pdbName(cluster),
			Namespace: cluster.Namespace,
		},
	}
	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		existing.Labels = labels(cluster)
		existing.Spec.MaxUnavailable = &maxUnavailable
		existing.Spec.MinAvailable = nil
		existing.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: map[string]string{
				LabelCluster: cluster.Name,
			},
		}
		return controllerutil.SetControllerReference(cluster, existing, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("upserting PDB: %w", err)
	}
	switch result {
	case controllerutil.OperationResultCreated:
		r.Recorder.Eventf(cluster, existing, corev1.EventTypeNormal, "PodDisruptionBudgetCreated", "CreatePodDisruptionBudget", "Created PodDisruptionBudget")
	case controllerutil.OperationResultUpdated:
		r.Recorder.Eventf(cluster, existing, corev1.EventTypeNormal, "PodDisruptionBudgetUpdated", "UpdatePodDisruptionBudget", "Updated PodDisruptionBudget")
	}
	return nil
}
