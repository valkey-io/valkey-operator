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

package v1alpha1

// ProbesSpec tunes the probes on the server container. Unset fields keep
// the operator defaults. spec.containers patches are applied afterwards.
type ProbesSpec struct {
	// Startup tunes the startup probe. The liveness and readiness probes
	// start only after it passes.
	// +optional
	Startup *StartupProbeSpec `json:"startup,omitempty"`

	// Liveness tunes the liveness probe, or removes it.
	// +optional
	Liveness *LivenessProbeSpec `json:"liveness,omitempty"`

	// Readiness tunes the readiness probe.
	// +optional
	Readiness *ReadinessProbeSpec `json:"readiness,omitempty"`
}

// StartupProbeSpec tunes the startup probe, which runs the liveness check.
type StartupProbeSpec struct {
	// InitialDelaySeconds is how long after the container starts the first
	// check runs. Defaults to 5.
	// +kubebuilder:validation:Minimum=0
	// +optional
	InitialDelaySeconds *int32 `json:"initialDelaySeconds,omitempty"`

	// PeriodSeconds is how often the check runs. Defaults to 5.
	// +kubebuilder:validation:Minimum=1
	// +optional
	PeriodSeconds *int32 `json:"periodSeconds,omitempty"`

	// TimeoutSeconds is how long Valkey has to answer a check. The probe's
	// own timeout is one second longer, so the check gives up first.
	// Defaults to 4.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600
	// +optional
	TimeoutSeconds *int32 `json:"timeoutSeconds,omitempty"`

	// FailureThreshold is how many checks in a row may fail before the
	// kubelet restarts the container. The probe gives up after
	// initialDelaySeconds + (failureThreshold - 1) x periodSeconds. A set
	// value is used as is. Unset, it is the smallest value that gives up no
	// sooner than 100s, or 400s on a node without persistence, where a
	// restarted primary can wait up to 300s for a replica hand-off before the
	// server starts: 20 or 80 at the default delay and period. Without
	// persistence, a probe that gives up before 400s is reported as a
	// StartupProbeTooShort ConfigurationWarning.
	// +kubebuilder:validation:Minimum=1
	// +optional
	FailureThreshold *int32 `json:"failureThreshold,omitempty"`
}

// LivenessProbeSpec tunes the liveness probe.
// +kubebuilder:validation:XValidation:rule="!has(self.mode) || self.mode != 'Disabled' || !has(self.periodSeconds)",message="periodSeconds cannot be set when mode is Disabled",fieldPath=".periodSeconds",reason="FieldValueForbidden"
// +kubebuilder:validation:XValidation:rule="!has(self.mode) || self.mode != 'Disabled' || !has(self.timeoutSeconds)",message="timeoutSeconds cannot be set when mode is Disabled",fieldPath=".timeoutSeconds",reason="FieldValueForbidden"
// +kubebuilder:validation:XValidation:rule="!has(self.mode) || self.mode != 'Disabled' || !has(self.failureThreshold)",message="failureThreshold cannot be set when mode is Disabled",fieldPath=".failureThreshold",reason="FieldValueForbidden"
type LivenessProbeSpec struct {
	// Mode selects the liveness check. Enabled runs the operator's built-in
	// check. Disabled removes the probe, so the kubelet never restarts the
	// server container for failing it. Defaults to Enabled when unset.
	// +optional
	Mode LivenessProbeMode `json:"mode,omitempty"`

	// PeriodSeconds is how often the check runs. Defaults to 5.
	// +kubebuilder:validation:Minimum=1
	// +optional
	PeriodSeconds *int32 `json:"periodSeconds,omitempty"`

	// TimeoutSeconds is how long Valkey has to answer a check. The probe's
	// own timeout is one second longer, so the check gives up first.
	// Defaults to 4.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600
	// +optional
	TimeoutSeconds *int32 `json:"timeoutSeconds,omitempty"`

	// FailureThreshold is how many checks in a row may fail before the
	// kubelet restarts the server container. Defaults to 5.
	// +kubebuilder:validation:Minimum=1
	// +optional
	FailureThreshold *int32 `json:"failureThreshold,omitempty"`
}

// ReadinessProbeSpec tunes the readiness probe.
type ReadinessProbeSpec struct {
	// PeriodSeconds is how often the check runs. Defaults to 5.
	// +kubebuilder:validation:Minimum=1
	// +optional
	PeriodSeconds *int32 `json:"periodSeconds,omitempty"`

	// TimeoutSeconds is how long Valkey has to answer a check. The probe's
	// own timeout is one second longer, so the check gives up first.
	// Defaults to 1.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600
	// +optional
	TimeoutSeconds *int32 `json:"timeoutSeconds,omitempty"`

	// SuccessThreshold is how many checks in a row must pass before the
	// container counts as ready, including after a failure. Defaults to 1.
	// +kubebuilder:validation:Minimum=1
	// +optional
	SuccessThreshold *int32 `json:"successThreshold,omitempty"`

	// FailureThreshold is how many checks in a row may fail before the
	// container stops counting as ready. Defaults to 5.
	// +kubebuilder:validation:Minimum=1
	// +optional
	FailureThreshold *int32 `json:"failureThreshold,omitempty"`
}

// LivenessProbeMode selects the liveness check on the server container.
// Enabled runs the operator's built-in check. Disabled removes the probe.
// Values may be added in future versions; clients must handle unknown values.
// +kubebuilder:validation:Enum=Enabled;Disabled
type LivenessProbeMode string

const (
	// LivenessProbeModeEnabled runs the operator's built-in liveness check.
	LivenessProbeModeEnabled LivenessProbeMode = "Enabled"
	// LivenessProbeModeDisabled removes the liveness probe.
	LivenessProbeModeDisabled LivenessProbeMode = "Disabled"
)
