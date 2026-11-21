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

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	valkeyv1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// legacyServerProbes is a frozen, self-contained copy of the server
// container's probes as the operator rendered them before spec.probes. It
// pins the upgrade contract: a node without spec.probes must render exactly
// these, or an operator upgrade rolls every pod.
//
// Do NOT update this copy to track production code. Change it only when a pod
// roll on operator upgrade is intended and understood.
func legacyServerProbes(primaryHandoff bool) (startup, liveness, readiness *corev1.Probe) {
	check := func(script string) corev1.ProbeHandler {
		return corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"/bin/sh", "-c", script}}}
	}
	startupFailures := int32(20)
	if primaryHandoff {
		startupFailures = 80
	}
	startup = &corev1.Probe{
		ProbeHandler:        check("/scripts/liveness-check.sh"),
		InitialDelaySeconds: 5,
		TimeoutSeconds:      5,
		PeriodSeconds:       5,
		SuccessThreshold:    1,
		FailureThreshold:    startupFailures,
	}
	liveness = &corev1.Probe{
		ProbeHandler:     check("/scripts/liveness-check.sh"),
		TimeoutSeconds:   5,
		PeriodSeconds:    5,
		SuccessThreshold: 1,
		FailureThreshold: 5,
	}
	readiness = &corev1.Probe{
		ProbeHandler:     check("/scripts/readiness-check.sh"),
		TimeoutSeconds:   2,
		PeriodSeconds:    5,
		SuccessThreshold: 1,
		FailureThreshold: 5,
	}
	return startup, liveness, readiness
}

// probeTestNode builds node 0-0 of a cluster with the given probes, as the
// cluster controller does. A persistent cluster does not run primary-handoff.sh.
func probeTestNode(persistent bool, probes *valkeyv1.ProbesSpec) *valkeyv1.ValkeyNode {
	cluster := &valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
		Spec:       valkeyv1.ValkeyClusterSpec{Shards: 1, ReplicasPerShard: 1, Probes: probes},
	}
	if persistent {
		cluster.Spec.Persistence = &valkeyv1.PersistenceSpec{Size: resource.MustParse("1Gi")}
	}
	return buildClusterValkeyNode(cluster, 0, 0)
}

func renderedServer(t *testing.T, node *valkeyv1.ValkeyNode) corev1.Container {
	t.Helper()
	containers, err := buildContainersDef(node)
	require.NoError(t, err)
	require.Equal(t, serverContainerName, containers[0].Name)
	return containers[0]
}

func TestServerProbesWithoutSpecProbesMatchTheLegacyRender(t *testing.T) {
	nodes := []struct {
		name    string
		node    func() *valkeyv1.ValkeyNode
		handoff bool
	}{
		{"cluster node without persistence", func() *valkeyv1.ValkeyNode { return probeTestNode(false, nil) }, true},
		{"cluster node with persistence", func() *valkeyv1.ValkeyNode { return probeTestNode(true, nil) }, false},
		{"standalone node", func() *valkeyv1.ValkeyNode { return newTestValkeyNode("n", "ns") }, false},
	}
	empty := []struct {
		name   string
		probes *valkeyv1.ProbesSpec
	}{
		{"empty probes", &valkeyv1.ProbesSpec{}},
		{"empty probe entries", &valkeyv1.ProbesSpec{
			Startup:   &valkeyv1.StartupProbeSpec{},
			Liveness:  &valkeyv1.LivenessProbeSpec{},
			Readiness: &valkeyv1.ReadinessProbeSpec{},
		}},
		{"liveness mode Enabled", &valkeyv1.ProbesSpec{Liveness: &valkeyv1.LivenessProbeSpec{Mode: valkeyv1.LivenessProbeModeEnabled}}},
	}
	for _, n := range nodes {
		t.Run(n.name, func(t *testing.T) {
			node := n.node()
			require.Nil(t, node.Spec.Probes)
			wantStartup, wantLiveness, wantReadiness := legacyServerProbes(n.handoff)
			server := renderedServer(t, node)
			assert.Equal(t, wantStartup, server.StartupProbe)
			assert.Equal(t, wantLiveness, server.LivenessProbe)
			assert.Equal(t, wantReadiness, server.ReadinessProbe)

			want, err := computeWorkloadRevision(node)
			require.NoError(t, err)
			for _, e := range empty {
				withEmpty := node.DeepCopy()
				withEmpty.Spec.Probes = e.probes
				got, err := computeWorkloadRevision(withEmpty)
				require.NoError(t, err)
				assert.Equal(t, want, got, "%s must render the same pod template as no spec.probes", e.name)
			}
		})
	}
}
