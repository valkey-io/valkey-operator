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
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// nodeTLSFromValkey resolves the Valkey's TLS intent into the node's TLS view.
// ClientAuth stays nil because ValkeyTLSSpec is server TLS only.
// NodeTLSSpec.ClientAuthMode treats nil as optional client certificates.
func nodeTLSFromValkey(v *valkeyiov1alpha1.Valkey) *valkeyiov1alpha1.NodeTLSSpec {
	tlsSpec := v.GetTLS()
	if tlsSpec == nil {
		return nil
	}
	return &valkeyiov1alpha1.NodeTLSSpec{
		ServerName: tlsServerName(tlsSpec.ServerName, v.Name, v.Namespace, v.GetClusterDomain()),
		Certificates: valkeyiov1alpha1.NodeTLSCertificates{
			Server: valkeyiov1alpha1.NodeCertificateRef{
				SecretName: tlsSpec.Certificates.Server.SecretName,
			},
		},
	}
}

// buildValkeyServerConfig renders the valkey.conf written to the Valkey's server ConfigMap.
// The node's pod roll hash still renders with the cluster base config.
// That is harmless because the hash only has to change when the node spec changes.
func buildValkeyServerConfig(v *valkeyiov1alpha1.Valkey) string {
	return buildServerConfigFrom(getStandaloneBaseConfig(nodeTLSFromValkey(v), v.Spec.Image), v.Spec.Image, v.Spec.Config)
}

// buildValkeyNode constructs the ValkeyNode of a Valkey at shard 0 and the given node index.
// Index 0 is the initial primary, and the rest start as replicas.
// WorkloadRevision stays unset, so the node applies template changes immediately.
// PreferredEndpointType stays empty, so the node announces its IP.
func buildValkeyNode(v *valkeyiov1alpha1.Valkey, index int) *valkeyiov1alpha1.ValkeyNode {
	// Start with recommended k8s labels; instance is the Valkey name and component is "valkey-node".
	l := baseLabels(v.Name, "valkey-node")
	// Inherit user-defined labels from the parent at lower priority.
	for k, val := range v.Labels {
		if _, exists := l[k]; !exists {
			l[k] = val
		}
	}
	// Operator-specific labels always take precedence.
	l[LabelCluster] = v.Name
	l[LabelShardIndex] = "0"
	l[LabelNodeIndex] = strconv.Itoa(index)

	var scheduling valkeyiov1alpha1.ValkeySchedulingSpec
	if v.Spec.Scheduling != nil {
		scheduling = *v.Spec.Scheduling
	}

	// A ValkeyNode runs the exporter sidecar only on an explicit true.
	// Disabled stays nil to match what buildClusterValkeyNode writes.
	exporter := v.Spec.Exporter
	exporter.Enabled = nil
	if v.Spec.ExporterEnabled() {
		enabled := true
		exporter.Enabled = &enabled
	}

	return &valkeyiov1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      valkeyNodeName(v.Name, 0, index),
			Namespace: v.Namespace,
			Labels:    l,
		},
		Spec: valkeyiov1alpha1.ValkeyNodeSpec{
			Image:                         v.Spec.Image,
			ImagePullSecrets:              v.Spec.ImagePullSecrets,
			WorkloadType:                  v.Spec.WorkloadType,
			Persistence:                   v.Spec.Persistence,
			Resources:                     v.Spec.Resources,
			NodeSelector:                  scheduling.NodeSelector,
			Affinity:                      scheduling.Affinity,
			Tolerations:                   scheduling.Tolerations,
			PriorityClassName:             scheduling.PriorityClassName,
			TopologySpreadConstraints:     scheduling.TopologySpreadConstraints,
			Exporter:                      exporter,
			Containers:                    v.Spec.Containers,
			ServerConfigMapName:           GetServerConfigMapName(v.Name),
			UsersACLSecretName:            getInternalSecretName(v.Name),
			TLS:                           nodeTLSFromValkey(v),
			Config:                        v.Spec.Config,
			PodSecurityContext:            v.Spec.PodSecurityContext,
			TerminationGracePeriodSeconds: v.Spec.TerminationGracePeriodSeconds,
			ClusterDomain:                 v.GetClusterDomain(),
		},
	}
}

// listValkeyNodes returns the ValkeyNodes this Valkey controls, ordered by node index.
// It filters on the controller reference because a ValkeyCluster of the same name sets the same cluster label.
func listValkeyNodes(ctx context.Context, c client.Client, v *valkeyiov1alpha1.Valkey) ([]*valkeyiov1alpha1.ValkeyNode, error) {
	list := &valkeyiov1alpha1.ValkeyNodeList{}
	if err := c.List(ctx, list, client.InNamespace(v.Namespace), client.MatchingLabels{LabelCluster: v.Name}); err != nil {
		return nil, err
	}
	nodes := make([]*valkeyiov1alpha1.ValkeyNode, 0, len(list.Items))
	for i := range list.Items {
		if metav1.IsControlledBy(&list.Items[i], v) {
			nodes = append(nodes, &list.Items[i])
		}
	}
	slices.SortFunc(nodes, func(a, b *valkeyiov1alpha1.ValkeyNode) int {
		return valkeyNodeIndex(a) - valkeyNodeIndex(b)
	})
	return nodes, nil
}

// valkeyNodeIndex returns the node index label of a Valkey's node, or -1 when it is missing or malformed.
func valkeyNodeIndex(node *valkeyiov1alpha1.ValkeyNode) int {
	index, err := strconv.Atoi(node.Labels[LabelNodeIndex])
	if err != nil {
		return -1
	}
	return index
}

// createValkeyNode creates the ValkeyNode at index, owned by the Valkey.
func createValkeyNode(ctx context.Context, rc *resourceClient, v *valkeyiov1alpha1.Valkey, index int) (*valkeyiov1alpha1.ValkeyNode, error) {
	node := buildValkeyNode(v, index)
	if err := controllerutil.SetControllerReference(v, node, rc.Scheme); err != nil {
		return nil, err
	}
	if err := rc.Create(ctx, node); err != nil {
		rc.Recorder.Eventf(v, node, corev1.EventTypeWarning, "ValkeyNodeFailed", "CreateValkeyNode", "Failed to create ValkeyNode: %v", err)
		return nil, err
	}
	rc.Recorder.Eventf(v, node, corev1.EventTypeNormal, "ValkeyNodeCreated", "CreateValkeyNode", "Created ValkeyNode %s", node.Name)
	return node, nil
}

// valkeyNodeChanged reports whether the node's labels or spec differ from what the Valkey wants at its index.
func valkeyNodeChanged(v *valkeyiov1alpha1.Valkey, node *valkeyiov1alpha1.ValkeyNode) bool {
	desired := buildValkeyNode(v, valkeyNodeIndex(node))
	return !equality.Semantic.DeepEqual(node.Labels, desired.Labels) || !equality.Semantic.DeepEqual(node.Spec, desired.Spec)
}

// valkeyNodeWouldRoll reports whether applying the Valkey's desired spec to the node replaces its pod.
// Valkey nodes leave WorkloadRevision unset, so this compares the pod templates both specs render.
func valkeyNodeWouldRoll(v *valkeyiov1alpha1.Valkey, node *valkeyiov1alpha1.ValkeyNode) (bool, error) {
	desired := buildValkeyNode(v, valkeyNodeIndex(node))
	if effectiveWorkloadType(node.Spec.WorkloadType) != effectiveWorkloadType(desired.Spec.WorkloadType) {
		return true, nil
	}
	current, err := computeWorkloadRevision(node)
	if err != nil {
		return false, err
	}
	want, err := computeWorkloadRevision(desired)
	if err != nil {
		return false, err
	}
	return current != want, nil
}

// updateValkeyNode applies the Valkey's desired labels and spec to an existing node.
func updateValkeyNode(ctx context.Context, rc *resourceClient, v *valkeyiov1alpha1.Valkey, node *valkeyiov1alpha1.ValkeyNode) error {
	desired := buildValkeyNode(v, valkeyNodeIndex(node))
	node.Labels = desired.Labels
	node.Spec = desired.Spec
	if err := controllerutil.SetControllerReference(v, node, rc.Scheme); err != nil {
		return err
	}
	if err := rc.Update(ctx, node); err != nil {
		rc.Recorder.Eventf(v, node, corev1.EventTypeWarning, "ValkeyNodeFailed", "UpdateValkeyNode", "Failed to update ValkeyNode: %v", err)
		return err
	}
	rc.Recorder.Eventf(v, node, corev1.EventTypeNormal, "ValkeyNodeUpdated", "UpdateValkeyNode", "Updated ValkeyNode %s", node.Name)
	return nil
}
