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

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

func TestFailoverTimeoutSeconds(t *testing.T) {
	cfg := func(v string) map[string]string {
		return map[string]string{"cluster-manual-failover-timeout": v}
	}
	assert.Equal(t, int64(5), failoverTimeoutSeconds(nil))           // unset map
	assert.Equal(t, int64(5), failoverTimeoutSeconds(cfg("")))       // empty value
	assert.Equal(t, int64(30), failoverTimeoutSeconds(cfg("30000"))) // 30s
	assert.Equal(t, int64(6), failoverTimeoutSeconds(cfg("5500")))   // rounds up
	assert.Equal(t, int64(5), failoverTimeoutSeconds(cfg("abc")))    // unparseable falls back
	assert.Equal(t, int64(5), failoverTimeoutSeconds(cfg("0")))      // non-positive falls back
}

func TestRecommendedGracePeriodSeconds(t *testing.T) {
	c := &valkeyiov1alpha1.ValkeyCluster{}
	assert.Equal(t, int64(15), recommendedGracePeriodSeconds(c)) // 5s default + 10s buffer

	c.Spec.Config = map[string]string{"cluster-manual-failover-timeout": "60000"}
	assert.Equal(t, int64(70), recommendedGracePeriodSeconds(c)) // 60s + 10s buffer
}

func TestEffectiveGracePeriodSeconds(t *testing.T) {
	clusterWith := func(grace *int64, cfg map[string]string) *valkeyiov1alpha1.ValkeyCluster {
		return &valkeyiov1alpha1.ValkeyCluster{
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{TerminationGracePeriodSeconds: grace, Config: cfg},
		}
	}
	ptr := func(v int64) *int64 { return &v }

	t.Run("unset with the default timeout keeps the kubernetes default", func(t *testing.T) {
		assert.Equal(t, int64(30), effectiveGracePeriodSeconds(clusterWith(nil, nil)))
	})

	t.Run("unset with a long timeout pulls the grace period up", func(t *testing.T) {
		c := clusterWith(nil, map[string]string{"cluster-manual-failover-timeout": "60000"})
		assert.Equal(t, int64(70), effectiveGracePeriodSeconds(c))
	})

	t.Run("an explicit value is honoured", func(t *testing.T) {
		assert.Equal(t, int64(45), effectiveGracePeriodSeconds(clusterWith(ptr(45), nil)))
	})

	t.Run("an explicit value below the recommended minimum is still honoured", func(t *testing.T) {
		assert.Equal(t, int64(5), effectiveGracePeriodSeconds(clusterWith(ptr(5), nil)))
	})
}
