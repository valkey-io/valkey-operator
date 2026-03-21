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
	"strings"
	"testing"

	//	corev1 "k8s.io/api/core/v1"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

func TestBuildAclFileContents(t *testing.T) {

	passwords := map[string][]string{
		"alice":   {"a71153805265764af6f55b4e0ce38858cde64e6e24b9a9b14e32262760572137"},
		"bob":     {},
		"charlie": {},
		"david": {
			"7447bd019c69af5975c54072b40f9c24d1105836cbd68408d6df7be76ac42ab1",
			"4b31cf3c1347d94fe80efb0c848579c5730d63efef2f5eaf32f78a7ca251833b",
		},
	}

	expecteds := map[string]string{
		"alice":   "user alice on #a71153805265764af6f55b4e0ce38858cde64e6e24b9a9b14e32262760572137 ~app:* ~cache:* %R~shared:* %R~config:* %W~logs:* %W~metrics:* resetchannels &notifications:* &events:* +@read +@write +@connection -@admin -@dangerous +client|setname +debug",
		"bob":     "user bob on nopass +@all -@admin ~* &*",
		"charlie": "user charlie off nopass +@admin",
		"david":   "user david on #7447bd019c69af5975c54072b40f9c24d1105836cbd68408d6df7be76ac42ab1 #4b31cf3c1347d94fe80efb0c848579c5730d63efef2f5eaf32f78a7ca251833b +@admin",
	}

	// UsersAcl
	users := []valkeyiov1alpha1.UserAclSpec{
		{
			Name:    "alice",
			Enabled: true,
			PasswordSecret: valkeyiov1alpha1.PasswordSecretSpec{
				Name: "alice-secret",
				Keys: []string{"alice"},
			},
			NoPassword: false,
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
		{
			Name:       "bob",
			Enabled:    true,
			NoPassword: true,
			RawAcl:     "+@all -@admin ~* &*",
		},
		{
			Name:       "charlie",
			Enabled:    false,
			NoPassword: true,
			Commands: valkeyiov1alpha1.CommandsAclSpec{
				Allow: []string{"@admin"},
			},
		},
		{
			Name:    "david",
			Enabled: true,
			PasswordSecret: valkeyiov1alpha1.PasswordSecretSpec{
				Keys: []string{"davidold", "davidnew"},
			},
			Commands: valkeyiov1alpha1.CommandsAclSpec{
				Allow: []string{"@admin"},
			},
		},
	}

	// iterate over the users
	for _, user := range users {

		userName := user.Name
		acl := buildUserAcl(user, passwords[userName])
		expected := expecteds[userName]

		if strings.TrimSpace(acl) != expected {
			t.Errorf("%s ACL Failed. Expected '%s'; got '%s'", userName, expected, acl)
		}
	}
}
