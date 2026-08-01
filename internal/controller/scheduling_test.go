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

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
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

func TestEffectiveZoneSpread_Defaults(t *testing.T) {
	for name, s := range map[string]*valkeyiov1alpha1.SchedulingSpec{
		"nil spec":   nil,
		"nil zone":   {},
		"empty zone": {Zone: &valkeyiov1alpha1.ZoneScheduling{}},
	} {
		t.Run(name, func(t *testing.T) {
			shard, primaries, pods := effectiveZoneSpread(s)
			assert.Equal(t, valkeyiov1alpha1.SpreadDisabled, shard, "shard default Disabled")
			assert.Equal(t, valkeyiov1alpha1.SpreadDisabled, primaries, "primaries default Disabled")
			assert.Equal(t, valkeyiov1alpha1.SpreadDisabled, pods, "pods default Disabled")
		})
	}
}

func TestEffectiveZoneSpread_Overrides(t *testing.T) {
	s := &valkeyiov1alpha1.SchedulingSpec{Zone: &valkeyiov1alpha1.ZoneScheduling{
		Spread: valkeyiov1alpha1.ZoneSpread{
			Shard:     valkeyiov1alpha1.SpreadConstraint{Mode: valkeyiov1alpha1.SpreadRequired},
			Primaries: valkeyiov1alpha1.SpreadConstraint{Mode: valkeyiov1alpha1.SpreadDisabled},
			Pods:      valkeyiov1alpha1.SpreadConstraint{Mode: valkeyiov1alpha1.SpreadPreferred},
		},
	}}
	shard, primaries, pods := effectiveZoneSpread(s)
	assert.Equal(t, valkeyiov1alpha1.SpreadRequired, shard)
	assert.Equal(t, valkeyiov1alpha1.SpreadDisabled, primaries)
	assert.Equal(t, valkeyiov1alpha1.SpreadPreferred, pods)
}

func TestZoneSpreadTSCs(t *testing.T) {
	t.Run("all Disabled renders nothing", func(t *testing.T) {
		assert.Empty(t, zoneSpreadTSCs("c", 0, 0, valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadDisabled))
	})

	t.Run("shard Required: zone TSC on all pods, shard-index selector", func(t *testing.T) {
		got := zoneSpreadTSCs("mycluster", 2, 1, valkeyiov1alpha1.SpreadRequired, valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadDisabled)
		require.Len(t, got, 1)
		assert.Equal(t, corev1.DoNotSchedule, got[0].WhenUnsatisfiable)
		assert.Equal(t, int32(1), got[0].MaxSkew)
		assert.Equal(t, "topology.kubernetes.io/zone", got[0].TopologyKey)
		assert.Equal(t, map[string]string{LabelCluster: "mycluster", LabelShardIndex: "2"}, got[0].LabelSelector.MatchLabels)
	})

	t.Run("primaries only emitted on node-index 0", func(t *testing.T) {
		on0 := zoneSpreadTSCs("mycluster", 0, 0, valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadPreferred, valkeyiov1alpha1.SpreadDisabled)
		require.Len(t, on0, 1)
		assert.Equal(t, corev1.ScheduleAnyway, on0[0].WhenUnsatisfiable)
		assert.Equal(t, map[string]string{LabelCluster: "mycluster", LabelNodeIndex: "0"}, on0[0].LabelSelector.MatchLabels)

		on1 := zoneSpreadTSCs("mycluster", 0, 1, valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadPreferred, valkeyiov1alpha1.SpreadDisabled)
		assert.Empty(t, on1, "primaries suppressed on non-zero node index")
	})

	t.Run("pods Required: cluster-wide zone TSC on every index", func(t *testing.T) {
		got := zoneSpreadTSCs("mycluster", 1, 3, valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadRequired)
		require.Len(t, got, 1)
		assert.Equal(t, map[string]string{LabelCluster: "mycluster"}, got[0].LabelSelector.MatchLabels)
	})

	t.Run("all three on node-index 0: shard, primaries, pods in order", func(t *testing.T) {
		got := zoneSpreadTSCs("mycluster", 1, 0, valkeyiov1alpha1.SpreadRequired, valkeyiov1alpha1.SpreadPreferred, valkeyiov1alpha1.SpreadPreferred)
		require.Len(t, got, 3)
		assert.Equal(t, "1", got[0].LabelSelector.MatchLabels[LabelShardIndex], "shard first")
		assert.Equal(t, "0", got[1].LabelSelector.MatchLabels[LabelNodeIndex], "primaries second")
		assert.NotContains(t, got[2].LabelSelector.MatchLabels, LabelShardIndex, "pods last")
		assert.NotContains(t, got[2].LabelSelector.MatchLabels, LabelNodeIndex, "pods last")
	})
}

func TestEffectiveZonePinning(t *testing.T) {
	assert.Nil(t, effectiveZonePinning(nil), "nil spec resolves to off")
	assert.Nil(t, effectiveZonePinning(&valkeyiov1alpha1.SchedulingSpec{}), "nil Zone resolves to off")
	assert.Nil(t, effectiveZonePinning(&valkeyiov1alpha1.SchedulingSpec{
		Zone: &valkeyiov1alpha1.ZoneScheduling{},
	}), "nil Pinning resolves to off")
	assert.Equal(t, []string{"az1", "az2"}, effectiveZonePinning(&valkeyiov1alpha1.SchedulingSpec{
		Zone: &valkeyiov1alpha1.ZoneScheduling{
			Pinning: &valkeyiov1alpha1.ZonePinning{Zones: []string{"az1", "az2"}},
		},
	}))
}

func TestZoneForPod(t *testing.T) {
	zones := []string{"az1", "az2", "az3"}

	t.Run("round-robin over shard and node index", func(t *testing.T) {
		// The worked example from the design doc: 3 shards x 2 nodes x 3 zones.
		for _, tc := range []struct {
			shard, node int
			want        string
		}{
			{0, 0, "az1"}, {0, 1, "az2"},
			{1, 0, "az2"}, {1, 1, "az3"},
			{2, 0, "az3"}, {2, 1, "az1"},
		} {
			assert.Equal(t, tc.want, zoneForPod(zones, tc.shard, tc.node),
				"shard %d node %d", tc.shard, tc.node)
		}
	})

	t.Run("single zone pins every pod to it", func(t *testing.T) {
		for shard := range 3 {
			for node := range 2 {
				assert.Equal(t, "az1", zoneForPod([]string{"az1"}, shard, node))
			}
		}
	})

	t.Run("pinning off returns the empty string", func(t *testing.T) {
		assert.Equal(t, "", zoneForPod(nil, 1, 1))
		assert.Equal(t, "", zoneForPod([]string{}, 1, 1))
	})
}

func TestWithZonePin(t *testing.T) {
	t.Run("nil base gains the zone key", func(t *testing.T) {
		assert.Equal(t, map[string]string{"topology.kubernetes.io/zone": "az3"}, withZonePin(nil, "az3"))
	})

	t.Run("user entries are preserved and the input is not mutated", func(t *testing.T) {
		base := map[string]string{"node.kubernetes.io/instance-type": "m6i.xlarge"}
		got := withZonePin(base, "az2")
		assert.Equal(t, map[string]string{
			"node.kubernetes.io/instance-type": "m6i.xlarge",
			"topology.kubernetes.io/zone":      "az2",
		}, got)
		assert.Equal(t, map[string]string{"node.kubernetes.io/instance-type": "m6i.xlarge"}, base,
			"the caller's map must not be mutated")
	})

	t.Run("empty zone returns the base unchanged, preserving nil", func(t *testing.T) {
		assert.Nil(t, withZonePin(nil, ""), "nil must stay nil so unpinned clusters are not rolled")
		base := map[string]string{"a": "b"}
		assert.Equal(t, base, withZonePin(base, ""))
	})

	t.Run("curated zone wins when base already carries the key", func(t *testing.T) {
		base := map[string]string{"topology.kubernetes.io/zone": "user-supplied"}
		got := withZonePin(base, "az2")
		assert.Equal(t, map[string]string{"topology.kubernetes.io/zone": "az2"}, got)
		assert.Equal(t, map[string]string{"topology.kubernetes.io/zone": "user-supplied"}, base,
			"the caller's map must not be mutated")
	})
}
