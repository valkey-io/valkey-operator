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
	"strings"

	semver "github.com/Masterminds/semver/v3"
)

// VersionFromImage parses the Valkey version from a container image reference's
// tag. It returns false when the version cannot be determined, for example for
// floating tags such as "latest" or digest-pinned references.
func VersionFromImage(image string) (*semver.Version, bool) {
	tag, ok := imageTag(image)
	if !ok {
		return nil, false
	}

	parsed, err := semver.NewVersion(tag)
	if err != nil {
		return nil, false
	}

	return semver.New(parsed.Major(), parsed.Minor(), parsed.Patch(), "", ""), true
}

// imageTag extracts the tag from a container image reference.
func imageTag(image string) (string, bool) {
	if image == "" || strings.Contains(image, "@") {
		return "", false
	}

	idx := strings.LastIndex(image, ":")
	if idx == -1 {
		return "", false
	}

	tag := image[idx+1:]
	if tag == "" || strings.Contains(tag, "/") {
		return "", false
	}
	return tag, true
}

// VersionStringFromImage returns the parsed version string or an empty string
// when the version cannot be determined.
func VersionStringFromImage(image string) string {
	if v, ok := VersionFromImage(image); ok {
		return v.String()
	}
	return ""
}

// MeetsMinVersion reports whether the version parsed from image meets min.
func MeetsMinVersion(image string, min *semver.Version) bool {
	if min == nil {
		return false
	}
	version, ok := VersionFromImage(image)
	if !ok {
		return false
	}
	return !version.LessThan(min)
}
