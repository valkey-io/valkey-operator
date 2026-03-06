package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWorkloadTypeConstants(t *testing.T) {
	assert.Equal(t, WorkloadType("StatefulSet"), WorkloadTypeStatefulSet)
	assert.Equal(t, WorkloadType("Deployment"), WorkloadTypeDeployment)
}
