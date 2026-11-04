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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// buildValkeyNode constructs the single ValkeyNode of a standalone Valkey at shard 0, node 0.
// WorkloadRevision stays unset, so the node applies template changes immediately.
// PreferredEndpointType stays empty, so the node announces its IP.
func buildValkeyNode(v *valkeyiov1alpha1.Valkey) *valkeyiov1alpha1.ValkeyNode {
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
	l[LabelNodeIndex] = "0"

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
			Name:      valkeyNodeName(v.Name, 0, 0),
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

// reconcileStandaloneNode creates or updates the single ValkeyNode of a Valkey and returns it.
func reconcileStandaloneNode(ctx context.Context, rc *resourceClient, v *valkeyiov1alpha1.Valkey) (*valkeyiov1alpha1.ValkeyNode, error) {
	desired := buildValkeyNode(v)
	node := &valkeyiov1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      desired.Name,
			Namespace: desired.Namespace,
		},
	}
	result, err := controllerutil.CreateOrUpdate(ctx, rc.Client, node, func() error {
		node.Labels = desired.Labels
		node.Spec = desired.Spec
		return controllerutil.SetControllerReference(v, node, rc.Scheme)
	})
	if err != nil {
		rc.Recorder.Eventf(v, node, corev1.EventTypeWarning, "ValkeyNodeFailed", "ReconcileValkeyNode", "Failed to reconcile ValkeyNode: %v", err)
		return nil, err
	}
	switch result {
	case controllerutil.OperationResultCreated:
		rc.Recorder.Eventf(v, node, corev1.EventTypeNormal, "ValkeyNodeCreated", "CreateValkeyNode", "Created ValkeyNode")
	case controllerutil.OperationResultUpdated:
		rc.Recorder.Eventf(v, node, corev1.EventTypeNormal, "ValkeyNodeUpdated", "UpdateValkeyNode", "Updated ValkeyNode %s", node.Name)
	}
	return node, nil
}
