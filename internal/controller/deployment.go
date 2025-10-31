package controller

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	valkeyv1 "valkey.io/valkey-operator/api/v1alpha1"
)

func (r *ValkeyClusterReconciler) upsertDeployment(ctx context.Context, cluster *valkeyv1.ValkeyCluster, shard int, salt string) error {
	logger := log.FromContext(ctx)

	existingDeployment := &appsv1.Deployment{}
	err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, existingDeployment)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Deployment does not exist, create it
			newDeployment := r.deploymentForValkeyCluster(cluster, shard, salt)
			logger.Info("Creating a new Deployment", "Deployment.Namespace", newDeployment.Namespace, "Deployment.Name", newDeployment.Name)
			if err := r.Create(ctx, newDeployment); err != nil {
				logger.Error(err, "Failed to create new Deployment", "Deployment.Namespace", newDeployment.Namespace, "Deployment.Name", newDeployment.Name)
				return err
			}
		}
		logger.Error(err, "Failed to get Deployment")
		return err
	}
	// Deployment exists, update it if necessary
	logger.Info("Deployment already exists, skipping creation", "Deployment.Namespace", existingDeployment.Namespace, "Deployment.Name", existingDeployment.Name)

	return nil
}

func (r *ValkeyClusterReconciler) deploymentForValkeyCluster(cluster *valkeyv1.ValkeyCluster, shard int, salt string) *appsv1.Deployment {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
			Labels:    labels(cluster, shard, salt),
		},
	}
	return deployment
}
