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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

// getSampleCluster returns a ValkeyCluster object with config options.
func getSampleCluster() *valkeyiov1alpha1.ValkeyCluster {
	return &valkeyiov1alpha1.ValkeyCluster{
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{
			Config: map[string]string{
				"maxmemory":        "50mb",
				"maxmemory-policy": "allkeys-lfu",
			},
		},
	}
}

var _ = Describe("When creating a cluster", Label("userconfig"), func() {
	It("should have base, and user-supplied configuration", func() {
		By("verifying the rendered config")
		cluster := getSampleCluster()

		testConfigString := buildServerConfig(cluster)

		// Check user-added parameter
		Expect(testConfigString).To(ContainSubstring("maxmemory-policy"))

		// Base, non-overridable parameter
		Expect(testConfigString).To(ContainSubstring("cluster-enabled"))

		// Primary fails over to a replica on SIGTERM (graceful shutdown)
		Expect(testConfigString).To(ContainSubstring("shutdown-on-sigterm failover"))
	})

	// dir and cluster-config-file must always be present, even
	// without spec.persistence, so nodes.conf can be written under
	// readOnlyRootFilesystem: true.
	It("should always emit dir and cluster-config-file directives", func() {
		By("verifying the rendered config without persistence")
		clusterNoPersist := getSampleCluster()
		cfgNoPersist := buildServerConfig(clusterNoPersist)
		Expect(cfgNoPersist).To(ContainSubstring("dir " + dataMountPath))
		Expect(cfgNoPersist).To(ContainSubstring("cluster-config-file " + dataMountPath + "/nodes.conf"))

		By("verifying the rendered config with persistence")
		clusterWithPersist := getSampleCluster()
		clusterWithPersist.Spec.Persistence = &valkeyiov1alpha1.PersistenceSpec{}
		cfgWithPersist := buildServerConfig(clusterWithPersist)
		Expect(cfgWithPersist).To(ContainSubstring("dir " + dataMountPath))
		Expect(cfgWithPersist).To(ContainSubstring("cluster-config-file " + dataMountPath + "/nodes.conf"))
	})
})

var _ = Describe("TLSConfig CEL admission rules", Label("tls", "cel"), func() {
	newCluster := func(name string, authClients valkeyiov1alpha1.TLSAuthClients, authClientsUser valkeyiov1alpha1.TLSAuthClientsUser) *valkeyiov1alpha1.ValkeyCluster {
		return &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards: 1,
				TLS: &valkeyiov1alpha1.TLSConfig{
					Certificate:     valkeyiov1alpha1.CertificateRef{SecretName: "tls-secret"},
					AuthClients:     authClients,
					AuthClientsUser: authClientsUser,
				},
			},
		}
	}

	It("rejects authClients=no with authClientsUser=CN", func() {
		cluster := newCluster("cel-disabled-cn", valkeyiov1alpha1.TLSAuthClientsDisabled, valkeyiov1alpha1.TLSAuthClientsUserCN)
		err := k8sClient.Create(ctx, cluster)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("authClientsUser=CN"))
	})

	It("rejects authClients=no with authClientsUser=URI", func() {
		cluster := newCluster("cel-disabled-uri", valkeyiov1alpha1.TLSAuthClientsDisabled, valkeyiov1alpha1.TLSAuthClientsUserURI)
		err := k8sClient.Create(ctx, cluster)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("authClientsUser=CN/URI"))
	})

	It("accepts authClients=yes with authClientsUser=CN", func() {
		cluster := newCluster("cel-required-cn", valkeyiov1alpha1.TLSAuthClientsRequired, valkeyiov1alpha1.TLSAuthClientsUserCN)
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
	})

	It("accepts authClients=yes with authClientsUser=URI", func() {
		cluster := newCluster("cel-required-uri", valkeyiov1alpha1.TLSAuthClientsRequired, valkeyiov1alpha1.TLSAuthClientsUserURI)
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
	})

	It("accepts authClients=no with authClientsUser=off", func() {
		cluster := newCluster("cel-disabled-off", valkeyiov1alpha1.TLSAuthClientsDisabled, valkeyiov1alpha1.TLSAuthClientsUserOff)
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
	})
})

var _ = Describe("When TLS client auth is configured", Label("tls"), func() {
	It("renders mTLS directives when AuthClients=Required and AuthClientsUser=CN", func() {
		cluster := getSampleCluster()
		cluster.Spec.TLS = &valkeyiov1alpha1.TLSConfig{
			Certificate:     valkeyiov1alpha1.CertificateRef{SecretName: "tls-secret"},
			AuthClients:     valkeyiov1alpha1.TLSAuthClientsRequired,
			AuthClientsUser: valkeyiov1alpha1.TLSAuthClientsUserCN,
		}
		conf := buildServerConfig(cluster)
		Expect(conf).To(ContainSubstring("tls-auth-clients yes"))
		Expect(conf).To(ContainSubstring("tls-auth-clients-user CN"))
	})

	It("renders mTLS directives when AuthClients=Required and AuthClientsUser=URI", func() {
		cluster := getSampleCluster()
		cluster.Spec.TLS = &valkeyiov1alpha1.TLSConfig{
			Certificate:     valkeyiov1alpha1.CertificateRef{SecretName: "tls-secret"},
			AuthClients:     valkeyiov1alpha1.TLSAuthClientsRequired,
			AuthClientsUser: valkeyiov1alpha1.TLSAuthClientsUserURI,
		}
		conf := buildServerConfig(cluster)
		Expect(conf).To(ContainSubstring("tls-auth-clients yes"))
		Expect(conf).To(ContainSubstring("tls-auth-clients-user URI"))
	})

	It("renders tls-auth-clients no when AuthClients=Disabled", func() {
		cluster := getSampleCluster()
		cluster.Spec.TLS = &valkeyiov1alpha1.TLSConfig{
			Certificate: valkeyiov1alpha1.CertificateRef{SecretName: "tls-secret"},
			AuthClients: valkeyiov1alpha1.TLSAuthClientsDisabled,
		}
		conf := buildServerConfig(cluster)
		Expect(conf).To(ContainSubstring("tls-auth-clients no"))
	})

	It("does not render TLS directives when TLS is unset", func() {
		cluster := getSampleCluster()
		conf := buildServerConfig(cluster)
		Expect(conf).NotTo(ContainSubstring("tls-port"))
		Expect(conf).NotTo(ContainSubstring("tls-auth-clients"))
	})
})

var _ = Describe("Live config", Label("liveconfig"), func() {
	newCluster := func(cfg map[string]string) *valkeyiov1alpha1.ValkeyCluster {
		return &valkeyiov1alpha1.ValkeyCluster{
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{Config: cfg},
		}
	}

	It("excludes allowlisted keys from the roll config but keeps others and base", func() {
		cluster := newCluster(map[string]string{
			"maxmemory-policy": "allkeys-lru", // allowlisted
			"appendonly":       "yes",         // not allowlisted
		})
		rollConfig := buildRollServerConfig(cluster)
		Expect(rollConfig).NotTo(ContainSubstring("maxmemory-policy"))
		Expect(rollConfig).To(ContainSubstring("appendonly"))
		Expect(rollConfig).To(ContainSubstring("cluster-enabled")) // base retained
	})

	It("keeps the roll hash stable when only an allowlisted key changes", func() {
		before := serverConfigRollHash(newCluster(map[string]string{
			"maxmemory-policy": "allkeys-lru",
			"appendonly":       "yes",
		}))
		after := serverConfigRollHash(newCluster(map[string]string{
			"maxmemory-policy": "volatile-lru",
			"appendonly":       "yes",
		}))
		Expect(after).To(Equal(before))
	})

	It("changes the roll hash when a non-allowlisted key changes", func() {
		before := serverConfigRollHash(newCluster(map[string]string{"appendonly": "yes"}))
		after := serverConfigRollHash(newCluster(map[string]string{"appendonly": "no"}))
		Expect(after).NotTo(Equal(before))
	})

	It("liveConfigToApply returns only allowlisted keys present in config", func() {
		out := liveConfigToApply(map[string]string{
			"maxmemory-policy": "allkeys-lru",
			"appendonly":       "yes",
		})
		Expect(out).To(Equal(map[string]string{"maxmemory-policy": "allkeys-lru"}))
	})
})
