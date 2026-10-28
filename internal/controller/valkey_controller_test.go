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
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

var _ = Describe("Valkey controller", func() {
	var (
		ctx        context.Context
		reconciler *ValkeyReconciler
		v          *valkeyiov1alpha1.Valkey
		req        reconcile.Request
	)

	BeforeEach(func() {
		ctx = context.Background()
		reconciler = &ValkeyReconciler{
			Client:    k8sClient,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  events.NewFakeRecorder(100),
		}
		v = &valkeyiov1alpha1.Valkey{
			ObjectMeta: metav1.ObjectMeta{Name: "standalone", Namespace: "default"},
		}
		Expect(k8sClient.Create(ctx, v)).To(Succeed())
		req = reconcile.Request{NamespacedName: types.NamespacedName{Name: v.Name, Namespace: v.Namespace}}

		// envtest runs no garbage collector, so delete the children explicitly.
		DeferCleanup(func() {
			for _, obj := range []client.Object{
				&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: headlessServiceName(v.Name), Namespace: v.Namespace}},
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: GetServerConfigMapName(v.Name), Namespace: v.Namespace}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: getInternalSecretName(v.Name), Namespace: v.Namespace}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: getSystemPasswordSecretName(v.Name), Namespace: v.Namespace}},
				&valkeyiov1alpha1.ValkeyNode{ObjectMeta: metav1.ObjectMeta{Name: valkeyNodeName(v.Name, 0, 0), Namespace: v.Namespace}},
				v,
			} {
				_ = k8sClient.Delete(ctx, obj)
			}
		})
	})

	getStatus := func() valkeyiov1alpha1.ValkeyStatus {
		current := &valkeyiov1alpha1.Valkey{}
		Expect(k8sClient.Get(ctx, req.NamespacedName, current)).To(Succeed())
		return current.Status
	}

	setNodeStatus := func(mutate func(*valkeyiov1alpha1.ValkeyNodeStatus)) {
		node := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: valkeyNodeName(v.Name, 0, 0), Namespace: v.Namespace}, node)).To(Succeed())
		mutate(&node.Status)
		Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
	}

	It("creates the standalone children owned by the Valkey", func() {
		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		ns := v.Namespace
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: headlessServiceName(v.Name), Namespace: ns}, &corev1.Service{})).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: getInternalSecretName(v.Name), Namespace: ns}, &corev1.Secret{})).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: getSystemPasswordSecretName(v.Name), Namespace: ns}, &corev1.Secret{})).To(Succeed())

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: GetServerConfigMapName(v.Name), Namespace: ns}, cm)).To(Succeed())
		Expect(cm.Data[configFileKey]).NotTo(ContainSubstring("cluster-enabled"))

		node := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: valkeyNodeName(v.Name, 0, 0), Namespace: ns}, node)).To(Succeed())
		Expect(metav1.IsControlledBy(node, v)).To(BeTrue())

		// replicas is 0, so no budget is created even though the mode defaults to Managed.
		err = k8sClient.Get(ctx, types.NamespacedName{Name: resourcePrefix + v.Name, Namespace: ns}, &policyv1.PodDisruptionBudget{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())

		status := getStatus()
		Expect(status.State).To(Equal(valkeyiov1alpha1.ValkeyStateReconciling))
		Expect(status.Primary).To(Equal(node.Name))
		Expect(status.Replicas).To(BeZero())
		Expect(status.ReadyReplicas).To(BeZero())
	})

	It("reports Ready once the node is ready", func() {
		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		setNodeStatus(func(s *valkeyiov1alpha1.ValkeyNodeStatus) { s.Ready = true })

		_, err = reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		status := getStatus()
		Expect(status.State).To(Equal(valkeyiov1alpha1.ValkeyStateReady))
		Expect(status.Reason).To(Equal(valkeyiov1alpha1.ReasonInstanceHealthy))
	})

	It("reports Degraded when the node cannot apply the ACL, and clears it after", func() {
		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		setNodeStatus(func(s *valkeyiov1alpha1.ValkeyNodeStatus) {
			s.Ready = true
			meta.SetStatusCondition(&s.Conditions, metav1.Condition{
				Type:   valkeyiov1alpha1.ValkeyNodeConditionACLApplied,
				Status: metav1.ConditionFalse,
				Reason: valkeyiov1alpha1.ValkeyNodeReasonApplyFailed,
			})
		})

		_, err = reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(getStatus().State).To(Equal(valkeyiov1alpha1.ValkeyStateDegraded))

		setNodeStatus(func(s *valkeyiov1alpha1.ValkeyNodeStatus) {
			meta.RemoveStatusCondition(&s.Conditions, valkeyiov1alpha1.ValkeyNodeConditionACLApplied)
		})
		_, err = reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(getStatus().State).To(Equal(valkeyiov1alpha1.ValkeyStateReady))
	})
})

func TestBuildValkeyNode(t *testing.T) {
	disabled := false
	tests := []struct {
		name         string
		spec         valkeyiov1alpha1.ValkeySpec
		wantExporter bool
		wantTLS      bool
	}{
		{name: "defaults", spec: valkeyiov1alpha1.ValkeySpec{}, wantExporter: true},
		{name: "exporter disabled", spec: valkeyiov1alpha1.ValkeySpec{Exporter: valkeyiov1alpha1.ExporterSpec{Enabled: &disabled}}},
		{
			name: "tls",
			spec: valkeyiov1alpha1.ValkeySpec{Networking: &valkeyiov1alpha1.ValkeyNetworkingSpec{
				TLS: &valkeyiov1alpha1.ValkeyTLSSpec{Certificates: valkeyiov1alpha1.TLSCertificates{
					Server: valkeyiov1alpha1.CertificateSource{SecretName: "server-tls"},
				}},
			}},
			wantExporter: true,
			wantTLS:      true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := &valkeyiov1alpha1.Valkey{
				ObjectMeta: metav1.ObjectMeta{Name: "vk", Namespace: "ns", Labels: map[string]string{LabelCluster: "user-value"}},
				Spec:       tt.spec,
			}
			node := buildValkeyNode(v)

			if node.Name != valkeyNodeName("vk", 0, 0) {
				t.Errorf("name = %q", node.Name)
			}
			if node.Labels[LabelCluster] != "vk" {
				t.Errorf("operator label must win over user label, got %q", node.Labels[LabelCluster])
			}
			if node.Spec.WorkloadRevision != "" || node.Spec.PreferredEndpointType != "" {
				t.Errorf("standalone node must leave WorkloadRevision and PreferredEndpointType empty")
			}
			if node.Spec.ClusterDomain != valkeyiov1alpha1.DefaultClusterDomain {
				t.Errorf("ClusterDomain = %q", node.Spec.ClusterDomain)
			}
			gotExporter := node.Spec.Exporter.Enabled != nil && *node.Spec.Exporter.Enabled
			if gotExporter != tt.wantExporter {
				t.Errorf("exporter enabled = %v, want %v", gotExporter, tt.wantExporter)
			}
			if !tt.wantExporter && node.Spec.Exporter.Enabled != nil {
				t.Errorf("disabled exporter must stay nil, not explicit false")
			}
			if (node.Spec.TLS != nil) != tt.wantTLS {
				t.Fatalf("TLS set = %v, want %v", node.Spec.TLS != nil, tt.wantTLS)
			}
			if tt.wantTLS {
				if node.Spec.TLS.ServerName != "valkey-vk.ns.svc.cluster.local" {
					t.Errorf("ServerName = %q", node.Spec.TLS.ServerName)
				}
				if node.Spec.TLS.ClientAuth != nil {
					t.Errorf("ClientAuth must be nil for server-only TLS")
				}
			}
		})
	}
}

func TestBuildValkeyServerConfig(t *testing.T) {
	v := &valkeyiov1alpha1.Valkey{Spec: valkeyiov1alpha1.ValkeySpec{Config: map[string]string{"maxmemory": "1gb"}}}
	conf := buildValkeyServerConfig(v)
	for line := range strings.SplitSeq(conf, "\n") {
		if strings.HasPrefix(line, "cluster-enabled") || strings.HasPrefix(line, "shutdown-on-sigterm") {
			t.Errorf("standalone config must not contain %q", line)
		}
	}
	if !strings.Contains(conf, "aclfile") || !strings.Contains(conf, "maxmemory 1gb") {
		t.Errorf("config is missing the ACL file or user config:\n%s", conf)
	}

	// The node's pod roll hash must change when a non-live key changes.
	before := nodeServerConfigRollHash(buildValkeyNode(v))
	v.Spec.Config["appendonly"] = "yes"
	if nodeServerConfigRollHash(buildValkeyNode(v)) == before {
		t.Errorf("roll hash did not change after a non-live config edit")
	}
}
