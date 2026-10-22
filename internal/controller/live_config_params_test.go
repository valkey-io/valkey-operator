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
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLiveConfigParams(t *testing.T) {
	t.Run("applies the live keys of spec.config", func(t *testing.T) {
		params, managed := liveConfigParams(map[string]string{"maxmemory": "1gb", "appendonly": "yes"}, nil)
		assert.Equal(t, map[string]string{"maxmemory": "1gb"}, params)
		assert.Equal(t, []string{"maxmemory"}, managed)
	})

	t.Run("sets a key that left spec.config back to its default", func(t *testing.T) {
		params, managed := liveConfigParams(map[string]string{"maxmemory": "1gb"}, []string{"maxmemory-policy", "maxmemory"})
		assert.Equal(t, map[string]string{"maxmemory": "1gb", "maxmemory-policy": "noeviction"}, params)
		assert.Equal(t, []string{"maxmemory"}, managed)
	})

	t.Run("ignores a remembered key that is not live-settable", func(t *testing.T) {
		params, managed := liveConfigParams(nil, []string{"appendonly"})
		assert.Empty(t, params)
		assert.Empty(t, managed)
	})

	t.Run("every allowlisted key has a default", func(t *testing.T) {
		for key := range liveConfigAllowlist {
			assert.Contains(t, liveConfigDefaults, key)
		}
	})
}
