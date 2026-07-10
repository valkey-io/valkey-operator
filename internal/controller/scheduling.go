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
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

// effectiveNodeSpread resolves the node-axis spread modes, defaulting every
// unset value to Disabled (the zero value — emit nothing). A nil spec or nil
// Node resolves entirely to Disabled.
func effectiveNodeSpread(s *valkeyiov1alpha1.SchedulingSpec) (shards, primaries, pods valkeyiov1alpha1.SpreadMode) {
	shards, primaries, pods = valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadDisabled
	if s == nil || s.Node == nil {
		return shards, primaries, pods
	}
	if m := s.Node.Spread.Shards.Mode; m != "" {
		shards = m
	}
	if m := s.Node.Spread.Primaries.Mode; m != "" {
		primaries = m
	}
	if m := s.Node.Spread.Pods.Mode; m != "" {
		pods = m
	}
	return shards, primaries, pods
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
