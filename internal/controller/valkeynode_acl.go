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

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// desiredUserPasswordHashes parses the aclfile the ValkeyCluster controller
// builds into each user's SHA-256 password hashes, keyed by username. The file
// is a sequence of "user <name> on #<hash> ... " lines (see buildUserAcl), so
// the hashes are the '#'-prefixed tokens.
//
// Hashes are sorted and deduplicated. Order is not guaranteed by either side,
// and Valkey keeps a user's passwords as a set: pointing two Secret keys at the
// same password yields a repeated hash in the file but a single one back from
// ACL GETUSER, which would otherwise never compare equal.
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
		out[fields[1]] = normalizeHashes(hashes)
	}
	return out
}

// normalizeHashes sorts and deduplicates password hashes so both sides of the
// comparison are in the same shape.
func normalizeHashes(hashes []string) []string {
	slices.Sort(hashes)
	return slices.Compact(hashes)
}

// aclObservablyInSync reports whether the ACL the cluster controller wrote is
// live on the server, by comparing the set of users and each user's password
// hashes.
//
// Permissions are not compared field by field. ACL GETUSER returns Valkey's
// normalized rendering of the rules, while the operator only holds the aclfile
// text, so comparing the two directly would mean reimplementing Valkey's own
// ACL parser and keeping it in step with the server. Instead the aclfile carries
// a revision user (see aclRevisionUser): a disabled user whose only
// password hash is the hash of the whole managed ACL. It is compared here like
// any other user, so an edit that changes only permissions still changes the
// revision hash and reads as out of sync until the running server loads the
// current revision. That closes the window where a permission-only change would
// otherwise read as applied while a stale mounted aclfile was still in effect.
func aclObservablyInSync(ctx context.Context, c valkeyConfigClient, desired map[string][]string) (bool, error) {
	actualUsers, err := c.UserNames(ctx)
	if err != nil {
		return false, err
	}
	serverUsers := slices.Sorted(slices.Values(actualUsers))
	// Valkey always keeps a `default` user of its own, and ACL LOAD cannot
	// remove it. When the aclfile does not manage `default` (it is absent from
	// spec.users), ignore the server's copy; otherwise the sets never match and
	// the node loops forever reporting the ACL as not yet live.
	if _, managed := desired["default"]; !managed {
		serverUsers = slices.DeleteFunc(serverUsers, func(u string) bool { return u == "default" })
	}
	if !slices.Equal(serverUsers, slices.Sorted(maps.Keys(desired))) {
		// A user was added or removed and the server has not picked it up yet.
		return false, nil
	}
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
// The reload is therefore unconditional: the operator cannot read the pod's copy
// of the file, so before a LOAD it has no way to know whether the mounted file
// is current. A repeated LOAD is idempotent and cheap, and it is what makes
// every kind of change converge, including permission edits and removed users,
// once the volume catches up.
//
// The returned bool reports whether the load took: aclObservablyInSync compares
// the user set, their password hashes, and the revision user whose hash
// covers the whole managed ACL. A True result therefore means the current
// revision is live, permissions included, not only that users and passwords
// match.
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
	return aclObservablyInSync(ctx, c, desired)
}
