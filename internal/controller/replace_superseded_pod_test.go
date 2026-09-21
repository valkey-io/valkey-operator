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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestReplaceSupersededPodWhileSyncing covers the one case where a pod that is
// not Ready on a superseded revision must still be left alone: it is loading a
// dataset and will turn Ready on its own, after which the StatefulSet rolls it.
func TestReplaceSupersededPodWhileSyncing(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	node := newTestValkeyNode("c-0-0", "ns")
	node.Labels = map[string]string{LabelCluster: "c", LabelShardIndex: "0", LabelNodeIndex: "0"}

	ctrl := true
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "valkey-c-0-0", Namespace: "ns", UID: "sts-uid", Generation: 2},
		Status:     appsv1.StatefulSetStatus{ObservedGeneration: 2, UpdateRevision: "new"},
	}
	labels := valkeyNodeLabels(node)
	labels[appsv1.StatefulSetRevisionLabel] = "old"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "valkey-c-0-0-0", Namespace: "ns", UID: "pod-uid", Labels: labels,
			OwnerReferences: []metav1.OwnerReference{{Kind: "StatefulSet", Name: sts.Name, UID: sts.UID, Controller: &ctrl}},
		},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}},
	}
	require.True(t, podSupersededAndStuck(pod, sts), "fixture must be a superseded, stuck pod")

	newReconciler := func(info string, infoErr error) (*ValkeyNodeReconciler, client.Client) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod.DeepCopy()).Build()
		return &ValkeyNodeReconciler{
			Client: c, APIReader: c, Scheme: scheme, Recorder: events.NewFakeRecorder(8),
			nodeInfoFunc: func(context.Context, *valkeyiov1alpha1.ValkeyNode) (string, error) { return info, infoErr },
		}, c
	}
	podExists := func(c client.Client) bool {
		err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{})
		return !apierrors.IsNotFound(err)
	}

	t.Run("loading an RDB is deferred, not deleted", func(t *testing.T) {
		r, c := newReconciler("# Persistence\r\nloading:1\r\n", nil)
		err := r.replaceSupersededPod(context.Background(), node, sts)
		assert.ErrorIs(t, err, errTransientRequeue)
		assert.True(t, podExists(c))
	})

	t.Run("receiving a sync from the primary is deferred, not deleted", func(t *testing.T) {
		r, c := newReconciler("# Replication\r\nmaster_sync_in_progress:1\r\n", nil)
		err := r.replaceSupersededPod(context.Background(), node, sts)
		assert.ErrorIs(t, err, errTransientRequeue)
		assert.True(t, podExists(c))
	})

	t.Run("a pod that cannot answer INFO is crash-looping and is deleted", func(t *testing.T) {
		r, c := newReconciler("", errors.New("connection refused"))
		require.NoError(t, r.replaceSupersededPod(context.Background(), node, sts))
		assert.False(t, podExists(c))
	})

	t.Run("a pod that answered and is not syncing is deleted", func(t *testing.T) {
		r, c := newReconciler("# Persistence\r\nloading:0\r\n", nil)
		require.NoError(t, r.replaceSupersededPod(context.Background(), node, sts))
		assert.False(t, podExists(c))
	})
}
