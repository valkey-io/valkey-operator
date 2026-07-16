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
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

// desiredUserPasswordHashes parses the aclfile the ValkeyCluster controller
// builds into each user's SHA-256 password hashes, keyed by username. The file
// is a sequence of "user <name> on #<hash> ... " lines (see buildUserAcl), so
// the hashes are the '#'-prefixed tokens. Hashes are sorted, since neither the
// file nor ACL GETUSER guarantees an order.
func desiredUserPasswordHashes(aclFile string) map[string][]string {
	out := map[string][]string{}
	for line := range strings.SplitSeq(aclFile, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "user" {
			continue
		}
		hashes := []string{}
		for _, f := range fields[2:] {
			if h, ok := strings.CutPrefix(f, "#"); ok {
				hashes = append(hashes, h)
			}
		}
		slices.Sort(hashes)
		out[fields[1]] = hashes
	}
	return out
}

// passwordsInSync reports whether every desired user's password hashes are live
// on the server.
//
// This is deliberately narrower than "the whole ACL matches". ACL GETUSER
// returns Valkey's normalised rendering of the rules, while the operator only
// holds the aclfile text, and normalising the two into a comparable form would
// mean reimplementing Valkey's own ACL parser. Password hashes are the part
// that does compare exactly, and they are what a rotation needs to observe, so
// they drive the ACLApplied condition. Correctness of the apply itself does not
// depend on this check: the reload is unconditional.
func passwordsInSync(ctx context.Context, c valkeyConfigClient, desired map[string][]string) (bool, error) {
	for _, user := range slices.Sorted(maps.Keys(desired)) {
		actual, err := c.UserPasswordHashes(ctx, user)
		if err != nil {
			return false, err
		}
		if !slices.Equal(actual, desired[user]) {
			return false, nil
		}
	}
	return true, nil
}

// applyLiveACL reloads the mounted aclfile into the running server so ACL
// changes take effect without a pod roll, and reports whether the desired
// passwords are live yet.
//
// The aclfile is mounted read-only from the Secret, so ACL SAVE is neither
// available nor needed: the Secret the cluster controller writes is what a
// restarting pod loads. That leaves ACL LOAD as the live path, with one catch.
// The projected volume is refreshed lazily by kubelet, so a LOAD fired right
// after the Secret changes can read the pre-update file and silently no-op.
//
// The reload is therefore unconditional. The operator cannot read the pod's
// copy of the file, and it cannot compare the whole ACL against the server
// either (ACL GETUSER renders Valkey's normalised form, the operator holds
// aclfile text), so there is no reliable way to know the file is current. A
// repeated LOAD is idempotent and cheap, and it is what makes every kind of
// change converge, including permission edits and removed users, once the
// volume catches up.
//
// The returned bool only reports whether the desired password hashes are live,
// which is the part that can be compared exactly and the part a rotation needs
// to wait on.
func (r *ValkeyNodeReconciler) applyLiveACL(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) (bool, error) {
	if node.Spec.UsersACLSecretName == "" {
		return true, nil
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: node.Spec.UsersACLSecretName, Namespace: node.Namespace}
	if err := r.APIReader.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			// The cluster controller owns this Secret and has not created it
			// yet. There is nothing to apply until it exists.
			return true, nil
		}
		return false, fmt.Errorf("get ACL secret %s: %w", key.Name, err)
	}
	desired := desiredUserPasswordHashes(string(secret.Data[aclFilename]))
	if len(desired) == 0 {
		return true, nil
	}

	c, err := r.newConfigClient(ctx, r, node)
	if err != nil {
		return false, err
	}
	defer c.Close()

	if err := c.LoadACL(ctx); err != nil {
		return false, err
	}
	return passwordsInSync(ctx, c, desired)
}
