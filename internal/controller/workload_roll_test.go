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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPodTemplateRollHashStable(t *testing.T) {
	tmpl := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      map[string]string{"app": "valkey"},
			Annotations: map[string]string{"valkey.io/config-hash": "abc"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "server",
				Image: "valkey/valkey:9.0.0",
				Env:   []corev1.EnvVar{{Name: "A", Value: "1"}},
			}},
		},
	}
	h1 := podTemplateRollHash(tmpl)
	h2 := podTemplateRollHash(tmpl)
	assert.Equal(t, h1, h2)
	assert.Len(t, h1, 64)
}

func TestPodTemplateWouldRoll(t *testing.T) {
	base := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "valkey"}},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "server", Image: "valkey/valkey:9.0.0"}},
		},
	}
	same := base.DeepCopy()
	assert.False(t, podTemplateWouldRoll(base, *same))

	changed := base.DeepCopy()
	changed.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "REPRO_MARKER", Value: "v2"}}
	assert.True(t, podTemplateWouldRoll(base, *changed))
	assert.NotEqual(t, podTemplateRollHash(base), podTemplateRollHash(*changed))
}

func TestWorkloadPermitAllows(t *testing.T) {
	node := &valkeyiov1alpha1.ValkeyNode{}
	assert.False(t, workloadPermitAllows(node, "abc"))

	node.Annotations = map[string]string{allowWorkloadRevisionAnnotation: allowWorkloadRevisionAny}
	assert.True(t, workloadPermitAllows(node, "abc"))

	node.Annotations[allowWorkloadRevisionAnnotation] = "abc"
	assert.True(t, workloadPermitAllows(node, "abc"))
	assert.False(t, workloadPermitAllows(node, "def"))
}

func TestIsClusterOwned(t *testing.T) {
	node := &valkeyiov1alpha1.ValkeyNode{}
	assert.False(t, isClusterOwned(node))

	ctrl := true
	node.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "valkey.io/v1alpha1",
		Kind:       "ValkeyCluster",
		Name:       "c",
		UID:        "1",
		Controller: &ctrl,
	}}
	assert.True(t, isClusterOwned(node))

	ctrl = false
	node.OwnerReferences[0].Controller = &ctrl
	assert.False(t, isClusterOwned(node))
}

func TestNodeNeedsWorkloadPermitAndInFlight(t *testing.T) {
	node := &valkeyiov1alpha1.ValkeyNode{}
	assert.False(t, nodeNeedsWorkloadPermit(node))
	assert.False(t, nodeHasInFlightWorkloadRoll(node))
	assert.False(t, nodeHasPendingWorkloadDrift(node))

	node.Annotations = map[string]string{desiredWorkloadRevisionAnnotation: "abc"}
	assert.True(t, nodeNeedsWorkloadPermit(node))
	assert.False(t, nodeHasInFlightWorkloadRoll(node))
	assert.True(t, nodeHasPendingWorkloadDrift(node))

	node.Annotations[allowWorkloadRevisionAnnotation] = "abc"
	assert.False(t, nodeNeedsWorkloadPermit(node))
	assert.True(t, nodeHasInFlightWorkloadRoll(node))
	assert.True(t, nodeHasPendingWorkloadDrift(node))

	node2 := &valkeyiov1alpha1.ValkeyNode{}
	require.True(t, meta.SetStatusCondition(&node2.Status.Conditions, metav1.Condition{
		Type:   valkeyiov1alpha1.ValkeyNodeConditionWorkloadDrift,
		Status: metav1.ConditionTrue,
		Reason: valkeyiov1alpha1.ValkeyNodeReasonAwaitingRollPermit,
	}))
	assert.True(t, nodeNeedsWorkloadPermit(node2))
}

func TestAnyNodeRequiresWorkloadRoll(t *testing.T) {
	list := &valkeyiov1alpha1.ValkeyNodeList{Items: []valkeyiov1alpha1.ValkeyNode{
		{ObjectMeta: metav1.ObjectMeta{Name: "a"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "b", Annotations: map[string]string{
			desiredWorkloadRevisionAnnotation: "h",
		}}},
	}}
	assert.True(t, anyNodeRequiresWorkloadRoll(list))
	assert.False(t, anyNodeRequiresWorkloadRoll(&valkeyiov1alpha1.ValkeyNodeList{}))
}
