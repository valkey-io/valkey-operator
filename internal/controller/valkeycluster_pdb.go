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

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

func pdbName(cluster *valkeyiov1alpha1.ValkeyCluster) string {
	return resourcePrefix + cluster.Name
}

func (r *ValkeyClusterReconciler) reconcilePodDisruptionBudget(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) error {
	mode := valkeyiov1alpha1.PDBModeCluster
	if cfg := cluster.Spec.PodDisruptionBudget; cfg != nil && cfg.Mode != "" {
		mode = cfg.Mode
	}
	rc := &resourceClient{Client: r.Client, APIReader: r.APIReader, Scheme: r.Scheme, Recorder: r.Recorder}
	// Cluster mode (and any unknown future value) creates the cluster-wide PDB.
	return reconcilePDB(ctx, rc, cluster, cluster.Name, cluster.Namespace, labels(cluster), mode != valkeyiov1alpha1.PDBModeDisabled)
}

// reconcilePDB keeps a maxUnavailable 1 PodDisruptionBudget over the pods labelled LabelCluster=name.
// When enabled is false it deletes any existing one instead.
// Shared by the ValkeyCluster and Valkey controllers.
func reconcilePDB(ctx context.Context, rc *resourceClient, owner client.Object, name, namespace string, labels map[string]string, enabled bool) error {
	pdbResourceName := resourcePrefix + name

	if !enabled {
		pdb := &policyv1.PodDisruptionBudget{}
		err := rc.Get(ctx, client.ObjectKey{Name: pdbResourceName, Namespace: namespace}, pdb)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("getting PDB for deletion: %w", err)
		}
		if err := rc.Delete(ctx, pdb); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting PDB: %w", err)
		}
		rc.Recorder.Eventf(owner, pdb, corev1.EventTypeNormal, "PodDisruptionBudgetDeleted", "DeletePodDisruptionBudget", "Deleted PodDisruptionBudget")
		return nil
	}

	maxUnavailable := intstr.FromInt32(1)
	existing := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pdbResourceName,
			Namespace: namespace,
		},
	}
	result, err := controllerutil.CreateOrUpdate(ctx, rc.Client, existing, func() error {
		existing.Labels = labels
		existing.Spec.MaxUnavailable = &maxUnavailable
		existing.Spec.MinAvailable = nil
		existing.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: map[string]string{
				LabelCluster: name,
			},
		}
		return controllerutil.SetControllerReference(owner, existing, rc.Scheme)
	})
	if err != nil {
		return fmt.Errorf("upserting PDB: %w", err)
	}
	switch result {
	case controllerutil.OperationResultCreated:
		rc.Recorder.Eventf(owner, existing, corev1.EventTypeNormal, "PodDisruptionBudgetCreated", "CreatePodDisruptionBudget", "Created PodDisruptionBudget")
	case controllerutil.OperationResultUpdated:
		rc.Recorder.Eventf(owner, existing, corev1.EventTypeNormal, "PodDisruptionBudgetUpdated", "UpdatePodDisruptionBudget", "Updated PodDisruptionBudget")
	}
	return nil
}
