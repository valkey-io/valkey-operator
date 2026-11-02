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
)

func TestManagedContainerSecurityDefaults(t *testing.T) {
	for _, tc := range []struct {
		name string
		sc   *corev1.SecurityContext
		want *bool
	}{
		{name: "omitted", want: new(false)},
		{name: "other fields preserved", sc: &corev1.SecurityContext{RunAsNonRoot: new(true)}, want: new(false)},
		{name: "explicit false", sc: &corev1.SecurityContext{AllowPrivilegeEscalation: new(false)}, want: new(false)},
		{name: "explicit true", sc: &corev1.SecurityContext{AllowPrivilegeEscalation: new(true)}, want: new(true)},
		{name: "privileged", sc: &corev1.SecurityContext{Privileged: new(true)}, want: nil},
		{name: "empty capabilities", sc: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{}}, want: new(false)},
		{name: "other capability", sc: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_BIND_SERVICE"}}}, want: new(false)},
		{name: "SYS_ADMIN", sc: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN"}}}, want: nil},
		{name: "SYS_ADMIN among capabilities", sc: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_BIND_SERVICE", "SYS_ADMIN"}, Drop: []corev1.Capability{"ALL"}}}, want: nil},
		{name: "SYS_ADMIN explicit false", sc: &corev1.SecurityContext{AllowPrivilegeEscalation: new(false), Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN"}}}, want: new(false)},
		{name: "SYS_ADMIN explicit true", sc: &corev1.SecurityContext{AllowPrivilegeEscalation: new(true), Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN"}}}, want: new(true)},
		{name: "CAP_SYS_ADMIN", sc: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"CAP_SYS_ADMIN"}}}, want: nil},
		{name: "CAP_SYS_ADMIN explicit false", sc: &corev1.SecurityContext{AllowPrivilegeEscalation: new(false), Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"CAP_SYS_ADMIN"}}}, want: new(false)},
		{name: "CAP_SYS_ADMIN explicit true", sc: &corev1.SecurityContext{AllowPrivilegeEscalation: new(true), Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"CAP_SYS_ADMIN"}}}, want: new(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := newTestValkeyNode("cache-0-0", "default")
			node.Spec.Exporter = valkeyv1.ExporterSpec{Enabled: new(true), SecurityContext: tc.sc}
			node.Spec.Containers = []corev1.Container{
				{Name: "server", SecurityContext: tc.sc},
				{Name: "custom-sidecar", Image: "example/sidecar:latest"},
			}
			before := node.DeepCopy()
			containers, err := buildContainersDef(node)
			require.NoError(t, err)
			require.Len(t, containers, 3)
			for _, c := range containers[:2] {
				require.NotNil(t, c.SecurityContext)
				assert.Equal(t, tc.want, c.SecurityContext.AllowPrivilegeEscalation, c.Name)
				if tc.sc != nil {
					assert.Equal(t, tc.sc.RunAsNonRoot, c.SecurityContext.RunAsNonRoot)
					assert.Equal(t, tc.sc.Privileged, c.SecurityContext.Privileged)
					assert.Equal(t, tc.sc.Capabilities, c.SecurityContext.Capabilities)
				}
			}
			assert.Nil(t, containers[2].SecurityContext, "do not change user-added sidecars")
			assert.Equal(t, before, node, "do not mutate caller-owned security contexts")
		})
	}
}

func TestDisabledExporterLeavesUserNamedExporterUnchanged(t *testing.T) {
	for _, enabled := range []*bool{nil, new(false)} {
		for _, sc := range []*corev1.SecurityContext{nil, {RunAsNonRoot: new(true)}} {
			node := newTestValkeyNode("cache-0-0", "default")
			node.Spec.Exporter.Enabled = enabled
			node.Spec.Containers = []corev1.Container{{
				Name: "metrics-exporter", Image: "example/custom-exporter:latest", SecurityContext: sc,
			}}
			before := node.DeepCopy()
			containers, err := buildContainersDef(node)
			require.NoError(t, err)
			require.Len(t, containers, 2)
			require.NotNil(t, containers[0].SecurityContext)
			assert.Equal(t, new(false), containers[0].SecurityContext.AllowPrivilegeEscalation)
			assert.Equal(t, sc, containers[1].SecurityContext, "a user-added metrics-exporter is not operator-owned")
			assert.Equal(t, before, node, "do not mutate caller-owned configuration")
		}
	}
}

func TestAdmissionHardeningDoesNotTriggerFailover(t *testing.T) {
	node := newTestValkeyNode("cache-0-0", "default")
	node.Spec.Exporter.Enabled = new(true)
	for _, kind := range []valkeyv1.WorkloadType{valkeyv1.WorkloadTypeStatefulSet, valkeyv1.WorkloadTypeDeployment} {
		t.Run(string(kind), func(t *testing.T) {
			node.Spec.WorkloadType = kind
			desired, err := buildNodePodTemplate(node)
			require.NoError(t, err)
			live := desired.DeepCopy()
			for i := range live.Spec.Containers {
				live.Spec.Containers[i].SecurityContext.AllowPrivilegeEscalation = new(false)
			}
			assert.False(t, podTemplateWouldRoll(*live, desired))
			assert.Equal(t, podTemplateRollHash(desired), podTemplateRollHash(*live))
			node.Status.PodIP = "10.0.0.1"
			require.NoError(t, setDesiredWorkloadRevision(node))
			assert.False(t, needsProactiveFailoverForRoll(node, node.DeepCopy(), podTemplateRollHash(*live)))
		})
	}
}
