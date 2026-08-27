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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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

func TestWorkloadRevisionAllows(t *testing.T) {
	node := &valkeyiov1alpha1.ValkeyNode{}
	assert.False(t, workloadRevisionAllows(node, "abc"))

	node.Spec.WorkloadRevision = "abc"
	assert.True(t, workloadRevisionAllows(node, "abc"))
	assert.False(t, workloadRevisionAllows(node, "def"))
	assert.False(t, workloadRevisionAllows(node, ""))
}

func TestComputeWorkloadRevisionStable(t *testing.T) {
	node := &valkeyiov1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "ns"},
		Spec: valkeyiov1alpha1.ValkeyNodeSpec{
			Image:        "valkey/valkey:9.0.0",
			WorkloadType: valkeyiov1alpha1.WorkloadTypeStatefulSet,
		},
	}
	h1, err := computeWorkloadRevision(node)
	require.NoError(t, err)
	h2, err := computeWorkloadRevision(node)
	require.NoError(t, err)
	assert.Equal(t, h1, h2)

	node.Spec.Image = "valkey/valkey:9.0.1"
	h3, err := computeWorkloadRevision(node)
	require.NoError(t, err)
	assert.NotEqual(t, h1, h3)
}

func TestPodSupersededAndStuck(t *testing.T) {
	sts := func(updateRevision string) *appsv1.StatefulSet {
		return &appsv1.StatefulSet{Status: appsv1.StatefulSetStatus{UpdateRevision: updateRevision}}
	}
	pod := func(revision string, ready bool) *corev1.Pod {
		status := corev1.ConditionFalse
		if ready {
			status = corev1.ConditionTrue
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "valkey-node-0",
				Labels: map[string]string{appsv1.StatefulSetRevisionLabel: revision},
			},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
			},
		}
	}

	t.Run("not ready on a superseded revision is stuck", func(t *testing.T) {
		// The StatefulSet holds the revision that replaces this pod, and
		// OrderedReady keeps it from acting while the pod is not ready.
		assert.True(t, podSupersededAndStuck(pod("old", false), sts("new")))
	})

	t.Run("not ready on the current revision is left alone", func(t *testing.T) {
		// Deleting this one recreates the same pod and the same crash, so a
		// configuration error would become an endless restart loop.
		assert.False(t, podSupersededAndStuck(pod("new", false), sts("new")))
	})

	t.Run("ready on a superseded revision is left to the StatefulSet", func(t *testing.T) {
		// An ordinary rolling update: the StatefulSet controller is able to
		// replace a ready pod on its own.
		assert.False(t, podSupersededAndStuck(pod("old", true), sts("new")))
	})

	t.Run("a pod already terminating is left alone", func(t *testing.T) {
		terminating := pod("old", false)
		now := metav1.Now()
		terminating.DeletionTimestamp = &now
		assert.False(t, podSupersededAndStuck(terminating, sts("new")))
	})

	t.Run("no revision observed yet", func(t *testing.T) {
		// Nothing has superseded the pod, so there is nothing to unblock.
		assert.False(t, podSupersededAndStuck(pod("old", false), sts("")))
	})

	t.Run("missing pod or statefulset", func(t *testing.T) {
		assert.False(t, podSupersededAndStuck(nil, sts("new")))
		assert.False(t, podSupersededAndStuck(pod("old", false), nil))
	})
}
