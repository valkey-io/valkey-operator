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
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBaseConfigGatesShutdownOnSigterm(t *testing.T) {
	for image, want := range map[string]bool{
		"":                           true, // DefaultImage
		"valkey/valkey:9.0.0":        true,
		"valkey/valkey:9.0.0-rc1":    true,
		"valkey/valkey:9.1.0":        true,
		"valkey/valkey:8.1.11":       false,
		"valkey/valkey:8.0.12":       false,
		"valkey/valkey:latest":       true, // no version in the tag
		"valkey/valkey@sha256:abcd0": true,
	} {
		_, got := getBaseConfig(nil, false, image)["shutdown-on-sigterm"]
		assert.Equal(t, want, got, image)
	}
}

func TestSystemUserAclGatesRulesByVersion(t *testing.T) {
	cases := map[string][]string{ // image -> rules that must be absent
		"valkey/valkey:9.0.0":  nil,
		"valkey/valkey:latest": nil,
		"valkey/valkey:8.1.11": {"+cluster|migrateslots", "+cluster|getslotmigrations", "+cluster|syncslots"},
		"valkey/valkey:8.0.12": {"+cluster|migrateslots", "+cluster|getslotmigrations", "+cluster|syncslots", "+commandlog|len"},
	}
	for image, absent := range cases {
		all := make([]string, 0, 64)
		for _, user := range systemUsers {
			all = append(all, strings.Fields(systemUserAcl(user, image))...)
		}
		for rule := range versionGatedAclRules {
			assert.Equal(t, !slices.Contains(absent, rule), slices.Contains(all, rule), "%s %s", image, rule)
		}
		assert.Contains(t, all, "+cluster|failover", image)
	}
}
