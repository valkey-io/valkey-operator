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
	"maps"
	"slices"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// configEntries returns config as a list sorted by name.
func configEntries(config map[string]string) []valkeyiov1alpha1.AppliedLiveConfigEntry {
	if len(config) == 0 {
		return nil
	}
	out := make([]valkeyiov1alpha1.AppliedLiveConfigEntry, 0, len(config))
	for _, name := range slices.Sorted(maps.Keys(config)) {
		out = append(out, valkeyiov1alpha1.AppliedLiveConfigEntry{Name: name, Value: config[name]})
	}
	return out
}
