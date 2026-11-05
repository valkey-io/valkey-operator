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

// An UserAclSpec contains user, authorization, and permissions-related configurations
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

	// Valkey command categories, commands, and subcommands restrictions for this user
	// +optional
	Commands CommandsAclSpec `json:"commands,omitempty"`

	// Key restrictions
	// +optional
	Keys KeysAclSpec `json:"keys,omitempty"`

	// Channel restrictions
	// +optional
	Channels ChannelsAclSpec `json:"channels,omitempty"`

	// Database restrictions. Requires Valkey 9.1+; on older images the
	// directives are dropped from the rendered ACL and a ConfigurationWarning
	// condition is set on the cluster.
	// +optional
	Databases DatabasesAclSpec `json:"databases,omitempty"`

	// Raw ACL for (additional) permissions. Appended to anything generated.
	// +optional
	RawAcl string `json:"permissions,omitempty"`
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

// DatabasesAclSpec scopes a user to specific logical databases.
//
// Note that this operator always runs Valkey in cluster mode, where the number
// of databases is governed by the cluster-databases directive and defaults to
// 1. On a default cluster db 0 is the only database that exists, so ids: [0]
// restricts nothing, and any selection that omits db 0 denies the user
// everything. Raise cluster-databases through spec.config before relying on
// this field.
//
// ids is guarded with has() in every rule below because it is optional: in CRD
// CEL, referencing an absent optional field errors and fails validation for the
// whole object. allDbs and resetDbs are always present after defaulting, so
// they need no guard.
// +kubebuilder:validation:XValidation:message="databases.ids and databases.allDbs are mutually exclusive",rule="!(has(self.ids) && self.allDbs)"
// +kubebuilder:validation:XValidation:message="databases.ids and databases.resetDbs are mutually exclusive",rule="!(has(self.ids) && self.resetDbs)"
// +kubebuilder:validation:XValidation:message="databases.allDbs and databases.resetDbs are mutually exclusive",rule="!(self.allDbs && self.resetDbs)"
type DatabasesAclSpec struct {

	// Specific database IDs this user is allowed to access; maps to Valkey: db=0,1,2
	// Selecting ids clears Valkey's default alldbs grant on its own, so no
	// separate reset is needed. At least one id is required: Valkey rejects an
	// id-less db= as a syntax error.
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:Minimum=0
	IDs []int32 `json:"ids,omitempty"`

	// Allow this user to access all databases; maps to Valkey: alldbs
	// This is already Valkey's default for every user, so setting it changes
	// nothing on the server: the resulting user is indistinguishable from one
	// that omits this block entirely. It is accepted as a way to state the
	// intent explicitly.
	// Whether alldbs is echoed back by ACL LIST depends on the server version
	// (9.1.0 prints it for every user; later 9.1.x omit it as the default), so
	// do not rely on its presence or absence either way.
	// +kubebuilder:default=false
	AllDbs bool `json:"allDbs,omitempty"`

	// Clear the allowed databases and the alldbs flag; maps to Valkey: resetdbs
	// The user cannot access any database until ids or allDbs is set.
	// +kubebuilder:default=false
	ResetDbs bool `json:"resetDbs,omitempty"`
}
