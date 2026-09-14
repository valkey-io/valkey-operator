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
	"regexp"
	"strings"

	semver "github.com/Masterminds/semver/v3"
)

// imageVersionRE extracts a Valkey release from a Docker tag: semver plus an
// optional -rcN prerelease. Distro suffixes such as -bookworm or -alpine3.26
// are not part of the match.
var imageVersionRE = regexp.MustCompile(`(?i)^v?(\d+\.\d+(?:\.\d+)?(?:-rc\d+)?)`)

// VersionFromImage parses the Valkey version from a container image reference's
// tag. It returns false when the version cannot be determined, for example for
// floating tags such as "latest" or digest-only references without a tag.
func VersionFromImage(image string) (*semver.Version, bool) {
	tag, ok := imageTag(image)
	if !ok {
		return nil, false
	}

	match := imageVersionRE.FindStringSubmatch(tag)
	if len(match) < 2 {
		return nil, false
	}
	parsed, err := semver.NewVersion(match[1])
	if err != nil {
		return nil, false
	}

	return parsed, true
}

// imageTag extracts the tag from a container image reference.
func imageTag(image string) (string, bool) {
	if image == "" {
		return "", false
	}

	if i := strings.IndexByte(image, '@'); i >= 0 {
		image = image[:i]
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
