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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// A node stored before spec.configRevision existed keeps its workload and
// gets no ConfigMap until the parent sets the field. The API server rejects
// such a node now, so this runs on the fake client.
func TestReconcileLeavesNodeWithoutConfigRevisionAlone(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	node := newTestValkeyNode("c-0-0", "ns")
	node.Labels = map[string]string{LabelCluster: "c"}
	node.OwnerReferences = testParentRefs()
	node.Spec.WorkloadType = valkeyiov1alpha1.WorkloadTypeStatefulSet
	require.Empty(t, node.Spec.ConfigRevision)

	live, err := buildValkeyNodeStatefulSet(node)
	require.NoError(t, err)
	live.Spec.Template.Annotations = map[string]string{"example.com/booted": "old"}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, live).
		WithStatusSubresource(&valkeyiov1alpha1.ValkeyNode{}).
		Build()
	r := &ValkeyNodeReconciler{
		Client:    c,
		APIReader: c,
		Scheme:    scheme,
		Recorder:  events.NewFakeRecorder(16),
	}
	_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(node)})
	require.NoError(t, err)

	sts := &appsv1.StatefulSet{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(live), sts))
	assert.Equal(t, live.Spec.Template.Annotations, sts.Spec.Template.Annotations, "the workload keeps the template it booted with")

	err = c.Get(context.Background(), client.ObjectKey{Name: nodeConfigMapName(node.Name), Namespace: node.Namespace}, &corev1.ConfigMap{})
	assert.True(t, apierrors.IsNotFound(err), "no node ConfigMap without a revision, got %v", err)

	got := &valkeyiov1alpha1.ValkeyNode{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(node), got))
	ready := meta.FindStatusCondition(got.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, "PodNotReady", ready.Reason, "status is reported as usual")
}

// ensureLiveConfig runs after the status write. It patches only live.conf and
// leaves a missing ConfigMap, or one the node does not control, to
// ensureNodeConfig.
func TestEnsureLiveConfigWritesOnlyLiveConf(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	newNode := func() *valkeyiov1alpha1.ValkeyNode {
		node := newTestValkeyNode("c-0-0", "ns")
		node.UID = "node-uid"
		node.Spec.ConfigRevision = "0123456789ab"
		node.Spec.Config = map[string]string{"maxmemory": "100mb"}
		return node
	}
	newConfigMap := func(node *valkeyiov1alpha1.ValkeyNode, controlled bool) *corev1.ConfigMap {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: nodeConfigMapName(node.Name), Namespace: node.Namespace},
			Data:       map[string]string{liveConfigFileKey: "maxmemory 50mb\n", "stale": "x"},
		}
		if controlled {
			require.NoError(t, controllerutil.SetControllerReference(node, cm, scheme))
		}
		return cm
	}
	reconciler := func(objs ...client.Object) (*ValkeyNodeReconciler, client.Client) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		return &ValkeyNodeReconciler{Client: c, APIReader: c, Scheme: scheme}, c
	}
	key := client.ObjectKey{Name: nodeConfigMapName("c-0-0"), Namespace: "ns"}

	t.Run("patches live.conf and nothing else", func(t *testing.T) {
		node := newNode()
		r, c := reconciler(newConfigMap(node, true))
		require.NoError(t, r.ensureLiveConfig(context.Background(), node))

		cm := &corev1.ConfigMap{}
		require.NoError(t, c.Get(context.Background(), key, cm))
		assert.Equal(t, map[string]string{liveConfigFileKey: "maxmemory 100mb\n", "stale": "x"}, cm.Data,
			"no revision key is written and no key is removed")
	})

	t.Run("leaves a ConfigMap the node does not control", func(t *testing.T) {
		node := newNode()
		r, c := reconciler(newConfigMap(node, false))
		require.NoError(t, r.ensureLiveConfig(context.Background(), node))

		cm := &corev1.ConfigMap{}
		require.NoError(t, c.Get(context.Background(), key, cm))
		assert.Equal(t, "maxmemory 50mb\n", cm.Data[liveConfigFileKey])
	})

	t.Run("leaves a ConfigMap the cache has not seen since ensureNodeConfig created it", func(t *testing.T) {
		node := newNode()
		node.Spec.ConfigRevision = pinnedConfigRevision(renderPinnedConfig(node))
		// The cache lags the Create: a Get of the ConfigMap misses until
		// the test catches it up.
		lagging := false
		c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if err := c.Create(ctx, obj, opts...); err != nil {
					return err
				}
				_, lagging = obj.(*corev1.ConfigMap)
				return nil
			},
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok && lagging {
					return apierrors.NewNotFound(corev1.Resource("configmaps"), key.Name)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
		r := &ValkeyNodeReconciler{Client: c, APIReader: c, Scheme: scheme}
		require.NoError(t, r.ensureNodeConfig(context.Background(), node))

		// The status write records a new applied value.
		node.Status.AppliedLiveConfig = []valkeyiov1alpha1.AppliedLiveConfigEntry{{Name: "maxmemory", Value: "200mb"}}
		require.NoError(t, r.ensureLiveConfig(context.Background(), node), "a cache miss is not an ownership error")

		lagging = false
		cm := &corev1.ConfigMap{}
		require.NoError(t, c.Get(context.Background(), key, cm))
		assert.Equal(t, "maxmemory 100mb\n", cm.Data[liveConfigFileKey], "live.conf is left for the requeue")
	})

	t.Run("creates no ConfigMap", func(t *testing.T) {
		r, c := reconciler()
		require.NoError(t, r.ensureLiveConfig(context.Background(), newNode()))

		err := c.Get(context.Background(), key, &corev1.ConfigMap{})
		assert.True(t, apierrors.IsNotFound(err), "got %v", err)
	})
}
