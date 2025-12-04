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
	"embed"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

const (
	DefaultPort           = 6379
	DefaultClusterBusPort = 16379
	DefaultImage          = "valkey/valkey:9.0.0"
)

// ValkeyClusterReconciler reconciles a ValkeyCluster object
type ValkeyClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//go:embed scripts/*
var scripts embed.FS

// +kubebuilder:rbac:groups=valkey.io,resources=valkeyclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=valkey.io,resources=valkeyclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=valkey.io,resources=valkeyclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="apps",resources=deployments,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/reconcile
func (r *ValkeyClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.V(1).Info("Reconcile...")

	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := r.upsertService(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.upsertConfigMap(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.upsertDeployments(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}

	log.V(1).Info("Reconcile done")
	return ctrl.Result{}, nil
}

// Create or update a headless service (client connects to pods directly)
func (r *ValkeyClusterReconciler) upsertService(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
			Labels:    labels(cluster),
		},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "None",
			Selector:  labels(cluster),
			Ports: []corev1.ServicePort{
				{
					Name: "valkey",
					Port: DefaultPort,
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(cluster, svc, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, svc); err != nil {
		if errors.IsAlreadyExists(err) {
			if err := r.Update(ctx, svc); err != nil {
				return err
			}
		} else {
			return err
		}
	}
	return nil
}

// Create or update a basic valkey.conf
func (r *ValkeyClusterReconciler) upsertConfigMap(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) error {
	readiness, err := scripts.ReadFile("scripts/readiness-check.sh")
	if err != nil {
		return err
	}
	liveness, err := scripts.ReadFile("scripts/liveness-check.sh")
	if err != nil {
		return err
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
			Labels:    labels(cluster),
		},
		Data: map[string]string{
			"readiness-check.sh": string(readiness),
			"liveness-check.sh":  string(liveness),
			"valkey.conf": `
cluster-enabled yes
protected-mode no`,
		},
	}
	if err := controllerutil.SetControllerReference(cluster, cm, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, cm); err != nil {
		if errors.IsAlreadyExists(err) {
			if err := r.Update(ctx, cm); err != nil {
				return err
			}
		} else {
			return err
		}
	}
	return nil
}

// Create Valkey instances, one Deployment and Pod each
func (r *ValkeyClusterReconciler) upsertDeployments(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) error {
	existing := &appsv1.DeploymentList{}
	if err := r.List(ctx, existing, client.InNamespace(cluster.Namespace)); err != nil {
		return err
	}

	replicas := int(cluster.Spec.Shards * (1 + cluster.Spec.Replicas))

	// Create missing deployments
	for i := len(existing.Items); i < replicas; i++ {
		deployment := createClusterDeployment(cluster)
		if err := controllerutil.SetControllerReference(cluster, deployment, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, deployment); err != nil {
			return err
		}
	}

	// TODO: update existing

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ValkeyClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&valkeyiov1alpha1.ValkeyCluster{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&appsv1.Deployment{}).
		Named("valkeycluster").
		Complete(r)
}
