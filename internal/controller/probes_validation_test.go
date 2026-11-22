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
	"context"
	"errors"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

var _ = Describe("spec.probes validation", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	// Both CRDs carry the probes type, so each case runs against both.
	probeObjects := func(name string, probes *valkeyiov1alpha1.ProbesSpec) []client.Object {
		return []client.Object{
			&valkeyiov1alpha1.ValkeyCluster{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-c", Namespace: "default"},
				Spec:       valkeyiov1alpha1.ValkeyClusterSpec{Shards: 1, Probes: probes},
			},
			&valkeyiov1alpha1.ValkeyNode{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-n", Namespace: "default"},
				Spec:       valkeyiov1alpha1.ValkeyNodeSpec{Probes: probes},
			},
		}
	}
	expectAccepted := func(name string, probes *valkeyiov1alpha1.ProbesSpec) {
		GinkgoHelper()
		for _, obj := range probeObjects(name, probes) {
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
		}
	}
	// expectRejected requires the one error to be on field, of type reason. A
	// schema error keeps the API server from running the CEL rules, which it
	// reports in a cause of its own.
	expectRejected := func(name string, probes *valkeyiov1alpha1.ProbesSpec, field string, reason metav1.CauseType, message string) {
		GinkgoHelper()
		for _, obj := range probeObjects(name, probes) {
			err := k8sClient.Create(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got %v", err)
			var status apierrors.APIStatus
			Expect(errors.As(err, &status)).To(BeTrue())
			var causes []metav1.StatusCause
			for _, cause := range status.Status().Details.Causes {
				if !strings.Contains(cause.Message, "some validation rules were not checked") {
					causes = append(causes, cause)
				}
			}
			Expect(causes).To(HaveLen(1), "%v", err)
			Expect(causes[0].Field).To(Equal(field))
			Expect(causes[0].Type).To(Equal(reason))
			Expect(causes[0].Message).To(ContainSubstring(message))
		}
	}

	It("accepts every field at its bounds with liveness enabled", func() {
		expectAccepted("probes-all", &valkeyiov1alpha1.ProbesSpec{
			Startup: &valkeyiov1alpha1.StartupProbeSpec{
				InitialDelaySeconds: new(int32(0)),
				PeriodSeconds:       new(int32(1)),
				TimeoutSeconds:      new(int32(3600)),
				FailureThreshold:    new(int32(1)),
			},
			Liveness: &valkeyiov1alpha1.LivenessProbeSpec{
				Mode:             valkeyiov1alpha1.LivenessProbeModeBuiltIn,
				PeriodSeconds:    new(int32(1)),
				TimeoutSeconds:   new(int32(3600)),
				FailureThreshold: new(int32(1)),
			},
			Readiness: &valkeyiov1alpha1.ReadinessProbeSpec{
				PeriodSeconds:    new(int32(1)),
				TimeoutSeconds:   new(int32(3600)),
				SuccessThreshold: new(int32(1)),
				FailureThreshold: new(int32(1)),
			},
		})
	})

	It("accepts liveness timing fields with mode unset", func() {
		expectAccepted("probes-unset-mode", &valkeyiov1alpha1.ProbesSpec{
			Liveness: &valkeyiov1alpha1.LivenessProbeSpec{PeriodSeconds: new(int32(10)), TimeoutSeconds: new(int32(8)), FailureThreshold: new(int32(10))},
		})
	})

	It("accepts liveness Disabled next to tuned startup and readiness probes", func() {
		expectAccepted("probes-disabled", &valkeyiov1alpha1.ProbesSpec{
			Startup:   &valkeyiov1alpha1.StartupProbeSpec{FailureThreshold: new(int32(60))},
			Liveness:  &valkeyiov1alpha1.LivenessProbeSpec{Mode: valkeyiov1alpha1.LivenessProbeModeDisabled},
			Readiness: &valkeyiov1alpha1.ReadinessProbeSpec{PeriodSeconds: new(int32(10)), TimeoutSeconds: new(int32(3))},
		})
	})

	disabledWith := []struct {
		field    string
		liveness *valkeyiov1alpha1.LivenessProbeSpec
	}{
		{"periodSeconds", &valkeyiov1alpha1.LivenessProbeSpec{Mode: valkeyiov1alpha1.LivenessProbeModeDisabled, PeriodSeconds: new(int32(10))}},
		{"timeoutSeconds", &valkeyiov1alpha1.LivenessProbeSpec{Mode: valkeyiov1alpha1.LivenessProbeModeDisabled, TimeoutSeconds: new(int32(8))}},
		{"failureThreshold", &valkeyiov1alpha1.LivenessProbeSpec{Mode: valkeyiov1alpha1.LivenessProbeModeDisabled, FailureThreshold: new(int32(10))}},
	}
	for _, tc := range disabledWith {
		It(fmt.Sprintf("rejects liveness %s with mode Disabled", tc.field), func() {
			expectRejected("probes-disabled-"+strings.ToLower(tc.field), &valkeyiov1alpha1.ProbesSpec{Liveness: tc.liveness},
				"spec.probes.liveness."+tc.field, metav1.CauseTypeForbidden, tc.field+" cannot be set when mode is Disabled")
		})
	}

	It("rejects a liveness mode it does not know", func() {
		expectRejected("probes-unknown-mode", &valkeyiov1alpha1.ProbesSpec{
			Liveness: &valkeyiov1alpha1.LivenessProbeSpec{Mode: "TCPSocket"},
		}, "spec.probes.liveness.mode", metav1.CauseTypeFieldValueNotSupported, `Unsupported value: "TCPSocket"`)
	})

	const atLeastOne = "should be greater than or equal to 1"
	const atMostAnHour = "should be less than or equal to 3600"
	outOfBounds := []struct {
		field   string
		probes  *valkeyiov1alpha1.ProbesSpec
		message string
	}{
		{"startup.initialDelaySeconds", &valkeyiov1alpha1.ProbesSpec{Startup: &valkeyiov1alpha1.StartupProbeSpec{InitialDelaySeconds: new(int32(-1))}}, "should be greater than or equal to 0"},
		{"startup.periodSeconds", &valkeyiov1alpha1.ProbesSpec{Startup: &valkeyiov1alpha1.StartupProbeSpec{PeriodSeconds: new(int32(0))}}, atLeastOne},
		{"startup.timeoutSeconds", &valkeyiov1alpha1.ProbesSpec{Startup: &valkeyiov1alpha1.StartupProbeSpec{TimeoutSeconds: new(int32(0))}}, atLeastOne},
		{"startup.timeoutSeconds", &valkeyiov1alpha1.ProbesSpec{Startup: &valkeyiov1alpha1.StartupProbeSpec{TimeoutSeconds: new(int32(3601))}}, atMostAnHour},
		{"startup.failureThreshold", &valkeyiov1alpha1.ProbesSpec{Startup: &valkeyiov1alpha1.StartupProbeSpec{FailureThreshold: new(int32(0))}}, atLeastOne},
		{"liveness.periodSeconds", &valkeyiov1alpha1.ProbesSpec{Liveness: &valkeyiov1alpha1.LivenessProbeSpec{PeriodSeconds: new(int32(0))}}, atLeastOne},
		{"liveness.timeoutSeconds", &valkeyiov1alpha1.ProbesSpec{Liveness: &valkeyiov1alpha1.LivenessProbeSpec{TimeoutSeconds: new(int32(0))}}, atLeastOne},
		{"liveness.timeoutSeconds", &valkeyiov1alpha1.ProbesSpec{Liveness: &valkeyiov1alpha1.LivenessProbeSpec{TimeoutSeconds: new(int32(3601))}}, atMostAnHour},
		{"liveness.failureThreshold", &valkeyiov1alpha1.ProbesSpec{Liveness: &valkeyiov1alpha1.LivenessProbeSpec{FailureThreshold: new(int32(0))}}, atLeastOne},
		{"readiness.periodSeconds", &valkeyiov1alpha1.ProbesSpec{Readiness: &valkeyiov1alpha1.ReadinessProbeSpec{PeriodSeconds: new(int32(0))}}, atLeastOne},
		{"readiness.timeoutSeconds", &valkeyiov1alpha1.ProbesSpec{Readiness: &valkeyiov1alpha1.ReadinessProbeSpec{TimeoutSeconds: new(int32(0))}}, atLeastOne},
		{"readiness.timeoutSeconds", &valkeyiov1alpha1.ProbesSpec{Readiness: &valkeyiov1alpha1.ReadinessProbeSpec{TimeoutSeconds: new(int32(3601))}}, atMostAnHour},
		{"readiness.successThreshold", &valkeyiov1alpha1.ProbesSpec{Readiness: &valkeyiov1alpha1.ReadinessProbeSpec{SuccessThreshold: new(int32(0))}}, atLeastOne},
		{"readiness.failureThreshold", &valkeyiov1alpha1.ProbesSpec{Readiness: &valkeyiov1alpha1.ReadinessProbeSpec{FailureThreshold: new(int32(0))}}, atLeastOne},
	}
	for i, tc := range outOfBounds {
		It(fmt.Sprintf("rejects %s out of bounds (%s)", tc.field, tc.message), func() {
			expectRejected(fmt.Sprintf("probes-bounds-%d", i), tc.probes, "spec.probes."+tc.field, metav1.CauseTypeFieldValueInvalid, tc.message)
		})
	}

	It("rejects disabling liveness in an update that keeps its timing fields", func() {
		key := types.NamespacedName{Name: "probes-disable-update", Namespace: "default"}
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{Shards: 1, Probes: &valkeyiov1alpha1.ProbesSpec{
				Liveness: &valkeyiov1alpha1.LivenessProbeSpec{FailureThreshold: new(int32(10))},
			}},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })

		cluster.Spec.Probes.Liveness.Mode = valkeyiov1alpha1.LivenessProbeModeDisabled
		err := k8sClient.Update(ctx, cluster)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("failureThreshold cannot be set when mode is Disabled"))

		Expect(k8sClient.Get(ctx, key, cluster)).To(Succeed())
		cluster.Spec.Probes.Liveness = &valkeyiov1alpha1.LivenessProbeSpec{Mode: valkeyiov1alpha1.LivenessProbeModeDisabled}
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
	})
})
