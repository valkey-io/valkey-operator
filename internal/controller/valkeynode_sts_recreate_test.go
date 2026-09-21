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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestOrphanAndRecreateStatefulSetAlreadyExistsThenAbsent(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	ctrl := true
	node := newTestValkeyNode("c-0-0", "ns")
	node.UID = "node-uid"
	node.Labels = map[string]string{LabelCluster: "c"}
	node.OwnerReferences = []metav1.OwnerReference{{
		Kind:       "ValkeyCluster",
		Controller: &ctrl,
	}}
	node.Spec.WorkloadRevision = "stale"

	desired, err := buildValkeyNodeStatefulSet(node)
	require.NoError(t, err)
	desired.Spec.Template.Annotations = map[string]string{"desired": "template"}

	live := desired.DeepCopy()
	live.Spec.ServiceName = "valkey-c-0-0"
	live.Spec.Template.Annotations = map[string]string{"live": "template"}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      live.Name + "-0",
			Namespace: live.Namespace,
		},
	}
	require.True(t, refuseDesiredSTSCreate(node, pod, podTemplateRollHash(desired.Spec.Template)))

	creates := 0
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, inner client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*appsv1.StatefulSet); !ok {
					return inner.Create(ctx, obj, opts...)
				}
				creates++
				if creates == 1 {
					return apierrors.NewAlreadyExists(appsv1.Resource("statefulsets"), obj.GetName())
				}
				return inner.Create(ctx, obj, opts...)
			},
		}).
		Build()

	r := &ValkeyNodeReconciler{
		Client:    c,
		APIReader: c,
		Scheme:    scheme,
		Recorder:  events.NewFakeRecorder(8),
	}

	got, err := r.orphanAndRecreateStatefulSet(context.Background(), node, live, desired)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 2, creates)
	assert.Equal(t, desired.Spec.ServiceName, got.Spec.ServiceName)
	assert.Equal(t, live.Spec.Template.Annotations, got.Spec.Template.Annotations)
	assert.NotEqual(t, desired.Spec.Template.Annotations, got.Spec.Template.Annotations)

	stored := &appsv1.StatefulSet{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(desired), stored))
	assert.Equal(t, desired.Spec.ServiceName, stored.Spec.ServiceName)
	assert.Equal(t, live.Spec.Template.Annotations, stored.Spec.Template.Annotations)
	assert.NotEqual(t, desired.Spec.Template.Annotations, stored.Spec.Template.Annotations)
}
