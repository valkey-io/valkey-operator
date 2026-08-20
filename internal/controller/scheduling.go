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
	"maps"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// effectiveNodeSpread resolves the node-axis spread modes, defaulting every
// unset value to Disabled (the zero value — emit nothing). A nil spec or nil
// Node resolves entirely to Disabled.
func effectiveNodeSpread(s *valkeyiov1alpha1.SchedulingSpec) (shard, primaries, pods valkeyiov1alpha1.SpreadMode) {
	shard, primaries, pods = valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadDisabled
	if s == nil || s.Node == nil {
		return shard, primaries, pods
	}
	if m := s.Node.Spread.Shard.Mode; m != "" {
		shard = m
	}
	if m := s.Node.Spread.Primaries.Mode; m != "" {
		primaries = m
	}
	if m := s.Node.Spread.Pods.Mode; m != "" {
		pods = m
	}
	return shard, primaries, pods
}

// withNodeShardAntiAffinity returns a copy of base with a node-hostname
// anti-affinity term for the shard added at the requested strength. base may be
// nil. Disabled returns base unchanged. The input is never mutated.
func withNodeShardAntiAffinity(base *corev1.Affinity, clusterName string, shardIndex int, mode valkeyiov1alpha1.SpreadMode) *corev1.Affinity {
	switch mode {
	case valkeyiov1alpha1.SpreadRequired, valkeyiov1alpha1.SpreadPreferred:
		// falls through to the emitting path below.
	default:
		// Disabled, or an unknown future mode: no-op.
		return base
	}

	out := base.DeepCopy()
	if out == nil {
		out = &corev1.Affinity{}
	}
	if out.PodAntiAffinity == nil {
		out.PodAntiAffinity = &corev1.PodAntiAffinity{}
	}
	term := corev1.PodAffinityTerm{
		TopologyKey: corev1.LabelHostname,
		LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
			LabelCluster:    clusterName,
			LabelShardIndex: strconv.Itoa(shardIndex),
		}},
	}
	switch mode {
	case valkeyiov1alpha1.SpreadRequired:
		out.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution =
			append(out.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution, term)
	case valkeyiov1alpha1.SpreadPreferred:
		out.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution =
			append(out.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution,
				corev1.WeightedPodAffinityTerm{Weight: 100, PodAffinityTerm: term})
	default:
		// Unreachable: filtered out above.
	}
	return out
}

// whenUnsatisfiable maps a spread mode to a topology-spread action. The second
// return is false when the mode emits no constraint (Disabled or unknown).
func whenUnsatisfiable(mode valkeyiov1alpha1.SpreadMode) (corev1.UnsatisfiableConstraintAction, bool) {
	switch mode {
	case valkeyiov1alpha1.SpreadRequired:
		return corev1.DoNotSchedule, true
	case valkeyiov1alpha1.SpreadPreferred:
		return corev1.ScheduleAnyway, true
	default:
		return "", false
	}
}

// nodeSpreadTSCs renders the primaries and pods node-axis topology spread
// constraints for one ValkeyNode. The primaries constraint is emitted only on
// node-index-0 pods (the shard's primary at creation); the pods constraint is
// emitted on every pod. Primaries precede pods in the returned slice.
func nodeSpreadTSCs(clusterName string, nodeIndex int, primaries, pods valkeyiov1alpha1.SpreadMode) []corev1.TopologySpreadConstraint {
	var out []corev1.TopologySpreadConstraint
	if nodeIndex == 0 {
		if action, ok := whenUnsatisfiable(primaries); ok {
			out = append(out, corev1.TopologySpreadConstraint{
				MaxSkew:           1,
				TopologyKey:       corev1.LabelHostname,
				WhenUnsatisfiable: action,
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
					LabelCluster:   clusterName,
					LabelNodeIndex: "0",
				}},
			})
		}
	}
	if action, ok := whenUnsatisfiable(pods); ok {
		out = append(out, corev1.TopologySpreadConstraint{
			MaxSkew:           1,
			TopologyKey:       corev1.LabelHostname,
			WhenUnsatisfiable: action,
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
				LabelCluster: clusterName,
			}},
		})
	}
	return out
}

// effectiveZoneSpread resolves the zone-axis spread modes, defaulting every
// unset value to Disabled (the zero value — emit nothing). A nil spec or nil
// Zone resolves entirely to Disabled.
func effectiveZoneSpread(s *valkeyiov1alpha1.SchedulingSpec) (shard, primaries, pods valkeyiov1alpha1.SpreadMode) {
	shard, primaries, pods = valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadDisabled
	if s == nil || s.Zone == nil {
		return shard, primaries, pods
	}
	if m := s.Zone.Spread.Shard.Mode; m != "" {
		shard = m
	}
	if m := s.Zone.Spread.Primaries.Mode; m != "" {
		primaries = m
	}
	if m := s.Zone.Spread.Pods.Mode; m != "" {
		pods = m
	}
	return shard, primaries, pods
}

// zoneSpreadTSCs renders the shard, primaries, and pods zone-axis topology
// spread constraints for one ValkeyNode. Unlike the node axis, shard is a
// balancing TSC (not anti-affinity). shard and pods are emitted on every pod;
// primaries is emitted only on node-index-0 pods. Order: shard, primaries, pods.
func zoneSpreadTSCs(clusterName string, shardIndex, nodeIndex int, shard, primaries, pods valkeyiov1alpha1.SpreadMode) []corev1.TopologySpreadConstraint {
	var out []corev1.TopologySpreadConstraint
	if action, ok := whenUnsatisfiable(shard); ok {
		out = append(out, corev1.TopologySpreadConstraint{
			MaxSkew:           1,
			TopologyKey:       corev1.LabelTopologyZone,
			WhenUnsatisfiable: action,
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
				LabelCluster:    clusterName,
				LabelShardIndex: strconv.Itoa(shardIndex),
			}},
		})
	}
	if nodeIndex == 0 {
		if action, ok := whenUnsatisfiable(primaries); ok {
			out = append(out, corev1.TopologySpreadConstraint{
				MaxSkew:           1,
				TopologyKey:       corev1.LabelTopologyZone,
				WhenUnsatisfiable: action,
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
					LabelCluster:   clusterName,
					LabelNodeIndex: "0",
				}},
			})
		}
	}
	if action, ok := whenUnsatisfiable(pods); ok {
		out = append(out, corev1.TopologySpreadConstraint{
			MaxSkew:           1,
			TopologyKey:       corev1.LabelTopologyZone,
			WhenUnsatisfiable: action,
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
				LabelCluster: clusterName,
			}},
		})
	}
	return out
}

// effectiveZonePinning returns the ordered zone list for pinning, or nil when
// pinning is off. A nil spec, a nil Zone, or a nil Pinning all resolve to off.
// The returned slice is the live backing array from the spec, not a copy;
// callers must treat it as read-only.
func effectiveZonePinning(s *valkeyiov1alpha1.SchedulingSpec) []string {
	if s == nil || s.Zone == nil || s.Zone.Pinning == nil {
		return nil
	}
	return s.Zone.Pinning.Zones
}

// zoneForPod returns the zone a pod is pinned to, or "" when pinning is off.
// Each shard walks the zone list from a different starting point, so a shard's
// members land in consecutive zones, and while there are at least as many
// zones as shards, primaries end up in distinct zones as a side effect of the
// modulo.
func zoneForPod(zones []string, shardIndex, nodeIndex int) string {
	if len(zones) == 0 {
		return ""
	}
	return zones[(shardIndex+nodeIndex)%len(zones)]
}

// withZonePin returns a copy of base with the pinned zone added. base may be
// nil and is never mutated. An empty zone returns base unchanged, so an
// unpinned cluster keeps a nil nodeSelector rather than gaining an empty map,
// which would re-render every pod on upgrade. If base already carries the
// zone key, the curated value wins; admission rejects a passthrough
// nodeSelector that sets it while pinning is on (the scheduling.nodeSelector
// CEL rule on ValkeyClusterSpec), so that collision should be unreachable here.
func withZonePin(nodeSelector map[string]string, zone string) map[string]string {
	if zone == "" {
		return nodeSelector
	}
	enrichedNodeSelector := make(map[string]string, len(nodeSelector)+1)
	maps.Copy(enrichedNodeSelector, nodeSelector)
	enrichedNodeSelector[corev1.LabelTopologyZone] = zone
	return enrichedNodeSelector
}
