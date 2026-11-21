/*
Copyright 2026 Valkey Contributors.

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
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	valkeyv1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func digestFixture(t *testing.T, image string) (*ValkeyNodeReconciler, *valkeyv1.ValkeyNode, *fakeConfigClient) {
	t.Helper()
	node := &valkeyv1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "ns", UID: types.UID("node-uid")},
		Spec:       valkeyv1.ValkeyNodeSpec{Image: image, UsersACLSecretName: "acl"},
		Status:     valkeyv1.ValkeyNodeStatus{AppliedACLRevision: "rev1"},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "acl", Namespace: "ns"}, Data: map[string][]byte{
		aclFilename: []byte("user default off\nuser " + aclRevisionUser + " off #rev1\n"),
	}}
	base, _, _ := newTrustReconciler(t)
	c := fake.NewClientBuilder().WithScheme(base.Scheme).WithStatusSubresource(&valkeyv1.ValkeyNode{}).WithObjects(node, secret).Build()
	f := &fakeConfigClient{aclHashes: map[string][]string{"default": {}, aclRevisionUser: {"rev1"}}, digest: strings.Repeat("a", 64)}
	r := &ValkeyNodeReconciler{Client: c, APIReader: c, Recorder: base.Recorder,
		newConfigClient: func(context.Context, *ValkeyNodeReconciler, *valkeyv1.ValkeyNode) (valkeyConfigClient, error) {
			return f, nil
		},
	}
	return r, node, f
}

func TestACLDigestFastPath(t *testing.T) {
	ctx := context.Background()
	r, node, f := digestFixture(t, "valkey/valkey:9.2.0")
	check := func() {
		t.Helper()
		ok, rev, err := r.applyLiveACL(ctx, node)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, "rev1", rev)
	}
	check()
	require.Equal(t, 1, f.aclLoads)
	require.Equal(t, 1, f.aclChecks)
	check()
	assert.Equal(t, 1, f.aclLoads, "unchanged ACL skips LOAD")
	assert.Equal(t, 1, f.aclChecks, "unchanged ACL skips the per-user walk")
	assert.Equal(t, 3, f.digestCalls, "two reads bracket the baseline check, one checks it")

	f.digest = strings.Repeat("b", 64) // a manual permission edit leaves the revision user's hash unchanged
	check()
	assert.Equal(t, 2, f.aclLoads, "server drift must reload even when the revision user matches")
	check()
	assert.Equal(t, 2, f.aclLoads)

	node.UID = "replacement"
	check()
	assert.Equal(t, 3, f.aclLoads, "a replacement object does not reuse the old baseline")
	r.aclDigests.Clear() // controller process restart
	check()
	assert.Equal(t, 4, f.aclLoads)
	f.digestErr = errors.New("NOPERM")
	check()
	assert.Equal(t, 5, f.aclLoads, "a failed digest read invalidates the cached shortcut")
}

func TestACLDigestExpiresEvenWhenDigestMatches(t *testing.T) {
	ctx := context.Background()
	r, node, f := digestFixture(t, "valkey/valkey:9.2.0")
	// The fake reports the same digest at both endpoints, as it would if
	// an external permission edit restored the pre-load ACL during checking.
	ok, _, err := r.applyLiveACL(ctx, node)
	require.NoError(t, err)
	require.True(t, ok)
	key := client.ObjectKeyFromObject(node)
	entry, found := r.aclDigests.Load(key)
	require.True(t, found)
	baseline := entry.(checkedACLDigest)
	baseline.checkedAt = time.Now().Add(-aclDigestMaxAge)
	r.aclDigests.Store(key, baseline)

	ok, _, err = r.applyLiveACL(ctx, node)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 2, f.aclLoads, "expiry forces a reload even if digest and revision still match")
	assert.Equal(t, 2, f.aclChecks)
}

func TestACLDigestFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name, image, digest string
		err                 error
		calls               bool
	}{
		{"8.0", "valkey/valkey:8.0.12", "", nil, false},
		{"8.1", "valkey/valkey:8.1.11", "", nil, false},
		{"9.0", "valkey/valkey:9.0.0", "", nil, false},
		{"release candidate", "valkey/valkey:9.2.0-rc2", "", nil, false},
		{"unknown image", "custom:unknown", "", nil, false},
		{"no permission", "valkey/valkey:9.2.0", "", errors.New("NOPERM"), true},
		{"unknown command", "valkey/valkey:9.2.0", "", errors.New("unknown subcommand"), true},
		{"read failure", "valkey/valkey:9.2.0", "", errors.New("connection closed"), true},
		{"bad digest", "valkey/valkey:9.2.0", strings.Repeat("z", 64), nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, node, f := digestFixture(t, tc.image)
			f.digest, f.digestErr = tc.digest, tc.err
			for range 2 {
				ok, _, err := r.applyLiveACL(context.Background(), node)
				require.NoError(t, err)
				require.True(t, ok)
			}
			assert.Equal(t, 2, f.aclLoads)
			assert.Equal(t, 2, f.aclChecks)
			assert.Equal(t, tc.calls, f.digestCalls > 0)
			_, cached := r.aclDigests.Load(client.ObjectKeyFromObject(node))
			assert.False(t, cached)
			grant := strings.Contains(systemUserAcl(operatorUser, tc.image), "+acl|digest")
			assert.Equal(t, supportsACLDigest(tc.image), grant)
		})
	}
}

func TestACLDigestDoesNotTrustAnUnstableCheck(t *testing.T) {
	r, node, f := digestFixture(t, "valkey/valkey:9.2.0")
	f.digests = []string{strings.Repeat("a", 64), strings.Repeat("b", 64)}
	ok, _, err := r.applyLiveACL(context.Background(), node)
	require.NoError(t, err)
	assert.True(t, ok, "the full check succeeds, but the changed digest must not seed a shortcut")
	_, cached := r.aclDigests.Load(client.ObjectKeyFromObject(node))
	assert.False(t, cached)
	ok, _, err = r.applyLiveACL(context.Background(), node)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, 2, f.aclLoads)
}

func TestACLDigestWaitsForDesiredRevision(t *testing.T) {
	ctx := context.Background()
	r, node, f := digestFixture(t, "valkey/valkey:9.2.0")
	ok, _, err := r.applyLiveACL(ctx, node)
	require.NoError(t, err)
	require.True(t, ok)
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: "ns", Name: "acl"}
	require.NoError(t, r.Get(ctx, key, secret))
	secret.Data[aclFilename] = []byte("user default off\nuser " + aclRevisionUser + " off #rev2\n")
	require.NoError(t, r.Update(ctx, secret))
	ok, _, err = r.applyLiveACL(ctx, node)
	require.NoError(t, err)
	assert.False(t, ok, "an unchanged digest must not bless a stale mounted file")
	f.aclOnLoad = map[string][]string{"default": {}, aclRevisionUser: {"rev2"}}
	ok, rev, err := r.applyLiveACL(ctx, node)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "rev2", rev)
	node.Status.AppliedACLRevision = "rev2"
	loads := f.aclLoads
	ok, _, err = r.applyLiveACL(ctx, node)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, loads, f.aclLoads)
}

func TestACLDigestRequiresARevisionMarker(t *testing.T) {
	r, node, f := digestFixture(t, "valkey/valkey:9.2.0")
	ctx := context.Background()
	secret := &corev1.Secret{}
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "acl"}, secret))
	secret.Data[aclFilename] = []byte("user default off\n")
	require.NoError(t, r.Update(ctx, secret))
	f.aclHashes = map[string][]string{"default": {}}
	for range 2 {
		ok, _, err := r.applyLiveACL(ctx, node)
		require.NoError(t, err)
		require.True(t, ok)
	}
	assert.Equal(t, 2, f.aclLoads)
	assert.Zero(t, f.digestCalls, "legacy files without a revision marker never seed a baseline")
}

func TestACLDigestRetriesPendingDefaultSessions(t *testing.T) {
	ctx := context.Background()
	r, node, f := digestFixture(t, "valkey/valkey:9.2.0")
	ok, _, err := r.applyLiveACL(ctx, node)
	require.NoError(t, err)
	require.True(t, ok)
	node.Status.Conditions = []metav1.Condition{{Type: valkeyv1.ValkeyNodeConditionDefaultUserSessionsOpen, Status: metav1.ConditionTrue}}
	require.NoError(t, r.Status().Update(ctx, node))
	f.killErr = errors.New("kill failed")
	ok, _, err = r.applyLiveACL(ctx, node)
	require.ErrorContains(t, err, "kill failed")
	assert.False(t, ok)
	f.killErr = nil
	ok, _, err = r.applyLiveACL(ctx, node)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, []string{defaultUser}, f.killed)
}
