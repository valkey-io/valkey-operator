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

package v1alpha1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkloadTypeConstants(t *testing.T) {
	assert.Equal(t, WorkloadType("StatefulSet"), WorkloadTypeStatefulSet)
	assert.Equal(t, WorkloadType("Deployment"), WorkloadTypeDeployment)
}

// The required fields the parent sets stay out of an empty spec. An update to
// a node stored before a field existed would otherwise send "" and fail its
// validation.
func TestValkeyNodeSpecOmitsEmptyParentFields(t *testing.T) {
	data, err := json.Marshal(ValkeyNodeSpec{})
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(data, &fields))
	for _, name := range []string{"configRevision"} {
		assert.NotContains(t, fields, name)
	}
}
