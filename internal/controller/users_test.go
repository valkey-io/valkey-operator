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
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

func TestOperatorUserPasswordSecret(t *testing.T) {
	sel := operatorUserPasswordSecret("c1")
	assert.NotNil(t, sel)
	assert.Equal(t, "internal-c1-system-passwords", sel.Name)
	assert.Equal(t, "_operator", sel.Key)
	assert.NotNil(t, sel.Optional)
	assert.True(t, *sel.Optional)
}

var buildAclPasswords = map[string][]string{
	"alice": {"a71153805265764af6f55b4e0ce38858cde64e6e24b9a9b14e32262760572137"},
	"david": {
		"7447bd019c69af5975c54072b40f9c24d1105836cbd68408d6df7be76ac42ab1",
		"4b31cf3c1347d94fe80efb0c848579c5730d63efef2f5eaf32f78a7ca251833b",
	},
	"frank": {"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"},
}

var buildAclExpected = map[string]string{
	"alice":   "user alice on #a71153805265764af6f55b4e0ce38858cde64e6e24b9a9b14e32262760572137 ~app:* ~cache:* %R~shared:* %R~config:* %W~logs:* %W~metrics:* resetchannels &notifications:* &events:* +@read +@write +@connection -@admin -@dangerous +client|setname +debug",
	"bob":     "user bob on nopass +@all -@admin ~* &*",
	"charlie": "user charlie off nopass +@admin",
	"david":   "user david on #7447bd019c69af5975c54072b40f9c24d1105836cbd68408d6df7be76ac42ab1 #4b31cf3c1347d94fe80efb0c848579c5730d63efef2f5eaf32f78a7ca251833b +@admin",
	"edward":  "user edward on +@admin",
	"frank":   "user frank on #9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08 +@read",
	"grace":   "user grace on nopass db=0,1 +@all",
	"henry":   "user henry on nopass resetdbs +@all",
	"ivan":    "user ivan on nopass alldbs +@all",
}

func TestBuildAclFileContents(t *testing.T) {
	adminOnly := valkeyiov1alpha1.ACLPermissions{
		Commands: valkeyiov1alpha1.CommandsAclSpec{Allow: []string{"@admin"}},
	}
	allCommands := valkeyiov1alpha1.CommandsAclSpec{Allow: []string{"@all"}}
	anyPassword := &valkeyiov1alpha1.ACLUserPassword{Type: valkeyiov1alpha1.ACLPasswordTypeAny}
	users := []valkeyiov1alpha1.ACLUserSpec{
		{
			Name:  "alice",
			State: valkeyiov1alpha1.ACLUserStateEnabled,
			Password: &valkeyiov1alpha1.ACLUserPassword{
				Type:   valkeyiov1alpha1.ACLPasswordTypeSecret,
				Secret: &valkeyiov1alpha1.PasswordSecretSpec{Name: "alice-secret", Keys: []string{"alice"}},
			},
			ACLPermissions: legacyBuildAclUsers[0].ACLPermissions,
		},
		{
			Name:           "bob",
			Password:       &valkeyiov1alpha1.ACLUserPassword{Type: valkeyiov1alpha1.ACLPasswordTypeAny},
			ACLPermissions: valkeyiov1alpha1.ACLPermissions{RawAcl: "+@all -@admin ~* &*"},
		},
		{
			Name:           "charlie",
			State:          valkeyiov1alpha1.ACLUserStateDisabled,
			Password:       &valkeyiov1alpha1.ACLUserPassword{Type: valkeyiov1alpha1.ACLPasswordTypeAny},
			ACLPermissions: adminOnly,
		},
		{
			Name: "david",
			Password: &valkeyiov1alpha1.ACLUserPassword{
				Secret: &valkeyiov1alpha1.PasswordSecretSpec{Keys: []string{"davidold", "davidnew"}},
			},
			ACLPermissions: adminOnly,
		},
		{
			Name:           "edward",
			Password:       &valkeyiov1alpha1.ACLUserPassword{Type: valkeyiov1alpha1.ACLPasswordTypeDisabled},
			ACLPermissions: adminOnly,
		},
		{
			// No state or password block behaves as Enabled and Secret.
			Name: "frank",
			ACLPermissions: valkeyiov1alpha1.ACLPermissions{
				Commands: valkeyiov1alpha1.CommandsAclSpec{Allow: []string{"@read"}},
			},
		},
		{
			// db= clears alldbs on its own, so unlike channels it must not be
			// preceded by a reset directive. The exact-match assertion below
			// is what pins that: a stray "resetdbs" would fail the comparison.
			Name:     "grace",
			Password: anyPassword,
			ACLPermissions: valkeyiov1alpha1.ACLPermissions{
				Databases: valkeyiov1alpha1.DatabasesAclSpec{IDs: []int32{0, 1}},
				Commands:  allCommands,
			},
		},
		{
			Name:     "henry",
			Password: anyPassword,
			ACLPermissions: valkeyiov1alpha1.ACLPermissions{
				Databases: valkeyiov1alpha1.DatabasesAclSpec{ResetDbs: true},
				Commands:  allCommands,
			},
		},
		{
			// Rendering-only expectation. alldbs is every user's default, so
			// what the operator writes here produces a user indistinguishable
			// from one with no databases block; this pins the rendered bytes,
			// not anything observable on the server.
			Name:     "ivan",
			Password: anyPassword,
			ACLPermissions: valkeyiov1alpha1.ACLPermissions{
				Databases: valkeyiov1alpha1.DatabasesAclSpec{AllDbs: true},
				Commands:  allCommands,
			},
		},
	}

	for _, user := range users {
		acl := buildUserAcl(user, buildAclPasswords[user.Name], true)
		assert.Equal(t, buildAclExpected[user.Name], strings.TrimSpace(acl), user.Name)
	}
}

// Converted legacy users must render the same lines they did before
// spec.acl.users existed, so moving users between the fields changes nothing.
func TestBuildUserAclLegacyParity(t *testing.T) {
	for _, legacy := range legacyBuildAclUsers {
		acl := buildUserAcl(legacy.ToACLUser(), buildAclPasswords[legacy.Name], true)
		assert.Equal(t, buildAclExpected[legacy.Name], strings.TrimSpace(acl), legacy.Name)
	}
}

var legacyBuildAclUsers = []valkeyiov1alpha1.UserAclSpec{
	{
		Name:    "alice",
		Enabled: true,
		PasswordSecret: valkeyiov1alpha1.PasswordSecretSpec{
			Name: "alice-secret",
			Keys: []string{"alice"},
		},
		NoPassword: false,
		ACLPermissions: valkeyiov1alpha1.ACLPermissions{
			Commands: valkeyiov1alpha1.CommandsAclSpec{
				Allow: []string{"@read", "@write", "@connection"},
				Deny:  []string{"@admin", "@dangerous"},
			},
			Keys: valkeyiov1alpha1.KeysAclSpec{
				ReadWrite: []string{"app:*", "cache:*"},
				ReadOnly:  []string{"shared:*", "config:*"},
				WriteOnly: []string{"logs:*", "metrics:*"},
			},
			Channels: valkeyiov1alpha1.ChannelsAclSpec{
				Patterns: []string{"notifications:*", "events:*"},
			},
			RawAcl: "+client|setname +debug",
		},
	},
	{
		Name:       "bob",
		Enabled:    true,
		NoPassword: true,
		ACLPermissions: valkeyiov1alpha1.ACLPermissions{
			RawAcl: "+@all -@admin ~* &*",
		},
	},
	{
		Name:       "charlie",
		Enabled:    false,
		NoPassword: true,
		ACLPermissions: valkeyiov1alpha1.ACLPermissions{
			Commands: valkeyiov1alpha1.CommandsAclSpec{
				Allow: []string{"@admin"},
			},
		},
	},
	{
		Name:    "david",
		Enabled: true,
		PasswordSecret: valkeyiov1alpha1.PasswordSecretSpec{
			Keys: []string{"davidold", "davidnew"},
		},
		ACLPermissions: valkeyiov1alpha1.ACLPermissions{
			Commands: valkeyiov1alpha1.CommandsAclSpec{
				Allow: []string{"@admin"},
			},
		},
	},
	{
		Name:       "edward",
		Enabled:    true,
		ResetPass:  true,
		NoPassword: true,
		ACLPermissions: valkeyiov1alpha1.ACLPermissions{
			Commands: valkeyiov1alpha1.CommandsAclSpec{
				Allow: []string{"@admin"},
			},
		},
	},
	{
		// Databases rides along in ACLPermissions, so the conversion must keep
		// it: this entry renders through ToACLUser in TestBuildUserAclLegacyParity.
		Name:       "grace",
		Enabled:    true,
		NoPassword: true,
		ACLPermissions: valkeyiov1alpha1.ACLPermissions{
			Databases: valkeyiov1alpha1.DatabasesAclSpec{IDs: []int32{0, 1}},
			Commands:  valkeyiov1alpha1.CommandsAclSpec{Allow: []string{"@all"}},
		},
	},
}

func TestBuildUserAcl_DatabasesVersionGate(t *testing.T) {
	// On an image that predates the db= directives the renderer must drop them
	// silently: emitting them would make ACL LOAD fail for the whole file, and
	// rejecting the spec is not the contract (see aclVersionGateWarnings).
	for name, databases := range map[string]valkeyiov1alpha1.DatabasesAclSpec{
		"ids":      {IDs: []int32{0, 1}},
		"allDbs":   {AllDbs: true},
		"resetDbs": {ResetDbs: true},
	} {
		t.Run(name, func(t *testing.T) {
			user := valkeyiov1alpha1.ACLUserSpec{
				Name:     "frank",
				Password: &valkeyiov1alpha1.ACLUserPassword{Type: valkeyiov1alpha1.ACLPasswordTypeAny},
				ACLPermissions: valkeyiov1alpha1.ACLPermissions{
					Databases: databases,
					Commands:  valkeyiov1alpha1.CommandsAclSpec{Allow: []string{"@all"}},
				},
			}

			acl := strings.TrimSpace(buildUserAcl(user, nil, false))

			assert.Equal(t, "user frank on nopass +@all", acl)
			assert.NotContains(t, acl, "db=")
			assert.NotContains(t, acl, "alldbs")
			assert.NotContains(t, acl, "resetdbs")
		})
	}
}

func TestBuildUserAcl_DatabasesPrecedence(t *testing.T) {
	// CEL rejects these combinations at admission, so they are unreachable
	// through the API. The renderer still has to pick exactly one directive
	// rather than emit two contradictory ones, for any object that predates
	// the CEL rules or arrives through a client that skips validation.
	tests := map[string]struct {
		databases valkeyiov1alpha1.DatabasesAclSpec
		expected  string
	}{
		"resetDbs wins over allDbs": {
			databases: valkeyiov1alpha1.DatabasesAclSpec{AllDbs: true, ResetDbs: true},
			expected:  "user frank on nopass resetdbs +@all",
		},
		"resetDbs wins over ids": {
			databases: valkeyiov1alpha1.DatabasesAclSpec{IDs: []int32{0}, ResetDbs: true},
			expected:  "user frank on nopass resetdbs +@all",
		},
		"allDbs wins over ids": {
			databases: valkeyiov1alpha1.DatabasesAclSpec{IDs: []int32{0}, AllDbs: true},
			expected:  "user frank on nopass alldbs +@all",
		},
		"empty ids renders no directive": {
			// db= with no id is a syntax error that breaks ACL LOAD for the
			// entire file, so an empty list must render nothing at all.
			databases: valkeyiov1alpha1.DatabasesAclSpec{IDs: []int32{}},
			expected:  "user frank on nopass +@all",
		},
		"unset renders no directive": {
			databases: valkeyiov1alpha1.DatabasesAclSpec{},
			expected:  "user frank on nopass +@all",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			user := valkeyiov1alpha1.ACLUserSpec{
				Name:     "frank",
				Password: &valkeyiov1alpha1.ACLUserPassword{Type: valkeyiov1alpha1.ACLPasswordTypeAny},
				ACLPermissions: valkeyiov1alpha1.ACLPermissions{
					Databases: tc.databases,
					Commands:  valkeyiov1alpha1.CommandsAclSpec{Allow: []string{"@all"}},
				},
			}

			assert.Equal(t, tc.expected, strings.TrimSpace(buildUserAcl(user, nil, true)))
		})
	}
}

func TestValidateSystemUserPasswordSecret(t *testing.T) {
	userSecret := map[string][]byte{
		operatorUser:    nil,
		replicationUser: nil,
	}
	cluster := &valkeyiov1alpha1.ValkeyCluster{
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{
			Exporter: valkeyiov1alpha1.ExporterSpec{
				Enabled: new(true),
			},
		},
	}
	err := validateSystemUserPasswordSecret(userSecret, cluster)
	if !errors.Is(err, errMissingSystemUser) {
		t.Errorf("Validate System Users Password Failed. Expected '%s', got: '%s'", errMissingSystemUser.Error(), err.Error())
	}
}

func TestValidateSystemUserPasswordSecret_UnknownUser(t *testing.T) {
	unknownUser := "_randomuser"
	userSecret := map[string][]byte{
		operatorUser:    nil,
		replicationUser: nil,
		unknownUser:     nil,
	}
	cluster := &valkeyiov1alpha1.ValkeyCluster{
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{
			Exporter: valkeyiov1alpha1.ExporterSpec{
				Enabled: new(false),
			},
		},
	}
	err := validateSystemUserPasswordSecret(userSecret, cluster)
	if !errors.Is(err, errUnknownSystemUser) {
		t.Errorf("Validate System Users Password Failed. Expected '%s', got: '%s'", errUnknownSystemUser.Error(), err.Error())
	}
}

func TestValidateSystemUserPasswordSecret_UnknownUserAndMissingRequiredUser(t *testing.T) {
	unknownUser := "_randomuser"
	userSecret := map[string][]byte{
		unknownUser: nil,
	}
	cluster := &valkeyiov1alpha1.ValkeyCluster{
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{
			Exporter: valkeyiov1alpha1.ExporterSpec{
				Enabled: new(false),
			},
		},
	}
	err := validateSystemUserPasswordSecret(userSecret, cluster)
	if !errors.Is(err, errMissingSystemUser) {
		t.Errorf("Validate System Users Password Failed. Expected '%s', got: '%s'", errMissingSystemUser.Error(), err.Error())
	}
}

func TestExporterUserAllowsCommandLogMetrics(t *testing.T) {
	rules := strings.Fields(systemUsersAcls[exporterUser])
	index := func(rule string) int {
		for i, r := range rules {
			if r == rule {
				return i
			}
		}
		return -1
	}

	// redis_exporter probes COMMANDLOG support with COMMAND INFO, then reads COMMANDLOG LEN.
	assert.Greater(t, index("+commandlog|len"), -1)
	// ACL rules apply left to right, so the grant must follow the blanket -command.
	assert.Greater(t, index("+command|info"), index("-command"))
	assert.Greater(t, index("-command"), -1)
}

func TestBuildUserAclUnknownStateIsOff(t *testing.T) {
	user := valkeyiov1alpha1.ACLUserSpec{
		Name:     "alice",
		State:    "Paused",
		Password: &valkeyiov1alpha1.ACLUserPassword{Type: valkeyiov1alpha1.ACLPasswordTypeAny},
	}

	assert.Equal(t, "user alice off nopass", strings.TrimSpace(buildUserAcl(user, nil, true)))
}

func TestDeprecatedFieldConfigWarnings(t *testing.T) {
	legacy := []valkeyiov1alpha1.UserAclSpec{{Name: "bob", Enabled: true, NoPassword: true}}

	warnings := deprecatedFieldConfigWarnings(&valkeyiov1alpha1.ValkeyCluster{
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{Users: legacy}, //nolint:staticcheck // legacy spec.users
	})
	if assert.Len(t, warnings, 1) {
		assert.Equal(t, valkeyiov1alpha1.ReasonDeprecatedField, warnings[0].reason)
		assert.Contains(t, warnings[0].message, "spec.users is deprecated")
		assert.Contains(t, warnings[0].message, "spec.acl.users")
	}

	replicas := &valkeyiov1alpha1.ValkeyCluster{}
	replicas.Spec.Replicas = 2 //nolint:staticcheck // warn until v1beta1 removes spec.replicas
	warnings = deprecatedFieldConfigWarnings(replicas)
	if assert.Len(t, warnings, 1) {
		assert.Equal(t, valkeyiov1alpha1.ReasonDeprecatedField, warnings[0].reason)
		assert.Contains(t, warnings[0].message, "set spec.replicasPerShard: 2 and remove spec.replicas in the same update")
	}

	both := &valkeyiov1alpha1.ValkeyCluster{
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{Users: legacy}, //nolint:staticcheck // legacy spec.users
	}
	both.Spec.Replicas = 1 //nolint:staticcheck // warn until v1beta1 removes spec.replicas
	assert.Len(t, deprecatedFieldConfigWarnings(both), 2)

	assert.Empty(t, deprecatedFieldConfigWarnings(&valkeyiov1alpha1.ValkeyCluster{
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{
			ACL: &valkeyiov1alpha1.ACLSpec{Users: []valkeyiov1alpha1.ACLUserSpec{{Name: "bob"}}},
		},
	}))
	assert.Empty(t, deprecatedFieldConfigWarnings(&valkeyiov1alpha1.ValkeyCluster{
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{Users: []valkeyiov1alpha1.UserAclSpec{}}, //nolint:staticcheck // legacy spec.users
	}))
}
