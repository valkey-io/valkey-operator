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
		{name: "v-prefixed tag", image: "valkey/valkey:v9.1.0", want: "9.1.0", wantOK: true},
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

func TestMeetsMinVersion(t *testing.T) {
	min := semver.MustParse("9.1.0")
	tests := []struct {
		name  string
		image string
		want  bool
	}{
		{name: "above minimum", image: "valkey/valkey:9.1.1", want: true},
		{name: "equal to minimum", image: "valkey/valkey:9.1.0", want: true},
		{name: "distro suffix meets gate", image: "valkey/valkey:9.1.0-alpine", want: true},
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

func TestVersionStringFromImage(t *testing.T) {
	tests := []struct {
		name  string
		image string
		want  string
	}{
		{name: "plain semver tag", image: "valkey/valkey:9.1.0", want: "9.1.0"},
		{name: "distro suffix alpine", image: "valkey/valkey:9.1.2-alpine", want: "9.1.2"},
		{name: "distro suffix bookworm", image: "valkey/valkey:9.1.0-bookworm", want: "9.1.0"},
		{name: "v-prefixed tag", image: "valkey/valkey:v9.1.0", want: "9.1.0"},
		{name: "registry with port", image: "myregistry:5000/valkey/valkey:9.1.0", want: "9.1.0"},
		{name: "floating latest tag returns empty", image: "valkey/valkey:latest", want: ""},
		{name: "no tag returns empty", image: "valkey/valkey", want: ""},
		{name: "digest pin returns empty", image: "valkey/valkey@sha256:abcd1234", want: ""},
		{name: "empty image returns empty", image: "", want: ""},
		{name: "non-numeric tag returns empty", image: "valkey/valkey:unstable", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, VersionStringFromImage(tt.image))
		})
	}
}
