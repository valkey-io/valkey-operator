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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

func TestBuildAclFileContents(t *testing.T) {

	ctx := context.TODO()

	// Simulate a Secret
	testSecrets := &corev1.Secret{
		Data: map[string][]byte{
			"charlie":  []byte("charliepassword"),
			"davidref": []byte("c90502e005ee957f29645e21d1e27f5bbfce539e38c949a00dfc12270f47fc59"),
		},
	}

	// alice should succeed as there is permissions, and valid password
	expected := "user alice on #b6366487efc982bfe450c46753917ee883f71a3f4fa5cdc5bde78d1784842e73 +@list +@connection ~jobs:*"
	acl := buildAclFileContents(ctx, map[string]valkeyiov1alpha1.UserAcl{
		"alice": valkeyiov1alpha1.UserAcl{
			Permissions: "+@list +@connection ~jobs:*",
			Password:    "thisisagoodpassword",
		}}, testSecrets)
	if strings.TrimRight(string(acl), "\n") != expected {
		t.Errorf("alice ACL Failed. Expected %s; got %s", expected, acl)
	}

	// bob fails to be added because there is no matching password ref in Secret
	expected = ""
	acl = buildAclFileContents(ctx, map[string]valkeyiov1alpha1.UserAcl{
		"bob": valkeyiov1alpha1.UserAcl{
			Permissions:    "-@all",
			PasswordKeyRef: "bobkeyref",
		}}, testSecrets)
	if strings.TrimRight(string(acl), "\n") != expected {
		t.Errorf("bob ACL Failed. Expected %s; got %s", expected, acl)
	}

	// charlie fails to be added because there is no password, and no password ref
	expected = ""
	acl = buildAclFileContents(ctx, map[string]valkeyiov1alpha1.UserAcl{
		"charlie": valkeyiov1alpha1.UserAcl{
			Permissions: "+@list +@connection ~jobs:*",
		}}, testSecrets)
	if strings.TrimRight(string(acl), "\n") != expected {
		t.Errorf("charlie ACL Failed. Expected %s; got %s", expected, acl)
	}

	// david should succeed as there is permissions, and valid key ref
	expected = "user david on #c90502e005ee957f29645e21d1e27f5bbfce539e38c949a00dfc12270f47fc59 +@all"
	acl = buildAclFileContents(ctx, map[string]valkeyiov1alpha1.UserAcl{
		"david": valkeyiov1alpha1.UserAcl{
			Permissions:    "+@all",
			PasswordKeyRef: "davidref",
		}}, testSecrets)
	if strings.TrimRight(string(acl), "\n") != expected {
		t.Errorf("david ACL Failed. Expected %s; got %s", expected, acl)
	}

	// edward fails to be added because there are no permissions
	expected = ""
	acl = buildAclFileContents(ctx, map[string]valkeyiov1alpha1.UserAcl{
		"edward": valkeyiov1alpha1.UserAcl{
			Password: "edwardpass",
		}}, testSecrets)
	if strings.TrimRight(string(acl), "\n") != expected {
		t.Errorf("edward ACL Failed. Expected %s; got %s", expected, acl)
	}
}
