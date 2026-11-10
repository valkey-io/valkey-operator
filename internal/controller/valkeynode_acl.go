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
	"sigs.k8s.io/controller-runtime/pkg/client"

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
	// remove it. The cluster controller always writes `default` now, but an
	// aclfile written by an older operator may not; then ignore the server's
	// copy, otherwise the sets never match and the node loops forever reporting
	// the ACL as not yet live.
	if _, managed := desired[defaultUser]; !managed {
		serverUsers = slices.DeleteFunc(serverUsers, func(u string) bool { return u == defaultUser })
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
// match. With it comes that revision, which the node publishes so the cluster
// controller knows which ACL every node is running.
func (r *ValkeyNodeReconciler) applyLiveACL(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) (bool, string, error) {
	if node.Spec.UsersACLSecretName == "" {
		return true, "", nil
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: node.Spec.UsersACLSecretName, Namespace: node.Namespace}
	if err := r.APIReader.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			// The cluster controller owns this Secret and has not created it
			// yet. There is nothing to apply until it exists.
			return true, "", nil
		}
		return false, "", fmt.Errorf("get ACL secret %s: %w", key.Name, err)
	}
	desired := desiredUserPasswordHashes(string(secret.Data[aclFilename]))
	if len(desired) == 0 {
		return true, "", nil
	}

	c, err := r.newConfigClient(ctx, r, node)
	if err != nil {
		return false, "", err
	}

	// Record before the load that connections may be logged in as default,
	// so that closing them survives a failed kill or an operator restart.
	pending, err := r.defaultUserSessionsPending(ctx, node)
	if err != nil {
		return false, "", err
	}
	if !pending {
		enabled, err := c.UserEnabled(ctx, defaultUser)
		if err != nil {
			return false, "", err
		}
		if enabled {
			if err := r.setDefaultUserSessionsPending(ctx, node, true); err != nil {
				return false, "", err
			}
			pending = true
		}
	}
	if err := c.LoadACL(ctx); err != nil {
		return false, "", err
	}
	if pending {
		if err := r.closeDisabledDefaultSessions(ctx, node, c); err != nil {
			return false, "", err
		}
	}
	synced, err := aclObservablyInSync(ctx, c, desired)
	if err != nil || !synced {
		return false, "", err
	}
	return true, aclRevision(desired), nil
}

// closeDisabledDefaultSessions closes the connections still logged in as
// default once it is off, and then clears DefaultUserSessionsPending.
//
// ACL LOAD copies the new default rules into the existing user, and a
// connection that logged in while default was on keeps its authenticated flag,
// so it goes on running commands with the new rules. The disabled default the
// cluster controller writes keeps every permission (see disabledDefaultUser),
// so without this such a connection keeps full access until it disconnects.
//
// It runs only while DefaultUserSessionsPending is set, not whenever default
// is off. Valkey reports clients that have connected but not yet sent AUTH as
// default too, and exposes no flag that tells the two apart, so killing on
// every reconcile would keep dropping them. The status field is what makes a
// failed kill, or an operator restart between the load and the kill, retry on
// the next reconcile. If the pod restarts meanwhile, the retry runs once on a
// fresh server, where it can only drop clients that have not authenticated.
func (r *ValkeyNodeReconciler) closeDisabledDefaultSessions(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode, c valkeyConfigClient) error {
	enabled, err := c.UserEnabled(ctx, defaultUser)
	if err != nil || enabled {
		return err
	}
	if err := c.KillUserClients(ctx, defaultUser); err != nil {
		r.Recorder.Eventf(node, nil, corev1.EventTypeWarning, "DefaultUserSessionsCloseFailed", "CloseDefaultUserSessions",
			"default is disabled but closing the connections still logged in as it failed, will retry: %v", err)
		return err
	}
	return r.setDefaultUserSessionsPending(ctx, node, false)
}

// defaultUserSessionsPending reads DefaultUserSessionsPending from the API
// server rather than the cache, so a value this reconciler just cleared is not
// read back stale and does not trigger a second kill.
func (r *ValkeyNodeReconciler) defaultUserSessionsPending(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) (bool, error) {
	current := &valkeyiov1alpha1.ValkeyNode{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(node), current); err != nil {
		return false, fmt.Errorf("get ValkeyNode: %w", err)
	}
	return current.Status.DefaultUserSessionsPending, nil
}

// setDefaultUserSessionsPending records in status whether connections may
// still be logged in as default.
func (r *ValkeyNodeReconciler) setDefaultUserSessionsPending(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode, pending bool) error {
	current := &valkeyiov1alpha1.ValkeyNode{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(node), current); err != nil {
		return fmt.Errorf("get ValkeyNode: %w", err)
	}
	if current.Status.DefaultUserSessionsPending == pending {
		return nil
	}
	patchBase := current.DeepCopy()
	current.Status.DefaultUserSessionsPending = pending
	if err := r.Status().Patch(ctx, current, client.MergeFrom(patchBase)); err != nil {
		return fmt.Errorf("patch defaultUserSessionsPending: %w", err)
	}
	return nil
}

// setAppliedACLRevision records in status the aclfile revision the running
// server was confirmed to hold.
func (r *ValkeyNodeReconciler) setAppliedACLRevision(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode, revision string) error {
	current := &valkeyiov1alpha1.ValkeyNode{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(node), current); err != nil {
		return fmt.Errorf("get ValkeyNode: %w", err)
	}
	if current.Status.AppliedACLRevision == revision {
		return nil
	}
	patchBase := current.DeepCopy()
	current.Status.AppliedACLRevision = revision
	if err := r.Status().Patch(ctx, current, client.MergeFrom(patchBase)); err != nil {
		return fmt.Errorf("patch appliedACLRevision: %w", err)
	}
	return nil
}

// aclRevision is the revision of a managed aclfile: the hash carried by its
// revision user, which covers every other entry. It is "" for an aclfile
// without one.
func aclRevision(desired map[string][]string) string {
	if hashes := desired[aclRevisionUser]; len(hashes) == 1 {
		return hashes[0]
	}
	return ""
}
