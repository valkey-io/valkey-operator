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

package v1alpha1

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserAclSpecToACLUser(t *testing.T) {
	perms := ACLPermissions{
		Commands: CommandsAclSpec{Allow: []string{"@read"}},
		Keys:     KeysAclSpec{ReadOnly: []string{"app:*"}},
		Channels: ChannelsAclSpec{Patterns: []string{"events:*"}},
		RawAcl:   "+ping",
	}

	tests := []struct {
		name             string
		legacy           UserAclSpec
		wantState        ACLUserState
		wantPasswordType ACLPasswordType
	}{
		{"enabled", UserAclSpec{Enabled: true}, ACLUserStateEnabled, ACLPasswordTypeSecret},
		{"disabled", UserAclSpec{Enabled: false}, ACLUserStateDisabled, ACLPasswordTypeSecret},
		{"nopass", UserAclSpec{Enabled: true, NoPassword: true}, ACLUserStateEnabled, ACLPasswordTypeAny},
		{"resetpass", UserAclSpec{Enabled: true, ResetPass: true}, ACLUserStateEnabled, ACLPasswordTypeDisabled},
		{"resetpass wins over nopass", UserAclSpec{Enabled: true, ResetPass: true, NoPassword: true}, ACLUserStateEnabled, ACLPasswordTypeDisabled},
		{"disabled nopass", UserAclSpec{Enabled: false, NoPassword: true}, ACLUserStateDisabled, ACLPasswordTypeAny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.legacy.Name = "alice"
			tt.legacy.ACLPermissions = perms

			got := tt.legacy.ToACLUser()

			assert.Equal(t, "alice", got.Name)
			assert.Equal(t, tt.wantState, got.State)
			require.NotNil(t, got.Password)
			assert.Equal(t, tt.wantPasswordType, got.Password.Type)
			assert.Nil(t, got.Password.Secret)
			assert.Equal(t, perms, got.ACLPermissions)
		})
	}
}

func TestUserAclSpecToACLUserCopiesPasswordSecret(t *testing.T) {
	legacy := UserAclSpec{
		Name:           "david",
		Enabled:        true,
		PasswordSecret: PasswordSecretSpec{Name: "users", Keys: []string{"old", "new"}},
	}

	got := legacy.ToACLUser()

	require.NotNil(t, got.Password)
	require.NotNil(t, got.Password.Secret)
	assert.Equal(t, legacy.PasswordSecret, *got.Password.Secret)
	got.Password.Secret.Keys[0] = "changed"
	assert.Equal(t, "old", legacy.PasswordSecret.Keys[0], "conversion must not alias the legacy keys")
}

func TestUserAclSpecToACLUserDropsUnusedPasswordSecret(t *testing.T) {
	secret := PasswordSecretSpec{Name: "users", Keys: []string{"bob"}}
	for _, legacy := range []UserAclSpec{
		{Name: "bob", Enabled: true, NoPassword: true, PasswordSecret: secret},
		{Name: "bob", Enabled: true, ResetPass: true, PasswordSecret: secret},
	} {
		got := legacy.ToACLUser()

		require.NotNil(t, got.Password)
		assert.Nil(t, got.Password.Secret, "type %s", got.Password.Type)
	}
}

func TestACLUsers(t *testing.T) {
	legacy := []UserAclSpec{{Name: "bob", Enabled: true, NoPassword: true}}
	current := []ACLUserSpec{
		{Name: "zed", Password: &ACLUserPassword{Type: ACLPasswordTypeAny}},
		{Name: "amy", Password: &ACLUserPassword{Type: ACLPasswordTypeAny}},
	}

	t.Run("spec.acl.users", func(t *testing.T) {
		s := ValkeyClusterSpec{ACL: &ACLSpec{Users: current}}
		assert.Equal(t, current, s.ACLUsers())
	})

	t.Run("legacy spec.users", func(t *testing.T) {
		s := ValkeyClusterSpec{Users: legacy}
		assert.Equal(t, []ACLUserSpec{{
			Name:     "bob",
			State:    ACLUserStateEnabled,
			Password: &ACLUserPassword{Type: ACLPasswordTypeAny},
		}}, s.ACLUsers())
	})

	t.Run("neither", func(t *testing.T) {
		assert.Empty(t, ValkeyClusterSpec{}.ACLUsers())
		assert.Empty(t, ValkeyClusterSpec{ACL: &ACLSpec{}}.ACLUsers())
	})

	t.Run("empty legacy list next to spec.acl.users", func(t *testing.T) {
		s := ValkeyClusterSpec{Users: []UserAclSpec{}, ACL: &ACLSpec{Users: current}}
		assert.Equal(t, current, s.ACLUsers())
	})

	t.Run("modifying the result leaves spec.acl.users untouched", func(t *testing.T) {
		s := ValkeyClusterSpec{ACL: &ACLSpec{Users: []ACLUserSpec{{
			Name:           "amy",
			Password:       &ACLUserPassword{Secret: &PasswordSecretSpec{Keys: []string{"old"}}},
			ACLPermissions: ACLPermissions{Commands: CommandsAclSpec{Allow: []string{"@read"}}},
		}}}}
		users := s.ACLUsers()
		users[0].Password.Type = ACLPasswordTypeAny
		users[0].Password.Secret.Keys[0] = "changed"
		users[0].Commands.Allow[0] = "@all"
		assert.Empty(t, s.ACL.Users[0].Password.Type)
		assert.Equal(t, "old", s.ACL.Users[0].Password.Secret.Keys[0])
		assert.Equal(t, "@read", s.ACL.Users[0].Commands.Allow[0])
	})

	t.Run("modifying the result leaves spec.users untouched", func(t *testing.T) {
		s := ValkeyClusterSpec{Users: []UserAclSpec{{
			Name:           "bob",
			Enabled:        true,
			ACLPermissions: ACLPermissions{Commands: CommandsAclSpec{Allow: []string{"@read"}}},
		}}}
		users := s.ACLUsers()
		users[0].Commands.Allow[0] = "@all"
		assert.Equal(t, "@read", s.Users[0].Commands.Allow[0])
	})

	t.Run("sorting the result leaves the spec untouched", func(t *testing.T) {
		s := ValkeyClusterSpec{ACL: &ACLSpec{Users: slices.Clone(current)}}
		users := s.ACLUsers()
		slices.SortFunc(users, func(a, b ACLUserSpec) int { return strings.Compare(a.Name, b.Name) })
		assert.Equal(t, "amy", users[0].Name)
		assert.Equal(t, "zed", s.ACL.Users[0].Name)
	})
}

func TestACLUserSpecPasswordType(t *testing.T) {
	tests := []struct {
		name string
		user ACLUserSpec
		want ACLPasswordType
	}{
		{"no password block", ACLUserSpec{}, ACLPasswordTypeSecret},
		{"empty type", ACLUserSpec{Password: &ACLUserPassword{}}, ACLPasswordTypeSecret},
		{"any", ACLUserSpec{Password: &ACLUserPassword{Type: ACLPasswordTypeAny}}, ACLPasswordTypeAny},
		{"disabled", ACLUserSpec{Password: &ACLUserPassword{Type: ACLPasswordTypeDisabled}}, ACLPasswordTypeDisabled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.user.PasswordType())
		})
	}
}
