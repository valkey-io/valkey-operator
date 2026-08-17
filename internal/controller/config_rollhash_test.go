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
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// legacyRollConfigRender is a frozen, self-contained copy of the cluster-side
// roll-config render (buildRollServerConfig + getBaseConfig + buildManagedConfig
// + writeConfigLine) as of the removal of ValkeyNode.spec.serverConfigHash. It
// pins the upgrade contract: nodeServerConfigRollHash must stay byte-identical,
// or an operator upgrade rolls every pod.
//
// Do NOT update this copy to track production code. Change it only when a pod
// roll on operator upgrade is intended and understood.
//
// The frozen pin covers the RENDER only. The input-copying contract that feeds
// it — parent controllers copying Config/TLS verbatim onto the node spec, and
// GetTLS's nil-semantics — is not frozen here; it is exercised relatively via
// buildClusterValkeyNode in the matching test below.
func legacyRollConfigRender(cluster *valkeyiov1alpha1.ValkeyCluster) string {
	base := map[string]string{
		"aclfile":                         "/config/users/users.acl",
		"dir":                             "/data",
		"cluster-config-file":             "/data/nodes.conf",
		"cluster-enabled":                 "yes",
		"protected-mode":                  "no",
		"cluster-node-timeout":            "2000",
		"cluster-allow-replica-migration": "no",
		"cluster-replica-validity-factor": "0",
		"shutdown-on-sigterm":             "failover",
	}
	if cluster.GetTLS() != nil {
		base["tls-port"] = "6379"
		base["port"] = "0"
		base["tls-cluster"] = "yes"
		base["tls-replication"] = "yes"
		base["tls-cert-file"] = "/tls/tls.crt"
		base["tls-key-file"] = "/tls/tls.key"
		base["tls-ca-cert-file"] = "/tls/ca.crt"
		base["tls-auth-clients"] = "optional"
	}
	exclude := map[string]struct{}{
		"maxmemory-policy": {},
		"maxmemory":        {},
		"maxclients":       {},
	}

	var b strings.Builder
	includedKeys := make([]string, 0, len(cluster.Spec.Config))
	for _, k := range slices.Sorted(maps.Keys(cluster.Spec.Config)) {
		if _, skip := exclude[k]; skip {
			continue
		}
		includedKeys = append(includedKeys, k)
	}
	if len(includedKeys) > 0 {
		b.WriteString("# User Config\n")
		for _, k := range includedKeys {
			b.WriteString(k + " " + cluster.Spec.Config[k] + "\n")
		}
	}
	b.WriteString("# Base Config\n")
	for _, k := range slices.Sorted(maps.Keys(base)) {
		b.WriteString(k + " " + base[k] + "\n")
	}
	return b.String()
}

func legacyServerConfigRollHash(cluster *valkeyiov1alpha1.ValkeyCluster) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(legacyRollConfigRender(cluster))))
}

func pinTestCluster(config map[string]string, tls bool) *valkeyiov1alpha1.ValkeyCluster {
	cluster := &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "pin-test", Namespace: "default"},
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{
			Shards:   1,
			Replicas: 0,
			Config:   config,
		},
	}
	if tls {
		cluster.Spec.Networking = &valkeyiov1alpha1.NetworkingSpec{
			TLS: &valkeyiov1alpha1.TLSConfig{
				Certificate: valkeyiov1alpha1.CertificateRef{SecretName: "tls-secret"},
			},
		}
	}
	return cluster
}

// The derived node-side hash must be byte-identical to the hash the cluster
// controller historically stamped into Spec.ServerConfigHash — otherwise an
// operator upgrade changes every pod template and rolls every pod.
func TestNodeServerConfigRollHashMatchesLegacy(t *testing.T) {
	cases := []struct {
		name   string
		config map[string]string
		tls    bool
	}{
		{"no user config", nil, false},
		{"live-settable keys only", map[string]string{"maxmemory": "100mb", "maxmemory-policy": "allkeys-lru"}, false},
		{"mixed keys", map[string]string{"appendfsync": "always", "maxmemory": "100mb"}, false},
		{"no user config with TLS", nil, true},
		{"mixed keys with TLS", map[string]string{"appendfsync": "always", "maxmemory": "100mb"}, true},
		{"live-settable keys only with TLS", map[string]string{"maxmemory": "100mb", "maxmemory-policy": "allkeys-lru"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cluster := pinTestCluster(tc.config, tc.tls)
			node := buildClusterValkeyNode(cluster, 0, 0)
			assert.Equal(t, legacyServerConfigRollHash(cluster), nodeServerConfigRollHash(node),
				"derived hash diverged from the frozen legacy hash; this WILL roll pods on upgrade")
		})
	}
}

func TestNodeServerConfigRollHashIgnoresLiveSettableKeys(t *testing.T) {
	before := buildClusterValkeyNode(pinTestCluster(map[string]string{
		"appendfsync": "always",
		"maxmemory":   "100mb",
	}, false), 0, 0)
	after := buildClusterValkeyNode(pinTestCluster(map[string]string{
		"appendfsync": "always",
		"maxmemory":   "500mb",
	}, false), 0, 0)
	assert.Equal(t, nodeServerConfigRollHash(before), nodeServerConfigRollHash(after),
		"live-settable key change must not change the roll hash")
}

func TestNodeServerConfigRollHashChangesOnNonLiveKey(t *testing.T) {
	before := buildClusterValkeyNode(pinTestCluster(map[string]string{"appendfsync": "always"}, false), 0, 0)
	after := buildClusterValkeyNode(pinTestCluster(map[string]string{"appendfsync": "everysec"}, false), 0, 0)
	assert.NotEqual(t, nodeServerConfigRollHash(before), nodeServerConfigRollHash(after),
		"non-live key change must change the roll hash so the pod rolls")
}
