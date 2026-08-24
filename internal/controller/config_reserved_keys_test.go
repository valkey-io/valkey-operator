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
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// operatorOwnedKeys returns every directive the operator's base config emits, with TLS off and on.
// The TLS branch matters on its own.
// Those keys are only emitted when TLS is configured, so with TLS off a user value takes effect instead of being discarded.
// That is how a user could move or close the port the operator connects to.
func operatorOwnedKeys() []string {
	seen := map[string]struct{}{}
	for _, tls := range []*valkeyiov1alpha1.NodeTLSSpec{
		nil,
		{Certificates: valkeyiov1alpha1.NodeTLSCertificates{
			Server: valkeyiov1alpha1.NodeCertificateRef{SecretName: "certs"},
		}},
	} {
		for key := range getBaseConfig(tls) {
			seen[key] = struct{}{}
		}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// TestReservedConfigKeysMatchBaseConfig pins ReservedConfigKeys to what the operator actually writes.
// Without this, adding a directive to getBaseConfig would silently reintroduce the bug.
// The new key would be accepted in spec.config and then overwritten, with nothing telling the user.
func TestReservedConfigKeysMatchBaseConfig(t *testing.T) {
	owned := operatorOwnedKeys()
	reserved := slices.Clone(valkeyiov1alpha1.ReservedConfigKeys)
	slices.Sort(reserved)

	assert.Equal(t, owned, reserved,
		"ReservedConfigKeys is out of sync with the operator's base config. "+
			"Update ReservedConfigKeys in api/v1alpha1/valkeycluster_types.go and the XValidation rule on ValkeyClusterSpec.Config, which cannot reference it.")
}

// TestReservedConfigKeysMatchCELRule checks the hand-maintained CEL literal against ReservedConfigKeys.
// The rule lives in a marker string, so the compiler cannot catch a mismatch and the two would drift silently.
func TestReservedConfigKeysMatchCELRule(t *testing.T) {
	source, err := os.ReadFile("../../api/v1alpha1/valkeycluster_types.go")
	require.NoError(t, err)

	const marker = "self.all(key, !(key.lowerAscii() in ["
	start := strings.Index(string(source), marker)
	require.NotEqual(t, -1, start, "CEL rule for spec.config not found")

	rest := string(source)[start+len(marker):]
	end := strings.Index(rest, "]")
	require.NotEqual(t, -1, end, "CEL rule key list is not terminated")

	list := rest[:end]
	inRule := make([]string, 0, strings.Count(list, ",")+1)
	for entry := range strings.SplitSeq(list, ",") {
		inRule = append(inRule, strings.Trim(strings.TrimSpace(entry), "'"))
	}
	slices.Sort(inRule)

	reserved := slices.Clone(valkeyiov1alpha1.ReservedConfigKeys)
	slices.Sort(reserved)

	assert.Equal(t, reserved, inRule,
		"the CEL rule on ValkeyClusterSpec.Config does not list the same keys as ReservedConfigKeys")
}

// TestReservedConfigKeysAreLowercase guards the CEL comparison, which lowercases the user's key before testing membership.
// An uppercase entry in the list could therefore never match.
func TestReservedConfigKeysAreLowercase(t *testing.T) {
	for _, key := range valkeyiov1alpha1.ReservedConfigKeys {
		assert.Equal(t, strings.ToLower(key), key, "ReservedConfigKeys entries must be lowercase")
	}
}
