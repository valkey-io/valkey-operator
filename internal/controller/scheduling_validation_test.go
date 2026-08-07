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
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// schedulingCluster builds a minimal ValkeyCluster with an explicit
// node.spread.primaries/pods pair, for exercising the two-slot CEL
// validation on ValkeyClusterSpec.
func schedulingCluster(name string, primaries, pods valkeyiov1alpha1.SpreadMode) *valkeyiov1alpha1.ValkeyCluster {
	return &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{
			Shards:   3,
			Replicas: 1,
			Scheduling: &valkeyiov1alpha1.SchedulingSpec{
				Node: &valkeyiov1alpha1.NodeScheduling{
					Spread: valkeyiov1alpha1.NodeSpread{
						Primaries: valkeyiov1alpha1.SpreadConstraint{Mode: primaries},
						Pods:      valkeyiov1alpha1.SpreadConstraint{Mode: pods},
					},
				},
			},
		},
	}
}

// schedulingClusterWithPassthrough builds a ValkeyCluster with a raw
// topologySpreadConstraints entry (the escape hatch) alongside explicit
// node.spread.primaries/pods modes, for exercising the passthrough-vs-curated
// collision CEL validation.
func schedulingClusterWithPassthrough(name, topologyKey string, action corev1.UnsatisfiableConstraintAction, primaries, pods valkeyiov1alpha1.SpreadMode) *valkeyiov1alpha1.ValkeyCluster {
	return &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{
			Shards:   3,
			Replicas: 1,
			Scheduling: &valkeyiov1alpha1.SchedulingSpec{
				TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
					{
						MaxSkew:           1,
						TopologyKey:       topologyKey,
						WhenUnsatisfiable: action,
					},
				},
				Node: &valkeyiov1alpha1.NodeScheduling{
					Spread: valkeyiov1alpha1.NodeSpread{
						Primaries: valkeyiov1alpha1.SpreadConstraint{Mode: primaries},
						Pods:      valkeyiov1alpha1.SpreadConstraint{Mode: pods},
					},
				},
			},
		},
	}
}

// zoneSchedulingCluster builds a minimal ValkeyCluster with explicit
// zone.spread.shard/primaries/pods modes, for exercising the zone two-slot CEL
// validation on ValkeyClusterSpec.
func zoneSchedulingCluster(name string, shard, primaries, pods valkeyiov1alpha1.SpreadMode) *valkeyiov1alpha1.ValkeyCluster {
	return &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{
			Shards:   3,
			Replicas: 1,
			Scheduling: &valkeyiov1alpha1.SchedulingSpec{
				Zone: &valkeyiov1alpha1.ZoneScheduling{
					Spread: valkeyiov1alpha1.ZoneSpread{
						Shard:     valkeyiov1alpha1.SpreadConstraint{Mode: shard},
						Primaries: valkeyiov1alpha1.SpreadConstraint{Mode: primaries},
						Pods:      valkeyiov1alpha1.SpreadConstraint{Mode: pods},
					},
				},
			},
		},
	}
}

// zoneSchedulingClusterWithPassthrough builds a ValkeyCluster with a raw
// topologySpreadConstraints entry alongside explicit zone.spread modes, for
// exercising the zone passthrough-vs-curated collision CEL validation.
func zoneSchedulingClusterWithPassthrough(name, topologyKey string, action corev1.UnsatisfiableConstraintAction, shard, primaries, pods valkeyiov1alpha1.SpreadMode) *valkeyiov1alpha1.ValkeyCluster {
	return &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{
			Shards:   3,
			Replicas: 1,
			Scheduling: &valkeyiov1alpha1.SchedulingSpec{
				TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
					{MaxSkew: 1, TopologyKey: topologyKey, WhenUnsatisfiable: action},
				},
				Zone: &valkeyiov1alpha1.ZoneScheduling{
					Spread: valkeyiov1alpha1.ZoneSpread{
						Shard:     valkeyiov1alpha1.SpreadConstraint{Mode: shard},
						Primaries: valkeyiov1alpha1.SpreadConstraint{Mode: primaries},
						Pods:      valkeyiov1alpha1.SpreadConstraint{Mode: pods},
					},
				},
			},
		},
	}
}

var _ = Describe("ValkeyClusterSpec node.spread CEL validation", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("rejects node.spread.primaries and node.spread.pods both explicitly Required", func() {
		err := k8sClient.Create(ctx, schedulingCluster("spread-both-required", valkeyiov1alpha1.SpreadRequired, valkeyiov1alpha1.SpreadRequired))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("cannot both be Required"))
	})

	It("rejects node.spread.primaries and node.spread.pods both explicitly Preferred", func() {
		err := k8sClient.Create(ctx, schedulingCluster("spread-both-preferred", valkeyiov1alpha1.SpreadPreferred, valkeyiov1alpha1.SpreadPreferred))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("cannot both be Preferred"))
	})

	It("accepts node.spread.pods Preferred alone with primaries left Disabled", func() {
		Expect(k8sClient.Create(ctx, schedulingCluster("spread-pods-only", valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadPreferred))).To(Succeed())
	})

	It("accepts node.spread.primaries Required with node.spread.pods Preferred", func() {
		Expect(k8sClient.Create(ctx, schedulingCluster("spread-mixed", valkeyiov1alpha1.SpreadRequired, valkeyiov1alpha1.SpreadPreferred))).To(Succeed())
	})

	It("accepts node.spread.pods Preferred alone with primaries omitted entirely (CEL fallback treats absent field as Disabled)", func() {
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "spread-pods-only-omitted-primaries",
				Namespace: "default",
			},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:   3,
				Replicas: 1,
				Scheduling: &valkeyiov1alpha1.SchedulingSpec{
					Node: &valkeyiov1alpha1.NodeScheduling{
						Spread: valkeyiov1alpha1.NodeSpread{
							Pods: valkeyiov1alpha1.SpreadConstraint{Mode: valkeyiov1alpha1.SpreadPreferred},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
	})

	It("rejects a hostname DoNotSchedule passthrough constraint with node.spread.pods Required", func() {
		err := k8sClient.Create(ctx, schedulingClusterWithPassthrough("spread-passthrough-pods-required", corev1.LabelHostname, corev1.DoNotSchedule, valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadRequired))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("DoNotSchedule collides"))
	})

	It("rejects a hostname DoNotSchedule passthrough constraint with node.spread.primaries Required", func() {
		err := k8sClient.Create(ctx, schedulingClusterWithPassthrough("spread-passthrough-primaries-required", corev1.LabelHostname, corev1.DoNotSchedule, valkeyiov1alpha1.SpreadRequired, valkeyiov1alpha1.SpreadDisabled))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("DoNotSchedule collides"))
	})

	It("rejects a hostname ScheduleAnyway passthrough constraint with node.spread.pods Preferred", func() {
		err := k8sClient.Create(ctx, schedulingClusterWithPassthrough("spread-passthrough-pods-preferred", corev1.LabelHostname, corev1.ScheduleAnyway, valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadPreferred))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("ScheduleAnyway collides"))
	})

	It("accepts a hostname DoNotSchedule passthrough constraint with node.spread.pods Preferred (different action, no collision)", func() {
		Expect(k8sClient.Create(ctx, schedulingClusterWithPassthrough("spread-passthrough-mixed-action", corev1.LabelHostname, corev1.DoNotSchedule, valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadPreferred))).To(Succeed())
	})

	It("accepts a non-hostname passthrough constraint with node.spread.pods Required (different topologyKey, no collision)", func() {
		Expect(k8sClient.Create(ctx, schedulingClusterWithPassthrough("spread-passthrough-zone", "topology.kubernetes.io/zone", corev1.DoNotSchedule, valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadRequired))).To(Succeed())
	})

	It("accepts a hostname DoNotSchedule passthrough constraint when all node.spread modes are Disabled", func() {
		Expect(k8sClient.Create(ctx, schedulingClusterWithPassthrough("spread-passthrough-no-curated", corev1.LabelHostname, corev1.DoNotSchedule, valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadDisabled))).To(Succeed())
	})

	It("rejects primaries Required + pods Preferred with passthrough hostname entries of both actions", func() {
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "spread-passthrough-both-actions",
				Namespace: "default",
			},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:   3,
				Replicas: 1,
				Scheduling: &valkeyiov1alpha1.SchedulingSpec{
					TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
						{MaxSkew: 1, TopologyKey: corev1.LabelHostname, WhenUnsatisfiable: corev1.DoNotSchedule},
						{MaxSkew: 1, TopologyKey: corev1.LabelHostname, WhenUnsatisfiable: corev1.ScheduleAnyway},
					},
					Node: &valkeyiov1alpha1.NodeScheduling{
						Spread: valkeyiov1alpha1.NodeSpread{
							Primaries: valkeyiov1alpha1.SpreadConstraint{Mode: valkeyiov1alpha1.SpreadRequired},
							Pods:      valkeyiov1alpha1.SpreadConstraint{Mode: valkeyiov1alpha1.SpreadPreferred},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).NotTo(Succeed())
	})

	It("accepts a ValkeyCluster with no scheduling.node set at all", func() {
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "spread-defaults",
				Namespace: "default",
			},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards: 1,
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
	})
})

var _ = Describe("ValkeyClusterSpec zone.spread CEL validation", func() {
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	It("rejects zone.spread.shard and zone.spread.primaries both Required", func() {
		err := k8sClient.Create(ctx, zoneSchedulingCluster("zone-shard-primaries-required", valkeyiov1alpha1.SpreadRequired, valkeyiov1alpha1.SpreadRequired, valkeyiov1alpha1.SpreadDisabled))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("may be Required"))
	})

	It("rejects zone.spread.primaries and zone.spread.pods both Preferred", func() {
		err := k8sClient.Create(ctx, zoneSchedulingCluster("zone-primaries-pods-preferred", valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadPreferred, valkeyiov1alpha1.SpreadPreferred))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("may be Preferred"))
	})

	It("accepts one Required and one Preferred across three zone dimensions", func() {
		Expect(k8sClient.Create(ctx, zoneSchedulingCluster("zone-mixed", valkeyiov1alpha1.SpreadRequired, valkeyiov1alpha1.SpreadPreferred, valkeyiov1alpha1.SpreadDisabled))).To(Succeed())
	})

	It("accepts zone.spread.shard Required with primaries and pods omitted entirely (CEL fallback treats absent fields as Disabled)", func() {
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "zone-shard-only-omitted-rest",
				Namespace: "default",
			},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:   3,
				Replicas: 1,
				Scheduling: &valkeyiov1alpha1.SchedulingSpec{
					Zone: &valkeyiov1alpha1.ZoneScheduling{
						Spread: valkeyiov1alpha1.ZoneSpread{
							Shard:     valkeyiov1alpha1.SpreadConstraint{Mode: valkeyiov1alpha1.SpreadRequired},
							Primaries: valkeyiov1alpha1.SpreadConstraint{},
							Pods:      valkeyiov1alpha1.SpreadConstraint{},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
	})

	It("accepts zone.spread.pods Preferred with shard and primaries omitted entirely (CEL fallback treats absent fields as Disabled)", func() {
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "zone-pods-only-omitted-rest",
				Namespace: "default",
			},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:   3,
				Replicas: 1,
				Scheduling: &valkeyiov1alpha1.SchedulingSpec{
					Zone: &valkeyiov1alpha1.ZoneScheduling{
						Spread: valkeyiov1alpha1.ZoneSpread{
							Shard:     valkeyiov1alpha1.SpreadConstraint{},
							Primaries: valkeyiov1alpha1.SpreadConstraint{},
							Pods:      valkeyiov1alpha1.SpreadConstraint{Mode: valkeyiov1alpha1.SpreadPreferred},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
	})

	It("rejects a zone DoNotSchedule passthrough with zone.spread.shard Required", func() {
		err := k8sClient.Create(ctx, zoneSchedulingClusterWithPassthrough("zone-passthrough-shard-required", corev1.LabelTopologyZone, corev1.DoNotSchedule, valkeyiov1alpha1.SpreadRequired, valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadDisabled))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("DoNotSchedule collides"))
	})

	It("rejects a zone DoNotSchedule passthrough with zone.spread.primaries Required", func() {
		err := k8sClient.Create(ctx, zoneSchedulingClusterWithPassthrough("zone-passthrough-primaries-required", corev1.LabelTopologyZone, corev1.DoNotSchedule, valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadRequired, valkeyiov1alpha1.SpreadDisabled))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("DoNotSchedule collides"))
	})

	It("rejects a zone ScheduleAnyway passthrough with zone.spread.pods Preferred", func() {
		err := k8sClient.Create(ctx, zoneSchedulingClusterWithPassthrough("zone-passthrough-pods-preferred", corev1.LabelTopologyZone, corev1.ScheduleAnyway, valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadPreferred))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("ScheduleAnyway collides"))
	})

	It("accepts a hostname passthrough with zone.spread.shard Required (different topologyKey, no collision)", func() {
		Expect(k8sClient.Create(ctx, zoneSchedulingClusterWithPassthrough("zone-passthrough-hostname", corev1.LabelHostname, corev1.DoNotSchedule, valkeyiov1alpha1.SpreadRequired, valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadDisabled))).To(Succeed())
	})

	It("accepts a zone DoNotSchedule passthrough when all zone.spread modes are Disabled", func() {
		Expect(k8sClient.Create(ctx, zoneSchedulingClusterWithPassthrough("zone-passthrough-no-curated", corev1.LabelTopologyZone, corev1.DoNotSchedule, valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadDisabled))).To(Succeed())
	})

	It("accepts node.spread Required alongside a zone passthrough (independent axes)", func() {
		// Regression guard: the existing node-axis test at line ~156 sets a zone
		// passthrough + node.spread.pods Required; with only node.spread enabled
		// (zone.spread all Disabled) the zone collision rule must not fire.
		Expect(k8sClient.Create(ctx, schedulingClusterWithPassthrough("zone-independent-of-node", corev1.LabelTopologyZone, corev1.DoNotSchedule, valkeyiov1alpha1.SpreadDisabled, valkeyiov1alpha1.SpreadRequired))).To(Succeed())
	})
})
