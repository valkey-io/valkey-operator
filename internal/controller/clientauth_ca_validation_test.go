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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

func clientAuthCACluster(name string, mode valkeyiov1alpha1.TLSAuthClients, n int) *valkeyiov1alpha1.ValkeyCluster {
	ca := make([]valkeyiov1alpha1.TrustSource, n)
	for i := range ca {
		ca[i] = valkeyiov1alpha1.TrustSource{SecretName: fmt.Sprintf("client-ca-%d", i)}
	}
	return &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{
			Shards: 1,
			Networking: &valkeyiov1alpha1.NetworkingSpec{
				TLS: &valkeyiov1alpha1.TLSSpec{
					Certificates: valkeyiov1alpha1.TLSCertificates{
						Server: valkeyiov1alpha1.CertificateSource{SecretName: "tls"},
					},
					ClientAuth: &valkeyiov1alpha1.TLSClientAuthSpec{Mode: mode, CA: ca},
				},
			},
		},
	}
}

var _ = Describe("clientAuth.ca validation", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("accepts a CA list with mode Required", func() {
		c := clientAuthCACluster("ca-ok-required", valkeyiov1alpha1.TLSAuthClientsRequired, 2)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Expect(k8sClient.Delete(ctx, c)).To(Succeed())
	})

	It("accepts a CA list when mode is defaulted", func() {
		c := clientAuthCACluster("ca-ok-default", "", 1)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Expect(c.Spec.Networking.TLS.ClientAuth.Mode).To(Equal(valkeyiov1alpha1.TLSAuthClientsOptional))
		Expect(k8sClient.Delete(ctx, c)).To(Succeed())
	})

	It("rejects a CA list with mode Disabled", func() {
		err := k8sClient.Create(ctx, clientAuthCACluster("ca-bad-disabled", valkeyiov1alpha1.TLSAuthClientsDisabled, 1))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("ca has no effect when mode=Disabled"))
	})

	It("accepts mode Disabled with an empty CA list", func() {
		c := clientAuthCACluster("ca-ok-disabled-empty", valkeyiov1alpha1.TLSAuthClientsDisabled, 0)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Expect(k8sClient.Delete(ctx, c)).To(Succeed())
	})

	It("rejects more than 16 entries", func() {
		err := k8sClient.Create(ctx, clientAuthCACluster("ca-bad-too-many", valkeyiov1alpha1.TLSAuthClientsRequired, 17))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("must have at most 16 items"))
	})

	It("accepts a ConfigMap source with a key", func() {
		c := clientAuthCACluster("ca-ok-configmap", valkeyiov1alpha1.TLSAuthClientsRequired, 0)
		c.Spec.Networking.TLS.ClientAuth.CA = []valkeyiov1alpha1.TrustSource{{ConfigMapName: "spire-bundle", Key: "bundle.spiffe"}}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Expect(k8sClient.Delete(ctx, c)).To(Succeed())
	})

	It("defaults the key to ca.crt", func() {
		c := clientAuthCACluster("ca-ok-default-key", valkeyiov1alpha1.TLSAuthClientsRequired, 1)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Expect(c.Spec.Networking.TLS.ClientAuth.CA[0].Key).To(Equal("ca.crt"))
		Expect(k8sClient.Delete(ctx, c)).To(Succeed())
	})

	It("rejects an entry naming both a Secret and a ConfigMap", func() {
		c := clientAuthCACluster("ca-bad-both", valkeyiov1alpha1.TLSAuthClientsRequired, 1)
		c.Spec.Networking.TLS.ClientAuth.CA[0].ConfigMapName = "spire-bundle"
		err := k8sClient.Create(ctx, c)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("exactly one of secretName and configMapName must be set"))
	})

	It("rejects an entry naming neither", func() {
		c := clientAuthCACluster("ca-bad-neither", valkeyiov1alpha1.TLSAuthClientsRequired, 1)
		c.Spec.Networking.TLS.ClientAuth.CA[0] = valkeyiov1alpha1.TrustSource{Key: "ca.crt"}
		err := k8sClient.Create(ctx, c)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("exactly one of secretName and configMapName must be set"))
	})

	It("rejects a key that is not a valid data key", func() {
		c := clientAuthCACluster("ca-bad-key", valkeyiov1alpha1.TLSAuthClientsRequired, 1)
		c.Spec.Networking.TLS.ClientAuth.CA[0].Key = "bad/key"
		err := k8sClient.Create(ctx, c)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("key"))
	})

	It("rejects an entry with an empty secretName", func() {
		c := clientAuthCACluster("ca-bad-empty-name", valkeyiov1alpha1.TLSAuthClientsRequired, 1)
		c.Spec.Networking.TLS.ClientAuth.CA[0].SecretName = ""
		err := k8sClient.Create(ctx, c)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("secretName"))
	})
})
