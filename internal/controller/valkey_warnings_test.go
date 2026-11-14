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

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

func TestValkeyConfigWarnings(t *testing.T) {
	tests := []struct {
		name     string
		failover *valkeyiov1alpha1.FailoverSpec
		replicas int32
		wantWarn bool
	}{
		{"no failover block", nil, 0, false},
		{"mode None, no replicas", &valkeyiov1alpha1.FailoverSpec{Mode: valkeyiov1alpha1.FailoverModeNone}, 0, false},
		{"mode Sentinel, no replicas", &valkeyiov1alpha1.FailoverSpec{Mode: valkeyiov1alpha1.FailoverModeSentinel}, 0, true},
		{"mode Sentinel, with replicas", &valkeyiov1alpha1.FailoverSpec{Mode: valkeyiov1alpha1.FailoverModeSentinel}, 2, false},
		{"mode None, with replicas", &valkeyiov1alpha1.FailoverSpec{Mode: valkeyiov1alpha1.FailoverModeNone}, 2, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			valkey := &valkeyiov1alpha1.Valkey{
				ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: "default"},
				Spec:       valkeyiov1alpha1.ValkeySpec{Replicas: tt.replicas, Failover: tt.failover},
			}
			warnings := valkeyConfigWarnings(valkey)
			if !tt.wantWarn {
				g.Expect(warnings).To(BeEmpty())
				return
			}
			g.Expect(warnings).To(HaveLen(1))
			g.Expect(warnings[0].reason).To(Equal(valkeyiov1alpha1.ReasonSentinelWithoutReplicas))
			g.Expect(warnings[0].message).To(ContainSubstring("spec.failover.mode is Sentinel"))
		})
	}
}
