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
}

func TestBuildAclFileContents(t *testing.T) {
	adminOnly := valkeyiov1alpha1.ACLPermissions{
		Commands: valkeyiov1alpha1.CommandsAclSpec{Allow: []string{"@admin"}},
	}
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
	}

	for _, user := range users {
		acl := buildUserAcl(user, buildAclPasswords[user.Name])
		assert.Equal(t, buildAclExpected[user.Name], strings.TrimSpace(acl), user.Name)
	}
}

// Converted legacy users must render the same lines they did before
// spec.acl.users existed, so moving users between the fields changes nothing.
func TestBuildUserAclLegacyParity(t *testing.T) {
	for _, legacy := range legacyBuildAclUsers {
		acl := buildUserAcl(legacy.ToACLUser(), buildAclPasswords[legacy.Name])
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

func TestDeprecatedFieldConfigWarnings(t *testing.T) {
	legacy := []valkeyiov1alpha1.UserAclSpec{{Name: "bob", Enabled: true, NoPassword: true}}

	warnings := deprecatedFieldConfigWarnings(&valkeyiov1alpha1.ValkeyCluster{
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{Users: legacy},
	})
	if assert.Len(t, warnings, 1) {
		assert.Equal(t, valkeyiov1alpha1.ReasonDeprecatedField, warnings[0].reason)
		assert.Contains(t, warnings[0].message, "spec.users is deprecated")
		assert.Contains(t, warnings[0].message, "spec.acl.users")
	}

	assert.Empty(t, deprecatedFieldConfigWarnings(&valkeyiov1alpha1.ValkeyCluster{
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{
			ACL: &valkeyiov1alpha1.ACLSpec{Users: []valkeyiov1alpha1.ACLUserSpec{{Name: "bob"}}},
		},
	}))
	assert.Empty(t, deprecatedFieldConfigWarnings(&valkeyiov1alpha1.ValkeyCluster{
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{Users: []valkeyiov1alpha1.UserAclSpec{}},
	}))
}
