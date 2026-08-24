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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// configCluster builds a minimal admissible ValkeyCluster carrying the given spec.config.
func configCluster(name string, config map[string]string) *valkeyiov1alpha1.ValkeyCluster {
	return &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{
			Shards:   1,
			Replicas: 0,
			Config:   config,
		},
	}
}

const reservedKeyMessage = "must not set operator-owned keys"

var _ = Describe("ValkeyCluster spec.config validation", func() {
	ctx := context.Background()

	It("admits tunables the operator does not own", func() {
		cluster := configCluster("cfg-tunables", map[string]string{
			"maxmemory":        "50mb",
			"maxmemory-policy": "allkeys-lfu",
			"maxclients":       "1000",
			"appendonly":       "yes",
			"timeout":          "0",
		})
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
	})

	It("admits cluster directives the operator leaves alone", func() {
		// The operator sets some cluster- keys and not others.
		// Only the ones it sets are reserved, so these have to keep working.
		cluster := configCluster("cfg-cluster-ok", map[string]string{
			"cluster-require-full-coverage": "no",
			"cluster-migration-barrier":     "1",
			"cluster-allow-reads-when-down": "yes",
		})
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
	})

	It("admits an absent config", func() {
		cluster := configCluster("cfg-absent", nil)
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
	})

	// Every reserved key is covered, so a key added to ReservedConfigKeys without a
	// matching CEL update fails here as well as in the drift guard tests.
	for i, key := range valkeyiov1alpha1.ReservedConfigKeys {
		reserved := key
		name := fmt.Sprintf("cfg-reserved-%d", i)

		It(fmt.Sprintf("rejects the operator-owned key %s", reserved), func() {
			err := k8sClient.Create(ctx, configCluster(name, map[string]string{reserved: "somevalue"}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(reservedKeyMessage))
		})
	}

	DescribeTable("rejects operator-owned keys regardless of case",
		func(key string) {
			err := k8sClient.Create(ctx, configCluster("cfg-case", map[string]string{key: "yes"}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(reservedKeyMessage))
		},
		Entry("mixed case", "Cluster-Enabled"),
		Entry("upper case", "PROTECTED-MODE"),
		Entry("mixed case tls", "TLS-Port"),
	)

	It("rejects a reserved key mixed in with valid ones", func() {
		err := k8sClient.Create(ctx, configCluster("cfg-mixed", map[string]string{
			"maxmemory": "50mb",
			"dir":       "/somewhere-else",
		}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(reservedKeyMessage))
	})

	It("rejects a reserved key added by update", func() {
		cluster := configCluster("cfg-update", map[string]string{"maxmemory": "50mb"})
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() {
			Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
		})

		cluster.Spec.Config["aclfile"] = "/tmp/users.acl"
		err := k8sClient.Update(ctx, cluster)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(reservedKeyMessage))
	})

	It("names the offending keys in the message", func() {
		// The message has to be actionable on its own, since there is no webhook to elaborate.
		err := k8sClient.Create(ctx, configCluster("cfg-message", map[string]string{"cluster-enabled": "no"}))
		Expect(err).To(HaveOccurred())
		for _, key := range []string{"aclfile", "cluster-enabled", "dir", "tls-port"} {
			Expect(err.Error()).To(ContainSubstring(key), "message should list the reserved keys")
		}
		Expect(strings.ToLower(err.Error())).To(ContainSubstring("operator sets these itself"))
	})
})
