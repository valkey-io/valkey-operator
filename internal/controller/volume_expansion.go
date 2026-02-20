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
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

// handleVolumeExpansion checks if PVCs need to be expanded and performs the expansion if needed
func (r *ValkeyClusterReconciler) handleVolumeExpansion(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) error {
	log := logf.FromContext(ctx)

	// Only proceed if storage is enabled
	if !cluster.Spec.Storage.Enabled {
		return nil
	}

	// Get the desired storage size from the cluster spec
	desiredSize := "1Gi"
	if cluster.Spec.Storage.Size != "" {
		desiredSize = cluster.Spec.Storage.Size
	}

	desiredQuantity, err := resource.ParseQuantity(desiredSize)
	if err != nil {
		log.Error(err, "failed to parse desired storage size", "size", desiredSize)
		return fmt.Errorf("invalid storage size format: %w", err)
	}

	// Get all PVCs for this cluster
	pvcList := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, pvcList, client.InNamespace(cluster.Namespace), client.MatchingLabels(labels(cluster))); err != nil {
		log.Error(err, "failed to list PVCs")
		return err
	}

	if len(pvcList.Items) == 0 {
		// No PVCs yet, nothing to expand
		return nil
	}

	// Check if any PVC needs expansion
	needsExpansion := false
	var pvcToExpand []corev1.PersistentVolumeClaim

	for _, pvc := range pvcList.Items {
		currentSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]

		// Compare sizes - only expand if desired is greater than current
		if desiredQuantity.Cmp(currentSize) > 0 {
			needsExpansion = true
			pvcToExpand = append(pvcToExpand, pvc)
			log.Info("PVC needs expansion", "pvc", pvc.Name, "currentSize", (&currentSize).String(), "desiredSize", desiredSize)
		} else if desiredQuantity.Cmp(currentSize) < 0 {
			// Size reduction is not supported
			log.Info("Storage size reduction is not supported", "pvc", pvc.Name, "currentSize", (&currentSize).String(), "requestedSize", desiredSize)
			r.Recorder.Eventf(cluster, &pvc, corev1.EventTypeWarning, "VolumeExpansionNotSupported", "VolumeExpansion", 
				"Cannot reduce storage size from %s to %s for PVC %s - shrinking is not supported", 
				(&currentSize).String(), desiredSize, pvc.Name)
		}
	}

	if !needsExpansion {
		// All PVCs are already at the desired size
		return nil
	}

	// Verify that the storage class supports volume expansion
	if len(pvcToExpand) > 0 {
		storageClassName := pvcToExpand[0].Spec.StorageClassName
		if storageClassName != nil && *storageClassName != "" {
			if err := r.verifyStorageClassSupportsExpansion(ctx, *storageClassName); err != nil {
				log.Error(err, "storage class does not support volume expansion", "storageClass", *storageClassName)
				r.Recorder.Eventf(cluster, &pvcToExpand[0], corev1.EventTypeWarning, "VolumeExpansionNotSupported", "VolumeExpansion", 
					"Storage class %s does not support volume expansion: %v", *storageClassName, err)
				return err
			}
		}
	}

	// Perform expansion on all PVCs that need it
	expandedCount := 0
	for i := range pvcToExpand {
		pvc := &pvcToExpand[i]
		currentSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		
		log.Info("Expanding PVC", "pvc", pvc.Name, "newSize", desiredSize)
		r.Recorder.Eventf(cluster, pvc, corev1.EventTypeNormal, "VolumeExpanding", "VolumeExpansion", 
			"Expanding PVC %s from %s to %s", 
			pvc.Name, currentSize.String(), desiredSize)

		// Update the PVC with the new size
		pvc.Spec.Resources.Requests[corev1.ResourceStorage] = desiredQuantity
		if err := r.Update(ctx, pvc); err != nil {
			log.Error(err, "failed to update PVC", "pvc", pvc.Name)
			r.Recorder.Eventf(cluster, pvc, corev1.EventTypeWarning, "VolumeExpansionFailed", "VolumeExpansion", 
				"Failed to expand PVC %s: %v", pvc.Name, err)
			return err
		}

		expandedCount++
		r.Recorder.Eventf(cluster, pvc, corev1.EventTypeNormal, "VolumeExpanded", "VolumeExpansion", 
			"Successfully initiated expansion of PVC %s to %s", pvc.Name, desiredSize)
	}

	if expandedCount > 0 {
		log.Info("Volume expansion initiated", "pvcCount", expandedCount, "newSize", desiredSize)
		setCondition(cluster, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonVolumeExpanding, 
			fmt.Sprintf("Expanding %d volumes to %s", expandedCount, desiredSize), metav1.ConditionTrue)
	}

	return nil
}

// verifyStorageClassSupportsExpansion checks if the storage class supports volume expansion
func (r *ValkeyClusterReconciler) verifyStorageClassSupportsExpansion(ctx context.Context, storageClassName string) error {
	log := logf.FromContext(ctx)

	storageClass := &storagev1.StorageClass{}
	if err := r.Get(ctx, client.ObjectKey{Name: storageClassName}, storageClass); err != nil {
		return fmt.Errorf("failed to get storage class: %w", err)
	}

	if storageClass.AllowVolumeExpansion == nil || !*storageClass.AllowVolumeExpansion {
		log.Info("Storage class does not support volume expansion", "storageClass", storageClassName)
		return fmt.Errorf("storage class %s does not have allowVolumeExpansion enabled", storageClassName)
	}

	log.V(1).Info("Storage class supports volume expansion", "storageClass", storageClassName)
	return nil
}

// checkVolumeExpansionStatus monitors the status of ongoing volume expansions
func (r *ValkeyClusterReconciler) checkVolumeExpansionStatus(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) (bool, error) {
	log := logf.FromContext(ctx)

	// Only proceed if storage is enabled
	if !cluster.Spec.Storage.Enabled {
		return true, nil
	}

	// Get all PVCs for this cluster
	pvcList := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, pvcList, client.InNamespace(cluster.Namespace), client.MatchingLabels(labels(cluster))); err != nil {
		log.Error(err, "failed to list PVCs")
		return false, err
	}

	allCompleted := true
	expandingCount := 0

	for _, pvc := range pvcList.Items {
		// Check if PVC is being resized
		for _, condition := range pvc.Status.Conditions {
			if condition.Type == corev1.PersistentVolumeClaimResizing {
				if condition.Status == corev1.ConditionTrue {
					allCompleted = false
					expandingCount++
					log.V(1).Info("PVC expansion in progress", "pvc", pvc.Name)
				}
			}
			if condition.Type == corev1.PersistentVolumeClaimFileSystemResizePending {
				if condition.Status == corev1.ConditionTrue {
					allCompleted = false
					expandingCount++
					log.V(1).Info("PVC filesystem resize pending", "pvc", pvc.Name)
				}
			}
		}

		// Compare spec vs status to detect pending expansions
		specSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		statusSize := pvc.Status.Capacity[corev1.ResourceStorage]
		if specSize.Cmp(statusSize) > 0 {
			allCompleted = false
			expandingCount++
		}
	}

	if expandingCount > 0 {
		log.Info("Volume expansions still in progress", "count", expandingCount)
	}

	return allCompleted, nil
}
