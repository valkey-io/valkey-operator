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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

var _ = Describe("clientAuth.ca reconcile", func() {
	ctx := context.Background()
	reconcileOnce := func(name string) {
		r := &ValkeyClusterReconciler{
			Client:    k8sClient,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  events.NewFakeRecorder(100),
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
	}
	nodeTrustBundles := func(cluster string) []string {
		nodes := &valkeyiov1alpha1.ValkeyNodeList{}
		Expect(k8sClient.List(ctx, nodes, client.InNamespace("default"), client.MatchingLabels{LabelCluster: cluster})).To(Succeed())
		Expect(nodes.Items).NotTo(BeEmpty())
		refs := make([]string, 0, len(nodes.Items))
		for _, n := range nodes.Items {
			ref := ""
			if b := n.Spec.TLS.Certificates.TrustBundle; b != nil {
				ref = b.SecretName
			}
			refs = append(refs, ref)
		}
		return refs
	}
	newSecret := func(name string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Data:       map[string][]byte{tlsSecretKeyCA: selfSignedCAPEM(GinkgoT())},
		}
	}

	It("writes the trust Secret and points every node at it", func() {
		server, clientCA := newSecret("ca-rec-server"), newSecret("ca-rec-client")
		Expect(k8sClient.Create(ctx, server)).To(Succeed())
		Expect(k8sClient.Create(ctx, clientCA)).To(Succeed())
		DeferCleanup(k8sClient.Delete, ctx, server)
		DeferCleanup(k8sClient.Delete, ctx, clientCA)

		cluster := clientAuthCACluster("ca-rec", valkeyiov1alpha1.TLSAuthClientsRequired, 0)
		cluster.Spec.Networking.TLS.Certificates.Server.SecretName = server.Name
		cluster.Spec.Networking.TLS.ClientAuth.CA = []valkeyiov1alpha1.TrustSource{{SecretName: clientCA.Name}}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(k8sClient.Delete, ctx, cluster)

		reconcileOnce(cluster.Name)

		trust := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ca-rec-tls-trust"}, trust)).To(Succeed())
		Expect(trust.Data).To(HaveKey(tlsSecretKeyCA))
		Expect(nodeTrustBundles(cluster.Name)).To(HaveEach("ca-rec-tls-trust"))

		got := &valkeyiov1alpha1.ValkeyCluster{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, valkeyiov1alpha1.ConditionTLSConfigured)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	})

	It("keeps nodes on the server root while a source is missing", func() {
		server := newSecret("ca-miss-server")
		Expect(k8sClient.Create(ctx, server)).To(Succeed())
		DeferCleanup(k8sClient.Delete, ctx, server)

		cluster := clientAuthCACluster("ca-miss", valkeyiov1alpha1.TLSAuthClientsRequired, 0)
		cluster.Spec.Networking.TLS.Certificates.Server.SecretName = server.Name
		cluster.Spec.Networking.TLS.ClientAuth.CA = []valkeyiov1alpha1.TrustSource{{SecretName: "ca-miss-absent"}}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(k8sClient.Delete, ctx, cluster)

		reconcileOnce(cluster.Name)

		err := k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ca-miss-tls-trust"}, &corev1.Secret{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		Expect(nodeTrustBundles(cluster.Name)).To(HaveEach(""))

		got := &valkeyiov1alpha1.ValkeyCluster{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, valkeyiov1alpha1.ConditionTLSConfigured)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(valkeyiov1alpha1.ReasonTrustSourceNotFound))
	})
})
