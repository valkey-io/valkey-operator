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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDesiredUserPasswordHashes(t *testing.T) {
	acl := `user alice on #aaa #bbb ~* +@all
user bob on #ccc ~key:* +get
user nopass-user on nopass ~* +@read

user _replication on #ddd -@all +psync +replconf +ping
not-a-user-line whatever
`
	got := desiredUserPasswordHashes(acl)

	assert.Equal(t, []string{"aaa", "bbb"}, got["alice"], "both hashes, sorted")
	assert.Equal(t, []string{"ccc"}, got["bob"])
	assert.Equal(t, []string{}, got["nopass-user"], "nopass user has no hashes")
	assert.Equal(t, []string{"ddd"}, got["_replication"], "system users are parsed like any other")
	assert.NotContains(t, got, "not-a-user-line", "lines that are not user entries are ignored")
	assert.Len(t, got, 4)
}

func TestDesiredUserPasswordHashesDeduplicates(t *testing.T) {
	// Two Secret keys holding the same password produce a repeated hash in the
	// file, while Valkey reports it once. Without deduplication the comparison
	// could never converge.
	got := desiredUserPasswordHashes("user alice on #aaa #aaa #bbb ~* +@all\n")
	assert.Equal(t, []string{"aaa", "bbb"}, got["alice"])
}

func TestDesiredUserPasswordHashesEmpty(t *testing.T) {
	assert.Empty(t, desiredUserPasswordHashes(""))
	assert.Empty(t, desiredUserPasswordHashes("\n\n"))
}

func TestACLObservablyInSync(t *testing.T) {
	ctx := context.Background()
	desired := map[string][]string{"alice": {"aaa", "bbb"}, "bob": {"ccc"}}

	t.Run("server matches desired", func(t *testing.T) {
		f := &fakeConfigClient{aclHashes: map[string][]string{"alice": {"aaa", "bbb"}, "bob": {"ccc"}}}
		inSync, err := aclObservablyInSync(ctx, f, desired)
		require.NoError(t, err)
		assert.True(t, inSync)
	})

	t.Run("a rotated password the server has not loaded reads as out of sync", func(t *testing.T) {
		f := &fakeConfigClient{aclHashes: map[string][]string{"alice": {"aaa"}, "bob": {"ccc"}}}
		inSync, err := aclObservablyInSync(ctx, f, desired)
		require.NoError(t, err)
		assert.False(t, inSync)
	})

	t.Run("a user the server has not added yet reads as out of sync", func(t *testing.T) {
		f := &fakeConfigClient{aclHashes: map[string][]string{"alice": {"aaa", "bbb"}}}
		inSync, err := aclObservablyInSync(ctx, f, desired)
		require.NoError(t, err)
		assert.False(t, inSync)
	})

	t.Run("a user the server has not removed yet reads as out of sync", func(t *testing.T) {
		f := &fakeConfigClient{aclHashes: map[string][]string{
			"alice": {"aaa", "bbb"}, "bob": {"ccc"}, "removed": {"ddd"},
		}}
		inSync, err := aclObservablyInSync(ctx, f, desired)
		require.NoError(t, err)
		assert.False(t, inSync, "a user only the server still has must not read as in sync")
	})

	t.Run("a password repeated in the aclfile still matches the server's set", func(t *testing.T) {
		// Valkey stores passwords as a set, so pointing two Secret keys at the
		// same password must not leave the node permanently out of sync.
		dup := map[string][]string{"alice": normaliseHashes([]string{"aaa", "aaa"})}
		f := &fakeConfigClient{aclHashes: map[string][]string{"alice": {"aaa"}}}
		inSync, err := aclObservablyInSync(ctx, f, dup)
		require.NoError(t, err)
		assert.True(t, inSync)
	})

	t.Run("client errors surface", func(t *testing.T) {
		f := &fakeConfigClient{aclErr: assert.AnError}
		_, err := aclObservablyInSync(ctx, f, desired)
		require.Error(t, err)
	})
}
