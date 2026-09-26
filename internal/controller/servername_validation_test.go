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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

func serverNameCluster(name, serverName string) *valkeyiov1alpha1.ValkeyCluster {
	return &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{
			Shards: 1,
			Networking: &valkeyiov1alpha1.NetworkingSpec{
				TLS: &valkeyiov1alpha1.TLSSpec{
					Certificates: valkeyiov1alpha1.TLSCertificates{
						Server: valkeyiov1alpha1.CertificateSource{SecretName: "tls"},
					},
					ServerName: serverName,
				},
			},
		},
	}
}

func serverNameNode(name, serverName string) *valkeyiov1alpha1.ValkeyNode {
	return &valkeyiov1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: valkeyiov1alpha1.ValkeyNodeSpec{
			TLS: &valkeyiov1alpha1.NodeTLSSpec{
				Certificates: valkeyiov1alpha1.NodeTLSCertificates{
					Server: valkeyiov1alpha1.NodeCertificateRef{SecretName: "tls"},
				},
				ServerName: serverName,
			},
		},
	}
}

// The serverName rule matches DNS-1123 subdomains with self.matches() rather
// than format.dns1123Subdomain(), which a Kubernetes 1.31 API server rejects.
// It accepts the same names: like IsDNS1123Subdomain, it limits only the total
// length (MaxLength=253), not each label.
var _ = Describe("serverName CEL validation", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	accepted := []string{
		"valkey-mycluster.default.svc.cluster.local",
		"custom.example",
		"a",
		"1-2.x",
		strings.Repeat("a", 64) + ".example",
	}
	rejected := map[string]string{
		"uppercase":     "Valkey.example",
		"leading dash":  "-valkey.example",
		"trailing dash": "valkey-.example",
		"trailing dot":  "valkey.example.",
		"empty label":   "valkey..example",
		"underscore":    "valkey_1.example",
		"wildcard":      "*.valkey.example",
	}

	for i, name := range accepted {
		It("accepts "+name+" on a ValkeyCluster and a ValkeyNode", func() {
			c := serverNameCluster("sn-ok-c-"+string(rune('a'+i)), name)
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
			n := serverNameNode("sn-ok-n-"+string(rune('a'+i)), name)
			Expect(k8sClient.Create(ctx, n)).To(Succeed())
			Expect(k8sClient.Delete(ctx, n)).To(Succeed())
		})
	}

	for desc, name := range rejected {
		It("rejects a "+desc+" on a ValkeyCluster and a ValkeyNode", func() {
			err := k8sClient.Create(ctx, serverNameCluster("sn-bad-c", name))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("must be a valid DNS-1123 subdomain"))
			err = k8sClient.Create(ctx, serverNameNode("sn-bad-n", name))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("must be a valid DNS-1123 subdomain"))
		})
	}
})
