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

// ACLSpec holds the cluster's ACL configuration.
type ACLSpec struct {
	// Users managed by the operator
	// +listType=map
	// +listMapKey=name
	// +optional
	Users []ACLUserSpec `json:"users,omitempty"`
}

// ACLUserState selects whether a user can authenticate.
// +kubebuilder:validation:Enum=Enabled;Disabled
type ACLUserState string

const (
	// ACLUserStateEnabled renders the user as on.
	ACLUserStateEnabled ACLUserState = "Enabled"
	// ACLUserStateDisabled renders the user as off; existing connections stay authenticated.
	ACLUserStateDisabled ACLUserState = "Disabled"
)

// ACLPasswordType selects how a user authenticates with a password.
// +kubebuilder:validation:Enum=Secret;Any;Disabled
type ACLPasswordType string

const (
	// ACLPasswordTypeSecret accepts the passwords referenced by secret.
	ACLPasswordTypeSecret ACLPasswordType = "Secret"
	// ACLPasswordTypeAny accepts any password (Valkey nopass).
	ACLPasswordTypeAny ACLPasswordType = "Any"
	// ACLPasswordTypeDisabled accepts no password (Valkey resetpass). The user can still
	// authenticate with a TLS client certificate (tls-auth-clients-user).
	ACLPasswordTypeDisabled ACLPasswordType = "Disabled"
)

// ACLUserPassword configures password authentication for a user.
// +kubebuilder:validation:XValidation:message="secret is only used when type is Secret",rule="!has(self.secret) || self.type == 'Secret'"
type ACLUserPassword struct {
	// How the user authenticates with a password
	// +kubebuilder:default=Secret
	// +optional
	Type ACLPasswordType `json:"type,omitempty"`

	// Reference information to a Secret containing user passwords
	// +optional
	Secret *PasswordSecretSpec `json:"secret,omitempty"`
}

// ACLPermissions holds the command, key and channel rules of an ACL entry.
type ACLPermissions struct {
	// Valkey command categories, commands, and subcommands restrictions for this user
	// +optional
	Commands CommandsAclSpec `json:"commands,omitempty"`

	// Key restrictions
	// +optional
	Keys KeysAclSpec `json:"keys,omitempty"`

	// Channel restrictions
	// +optional
	Channels ChannelsAclSpec `json:"channels,omitempty"`

	// Raw ACL for (additional) permissions. Appended to anything generated.
	// +optional
	RawAcl string `json:"permissions,omitempty"`
}

// ACLUserSpec defines a Valkey ACL user.
type ACLUserSpec struct {
	// Username
	// +kubebuilder:required:message=A username is required
	// +kubebuilder:validation:XValidation:message="Username cannot start with '_'",rule="!self.startsWith('_')"
	Name string `json:"name"`

	// Whether the user can authenticate
	// +kubebuilder:default=Enabled
	// +optional
	State ACLUserState `json:"state,omitempty"`

	// Password authentication; defaults to the passwords in the Secret <cluster>-users
	// +kubebuilder:default={}
	// +optional
	Password *ACLUserPassword `json:"password,omitempty"`

	ACLPermissions `json:",inline"`
}

// PasswordType returns the password type, treating an unset one as Secret.
func (u ACLUserSpec) PasswordType() ACLPasswordType {
	if u.Password == nil || u.Password.Type == "" {
		return ACLPasswordTypeSecret
	}
	return u.Password.Type
}

// An UserAclSpec contains user, authorization, and permissions-related configurations.
// It is the legacy spec.users entry, replaced by ACLUserSpec and removed in v1beta1.
type UserAclSpec struct {

	// Username
	// +kubebuilder:required:message=A username is required
	// +kubebuilder:validation:XValidation:message="Username cannot start with '_'",rule="!self.startsWith('_')"
	Name string `json:"name"`

	// If the user is enabled or not
	// +kubebuilder:default=true
	Enabled bool `json:"enabled,omitempty"`

	// Reference information to a Secret containing user passwords
	// +optional
	PasswordSecret PasswordSecretSpec `json:"passwordSecret,omitempty"`

	// Do not apply a password to this user
	// +kubebuilder:default=false
	NoPassword bool `json:"nopass,omitempty"`

	// Apply the resetpass flag to this user
	// +kubebuilder:default=false
	ResetPass bool `json:"resetpass,omitempty"`

	ACLPermissions `json:",inline"`
}

// ToACLUser converts a legacy user. resetpass takes precedence over nopass,
// and passwordSecret is kept only for the Secret type.
func (u UserAclSpec) ToACLUser() ACLUserSpec {
	out := ACLUserSpec{
		Name:           u.Name,
		State:          ACLUserStateEnabled,
		Password:       &ACLUserPassword{Type: ACLPasswordTypeSecret},
		ACLPermissions: u.ACLPermissions,
	}
	if !u.Enabled {
		out.State = ACLUserStateDisabled
	}
	switch {
	case u.ResetPass:
		out.Password.Type = ACLPasswordTypeDisabled
	case u.NoPassword:
		out.Password.Type = ACLPasswordTypeAny
	}
	if out.Password.Type == ACLPasswordTypeSecret && (u.PasswordSecret.Name != "" || len(u.PasswordSecret.Keys) > 0) {
		out.Password.Secret = u.PasswordSecret.DeepCopy()
	}
	return out
}

type PasswordSecretSpec struct {

	// Name of the referencing Secret; Defaults to clustername-users
	// +optional
	Name string `json:"name,omitempty"`

	// An array of keys inside the referencing Secret to find passwords; defaults to username
	// Valkey supports multiple passwords per user for rotation
	// +optional
	Keys []string `json:"keys,omitempty"`
}

type CommandsAclSpec struct {

	// Command categories (@all, @read, @write, @admin, etc.)
	// Individual commands (get, set, ping, etc.)
	// Module commands (json.set, bf.add, etc.)
	// Subcommands (client|setname, config|get, etc.)

	// Allowed commands for this user
	// +kubebuilder:validation:items:Pattern=`^(@[A-Za-z]+|[A-Za-z0-9_]+(-[A-Za-z0-9_]+)*(\.[A-Za-z0-9_]+(-[A-Za-z0-9_]+)*)*(\|[A-Za-z0-9_]+(-[A-Za-z0-9_]+)*(\.[A-Za-z0-9_]+(-[A-Za-z0-9_]+)*)*)?)$`
	Allow []string `json:"allow,omitempty"`

	// Denied commands for this user
	// +kubebuilder:validation:items:Pattern=`^(@[A-Za-z]+|[A-Za-z0-9_]+(-[A-Za-z0-9_]+)*(\.[A-Za-z0-9_]+(-[A-Za-z0-9_]+)*)*(\|[A-Za-z0-9_]+(-[A-Za-z0-9_]+)*(\.[A-Za-z0-9_]+(-[A-Za-z0-9_]+)*)*)?)$`
	Deny []string `json:"deny,omitempty"`
}

type KeysAclSpec struct {

	// Keys on which this user can read, and write; maps to Valkey: ~pattern
	// +optional
	ReadWrite []string `json:"readWrite,omitempty"`

	// Keys restricted to read-only; maps to Valkey: %R~pattern
	// +optional
	ReadOnly []string `json:"readOnly,omitempty"`

	// Keys restricted to write-only; maps to Valkey: %W~pattern
	// +optional
	WriteOnly []string `json:"writeOnly,omitempty"`
}

type ChannelsAclSpec struct {

	// Pub/Sub channel patterns - maps to Valkey: &pattern
	// +optional
	Patterns []string `json:"patterns,omitempty"`
}
