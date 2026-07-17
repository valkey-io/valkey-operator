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
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

func TestEffectiveNodeSpread_Defaults(t *testing.T) {
	for name, s := range map[string]*valkeyiov1alpha1.SchedulingSpec{
		"nil spec":   nil,
		"nil node":   {},
		"empty node": {Node: &valkeyiov1alpha1.NodeScheduling{}},
	} {
		t.Run(name, func(t *testing.T) {
			shard, primaries, pods := effectiveNodeSpread(s)
			assert.Equal(t, valkeyiov1alpha1.SpreadDisabled, shard, "shard default Disabled")
			assert.Equal(t, valkeyiov1alpha1.SpreadDisabled, primaries, "primaries default Disabled")
			assert.Equal(t, valkeyiov1alpha1.SpreadDisabled, pods, "pods default Disabled")
		})
	}
}

func TestEffectiveNodeSpread_Overrides(t *testing.T) {
	s := &valkeyiov1alpha1.SchedulingSpec{Node: &valkeyiov1alpha1.NodeScheduling{
		Spread: valkeyiov1alpha1.NodeSpread{
			Shard:     valkeyiov1alpha1.SpreadConstraint{Mode: valkeyiov1alpha1.SpreadRequired},
			Primaries: valkeyiov1alpha1.SpreadConstraint{Mode: valkeyiov1alpha1.SpreadDisabled},
			Pods:      valkeyiov1alpha1.SpreadConstraint{Mode: valkeyiov1alpha1.SpreadPreferred},
		},
	}}
	shard, primaries, pods := effectiveNodeSpread(s)
	assert.Equal(t, valkeyiov1alpha1.SpreadRequired, shard)
	assert.Equal(t, valkeyiov1alpha1.SpreadDisabled, primaries)
	assert.Equal(t, valkeyiov1alpha1.SpreadPreferred, pods)
}

func TestWithNodeShardAntiAffinity(t *testing.T) {
	t.Run("disabled returns base unchanged", func(t *testing.T) {
		assert.Nil(t, withNodeShardAntiAffinity(nil, "c", 0, valkeyiov1alpha1.SpreadDisabled))
	})

	t.Run("required adds hard anti-affinity term", func(t *testing.T) {
		got := withNodeShardAntiAffinity(nil, "mycluster", 2, valkeyiov1alpha1.SpreadRequired)
		require.NotNil(t, got.PodAntiAffinity)
		require.Len(t, got.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution, 1)
		term := got.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0]
		assert.Equal(t, "kubernetes.io/hostname", term.TopologyKey)
		assert.Equal(t, map[string]string{
			LabelCluster:    "mycluster",
			LabelShardIndex: "2",
		}, term.LabelSelector.MatchLabels)
	})

	t.Run("preferred adds weighted soft term", func(t *testing.T) {
		got := withNodeShardAntiAffinity(nil, "mycluster", 0, valkeyiov1alpha1.SpreadPreferred)
		require.Len(t, got.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution, 1)
		w := got.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution[0]
		assert.Equal(t, int32(100), w.Weight)
		assert.Equal(t, "kubernetes.io/hostname", w.PodAffinityTerm.TopologyKey)
	})

	t.Run("preserves and does not mutate the user's affinity", func(t *testing.T) {
		base := &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{}}
		got := withNodeShardAntiAffinity(base, "c", 0, valkeyiov1alpha1.SpreadRequired)
		assert.NotNil(t, got.NodeAffinity, "existing NodeAffinity retained")
		assert.Nil(t, base.PodAntiAffinity, "input must not be mutated")
	})
}

func TestNodeSpreadTSCs(t *testing.T) {
	t.Run("primaries Preferred on node-index 0: one ScheduleAnyway TSC", func(t *testing.T) {
		got := nodeSpreadTSCs("mycluster", 0, valkeyiov1alpha1.SpreadPreferred, valkeyiov1alpha1.SpreadDisabled)
		require.Len(t, got, 1)
		assert.Equal(t, corev1.ScheduleAnyway, got[0].WhenUnsatisfiable)
		assert.Equal(t, int32(1), got[0].MaxSkew)
		assert.Equal(t, "kubernetes.io/hostname", got[0].TopologyKey)
		assert.Equal(t, map[string]string{LabelCluster: "mycluster", LabelNodeIndex: "0"}, got[0].LabelSelector.MatchLabels)
	})

	t.Run("primaries suppressed on non-zero node index", func(t *testing.T) {
		got := nodeSpreadTSCs("mycluster", 1, valkeyiov1alpha1.SpreadPreferred, valkeyiov1alpha1.SpreadDisabled)
		assert.Empty(t, got)
	})

	t.Run("pods Required emits cluster-wide DoNotSchedule on every index", func(t *testing.T) {
		got := nodeSpreadTSCs("mycluster", 3, valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadRequired)
		require.Len(t, got, 1)
		assert.Equal(t, corev1.DoNotSchedule, got[0].WhenUnsatisfiable)
		assert.Equal(t, map[string]string{LabelCluster: "mycluster"}, got[0].LabelSelector.MatchLabels)
	})

	t.Run("primaries + pods on node-index 0 emits both, primaries first", func(t *testing.T) {
		got := nodeSpreadTSCs("mycluster", 0, valkeyiov1alpha1.SpreadRequired, valkeyiov1alpha1.SpreadPreferred)
		require.Len(t, got, 2)
		assert.Equal(t, "0", got[0].LabelSelector.MatchLabels[LabelNodeIndex], "primaries first")
		assert.NotContains(t, got[1].LabelSelector.MatchLabels, LabelNodeIndex, "pods second")
	})
}
