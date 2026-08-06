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
