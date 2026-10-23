/*
Copyright 2026 Valkey Contributors.

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

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// ensureNodeService creates or deletes this node's ClusterIP Service.
// The ValkeyNode owns the Service, so scale-in removes it with the node.
// A nil spec.Service deletes the Service this node owns.
// The 63 character limit applies only when the field is set.
func (r *ValkeyNodeReconciler) ensureNodeService(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) error {
	name := valkeyNodeResourceName(node)
	if node.Spec.Service == nil {
		if len(name) > validation.DNS1123LabelMaxLength {
			return nil
		}
		return r.deleteNodeService(ctx, node, name)
	}
	if len(name) > validation.DNS1123LabelMaxLength {
		return fmt.Errorf("per-node Service name %q is %d characters; the limit is %d", name, len(name), validation.DNS1123LabelMaxLength)
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: node.Namespace,
		},
	}
	notOwned := fmt.Errorf("service %s/%s exists and is not owned by ValkeyNode %s", svc.Namespace, svc.Name, node.Name)
	// The cache holds only Services with the operator managed-by label. A
	// user Service is not in it, so Create returns AlreadyExists for it.
	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if svc.UID != "" && !metav1.IsControlledBy(svc, node) {
			return notOwned
		}
		svc.Labels = valkeyNodeLabels(node)
		svc.Spec.Type = corev1.ServiceTypeClusterIP
		svc.Spec.Selector = valkeyNodeLabels(node)
		// Protocol and TargetPort are API-server defaults. Set them so a
		// second reconcile does not update the Service (#315).
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       appName,
			Port:       DefaultPort,
			Protocol:   corev1.ProtocolTCP,
			TargetPort: intstr.FromInt32(DefaultPort),
		}}
		return controllerutil.SetControllerReference(node, svc, r.Scheme)
	})
	if apierrors.IsAlreadyExists(err) {
		err = notOwned
	}
	if err != nil {
		r.Recorder.Eventf(node, svc, corev1.EventTypeWarning, "ServiceUpdateFailed", "UpdateService", "Failed to upsert per-node Service: %v", err)
		return err
	}
	if result == controllerutil.OperationResultCreated {
		r.Recorder.Eventf(node, svc, corev1.EventTypeNormal, "ServiceCreated", "CreateService", "Created per-node Service %s", svc.Name)
	}
	return nil
}

func (r *ValkeyNodeReconciler) deleteNodeService(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode, name string) error {
	svc := &corev1.Service{}
	err := r.Get(ctx, client.ObjectKey{Namespace: node.Namespace, Name: name}, svc)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	// A user Service may already use this name. Delete only the one we own.
	if !metav1.IsControlledBy(svc, node) {
		return nil
	}
	// Get reads the cache. Delete addresses the Service by name, so a
	// replacement created while that snapshot is stale would be removed.
	// A UID mismatch fails the request instead.
	if err := r.Delete(ctx, svc, client.Preconditions{UID: &svc.UID}); err != nil {
		if apierrors.IsConflict(err) {
			return nil
		}
		return client.IgnoreNotFound(err)
	}
	return nil
}
