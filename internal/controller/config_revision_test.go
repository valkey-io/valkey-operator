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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

func TestConfigRevisionKeysPredicate(t *testing.T) {
	const rev1, rev2 = "aaaaaaaaaaaa", "bbbbbbbbbbbb"
	cm := func(data map[string]string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "valkey-c-0-0", Namespace: "default"}, Data: data}
	}
	p := configRevisionKeysPredicate()

	tests := []struct {
		name     string
		old, neu map[string]string
		want     bool
	}{
		{
			name: "live.conf write",
			old:  map[string]string{configRevisionFileKey(rev1): "a", liveConfigFileKey: "maxmemory 100mb\n"},
			neu:  map[string]string{configRevisionFileKey(rev1): "a", liveConfigFileKey: "maxmemory 200mb\n"},
			want: false,
		},
		{
			name: "revision key added",
			old:  map[string]string{configRevisionFileKey(rev1): "a", liveConfigFileKey: ""},
			neu:  map[string]string{configRevisionFileKey(rev1): "a", configRevisionFileKey(rev2): "b", liveConfigFileKey: ""},
			want: true,
		},
		{
			name: "revision key removed",
			old:  map[string]string{configRevisionFileKey(rev1): "a", configRevisionFileKey(rev2): "b"},
			neu:  map[string]string{configRevisionFileKey(rev2): "b"},
			want: true,
		},
		{
			name: "revision content edited",
			old:  map[string]string{configRevisionFileKey(rev1): "a"},
			neu:  map[string]string{configRevisionFileKey(rev1): "edited"},
			want: false,
		},
		{
			name: "other key added",
			old:  map[string]string{configRevisionFileKey(rev1): "a"},
			neu:  map[string]string{configRevisionFileKey(rev1): "a", "valkey-extra.conf": "x"},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, p.Update(event.UpdateEvent{ObjectOld: cm(tt.old), ObjectNew: cm(tt.neu)}))
		})
	}

	t.Run("create and delete", func(t *testing.T) {
		assert.True(t, p.Create(event.CreateEvent{Object: cm(nil)}))
		assert.True(t, p.Delete(event.DeleteEvent{Object: cm(nil)}))
	})
	t.Run("generic", func(t *testing.T) {
		assert.False(t, p.Generic(event.GenericEvent{Object: cm(nil)}))
	})
	t.Run("non-ConfigMap update", func(t *testing.T) {
		pod := &corev1.Pod{}
		assert.False(t, p.Update(event.UpdateEvent{ObjectOld: pod, ObjectNew: pod}))
	})
}

func TestNodeConfigToCluster(t *testing.T) {
	labelled := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "valkey-c-0-0", Namespace: "ns", Labels: map[string]string{LabelCluster: "c"},
	}}
	assert.Equal(t, []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "c", Namespace: "ns"}}},
		nodeConfigToCluster(context.Background(), labelled))

	unlabelled := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "standalone-config", Namespace: "ns"}}
	assert.Empty(t, nodeConfigToCluster(context.Background(), unlabelled))
}

// The parent reads a revision only from a ConfigMap the node controls. A
// ConfigMap of the same name, such as another cluster's shared ConfigMap,
// counts as missing.
func TestReadConfigRevisionIgnoresConfigMapNodeDoesNotControl(t *testing.T) {
	const rev = "aaaaaaaaaaaa"
	scheme := runtime.NewScheme()
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	node := newTestValkeyNode("c-0-0", "default")
	node.UID = "node-uid"
	newConfigMap := func(controlled bool) *corev1.ConfigMap {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: nodeConfigMapName(node.Name), Namespace: node.Namespace},
			Data:       map[string]string{configRevisionFileKey(rev): "maxmemory 100mb\n"},
		}
		if controlled {
			require.NoError(t, controllerutil.SetControllerReference(node, cm, scheme))
		}
		return cm
	}

	for _, controlled := range []bool{true, false} {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(newConfigMap(controlled)).Build()
		content, found, err := readConfigRevision(context.Background(), c, node, rev)
		require.NoError(t, err)
		assert.Equal(t, controlled, found, "controlled=%v", controlled)
		if controlled {
			assert.Equal(t, "maxmemory 100mb\n", content)
		}
	}
}
