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

// aclInSync reports whether every desired user's password hashes match what the
// server currently has. It is the authoritative check: the operator cannot read
// the pod's mounted aclfile without an exec, so the running server is the only
// place to observe what was actually loaded.
func aclInSync(ctx context.Context, c valkeyConfigClient, desired map[string][]string) (bool, error) {
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

// applyLiveACL brings the running server's ACL in line with the aclfile Secret
// without rolling the pod, and reports whether it is in sync.
//
// The aclfile is mounted read-only from the Secret, so ACL SAVE is neither
// available nor needed: the Secret the cluster controller writes is what a
// restarting pod loads. That leaves ACL LOAD as the live path, with one catch.
// The projected volume is refreshed lazily by kubelet, so a LOAD fired right
// after the Secret changes can read the pre-update file and silently no-op.
// Verifying against the server rather than trusting the LOAD makes that
// self-correcting: a premature LOAD leaves the node out of sync, and the
// requeue tries again once the volume has caught up.
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

	inSync, err := aclInSync(ctx, c, desired)
	if err != nil || inSync {
		return inSync, err
	}

	if err := c.LoadACL(ctx); err != nil {
		return false, err
	}
	return aclInSync(ctx, c, desired)
}
