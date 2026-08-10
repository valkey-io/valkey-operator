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

// zonePinningCluster builds a minimal ValkeyCluster with zone.pinning set, for
// exercising the pinning CEL validation on ValkeyClusterSpec.
func zonePinningCluster(name string, zones []string) *valkeyiov1alpha1.ValkeyCluster {
	return &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{
			Shards:   3,
			Replicas: 1,
			Scheduling: &valkeyiov1alpha1.SchedulingSpec{
				Zone: &valkeyiov1alpha1.ZoneScheduling{
					Pinning: &valkeyiov1alpha1.ZonePinning{Zones: zones},
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

var _ = Describe("ValkeyClusterSpec zone.pinning CEL validation", func() {
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	It("accepts pinning with a unique ordered zone list", func() {
		Expect(k8sClient.Create(ctx, zonePinningCluster("pin-valid", []string{"az1", "az2", "az3"}))).To(Succeed())
	})

	It("accepts a single-zone pinning list", func() {
		Expect(k8sClient.Create(ctx, zonePinningCluster("pin-single", []string{"az1"}))).To(Succeed())
	})

	It("rejects duplicate zones", func() {
		err := k8sClient.Create(ctx, zonePinningCluster("pin-dupes", []string{"az1", "az1", "az2"}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("must be unique"))
	})

	It("rejects an empty zone list", func() {
		err := k8sClient.Create(ctx, zonePinningCluster("pin-empty", []string{}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("should have at least 1 items"))
	})

	It("rejects an empty-string zone entry", func() {
		err := k8sClient.Create(ctx, zonePinningCluster("pin-empty-entry", []string{"az1", ""}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("should be at least 1 chars long"))
	})

	// A zone entry is rendered as a topology.kubernetes.io/zone nodeSelector
	// value, so anything Kubernetes rejects as a label value has to be caught
	// here. Otherwise it is only rejected when the StatefulSet is created, three
	// layers below the ValkeyCluster the user edited.
	It("rejects a zone entry that is not a valid label value", func() {
		err := k8sClient.Create(ctx, zonePinningCluster("pin-invalid-label", []string{"eu-west-1a!"}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("should match"))
	})

	It("accepts zone entries using the full label-value charset", func() {
		Expect(k8sClient.Create(ctx, zonePinningCluster("pin-label-charset", []string{"eu-west-1a", "zone_b.2", "c"}))).To(Succeed())
	})

	It("rejects pinning alongside a non-Disabled zone.spread.shard", func() {
		cluster := zonePinningCluster("pin-with-zone-spread", []string{"az1", "az2"})
		cluster.Spec.Scheduling.Zone.Spread.Shard = valkeyiov1alpha1.SpreadConstraint{Mode: valkeyiov1alpha1.SpreadPreferred}
		err := k8sClient.Create(ctx, cluster)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("zone.pinning cannot be combined"))
	})

	It("rejects pinning alongside a Required zone.spread.pods", func() {
		cluster := zonePinningCluster("pin-with-zone-pods", []string{"az1", "az2"})
		cluster.Spec.Scheduling.Zone.Spread.Pods = valkeyiov1alpha1.SpreadConstraint{Mode: valkeyiov1alpha1.SpreadRequired}
		err := k8sClient.Create(ctx, cluster)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("zone.pinning cannot be combined"))
	})

	It("accepts pinning with all zone.spread modes explicitly Disabled", func() {
		cluster := zonePinningCluster("pin-zone-spread-disabled", []string{"az1", "az2"})
		cluster.Spec.Scheduling.Zone.Spread = valkeyiov1alpha1.ZoneSpread{
			Shard:     valkeyiov1alpha1.SpreadConstraint{Mode: valkeyiov1alpha1.SpreadDisabled},
			Primaries: valkeyiov1alpha1.SpreadConstraint{Mode: valkeyiov1alpha1.SpreadDisabled},
			Pods:      valkeyiov1alpha1.SpreadConstraint{Mode: valkeyiov1alpha1.SpreadDisabled},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
	})

	It("accepts pinning alongside node.spread (independent axis)", func() {
		cluster := zonePinningCluster("pin-with-node-spread", []string{"az1", "az2"})
		cluster.Spec.Scheduling.Node = &valkeyiov1alpha1.NodeScheduling{
			Spread: valkeyiov1alpha1.NodeSpread{
				Shard: valkeyiov1alpha1.SpreadConstraint{Mode: valkeyiov1alpha1.SpreadRequired},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
	})

	It("rejects changing the zone list while pinning is set", func() {
		cluster := zonePinningCluster("pin-immutable", []string{"az1", "az2"})
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		cluster.Spec.Scheduling.Zone.Pinning.Zones = []string{"az2", "az1"}
		err := k8sClient.Update(ctx, cluster)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("zone.pinning.zones is immutable"))
	})

	It("accepts removing pinning, then re-adding it with a different list", func() {
		cluster := zonePinningCluster("pin-two-step", []string{"az1", "az2"})
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		cluster.Spec.Scheduling.Zone.Pinning = nil
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

		cluster.Spec.Scheduling.Zone.Pinning = &valkeyiov1alpha1.ZonePinning{Zones: []string{"az3", "az4"}}
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
	})

	It("rejects a nodeSelector that sets the zone key while pinning is set", func() {
		cluster := zonePinningCluster("pin-nodeselector-collision", []string{"az1", "az2"})
		cluster.Spec.Scheduling.NodeSelector = map[string]string{"topology.kubernetes.io/zone": "az1"}
		err := k8sClient.Create(ctx, cluster)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("nodeSelector cannot set topology.kubernetes.io/zone"))
	})

	It("accepts a nodeSelector that sets the zone key when pinning is unset", func() {
		cluster := zonePinningCluster("pin-nodeselector-no-pinning", []string{"az1"})
		cluster.Spec.Scheduling.Zone.Pinning = nil
		cluster.Spec.Scheduling.NodeSelector = map[string]string{"topology.kubernetes.io/zone": "az1"}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
	})

	It("accepts a non-zone nodeSelector alongside pinning", func() {
		cluster := zonePinningCluster("pin-nodeselector-other-key", []string{"az1", "az2"})
		cluster.Spec.Scheduling.NodeSelector = map[string]string{"node.kubernetes.io/instance-type": "m6i.xlarge"}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
	})
})
