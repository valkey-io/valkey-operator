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
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// systemPasswordFixture builds a cluster and, when withSecret is set, its
// system passwords Secret holding all three system user keys.
func systemPasswordFixture(withSecret bool) (*valkeyiov1alpha1.ValkeyCluster, *corev1.Secret, []client.Object) {
	cluster := &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "h1", Namespace: "ns"},
	}
	cluster.Spec.Shards = 3
	var secret *corev1.Secret
	objs := []client.Object{cluster}
	if withSecret {
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: getSystemPasswordSecretName(cluster.Name), Namespace: cluster.Namespace},
			Data: map[string][]byte{
				operatorUser:    []byte("op-old"),
				exporterUser:    []byte("ex-old"),
				replicationUser: []byte("re-old"),
			},
		}
		objs = append(objs, secret)
	}
	return cluster, secret, objs
}

// systemPasswordNode builds one ValkeyNode owned by the cluster.
func systemPasswordNode(cluster *valkeyiov1alpha1.ValkeyCluster) *valkeyiov1alpha1.ValkeyNode {
	return &valkeyiov1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "h1-0-0",
			Namespace: cluster.Namespace,
			Labels:    map[string]string{LabelCluster: cluster.Name, LabelShardIndex: "0", LabelNodeIndex: "0"},
		},
	}
}

func systemPasswordReconciler(t *testing.T, objs []client.Object) (*ValkeyClusterReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &ValkeyClusterReconciler{Client: c, APIReader: c, Scheme: scheme, Recorder: events.NewFakeRecorder(64)}, c
}

// TestUpsertRefusesRegenerationOnRunningCluster pins the reported regression: a
// cluster with live nodes must not adopt new system passwords, because the
// running servers still authorize the old ones and the operator would be
// locked out (WRONGPASS on every call) until every pod restarts.
func TestUpsertRefusesRegenerationOnRunningCluster(t *testing.T) {
	cluster, _, objs := systemPasswordFixture(false)
	node := systemPasswordNode(cluster)
	objs = append(objs, node)
	r, c := systemPasswordReconciler(t, objs)

	_, err := r.upsertSystemUsersPasswordSecret(context.Background(), cluster)
	require.Error(t, err)
	require.ErrorIs(t, err, errSystemPasswordSecretMissing)
	require.Contains(t, err.Error(), getSystemPasswordSecretName(cluster.Name))
	require.Contains(t, err.Error(), "_operator")
	require.Contains(t, err.Error(), "_replication")
	require.Contains(t, err.Error(), "restore the Secret to recover the cluster",
		"a confirmed running cluster must keep the restore instruction")

	// No Secret may be created: a regenerated Secret would replace the only
	// copy of the passwords the running nodes still accept.
	secret := &corev1.Secret{}
	apiErr := c.Get(context.Background(), client.ObjectKey{Name: getSystemPasswordSecretName(cluster.Name), Namespace: cluster.Namespace}, secret)
	require.Error(t, apiErr, "a regenerated Secret would overwrite the only copy of the old passwords")

	// The refusal must be observable in the event stream.
	recorder := r.Recorder.(*events.FakeRecorder)
	select {
	case e := <-recorder.Events:
		require.Contains(t, e, "SystemPasswordSecretMissing")
		require.Contains(t, e, getSystemPasswordSecretName(cluster.Name))
	default:
		t.Fatal("expected a SystemPasswordSecretMissing warning event")
	}
}

// TestUpsertRegeneratesOnFreshCluster keeps the bootstrap path: a brand-new
// cluster has no nodes to lock out, so the Secret is generated as today.
func TestUpsertRegeneratesOnFreshCluster(t *testing.T) {
	cluster, _, objs := systemPasswordFixture(false)
	r, c := systemPasswordReconciler(t, objs)

	secret, err := r.upsertSystemUsersPasswordSecret(context.Background(), cluster)
	require.NoError(t, err)
	require.NotEmpty(t, secret.Data[operatorUser])
	require.NotEmpty(t, secret.Data[replicationUser])

	stored := &corev1.Secret{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: getSystemPasswordSecretName(cluster.Name), Namespace: cluster.Namespace}, stored))
	require.NotEmpty(t, stored.Data[operatorUser])
}

// TestUpsertRegeneratesExporterKeyOnRunningCluster keeps the exporter-enable
// path intact: a missing _exporter key on a running cluster is regenerable,
// because enabling the exporter rolls the pods and the new key rides the pod
// environment together with the new aclfile.
func TestUpsertRegeneratesExporterKeyOnRunningCluster(t *testing.T) {
	cluster, secret, objs := systemPasswordFixture(true)
	delete(secret.Data, exporterUser)
	node := systemPasswordNode(cluster)
	objs = append(objs, node)
	enabled := true
	cluster.Spec.Exporter = valkeyiov1alpha1.ExporterSpec{Enabled: &enabled}
	r, c := systemPasswordReconciler(t, objs)

	updated, err := r.upsertSystemUsersPasswordSecret(context.Background(), cluster)
	require.NoError(t, err)
	require.Equal(t, []byte("op-old"), updated.Data[operatorUser], "the operator password must not change")
	require.Equal(t, []byte("re-old"), updated.Data[replicationUser], "the replication password must not change")
	require.NotEmpty(t, updated.Data[exporterUser], "the exporter key is regenerated")

	stored := &corev1.Secret{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: getSystemPasswordSecretName(cluster.Name), Namespace: cluster.Namespace}, stored))
	require.Equal(t, []byte("op-old"), stored.Data[operatorUser])
}

// TestCreateSystemUsersAclRefusesOnRunningCluster pins the integration: the
// guard must flow through createSystemUsersAcl, so the cluster surfaces
// UsersACLError instead of freezing every node out of its ACL.
func TestCreateSystemUsersAclRefusesOnRunningCluster(t *testing.T) {
	cluster, _, objs := systemPasswordFixture(false)
	node := systemPasswordNode(cluster)
	objs = append(objs, node)
	r, _ := systemPasswordReconciler(t, objs)

	_, err := r.createSystemUsersAcl(context.Background(), cluster)
	require.Error(t, err)
	require.ErrorIs(t, err, errSystemPasswordSecretMissing)
}

// TestUpsertRefusesExporterKeyRegenerationWhenSidecarRuns pins the live-sidecar
// half of the guard: once a node actually runs the exporter sidecar, its
// environment holds the _exporter password read at pod start and a
// regenerated key would reach the servers' ACL without any pod roll (Secret
// data is not part of WorkloadRevision), breaking the exporter's
// authentication. The key must be refused until the Secret is restored.
func TestUpsertRefusesExporterKeyRegenerationWhenSidecarRuns(t *testing.T) {
	cluster, secret, objs := systemPasswordFixture(true)
	delete(secret.Data, exporterUser)
	node := systemPasswordNode(cluster)
	enabled := true
	node.Spec.Exporter.Enabled = &enabled
	objs = append(objs, node)
	r, c := systemPasswordReconciler(t, objs)

	_, err := r.upsertSystemUsersPasswordSecret(context.Background(), cluster)
	require.Error(t, err)
	require.ErrorIs(t, err, errSystemPasswordSecretMissing)
	require.Contains(t, err.Error(), "_exporter")

	// The Secret must be untouched: no partial regeneration may replace the
	// keys the running nodes still accept.
	stored := &corev1.Secret{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: getSystemPasswordSecretName(cluster.Name), Namespace: cluster.Namespace}, stored))
	require.Equal(t, []byte("op-old"), stored.Data[operatorUser], "the operator password must not change")
	require.Empty(t, stored.Data[exporterUser], "the exporter key must not be regenerated while the sidecar runs")

	recorder := r.Recorder.(*events.FakeRecorder)
	select {
	case e := <-recorder.Events:
		require.Contains(t, e, "SystemPasswordSecretMissing")
		require.Contains(t, e, "_exporter")
	default:
		t.Fatal("expected a SystemPasswordSecretMissing warning event")
	}
}

// listErrorClient fails every List call, pinning the fail-closed path: an
// operator that cannot check whether nodes exist must not regenerate.
type listErrorClient struct {
	client.Client
}

func (listErrorClient) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return fmt.Errorf("list nodes: api unavailable")
}

// TestUpsertRefusesWhenTheNodeListFails pins the list-error branch of the
// guard: the regeneration decision comes from the one node list it makes,
// so when that list fails the upsert must refuse instead of regenerating.
func TestUpsertRefusesWhenTheNodeListFails(t *testing.T) {
	cluster, _, objs := systemPasswordFixture(false)
	r, _ := systemPasswordReconciler(t, objs)
	r.Client = listErrorClient{Client: r.Client}

	_, err := r.upsertSystemUsersPasswordSecret(context.Background(), cluster)
	require.Error(t, err)
	require.ErrorIs(t, err, errSystemPasswordSecretMissing, "the upsert must refuse to regenerate when its node list cannot be read")
	require.Contains(t, err.Error(), "node state could not be checked",
		"an unreadable node list must not be reported as a running cluster")
	require.NotContains(t, err.Error(), "restore the Secret",
		"a cluster whose Secret was never confirmed missing must not be told to restore one")
	require.NotContains(t, err.Error(), "running cluster",
		"an unreadable node list is not proof the cluster is running")

	// The warning event must carry the same distinction.
	recorder := r.Recorder.(*events.FakeRecorder)
	select {
	case e := <-recorder.Events:
		require.Contains(t, e, "SystemPasswordSecretMissing")
		require.Contains(t, e, "node state could not be checked")
		require.NotContains(t, e, "restore the Secret")
	default:
		t.Fatal("expected a SystemPasswordSecretMissing warning event")
	}
}

// cacheMissClient simulates the manager cache for the read-through: the
// cache is configured to only hold Secrets carrying the
// app.kubernetes.io/managed-by label, so a Secret that lost the label
// reads as NotFound through the cached client even though the API server
// still has it. Everything else, including writes, delegates to the real
// client the way uncached reads and all writes go to the API server.
type cacheMissClient struct {
	client.Client
}

func (c cacheMissClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Secret); ok {
		return apierrors.NewNotFound(corev1.Resource("secret"), key.Name)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// TestUpsertAdoptsUnlabeledSecretThroughAPIReader pins the read-through
// from the review: the manager cache only holds Secrets with the
// managed-by label, so a Secret that lost the label reads as NotFound from
// the cached client. The upsert must read through the API reader, adopt the
// Secret (standard labels + controller reference) and keep its existing
// passwords instead of failing with AlreadyExists on every reconcile or
// generating fresh keys the running nodes do not know.
func TestUpsertAdoptsUnlabeledSecretThroughAPIReader(t *testing.T) {
	cluster, _, objs := systemPasswordFixture(true)
	r, c := systemPasswordReconciler(t, objs)
	r.Client = cacheMissClient{Client: c}
	r.APIReader = c

	updated, err := r.upsertSystemUsersPasswordSecret(context.Background(), cluster)
	require.NoError(t, err)
	require.Equal(t, []byte("op-old"), updated.Data[operatorUser], "an adopted Secret keeps its passwords")
	require.Equal(t, []byte("re-old"), updated.Data[replicationUser], "an adopted Secret keeps its passwords")

	stored := &corev1.Secret{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: getSystemPasswordSecretName(cluster.Name), Namespace: cluster.Namespace}, stored))
	require.Equal(t, "valkey-operator", stored.Labels["app.kubernetes.io/managed-by"], "adoption restores the label the cache selects on")
	require.True(t, metav1.IsControlledBy(stored, cluster), "adoption restores the controller reference")
}
