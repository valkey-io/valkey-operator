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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
