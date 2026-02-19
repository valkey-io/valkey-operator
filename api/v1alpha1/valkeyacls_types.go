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
	// Subcommands (client|setname, config|get, etc.)

	// Allowed commands for this user
	// +kubebuilder:validation:Items:Pattern=^[@a-z|]+$}
	Allow []string `json:"allow,omitempty"`

	// Denied commands for this user
	// +kubebuilder:validation:Items:Pattern=^[@a-z|]+$}
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
