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
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
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

// systemPasswordPod builds a pod of clusterName with a server and an exporter
// container. The containers named in running are Running; the others wait,
// the way they do when their Secret is missing.
func systemPasswordPod(clusterName, name string, running ...string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "ns",
			Labels:    map[string]string{LabelCluster: clusterName},
		},
	}
	for _, container := range []string{serverContainerName, metricsExporterContainerName} {
		status := corev1.ContainerStatus{Name: container}
		if slices.Contains(running, container) {
			status.State.Running = &corev1.ContainerStateRunning{}
		} else {
			status.State.Waiting = &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError"}
		}
		pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, status)
	}
	return pod
}

func systemPasswordReconciler(t *testing.T, objs []client.Object) (*ValkeyClusterReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &ValkeyClusterReconciler{Client: c, APIReader: c, Scheme: scheme, Recorder: events.NewFakeRecorder(64)}, c
}

func storedSystemPasswords(t *testing.T, c client.Client) (*corev1.Secret, bool) {
	t.Helper()
	return getSecret(t, c, getSystemPasswordSecretName("h1"))
}

func requireWarningEvent(t *testing.T, r *ValkeyClusterReconciler, contains ...string) {
	t.Helper()
	recorder := r.Recorder.(*events.FakeRecorder)
	select {
	case e := <-recorder.Events:
		require.Contains(t, e, "SystemPasswordSecretMissing")
		for _, s := range contains {
			require.Contains(t, e, s)
		}
	default:
		t.Fatal("expected a SystemPasswordSecretMissing warning event")
	}
}

// A running server holds an ACL built from the old passwords. New ones would
// lock the operator out of it, so the Secret is not recreated.
func TestUpsertRefusesRegenerationWhileAServerRuns(t *testing.T) {
	cluster, _, objs := systemPasswordFixture(false)
	objs = append(objs, systemPasswordPod("h1", "valkey-h1-0-0-0", serverContainerName))
	r, c := systemPasswordReconciler(t, objs)

	_, err := r.upsertSystemUsersPasswordSecret(context.Background(), cluster)
	require.ErrorIs(t, err, errSystemPasswordSecretMissing)
	require.Contains(t, err.Error(), "valkey-h1-system-passwords")
	require.Contains(t, err.Error(), "_operator, _replication")
	require.Contains(t, err.Error(), "restore the Secret")

	_, exists := storedSystemPasswords(t, c)
	require.False(t, exists, "a regenerated Secret would replace the only copy of the passwords the servers accept")
	requireWarningEvent(t, r, "valkey-h1-system-passwords", "restore the Secret")
}

func TestUpsertRegeneratesOnFreshCluster(t *testing.T) {
	cluster, _, objs := systemPasswordFixture(false)
	r, c := systemPasswordReconciler(t, objs)

	_, err := r.upsertSystemUsersPasswordSecret(context.Background(), cluster)
	require.NoError(t, err)
	stored, exists := storedSystemPasswords(t, c)
	require.True(t, exists)
	require.NotEmpty(t, stored.Data[operatorUser])
	require.NotEmpty(t, stored.Data[replicationUser])
}

// ValkeyNodes and pods exist, but no server has started: a hibernated or
// restored cluster. Every server that starts later loads the new aclfile.
func TestUpsertRegeneratesWhenNoServerRuns(t *testing.T) {
	cluster, _, objs := systemPasswordFixture(false)
	node := &valkeyiov1alpha1.ValkeyNode{ObjectMeta: metav1.ObjectMeta{
		Name: "h1-0-0", Namespace: "ns", Labels: map[string]string{LabelCluster: "h1"},
	}}
	objs = append(objs, node, systemPasswordPod("h1", "valkey-h1-0-0-0"))
	r, c := systemPasswordReconciler(t, objs)

	_, err := r.upsertSystemUsersPasswordSecret(context.Background(), cluster)
	require.NoError(t, err)
	stored, exists := storedSystemPasswords(t, c)
	require.True(t, exists)
	require.NotEmpty(t, stored.Data[operatorUser])
}

func TestUpsertRefusesWhileATerminatingPodRunsAServer(t *testing.T) {
	cluster, _, objs := systemPasswordFixture(false)
	pod := systemPasswordPod("h1", "valkey-h1-0-0-0", serverContainerName)
	pod.Finalizers = []string{"test"}
	pod.DeletionTimestamp = &metav1.Time{}
	objs = append(objs, pod)
	r, _ := systemPasswordReconciler(t, objs)

	_, err := r.upsertSystemUsersPasswordSecret(context.Background(), cluster)
	require.ErrorIs(t, err, errSystemPasswordSecretMissing)
}

// _operator.next is not the password the servers authenticate the operator
// with yet, so a lost _operator is refused mid-rotation too.
func TestUpsertRefusesMidRotationWhileAServerRuns(t *testing.T) {
	cluster, secret, objs := systemPasswordFixture(true)
	secret.Data = map[string][]byte{
		systemPasswordNextKey(operatorUser): []byte("op-next"),
		replicationUser:                     []byte("re-old"),
	}
	objs = append(objs, systemPasswordPod("h1", "valkey-h1-0-0-0", serverContainerName))
	r, c := systemPasswordReconciler(t, objs)

	_, err := r.upsertSystemUsersPasswordSecret(context.Background(), cluster)
	require.ErrorIs(t, err, errSystemPasswordSecretMissing)
	require.Contains(t, err.Error(), "missing _operator ")
	require.NotContains(t, err.Error(), "_replication")
	stored, _ := storedSystemPasswords(t, c)
	require.Equal(t, map[string][]byte{
		systemPasswordNextKey(operatorUser): []byte("op-next"),
		replicationUser:                     []byte("re-old"),
	}, stored.Data)
}

// Enabling the exporter generates _exporter before the pods roll onto the
// sidecar, so the key is generated while only the servers run.
func TestUpsertRegeneratesExporterKeyWhileNoExporterRuns(t *testing.T) {
	cluster, secret, objs := systemPasswordFixture(true)
	delete(secret.Data, exporterUser)
	enabled := true
	cluster.Spec.Exporter = valkeyiov1alpha1.ExporterSpec{Enabled: &enabled}
	objs = append(objs, systemPasswordPod("h1", "valkey-h1-0-0-0", serverContainerName))
	r, c := systemPasswordReconciler(t, objs)

	_, err := r.upsertSystemUsersPasswordSecret(context.Background(), cluster)
	require.NoError(t, err)
	stored, _ := storedSystemPasswords(t, c)
	require.Equal(t, "op-old", string(stored.Data[operatorUser]))
	require.Equal(t, "re-old", string(stored.Data[replicationUser]))
	require.NotEmpty(t, stored.Data[exporterUser])
}

// A running exporter read REDIS_PASSWORD at start and keeps it, so a new
// _exporter would lock it out.
func TestUpsertRefusesExporterKeyWhileTheExporterRuns(t *testing.T) {
	cluster, secret, objs := systemPasswordFixture(true)
	delete(secret.Data, exporterUser)
	enabled := true
	cluster.Spec.Exporter = valkeyiov1alpha1.ExporterSpec{Enabled: &enabled}
	objs = append(objs, systemPasswordPod("h1", "valkey-h1-0-0-0", serverContainerName, metricsExporterContainerName))
	r, c := systemPasswordReconciler(t, objs)

	_, err := r.upsertSystemUsersPasswordSecret(context.Background(), cluster)
	require.ErrorIs(t, err, errSystemPasswordSecretMissing)
	require.Contains(t, err.Error(), "_exporter")
	stored, _ := storedSystemPasswords(t, c)
	require.Empty(t, stored.Data[exporterUser])
	requireWarningEvent(t, r, "_exporter")
}

// The pods are matched on the exact cluster label, so h10's servers do not
// hold h1's passwords.
func TestUpsertIgnoresPodsOfAnotherCluster(t *testing.T) {
	cluster, _, objs := systemPasswordFixture(false)
	objs = append(objs, systemPasswordPod("h10", "valkey-h10-0-0-0", serverContainerName))
	r, c := systemPasswordReconciler(t, objs)

	_, err := r.upsertSystemUsersPasswordSecret(context.Background(), cluster)
	require.NoError(t, err)
	_, exists := storedSystemPasswords(t, c)
	require.True(t, exists)
}

// listErrorReader fails every List.
type listErrorReader struct {
	client.Reader
}

func (listErrorReader) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return fmt.Errorf("api unavailable")
}

// An operator that cannot see the pods cannot rule out a running server, so
// it refuses, without claiming the servers run or that a Secret existed.
func TestUpsertRefusesWhenThePodListFails(t *testing.T) {
	cluster, _, objs := systemPasswordFixture(false)
	r, c := systemPasswordReconciler(t, objs)
	r.APIReader = listErrorReader{Reader: c}

	_, err := r.upsertSystemUsersPasswordSecret(context.Background(), cluster)
	require.ErrorIs(t, err, errSystemPasswordSecretMissing)
	require.Contains(t, err.Error(), "pod state could not be checked")
	require.NotContains(t, err.Error(), "restore the Secret")
	requireWarningEvent(t, r, "pod state could not be checked")
}

func TestCreateSystemUsersAclRefusesWhileAServerRuns(t *testing.T) {
	cluster, _, objs := systemPasswordFixture(false)
	objs = append(objs, systemPasswordPod("h1", "valkey-h1-0-0-0", serverContainerName))
	r, _ := systemPasswordReconciler(t, objs)

	_, err := r.createSystemUsersAcl(context.Background(), cluster)
	require.ErrorIs(t, err, errSystemPasswordSecretMissing)
}

func TestReportSystemPasswordSecretMissingSetsDegraded(t *testing.T) {
	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	err := fmt.Errorf("users: %w: x is missing _operator", errSystemPasswordSecretMissing)

	require.True(t, reportSystemPasswordSecretMissing(cluster, err))
	degraded := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)
	require.NotNil(t, degraded)
	require.Equal(t, metav1.ConditionTrue, degraded.Status)
	require.Equal(t, "SystemPasswordSecretMissing", degraded.Reason)
	require.Equal(t, err.Error(), degraded.Message)
	ready := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionReady)
	require.NotNil(t, ready)
	require.Equal(t, metav1.ConditionFalse, ready.Status)
	require.Equal(t, "UsersACLError", ready.Reason)
}

// Degraded clears on the first pass past the guard, whatever happens next.
func TestReportSystemPasswordSecretMissingClearsOnSuccess(t *testing.T) {
	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	require.True(t, reportSystemPasswordSecretMissing(cluster, fmt.Errorf("%w", errSystemPasswordSecretMissing)))

	require.False(t, reportSystemPasswordSecretMissing(cluster, nil))
	require.Nil(t, meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionDegraded))

	require.True(t, reportSystemPasswordSecretMissing(cluster, fmt.Errorf("%w", errSystemPasswordSecretMissing)))
	require.False(t, reportSystemPasswordSecretMissing(cluster, fmt.Errorf("other")))
	require.Nil(t, meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionDegraded))
}

func TestReportSystemPasswordSecretMissingKeepsOtherDegradedReasons(t *testing.T) {
	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	setCondition(cluster, valkeyiov1alpha1.ConditionDegraded, valkeyiov1alpha1.ReasonUsersACLUnresolved, "alice", metav1.ConditionTrue)

	require.False(t, reportSystemPasswordSecretMissing(cluster, nil))
	degraded := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)
	require.NotNil(t, degraded)
	require.Equal(t, valkeyiov1alpha1.ReasonUsersACLUnresolved, degraded.Reason)
}
