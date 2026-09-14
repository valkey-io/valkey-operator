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

package valkey

import (
	"testing"

	semver "github.com/Masterminds/semver/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVersionFromImage(t *testing.T) {
	tests := []struct {
		name   string
		image  string
		want   string
		wantOK bool
	}{
		{name: "plain semver tag", image: "valkey/valkey:9.1.0", want: "9.1.0", wantOK: true},
		{name: "major.minor only", image: "valkey/valkey:9.1", want: "9.1.0", wantOK: true},
		{name: "distro suffix bookworm", image: "valkey/valkey:9.1.0-bookworm", want: "9.1.0", wantOK: true},
		{name: "alpine suffix", image: "valkey/valkey:9.1.2-alpine", want: "9.1.2", wantOK: true},
		{name: "prerelease tag", image: "valkey/valkey:9.1.0-rc1", want: "9.1.0-rc1", wantOK: true},
		{name: "rc2 tag", image: "valkey/valkey:9.1.0-rc2", want: "9.1.0-rc2", wantOK: true},
		{name: "v-prefixed tag", image: "valkey/valkey:v9.1.0", want: "9.1.0", wantOK: true},
		{name: "tag plus digest", image: "valkey/valkey:9.1.1@sha256:70739f85ad2ee01a726a965584a0f94895f01b0c60b3cc8b0aeef11eaa6888cf", want: "9.1.1", wantOK: true},
		{name: "registry with port", image: "myregistry:5000/valkey/valkey:9.1.0", want: "9.1.0", wantOK: true},
		{name: "floating latest tag", image: "valkey/valkey:latest", wantOK: false},
		{name: "no tag", image: "valkey/valkey", wantOK: false},
		{name: "pinned by digest", image: "valkey/valkey@sha256:abcd1234", wantOK: false},
		{name: "empty image", image: "", wantOK: false},
		{name: "non-numeric tag", image: "valkey/valkey:unstable", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := VersionFromImage(tt.image)
			require.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.want, got.String())
			} else {
				assert.Nil(t, got)
			}
		})
	}
}

func TestMeetsMinVersionFinalRelease(t *testing.T) {
	// A directive introduced in the 9.1.0 line, recorded by its final release.
	min := semver.MustParse("9.1.0")
	tests := []struct {
		name  string
		image string
		want  bool
	}{
		{name: "release candidate of the minimum itself", image: "valkey/valkey:9.1.0-rc1", want: true},
		{name: "later release candidate of the minimum", image: "valkey/valkey:9.1.0-rc2", want: true},
		{name: "the minimum", image: "valkey/valkey:9.1.0", want: true},
		{name: "patch above the minimum", image: "valkey/valkey:9.1.2", want: true},
		{name: "release candidate of a later minor", image: "valkey/valkey:9.2.0-rc1", want: true},
		{name: "distro suffix on the minimum", image: "valkey/valkey:9.1.0-alpine", want: true},
		{name: "patch below the minimum", image: "valkey/valkey:9.0.6", want: false},
		{name: "release candidate below the minimum", image: "valkey/valkey:9.0.0-rc1", want: false},
		{name: "unknown version floating tag", image: "valkey/valkey:latest", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, MeetsMinVersion(tt.image, min))
		})
	}
}

// A prerelease minimum orders its own release candidates correctly on its own,
// so an earlier candidate than the one that introduced the directive stays out.
func TestMeetsMinVersionPrereleaseMinimumKeepsOrder(t *testing.T) {
	min := semver.MustParse("9.1.0-rc2")
	assert.False(t, MeetsMinVersion("valkey/valkey:9.1.0-rc1", min))
	assert.True(t, MeetsMinVersion("valkey/valkey:9.1.0-rc2", min))
	assert.True(t, MeetsMinVersion("valkey/valkey:9.1.0", min))
}

func TestMeetsMinVersion(t *testing.T) {
	min := semver.MustParse("9.1.0-rc1")
	tests := []struct {
		name  string
		image string
		want  bool
	}{
		{name: "above minimum", image: "valkey/valkey:9.1.1", want: true},
		{name: "equal to minimum", image: "valkey/valkey:9.1.0", want: true},
		{name: "prerelease meets minimum", image: "valkey/valkey:9.1.0-rc1", want: true},
		{name: "prerelease below minimum", image: "valkey/valkey:9.1.0-rc0", want: false},
		{name: "rc2 tag", image: "valkey/valkey:9.1.0-rc2", want: true},
		{name: "distro suffix meets gate", image: "valkey/valkey:9.1.0-alpine", want: true},
		{name: "pinned by digest only", image: "valkey/valkey@sha256:abcd1234", want: false},
		{name: "tag plus digest meets gate", image: "valkey/valkey:9.1.1@sha256:70739f85ad2ee01a726a965584a0f94895f01b0c60b3cc8b0aeef11eaa6888cf", want: true},
		{name: "below minimum", image: "valkey/valkey:9.0.0", want: false},
		{name: "below minimum with suffix", image: "valkey/valkey:9.0.5-bookworm", want: false},
		{name: "unknown version floating tag", image: "valkey/valkey:latest", want: false},
		{name: "unknown version digest", image: "valkey/valkey@sha256:abcd1234", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, MeetsMinVersion(tt.image, min))
		})
	}
}
