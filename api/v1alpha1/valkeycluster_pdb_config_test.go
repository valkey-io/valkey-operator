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

func TestPodDisruptionBudgetConfigUnmarshalLegacyManaged(t *testing.T) {
	var c PodDisruptionBudgetConfig
	require.NoError(t, json.Unmarshal([]byte(`"Managed"`), &c))
	assert.Equal(t, PDBModeCluster, c.Mode)
}

func TestPodDisruptionBudgetConfigUnmarshalLegacyDisabled(t *testing.T) {
	var c PodDisruptionBudgetConfig
	require.NoError(t, json.Unmarshal([]byte(`"Disabled"`), &c))
	assert.Equal(t, PDBModeDisabled, c.Mode)
}

func TestPodDisruptionBudgetConfigUnmarshalObject(t *testing.T) {
	var c PodDisruptionBudgetConfig
	require.NoError(t, json.Unmarshal([]byte(`{"mode":"Disabled"}`), &c))
	assert.Equal(t, PDBModeDisabled, c.Mode)
}

func TestPodDisruptionBudgetConfigUnmarshalUnknownStringPassesThrough(t *testing.T) {
	var c PodDisruptionBudgetConfig
	require.NoError(t, json.Unmarshal([]byte(`"Shard"`), &c))
	assert.Equal(t, PDBMode("Shard"), c.Mode)
}
