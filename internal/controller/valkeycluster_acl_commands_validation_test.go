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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// The pattern markers on commands.allow/deny were malformed, so controller-gen
// dropped them and any string reached the ACL file. Valkey then rejected the
// file at ACL LOAD (#395).
var _ = Describe("users commands validation", func() {
	var (
		ctx     context.Context
		counter int
	)

	BeforeEach(func() {
		ctx = context.Background()
	})

	// applyWithCommands creates a cluster carrying a single user whose
	// commands.allow holds entry, and returns the API server's verdict.
	applyWithCommands := func(entry string) error {
		counter++
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("acl-cmd-%d", counter),
				Namespace: "default",
			},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:   1,
				Replicas: 0,
				Users: []valkeyiov1alpha1.UserAclSpec{{
					Name:       "alice",
					Enabled:    true,
					NoPassword: true,
					Commands:   valkeyiov1alpha1.CommandsAclSpec{Allow: []string{entry}},
				}},
			},
		}
		err := k8sClient.Create(ctx, cluster)
		if err == nil {
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, cluster)
			})
		}
		return err
	}

	// Every one of these is accepted by Valkey's own ACL parser. Categories and
	// plain commands aside, the awkward ones carry a digit, an underscore or a
	// hyphen, and Valkey normalises case, so the pattern cannot be lowercase
	// letters only.
	It("accepts command entries that Valkey accepts", func() {
		for _, entry := range []string{
			"@read",
			"@all",
			"get",
			"client|setname",
			"cluster|set-config-epoch",
			"restore-asking",
			"eval_ro",
			"bitfield_ro",
			"memory|malloc-stats",
			"GET",
			"CLIENT|SETNAME",
		} {
			Expect(applyWithCommands(entry)).To(Succeed(), "entry %q must be accepted", entry)
		}
	})

	It("rejects entries that cannot be a command or category", func() {
		for _, entry := range []string{
			"THIS IS NOT A COMMAND !!!",
			"get set",
			"get;flushall",
			"+get",
			"",
		} {
			Expect(applyWithCommands(entry)).NotTo(Succeed(), "entry %q must be rejected", entry)
		}
	})

	// Character-class-only validation let these through. Valkey rejects every
	// one of them, so the pattern has to constrain structure, not just the
	// alphabet.
	It("rejects entries with the right characters but the wrong shape", func() {
		for _, entry := range []string{
			"@",
			"@@read",
			"@-read",
			"|",
			"get|",
			"|get",
			"get||set",
			"client|no|evict",
		} {
			Expect(applyWithCommands(entry)).NotTo(Succeed(), "entry %q must be rejected", entry)
		}
	})

	It("rejects a bad entry in commands.deny as well", func() {
		counter++
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("acl-cmd-deny-%d", counter),
				Namespace: "default",
			},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:   1,
				Replicas: 0,
				Users: []valkeyiov1alpha1.UserAclSpec{{
					Name:       "alice",
					Enabled:    true,
					NoPassword: true,
					Commands:   valkeyiov1alpha1.CommandsAclSpec{Deny: []string{"NOT A COMMAND"}},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).NotTo(Succeed())
	})
})
