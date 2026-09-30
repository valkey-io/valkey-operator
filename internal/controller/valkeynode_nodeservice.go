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

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func nodeServiceName(clusterName string, shardIndex, nodeIndex int) string {
	return resourcePrefix + valkeyNodeName(clusterName, shardIndex, nodeIndex)
}

// validateNodeServiceName rejects a name Kubernetes will not accept.
// A Service name is a DNS-1123 label, at most 63 characters.
func validateNodeServiceName(clusterName string, shardIndex, nodeIndex int) error {
	name := nodeServiceName(clusterName, shardIndex, nodeIndex)
	if len(name) <= validation.DNS1123LabelMaxLength {
		return nil
	}
	return fmt.Errorf("per-node Service name %q is %d characters; the limit is %d. Shorten cluster name %q", name, len(name), validation.DNS1123LabelMaxLength, clusterName)
}

// validateClusterNodeServiceNames checks every node name before the cluster
// controller copies the opt-in onto ValkeyNodes.
func validateClusterNodeServiceNames(cluster *valkeyiov1alpha1.ValkeyCluster) error {
	if !cluster.NodeServiceEnabled() {
		return nil
	}
	nodesPerShard := 1 + int(cluster.Spec.Replicas)
	for shardIndex := range int(cluster.Spec.Shards) {
		for nodeIndex := range nodesPerShard {
			if err := validateNodeServiceName(cluster.Name, shardIndex, nodeIndex); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureNodeService creates or deletes this node's ClusterIP Service.
// The ValkeyNode owns the Service, so scale-in removes it with the node.
// A nil spec.NodeService deletes the Service.
func (r *ValkeyNodeReconciler) ensureNodeService(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) error {
	name := valkeyNodeResourceName(node)
	if len(name) > validation.DNS1123LabelMaxLength {
		return fmt.Errorf("per-node Service name %q is %d characters; the limit is %d", name, len(name), validation.DNS1123LabelMaxLength)
	}
	if node.Spec.NodeService == nil {
		return r.deleteNodeService(ctx, node, name)
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: node.Namespace,
		},
	}
	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if svc.UID != "" && !metav1.IsControlledBy(svc, node) {
			return fmt.Errorf("Service %s/%s exists and is not owned by ValkeyNode %s", svc.Namespace, svc.Name, node.Name)
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
	if err := r.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
