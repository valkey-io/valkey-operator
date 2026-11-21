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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	valkeyv1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
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

func TestServerProbesOverrideOneDefaultPerField(t *testing.T) {
	cases := []struct {
		name   string
		probes valkeyv1.ProbesSpec
		want   func(startup, liveness, readiness *corev1.Probe) (s, l, r *corev1.Probe)
	}{
		{
			name:   "startup initialDelaySeconds keeps the default window",
			probes: valkeyv1.ProbesSpec{Startup: &valkeyv1.StartupProbeSpec{InitialDelaySeconds: new(int32(0))}},
			want: func(s, l, r *corev1.Probe) (*corev1.Probe, *corev1.Probe, *corev1.Probe) {
				s.InitialDelaySeconds = 0
				s.FailureThreshold = 21
				return s, l, r
			},
		},
		{
			name:   "startup periodSeconds keeps the default window",
			probes: valkeyv1.ProbesSpec{Startup: &valkeyv1.StartupProbeSpec{PeriodSeconds: new(int32(10))}},
			want: func(s, l, r *corev1.Probe) (*corev1.Probe, *corev1.Probe, *corev1.Probe) {
				s.PeriodSeconds = 10
				s.FailureThreshold = 11
				return s, l, r
			},
		},
		{
			name:   "startup timeoutSeconds",
			probes: valkeyv1.ProbesSpec{Startup: &valkeyv1.StartupProbeSpec{TimeoutSeconds: new(int32(10))}},
			want: func(s, l, r *corev1.Probe) (*corev1.Probe, *corev1.Probe, *corev1.Probe) {
				s.Exec.Command = []string{shellPath, "-c", "/scripts/liveness-check.sh 10"}
				s.TimeoutSeconds = 11
				return s, l, r
			},
		},
		{
			name:   "startup failureThreshold",
			probes: valkeyv1.ProbesSpec{Startup: &valkeyv1.StartupProbeSpec{FailureThreshold: new(int32(60))}},
			want: func(s, l, r *corev1.Probe) (*corev1.Probe, *corev1.Probe, *corev1.Probe) {
				s.FailureThreshold = 60
				return s, l, r
			},
		},
		{
			name:   "liveness periodSeconds",
			probes: valkeyv1.ProbesSpec{Liveness: &valkeyv1.LivenessProbeSpec{PeriodSeconds: new(int32(10))}},
			want: func(s, l, r *corev1.Probe) (*corev1.Probe, *corev1.Probe, *corev1.Probe) {
				l.PeriodSeconds = 10
				return s, l, r
			},
		},
		{
			name:   "liveness timeoutSeconds",
			probes: valkeyv1.ProbesSpec{Liveness: &valkeyv1.LivenessProbeSpec{TimeoutSeconds: new(int32(8))}},
			want: func(s, l, r *corev1.Probe) (*corev1.Probe, *corev1.Probe, *corev1.Probe) {
				l.Exec.Command = []string{shellPath, "-c", "/scripts/liveness-check.sh 8"}
				l.TimeoutSeconds = 9
				return s, l, r
			},
		},
		{
			name:   "liveness failureThreshold",
			probes: valkeyv1.ProbesSpec{Liveness: &valkeyv1.LivenessProbeSpec{FailureThreshold: new(int32(12))}},
			want: func(s, l, r *corev1.Probe) (*corev1.Probe, *corev1.Probe, *corev1.Probe) {
				l.FailureThreshold = 12
				return s, l, r
			},
		},
		{
			name: "liveness failureThreshold with mode Enabled",
			probes: valkeyv1.ProbesSpec{Liveness: &valkeyv1.LivenessProbeSpec{
				Mode: valkeyv1.LivenessProbeModeEnabled, FailureThreshold: new(int32(7)),
			}},
			want: func(s, l, r *corev1.Probe) (*corev1.Probe, *corev1.Probe, *corev1.Probe) {
				l.FailureThreshold = 7
				return s, l, r
			},
		},
		{
			name:   "liveness mode Disabled removes only the liveness probe",
			probes: valkeyv1.ProbesSpec{Liveness: &valkeyv1.LivenessProbeSpec{Mode: valkeyv1.LivenessProbeModeDisabled}},
			want: func(s, _, r *corev1.Probe) (*corev1.Probe, *corev1.Probe, *corev1.Probe) {
				return s, nil, r
			},
		},
		{
			name:   "readiness periodSeconds",
			probes: valkeyv1.ProbesSpec{Readiness: &valkeyv1.ReadinessProbeSpec{PeriodSeconds: new(int32(10))}},
			want: func(s, l, r *corev1.Probe) (*corev1.Probe, *corev1.Probe, *corev1.Probe) {
				r.PeriodSeconds = 10
				return s, l, r
			},
		},
		{
			name:   "readiness timeoutSeconds",
			probes: valkeyv1.ProbesSpec{Readiness: &valkeyv1.ReadinessProbeSpec{TimeoutSeconds: new(int32(3))}},
			want: func(s, l, r *corev1.Probe) (*corev1.Probe, *corev1.Probe, *corev1.Probe) {
				r.Exec.Command = []string{shellPath, "-c", "/scripts/readiness-check.sh 3"}
				r.TimeoutSeconds = 4
				return s, l, r
			},
		},
		{
			name:   "readiness successThreshold",
			probes: valkeyv1.ProbesSpec{Readiness: &valkeyv1.ReadinessProbeSpec{SuccessThreshold: new(int32(2))}},
			want: func(s, l, r *corev1.Probe) (*corev1.Probe, *corev1.Probe, *corev1.Probe) {
				r.SuccessThreshold = 2
				return s, l, r
			},
		},
		{
			name:   "readiness failureThreshold",
			probes: valkeyv1.ProbesSpec{Readiness: &valkeyv1.ReadinessProbeSpec{FailureThreshold: new(int32(3))}},
			want: func(s, l, r *corev1.Probe) (*corev1.Probe, *corev1.Probe, *corev1.Probe) {
				r.FailureThreshold = 3
				return s, l, r
			},
		},
	}
	// A persistent node, which does not run primary-handoff.sh;
	// TestStartupFailureThresholdFromSpec covers one that does.
	defaultRevision, err := computeWorkloadRevision(probeTestNode(true, nil))
	require.NoError(t, err)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := probeTestNode(true, &tc.probes)
			wantStartup, wantLiveness, wantReadiness := tc.want(legacyServerProbes(false))
			server := renderedServer(t, node)
			assert.Equal(t, wantStartup, server.StartupProbe)
			assert.Equal(t, wantLiveness, server.LivenessProbe)
			assert.Equal(t, wantReadiness, server.ReadinessProbe)

			revision, err := computeWorkloadRevision(node)
			require.NoError(t, err)
			assert.NotEqual(t, defaultRevision, revision, "a spec.probes change must change the pod template so the pods roll")
		})
	}
}

func TestStartupFailureThresholdFromSpec(t *testing.T) {
	cases := []struct {
		name       string
		persistent bool
		delay      *int32
		period     *int32
		threshold  *int32
		want       int32
	}{
		{name: "the default covers 400s where primary-handoff.sh runs", want: 80},
		{name: "a set threshold is applied as is", threshold: new(int32(10)), want: 10},
		{name: "a higher threshold is applied as is", threshold: new(int32(100)), want: 100},
		{name: "a set threshold does not follow the period", period: new(int32(1)), threshold: new(int32(30)), want: 30},
		{name: "the default keeps 400s at a longer period", period: new(int32(10)), want: 41},
		{name: "the default keeps 400s at a shorter period", period: new(int32(1)), want: 396},
		{name: "the default rounds up to cover 400s", period: new(int32(7)), want: 58},
		{name: "the default waits past the hand-off at a long period", period: new(int32(250)), want: 3},
		{name: "the default is two checks for a period past 400s", period: new(int32(600)), want: 2},
		{name: "the initial delay counts toward 400s", delay: new(int32(300)), want: 21},
		{name: "one check after an initial delay past 400s", delay: new(int32(500)), want: 1},
		{name: "the default covers 100s with persistence", persistent: true, want: 20},
		{name: "the default keeps 100s at a longer period", persistent: true, period: new(int32(10)), want: 11},
		{name: "the default rounds up to cover 100s", persistent: true, period: new(int32(7)), want: 15},
		{name: "a set threshold is applied as is with persistence", persistent: true, threshold: new(int32(10)), want: 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := probeTestNode(tc.persistent, &valkeyv1.ProbesSpec{Startup: &valkeyv1.StartupProbeSpec{
				InitialDelaySeconds: tc.delay, PeriodSeconds: tc.period, FailureThreshold: tc.threshold,
			}})
			assert.Equal(t, tc.want, renderedServer(t, node).StartupProbe.FailureThreshold)
		})
	}
}

func TestStartupDefaultThresholdIsTheSmallestCoveringTheHandoffWindow(t *testing.T) {
	for _, delay := range []int32{0, 5, 300, 399, 400, 600} {
		for period := int32(1); period <= 450; period++ {
			d, p := delay, period
			got := startupFailureThreshold(&valkeyv1.StartupProbeSpec{InitialDelaySeconds: &d, PeriodSeconds: &p}, true)
			require.GreaterOrEqual(t, startupWindowSeconds(d, p, got), int64(handoffStartupWindowSeconds),
				"delay %ds, period %ds: threshold %d gives up before the hand-off window", d, p, got)
			if got > 1 {
				require.Less(t, startupWindowSeconds(d, p, got-1), int64(handoffStartupWindowSeconds),
					"delay %ds, period %ds: threshold %d is not the smallest", d, p, got)
			}
		}
	}
}

func TestStartupProbeConfigWarnings(t *testing.T) {
	cluster := func(persistent bool, startup *valkeyv1.StartupProbeSpec) *valkeyv1.ValkeyCluster {
		c := &valkeyv1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
			Spec:       valkeyv1.ValkeyClusterSpec{Shards: 1},
		}
		if startup != nil {
			c.Spec.Probes = &valkeyv1.ProbesSpec{Startup: startup}
		}
		if persistent {
			c.Spec.Persistence = &valkeyv1.PersistenceSpec{Size: resource.MustParse("1Gi")}
		}
		return c
	}

	t.Run("the message says why and what to set", func(t *testing.T) {
		warnings := startupProbeConfigWarnings(cluster(false, &valkeyv1.StartupProbeSpec{FailureThreshold: new(int32(10))}))
		require.Len(t, warnings, 1)
		assert.Equal(t, valkeyv1.ReasonStartupProbeTooShort, warnings[0].reason)
		assert.Equal(t, "spec.probes.startup gives up after 50s (initialDelaySeconds 5 + (failureThreshold 10 - 1) x periodSeconds 5s), below the recommended 400s without persistence: "+
			"a restarted primary can wait up to 300s for a replica hand-off before the server starts, and a shorter startup probe restarts the container into the same wait; "+
			"set failureThreshold to at least 80, or leave it unset", warnings[0].message)
	})

	cases := []struct {
		name       string
		persistent bool
		startup    *valkeyv1.StartupProbeSpec
		atLeast    int // the failureThreshold the warning asks for; 0 for no warning
	}{
		{name: "no spec.probes"},
		{name: "a period with the default threshold", startup: &valkeyv1.StartupProbeSpec{PeriodSeconds: new(int32(10))}},
		{name: "exactly 400s", startup: &valkeyv1.StartupProbeSpec{FailureThreshold: new(int32(80))}},
		{name: "one period short of 400s", startup: &valkeyv1.StartupProbeSpec{FailureThreshold: new(int32(79))}, atLeast: 80},
		{name: "a longer period", startup: &valkeyv1.StartupProbeSpec{PeriodSeconds: new(int32(10)), FailureThreshold: new(int32(30))}, atLeast: 41},
		{name: "a period that does not divide 400", startup: &valkeyv1.StartupProbeSpec{PeriodSeconds: new(int32(7)), FailureThreshold: new(int32(57))}, atLeast: 58},
		{name: "forty checks at 10s give up at 395s", startup: &valkeyv1.StartupProbeSpec{PeriodSeconds: new(int32(10)), FailureThreshold: new(int32(40))}, atLeast: 41},
		{name: "raised to cover 400s", startup: &valkeyv1.StartupProbeSpec{PeriodSeconds: new(int32(10)), FailureThreshold: new(int32(41))}},
		{name: "two checks at 250s give up before the hand-off", startup: &valkeyv1.StartupProbeSpec{PeriodSeconds: new(int32(250)), FailureThreshold: new(int32(2))}, atLeast: 3},
		{name: "the initial delay counts toward 400s", startup: &valkeyv1.StartupProbeSpec{InitialDelaySeconds: new(int32(300)), FailureThreshold: new(int32(21))}},
		{name: "one check short after an initial delay", startup: &valkeyv1.StartupProbeSpec{InitialDelaySeconds: new(int32(300)), FailureThreshold: new(int32(20))}, atLeast: 21},
		{name: "persistence runs no hand-off", persistent: true, startup: &valkeyv1.StartupProbeSpec{FailureThreshold: new(int32(1))}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := cluster(tc.persistent, tc.startup)
			require.Equal(t, !tc.persistent, usesPrimaryHandoffScript(buildClusterValkeyNode(c, 0, 0)),
				"the warning must follow the nodes that run primary-handoff.sh")
			warnings := startupProbeConfigWarnings(c)
			if tc.atLeast == 0 {
				assert.Empty(t, warnings)
				return
			}
			require.Len(t, warnings, 1)
			assert.Equal(t, valkeyv1.ReasonStartupProbeTooShort, warnings[0].reason)
			assert.Contains(t, warnings[0].message, fmt.Sprintf("set failureThreshold to at least %d,", tc.atLeast))
		})
	}
}

func TestContainersPatchesApplyAfterSpecProbes(t *testing.T) {
	withPatch := func(probes *valkeyv1.ProbesSpec, server corev1.Container) *valkeyv1.ValkeyNode {
		node := probeTestNode(true, probes)
		server.Name = serverContainerName
		node.Spec.Containers = []corev1.Container{server}
		return node
	}

	t.Run("a patched field wins over spec.probes", func(t *testing.T) {
		node := withPatch(
			&valkeyv1.ProbesSpec{Liveness: &valkeyv1.LivenessProbeSpec{FailureThreshold: new(int32(7)), PeriodSeconds: new(int32(10))}},
			corev1.Container{LivenessProbe: &corev1.Probe{FailureThreshold: 9}},
		)
		_, want, _ := legacyServerProbes(false)
		want.PeriodSeconds = 10
		want.FailureThreshold = 9
		assert.Equal(t, want, renderedServer(t, node).LivenessProbe)
	})

	t.Run("a patch adds to the fields spec.probes sets", func(t *testing.T) {
		node := withPatch(
			&valkeyv1.ProbesSpec{Readiness: &valkeyv1.ReadinessProbeSpec{TimeoutSeconds: new(int32(3))}},
			corev1.Container{ReadinessProbe: &corev1.Probe{PeriodSeconds: 20}},
		)
		_, _, want := legacyServerProbes(false)
		want.Exec.Command = []string{shellPath, "-c", "/scripts/readiness-check.sh 3"}
		want.TimeoutSeconds = 4
		want.PeriodSeconds = 20
		assert.Equal(t, want, renderedServer(t, node).ReadinessProbe)
	})

	t.Run("with liveness disabled a patch supplies its own handler", func(t *testing.T) {
		node := withPatch(
			&valkeyv1.ProbesSpec{Liveness: &valkeyv1.LivenessProbeSpec{Mode: valkeyv1.LivenessProbeModeDisabled}},
			corev1.Container{LivenessProbe: &corev1.Probe{
				ProbeHandler:  corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(DefaultPort)}},
				PeriodSeconds: 10,
			}},
		)
		got := renderedServer(t, node).LivenessProbe
		require.NotNil(t, got)
		assert.Nil(t, got.Exec, "the operator's check must not be merged into the user's handler")
		require.NotNil(t, got.TCPSocket)
		assert.Equal(t, int32(10), got.PeriodSeconds)
	})
}

// The probe command is run by a shell, so the timeout must reach the script
// as its first argument, not as the shell's $0.
func TestProbeCommandPassesTheTimeoutToTheScript(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	stub := "#!/bin/sh\nprintf '%s:%s' \"$(basename \"$0\")\" \"$*\" > \"" + argsFile + "\"\n"
	for _, name := range []string{livenessScriptKey, readinessScriptKey} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(stub), 0o755))
	}
	run := func(probe *corev1.Probe) string {
		t.Helper()
		require.NotNil(t, probe)
		command := make([]string, len(probe.Exec.Command))
		for i, arg := range probe.Exec.Command {
			command[i] = strings.ReplaceAll(arg, scriptsMountPath+"/", dir+"/")
		}
		out, err := exec.Command(command[0], command[1:]...).CombinedOutput()
		require.NoError(t, err, string(out))
		got, err := os.ReadFile(argsFile)
		require.NoError(t, err)
		return string(got)
	}

	set := probeTestNode(false, &valkeyv1.ProbesSpec{
		Startup:   &valkeyv1.StartupProbeSpec{TimeoutSeconds: new(int32(6))},
		Liveness:  &valkeyv1.LivenessProbeSpec{TimeoutSeconds: new(int32(7))},
		Readiness: &valkeyv1.ReadinessProbeSpec{TimeoutSeconds: new(int32(3))},
	})
	server := renderedServer(t, set)
	assert.Equal(t, livenessScriptKey+":6", run(server.StartupProbe))
	assert.Equal(t, livenessScriptKey+":7", run(server.LivenessProbe))
	assert.Equal(t, readinessScriptKey+":3", run(server.ReadinessProbe))

	unset := renderedServer(t, probeTestNode(false, nil))
	assert.Equal(t, livenessScriptKey+":", run(unset.StartupProbe), "an unset timeout leaves the script on its default")
	assert.Equal(t, livenessScriptKey+":", run(unset.LivenessProbe))
	assert.Equal(t, readinessScriptKey+":", run(unset.ReadinessProbe))
}

// The script timeouts the operator assumes when it passes none must be the
// defaults the scripts use.
func TestProbeScriptDefaultTimeouts(t *testing.T) {
	for script, want := range map[string]int{
		livenessScriptKey:  defaultLivenessCheckTimeoutSeconds,
		readinessScriptKey: defaultReadinessCheckTimeoutSeconds,
	} {
		body, err := scripts.ReadFile("scripts/" + script)
		require.NoError(t, err)
		assert.Contains(t, string(body), fmt.Sprintf(`timeout=${1:-"%d"}`, want), script)
	}
}

func TestBuildClusterValkeyNodeCopiesProbes(t *testing.T) {
	probes := &valkeyv1.ProbesSpec{
		Startup:   &valkeyv1.StartupProbeSpec{FailureThreshold: new(int32(60))},
		Liveness:  &valkeyv1.LivenessProbeSpec{Mode: valkeyv1.LivenessProbeModeDisabled},
		Readiness: &valkeyv1.ReadinessProbeSpec{PeriodSeconds: new(int32(10)), TimeoutSeconds: new(int32(3))},
	}
	cluster := &valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
		Spec:       valkeyv1.ValkeyClusterSpec{Shards: 2, ReplicasPerShard: 1, Probes: probes},
	}
	for _, position := range [][2]int{{0, 0}, {0, 1}, {1, 0}, {1, 1}} {
		node := buildClusterValkeyNode(cluster, position[0], position[1])
		assert.Equal(t, probes, node.Spec.Probes, "node %d-%d", position[0], position[1])
	}

	cluster.Spec.Probes = nil
	assert.Nil(t, buildClusterValkeyNode(cluster, 0, 0).Spec.Probes,
		"a cluster without spec.probes must leave the node field unset, so the stored ValkeyNode does not change")
}

// The cluster controller hashes the node it builds before the owner reference
// is set and the node controller hashes the stored node; the startup
// threshold depends on the node being a cluster member, so both must agree.
func TestWorkloadRevisionParityWithProbes(t *testing.T) {
	desired := probeTestNode(false, &valkeyv1.ProbesSpec{Startup: &valkeyv1.StartupProbeSpec{FailureThreshold: new(int32(10))}})
	before, err := computeWorkloadRevision(desired)
	require.NoError(t, err)
	stored := desired.DeepCopy()
	stored.OwnerReferences = []metav1.OwnerReference{{Kind: "ValkeyCluster", Name: "c", Controller: new(true)}}
	after, err := computeWorkloadRevision(stored)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}
