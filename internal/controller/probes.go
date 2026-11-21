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

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

const (
	livenessCheckScript  = scriptsMountPath + "/" + livenessScriptKey
	readinessCheckScript = scriptsMountPath + "/" + readinessScriptKey

	// Defaults of the server container's probes. spec.probes overrides them
	// field by field.
	defaultProbePeriodSeconds         = 5
	defaultStartupInitialDelaySeconds = 5
	defaultLivenessFailureThreshold   = 5
	defaultReadinessFailureThreshold  = 5
	defaultReadinessSuccessThreshold  = 1

	// The timeouts liveness-check.sh and readiness-check.sh use when the
	// command passes none.
	defaultLivenessCheckTimeoutSeconds  = 4
	defaultReadinessCheckTimeoutSeconds = 1

	// defaultStartupWindowSeconds is how long an unset startup
	// failureThreshold gives the server to answer: 20 checks at the default
	// period.
	defaultStartupWindowSeconds = 20 * defaultProbePeriodSeconds
	// primaryHandoffMaxWaitSeconds is the longest primary-handoff.sh holds
	// the server back: six cluster-node-timeouts, at most 300s.
	primaryHandoffMaxWaitSeconds = 300
	// handoffStartupWindowSeconds is the startup window where
	// primary-handoff.sh runs: the wait plus the default window.
	handoffStartupWindowSeconds = primaryHandoffMaxWaitSeconds + defaultStartupWindowSeconds
)

// serverProbes returns the startup, liveness and readiness probes of the
// server container. The liveness probe is nil when spec.probes.liveness.mode
// is Disabled.
func serverProbes(node *valkeyiov1alpha1.ValkeyNode) (startup, liveness, readiness *corev1.Probe) {
	spec := probesOrEmpty(node.Spec.Probes)

	handler, timeoutSeconds := execCheckAndTimeout(livenessCheckScript, spec.Startup.TimeoutSeconds, defaultLivenessCheckTimeoutSeconds)
	startup = &corev1.Probe{
		ProbeHandler:        handler,
		InitialDelaySeconds: orDefault(spec.Startup.InitialDelaySeconds, defaultStartupInitialDelaySeconds),
		TimeoutSeconds:      timeoutSeconds,
		PeriodSeconds:       orDefault(spec.Startup.PeriodSeconds, defaultProbePeriodSeconds),
		SuccessThreshold:    1,
		FailureThreshold:    startupFailureThreshold(spec.Startup, usesPrimaryHandoffScript(node)),
	}

	// Any mode but Disabled keeps the probe, so an operator that does not
	// know a newer mode fails safe.
	if spec.Liveness.Mode != valkeyiov1alpha1.LivenessProbeModeDisabled {
		handler, timeoutSeconds = execCheckAndTimeout(livenessCheckScript, spec.Liveness.TimeoutSeconds, defaultLivenessCheckTimeoutSeconds)
		liveness = &corev1.Probe{
			ProbeHandler:     handler,
			TimeoutSeconds:   timeoutSeconds,
			PeriodSeconds:    orDefault(spec.Liveness.PeriodSeconds, defaultProbePeriodSeconds),
			SuccessThreshold: 1,
			FailureThreshold: orDefault(spec.Liveness.FailureThreshold, defaultLivenessFailureThreshold),
		}
	}

	handler, timeoutSeconds = execCheckAndTimeout(readinessCheckScript, spec.Readiness.TimeoutSeconds, defaultReadinessCheckTimeoutSeconds)
	readiness = &corev1.Probe{
		ProbeHandler:     handler,
		TimeoutSeconds:   timeoutSeconds,
		PeriodSeconds:    orDefault(spec.Readiness.PeriodSeconds, defaultProbePeriodSeconds),
		SuccessThreshold: orDefault(spec.Readiness.SuccessThreshold, defaultReadinessSuccessThreshold),
		FailureThreshold: orDefault(spec.Readiness.FailureThreshold, defaultReadinessFailureThreshold),
	}
	return startup, liveness, readiness
}

// execCheckAndTimeout returns the handler that runs script, and the probe
// timeout. A timeout from spec.probes is passed to the script, and the probe
// gets one second more, so the script gives up before the kubelet does.
// Without one the command passes none and the script keeps its default: the
// command of a cluster that does not set spec.probes must not change, or an
// operator upgrade rolls its pods.
func execCheckAndTimeout(script string, timeoutSeconds *int32, scriptDefaultSeconds int32) (corev1.ProbeHandler, int32) {
	command, scriptTimeoutSeconds := script, scriptDefaultSeconds
	if timeoutSeconds != nil {
		scriptTimeoutSeconds = *timeoutSeconds
		command = fmt.Sprintf("%s %d", script, scriptTimeoutSeconds)
	}
	return corev1.ProbeHandler{
		Exec: &corev1.ExecAction{Command: []string{shellPath, "-c", command}},
	}, scriptTimeoutSeconds + 1
}

// startupFailureThreshold returns the failure threshold of the startup probe.
// A value set in spec.probes is used as is. Unset, it is the number of checks
// at the probe's period that cover the default window: 100s, or 400s where
// primary-handoff.sh runs, since the script can hold the server back for up
// to 300s for a replica hand-off. At the default period of 5s that is 20 or
// 80. The window does not depend on cluster-node-timeout, which the script
// reads from the config, so a change of it never rolls the pod.
func startupFailureThreshold(spec *valkeyiov1alpha1.StartupProbeSpec, primaryHandoff bool) int32 {
	if spec.FailureThreshold != nil {
		return *spec.FailureThreshold
	}
	windowSeconds := int64(defaultStartupWindowSeconds)
	if primaryHandoff {
		windowSeconds = handoffStartupWindowSeconds
	}
	return int32(checksCovering(windowSeconds, orDefault(spec.PeriodSeconds, defaultProbePeriodSeconds)))
}

// startupProbeConfigWarnings warns when the startup probe of a cluster
// without persistence, whose nodes start through primary-handoff.sh, gives
// up within handoffStartupWindowSeconds: a restarted primary can wait up to
// 300s for a replica hand-off before the server starts, and a shorter
// startup probe restarts the container into the same wait. The value is
// still applied.
func startupProbeConfigWarnings(cluster *valkeyiov1alpha1.ValkeyCluster) []configWarning {
	if cluster.Spec.Persistence != nil {
		return nil
	}
	spec := probesOrEmpty(cluster.Spec.Probes).Startup
	threshold := startupFailureThreshold(spec, true)
	periodSeconds := orDefault(spec.PeriodSeconds, defaultProbePeriodSeconds)
	windowSeconds := int64(threshold) * int64(periodSeconds)
	if windowSeconds >= handoffStartupWindowSeconds {
		return nil
	}
	minThreshold := checksCovering(handoffStartupWindowSeconds, periodSeconds)
	return []configWarning{{
		reason: valkeyiov1alpha1.ReasonStartupProbeTooShort,
		message: fmt.Sprintf("spec.probes.startup gives up after %ds (failureThreshold %d x periodSeconds %ds), below the recommended %ds without persistence: "+
			"a restarted primary can wait up to %ds for a replica hand-off before the server starts, and a shorter startup probe restarts the container into the same wait; "+
			"set failureThreshold to at least %d, or leave it unset",
			windowSeconds, threshold, periodSeconds, handoffStartupWindowSeconds, primaryHandoffMaxWaitSeconds, minThreshold),
	}}
}

// checksCovering returns how many checks at periodSeconds cover windowSeconds.
func checksCovering(windowSeconds int64, periodSeconds int32) int64 {
	period := int64(max(periodSeconds, 1))
	return (windowSeconds + period - 1) / period
}

// probesOrEmpty returns probes with every probe set, empty where unset.
func probesOrEmpty(probes *valkeyiov1alpha1.ProbesSpec) valkeyiov1alpha1.ProbesSpec {
	var out valkeyiov1alpha1.ProbesSpec
	if probes != nil {
		out = *probes
	}
	if out.Startup == nil {
		out.Startup = &valkeyiov1alpha1.StartupProbeSpec{}
	}
	if out.Liveness == nil {
		out.Liveness = &valkeyiov1alpha1.LivenessProbeSpec{}
	}
	if out.Readiness == nil {
		out.Readiness = &valkeyiov1alpha1.ReadinessProbeSpec{}
	}
	return out
}

func orDefault(v *int32, def int32) int32 {
	if v == nil {
		return def
	}
	return *v
}
