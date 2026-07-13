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
	"strconv"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

const appInstanceLabel = "app.kubernetes.io/instance"

func pdbName(cluster *valkeyiov1alpha1.ValkeyCluster, shardIndex int) string {
	return fmt.Sprintf("%s%s-%d", resourcePrefix, cluster.Name, shardIndex)
}

func (r *ValkeyClusterReconciler) reconcilePodDisruptionBudget(
	ctx context.Context,
	cluster *valkeyiov1alpha1.ValkeyCluster,
) error {
	// Remove the legacy cluster-wide PDB created by older operator versions.
	legacyPDB := &policyv1.PodDisruptionBudget{}
	legacyPDBName := resourcePrefix + cluster.Name

	err := r.Get(
		ctx,
		client.ObjectKey{
			Name:      legacyPDBName,
			Namespace: cluster.Namespace,
		},
		legacyPDB,
	)
	switch {
	case err == nil:
		if metav1.IsControlledBy(legacyPDB, cluster) {
			if err := r.Delete(ctx, legacyPDB); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting legacy PodDisruptionBudget %q: %w", legacyPDB.Name, err)
			}

			r.Recorder.Eventf(
				cluster,
				legacyPDB,
				corev1.EventTypeNormal,
				"PodDisruptionBudgetDeleted",
				"DeletePodDisruptionBudget",
				"Deleted legacy cluster-wide PodDisruptionBudget",
			)
		}
	case apierrors.IsNotFound(err):
		// Nothing to migrate.
	default:
		return fmt.Errorf("getting legacy PodDisruptionBudget %q: %w", legacyPDBName, err)
	}

	// List all PDBs belonging to this ValkeyCluster instance.
	existingPDBs := &policyv1.PodDisruptionBudgetList{}
	if err := r.List(
		ctx,
		existingPDBs,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{
			appInstanceLabel: cluster.Name,
		},
	); err != nil {
		return fmt.Errorf("listing PodDisruptionBudgets: %w", err)
	}

	if cluster.Spec.PodDisruptionBudget == valkeyiov1alpha1.PDBPolicyDisabled {
		for i := range existingPDBs.Items {
			pdb := &existingPDBs.Items[i]

			if !metav1.IsControlledBy(pdb, cluster) {
				continue
			}

			if err := r.Delete(ctx, pdb); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting PodDisruptionBudget %q: %w", pdb.Name, err)
			}

			r.Recorder.Eventf(
				cluster,
				pdb,
				corev1.EventTypeNormal,
				"PodDisruptionBudgetDeleted",
				"DeletePodDisruptionBudget",
				"Deleted PodDisruptionBudget %q",
				pdb.Name,
			)
		}

		return nil
	}

	desiredShards := int(cluster.Spec.Shards)

	// Remove per-shard PDBs whose shard no longer exists after scale-in.
	for i := range existingPDBs.Items {
		pdb := &existingPDBs.Items[i]

		if !metav1.IsControlledBy(pdb, cluster) {
			continue
		}

		shardLabel, ok := pdb.Labels[LabelShardIndex]
		if !ok {
			continue
		}

		shardIndex, err := strconv.Atoi(shardLabel)
		if err != nil {
			return fmt.Errorf(
				"parsing shard index label %q on PodDisruptionBudget %q: %w",
				shardLabel,
				pdb.Name,
				err,
			)
		}

		if shardIndex >= 0 && shardIndex < desiredShards {
			continue
		}

		if err := r.Delete(ctx, pdb); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf(
				"deleting PodDisruptionBudget %q for stale shard %d: %w",
				pdb.Name,
				shardIndex,
				err,
			)
		}

		r.Recorder.Eventf(
			cluster,
			pdb,
			corev1.EventTypeNormal,
			"PodDisruptionBudgetDeleted",
			"DeletePodDisruptionBudget",
			"Deleted PodDisruptionBudget for stale shard %d",
			shardIndex,
		)
	}

	minAvailable := intstr.FromInt32(1)

	for shardIndex := range desiredShards {
		existing := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pdbName(cluster, shardIndex),
				Namespace: cluster.Namespace,
			},
		}

		result, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
			existing.Labels = labels(cluster)
			existing.Labels[LabelShardIndex] = strconv.Itoa(shardIndex)

			existing.Spec.MinAvailable = &minAvailable
			existing.Spec.MaxUnavailable = nil
			existing.Spec.Selector = &metav1.LabelSelector{
				MatchLabels: map[string]string{
					LabelCluster:    cluster.Name,
					LabelShardIndex: strconv.Itoa(shardIndex),
				},
			}

			return controllerutil.SetControllerReference(cluster, existing, r.Scheme)
		})
		if err != nil {
			return fmt.Errorf("upserting PodDisruptionBudget for shard %d: %w", shardIndex, err)
		}

		switch result {
		case controllerutil.OperationResultCreated:
			r.Recorder.Eventf(
				cluster,
				existing,
				corev1.EventTypeNormal,
				"PodDisruptionBudgetCreated",
				"CreatePodDisruptionBudget",
				"Created PodDisruptionBudget for shard %d",
				shardIndex,
			)
		case controllerutil.OperationResultUpdated:
			r.Recorder.Eventf(
				cluster,
				existing,
				corev1.EventTypeNormal,
				"PodDisruptionBudgetUpdated",
				"UpdatePodDisruptionBudget",
				"Updated PodDisruptionBudget for shard %d",
				shardIndex,
			)
		}
	}

	return nil
}
