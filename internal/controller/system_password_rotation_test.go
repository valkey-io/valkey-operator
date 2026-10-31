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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

func TestReplicationAuthShellIsNotExpandedByKubelet(t *testing.T) {
	shell := replicationAuthShell()
	require.NotContains(t, shell, "$(")
	require.Contains(t, shell, systemPasswordMountPath+"/"+replicationUser)
	command := valkeyServerCommand([]string{"--cluster-announce-ip", "$(POD_IP)"})
	require.Equal(t, shellPath, command[0])
	require.NotContains(t, command, "$(PRIMARY_AUTH)")
}

func TestSystemPasswordVolumeFollowsClusterLabel(t *testing.T) {
	node := newTestValkeyNode("mynode", "test-ns")
	node.Labels = map[string]string{LabelCluster: "c1"}
	pts, err := buildValkeyNodePodTemplateSpec(node, valkeyNodeLabels(node))
	require.NoError(t, err)

	var volume *corev1.Volume
	for i := range pts.Spec.Volumes {
		if pts.Spec.Volumes[i].Name == systemPasswordVolumeName {
			volume = &pts.Spec.Volumes[i]
		}
	}
	require.NotNil(t, volume)
	require.Equal(t, getSystemPasswordSecretName("c1"), volume.Secret.SecretName)
	require.Equal(t, int32(0444), *volume.Secret.DefaultMode)
	require.Equal(t, []corev1.KeyToPath{
		{Key: operatorUser, Path: operatorUser},
		{Key: replicationUser, Path: replicationUser},
	}, volume.Secret.Items)

	var mount *corev1.VolumeMount
	for i := range pts.Spec.Containers[0].VolumeMounts {
		if pts.Spec.Containers[0].VolumeMounts[i].Name == systemPasswordVolumeName {
			mount = &pts.Spec.Containers[0].VolumeMounts[i]
		}
	}
	require.NotNil(t, mount)
	require.Equal(t, systemPasswordMountPath, mount.MountPath)
	require.True(t, mount.ReadOnly)
	require.Contains(t, pts.Spec.InitContainers[0].VolumeMounts, *mount)
}

func TestValidateSystemUserPasswordSecretAllowsRotationKeys(t *testing.T) {
	enabled := false
	cluster := &valkeyiov1alpha1.ValkeyCluster{Spec: valkeyiov1alpha1.ValkeyClusterSpec{
		Exporter: valkeyiov1alpha1.ExporterSpec{Enabled: &enabled},
	}}
	data := map[string][]byte{
		operatorUser:                            []byte("op"),
		replicationUser:                         []byte("re"),
		systemPasswordNextKey(operatorUser):     []byte("op-next"),
		systemPasswordPreviousKey(exporterUser): []byte("ex-prev"),
	}
	require.NoError(t, validateSystemUserPasswordSecret(data, cluster))

	data["_stranger"] = []byte("no")
	err := validateSystemUserPasswordSecret(data, cluster)
	require.ErrorIs(t, err, errUnknownSystemUser)
}

func TestAdvanceSystemPasswordRotationKeepsTheOldPasswordUntilClientsSwitch(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	secret := &corev1.Secret{Data: map[string][]byte{
		operatorUser:    []byte("op-old"),
		replicationUser: []byte("re-old"),
		exporterUser:    []byte("ex-old"),
	}}

	event, err := advanceSystemPasswordRotation(secret, true, systemPasswordSignals{
		Now:      now,
		SkipNext: map[string]bool{operatorUser: true, replicationUser: true, exporterUser: true},
	})
	require.NoError(t, err)
	require.Equal(t, "clock", event)
	require.Empty(t, secret.Data[systemPasswordNextKey(operatorUser)])
	require.Equal(t, now.Format(time.RFC3339Nano), secret.Annotations[annotationSystemPasswordRotatedAt])

	secret.Annotations[annotationSystemPasswordRotatedAt] = now.Add(-systemPasswordRotationPeriod - time.Second).Format(time.RFC3339Nano)
	event, err = advanceSystemPasswordRotation(secret, true, systemPasswordSignals{Now: now})
	require.NoError(t, err)
	require.Equal(t, "started", event)
	require.Equal(t, []byte("op-old"), secret.Data[operatorUser], "clients keep sending the current password")
	require.Equal(t, []byte("re-old"), secret.Data[replicationUser])
	require.Equal(t, []byte("ex-old"), secret.Data[exporterUser])
	nextOperator := append([]byte(nil), secret.Data[systemPasswordNextKey(operatorUser)]...)
	nextReplication := append([]byte(nil), secret.Data[systemPasswordNextKey(replicationUser)]...)
	nextExporter := append([]byte(nil), secret.Data[systemPasswordNextKey(exporterUser)]...)
	require.NotEmpty(t, nextOperator)
	require.NotEqual(t, []byte("op-old"), nextOperator)
	require.Equal(t, systemPasswordPhasePending, secret.Annotations[annotationSystemPasswordPhase])

	event, err = advanceSystemPasswordRotation(secret, true, systemPasswordSignals{Now: now})
	require.NoError(t, err)
	require.Empty(t, event)
	require.Equal(t, []byte("op-old"), secret.Data[operatorUser], "a new password is not installed before the nodes load it")

	event, err = advanceSystemPasswordRotation(secret, true, systemPasswordSignals{Now: now, PasswordsLive: true})
	require.NoError(t, err)
	require.Equal(t, "cutover", event)
	require.Equal(t, nextOperator, secret.Data[operatorUser])
	require.Equal(t, nextReplication, secret.Data[replicationUser])
	require.Equal(t, nextExporter, secret.Data[exporterUser])
	require.Equal(t, []byte("op-old"), secret.Data[systemPasswordPreviousKey(operatorUser)])
	require.Equal(t, []byte("re-old"), secret.Data[systemPasswordPreviousKey(replicationUser)])
	require.Equal(t, []byte("ex-old"), secret.Data[systemPasswordPreviousKey(exporterUser)])
	require.Empty(t, secret.Data[systemPasswordNextKey(operatorUser)])

	event, err = advanceSystemPasswordRotation(secret, true, systemPasswordSignals{
		Now: now.Add(time.Minute), ExporterCaughtUp: true, ReplicationCaughtUp: true,
	})
	require.NoError(t, err)
	require.Empty(t, event)
	require.Equal(t, []byte("op-old"), secret.Data[systemPasswordPreviousKey(operatorUser)])

	event, err = advanceSystemPasswordRotation(secret, true, systemPasswordSignals{
		Now: now.Add(systemPasswordCutoverGrace + time.Second),
	})
	require.NoError(t, err)
	require.Empty(t, event)
	require.Equal(t, []byte("op-old"), secret.Data[systemPasswordPreviousKey(operatorUser)], "probes on a pod without the mount still send the old password")

	event, err = advanceSystemPasswordRotation(secret, true, systemPasswordSignals{
		Now: now.Add(systemPasswordCutoverGrace + time.Second), OperatorCaughtUp: true, ExporterCaughtUp: false, ReplicationCaughtUp: false,
	})
	require.NoError(t, err)
	require.Equal(t, "holding", event)
	require.Empty(t, secret.Data[systemPasswordPreviousKey(operatorUser)])
	require.Equal(t, []byte("re-old"), secret.Data[systemPasswordPreviousKey(replicationUser)])
	require.Equal(t, []byte("ex-old"), secret.Data[systemPasswordPreviousKey(exporterUser)])

	event, err = advanceSystemPasswordRotation(secret, true, systemPasswordSignals{
		Now: now.Add(systemPasswordCutoverGrace + time.Second), ExporterCaughtUp: true, ReplicationCaughtUp: true,
	})
	require.NoError(t, err)
	require.Equal(t, "complete", event)
	require.False(t, systemPasswordHasSuffix(secret.Data, systemPasswordPreviousSuffix))
	require.Empty(t, secret.Annotations[annotationSystemPasswordPhase])
	require.Equal(t, nextOperator, secret.Data[operatorUser])
}

func TestExistingSecretStartsTheClockInsteadOfRotating(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	secret := &corev1.Secret{Data: map[string][]byte{
		operatorUser:    []byte("op-old"),
		replicationUser: []byte("re-old"),
		exporterUser:    []byte("ex-old"),
	}}
	event, err := advanceSystemPasswordRotation(secret, true, systemPasswordSignals{Now: now})
	require.NoError(t, err)
	require.Equal(t, "clock", event)
	require.Empty(t, secret.Data[systemPasswordNextKey(operatorUser)])
	require.Equal(t, now.Format(time.RFC3339Nano), secret.Annotations[annotationSystemPasswordRotatedAt])

	event, err = advanceSystemPasswordRotation(secret, true, systemPasswordSignals{Now: now.Add(time.Hour)})
	require.NoError(t, err)
	require.Empty(t, event)
	require.Empty(t, secret.Data[systemPasswordNextKey(operatorUser)])
}

func TestExporterHoldDoesNotBlockTheNextRotation(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	secret := &corev1.Secret{
		Data: map[string][]byte{
			operatorUser:                               []byte("op-new"),
			replicationUser:                            []byte("re-new"),
			exporterUser:                               []byte("ex-new"),
			systemPasswordPreviousKey(operatorUser):    []byte("op-old"),
			systemPasswordPreviousKey(replicationUser): []byte("re-old"),
			systemPasswordPreviousKey(exporterUser):    []byte("ex-old"),
		},
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			annotationSystemPasswordPhase:     systemPasswordPhaseCutover,
			annotationSystemPasswordCutoverAt: now.Format(time.RFC3339Nano),
			annotationSystemPasswordRotatedAt: now.Format(time.RFC3339Nano),
		}},
	}
	after := now.Add(systemPasswordCutoverGrace + time.Second)
	event, err := advanceSystemPasswordRotation(secret, true, systemPasswordSignals{
		Now: after, OperatorCaughtUp: true, ReplicationCaughtUp: true,
	})
	require.NoError(t, err)
	require.Equal(t, "holding", event)
	require.Empty(t, secret.Annotations[annotationSystemPasswordPhase])
	require.Equal(t, []byte("ex-old"), secret.Data[systemPasswordPreviousKey(exporterUser)])
	require.Empty(t, secret.Data[systemPasswordPreviousKey(operatorUser)])
	require.Empty(t, secret.Data[systemPasswordPreviousKey(replicationUser)])

	later := after.Add(systemPasswordRotationPeriod + time.Second)
	event, err = advanceSystemPasswordRotation(secret, true, systemPasswordSignals{Now: later})
	require.NoError(t, err)
	require.Equal(t, "started", event)
	require.Equal(t, []byte("op-new"), secret.Data[operatorUser])
	require.NotEmpty(t, secret.Data[systemPasswordNextKey(operatorUser)])
	require.Empty(t, secret.Data[systemPasswordNextKey(exporterUser)], "the held exporter password must not be promoted over")
	require.Equal(t, []byte("ex-old"), secret.Data[systemPasswordPreviousKey(exporterUser)])

	event, err = advanceSystemPasswordRotation(secret, true, systemPasswordSignals{Now: later, ExporterCaughtUp: true})
	require.NoError(t, err)
	require.Equal(t, "exporter-released", event)
	require.Empty(t, secret.Data[systemPasswordPreviousKey(exporterUser)])
}

func TestSystemUserPasswordHashesCoverEveryAcceptedPassword(t *testing.T) {
	data := map[string][]byte{
		operatorUser:                            []byte("current"),
		systemPasswordNextKey(operatorUser):     []byte("next"),
		systemPasswordPreviousKey(operatorUser): []byte("previous"),
	}
	hashes := systemUserPasswordHashes(data, operatorUser)
	require.Equal(t, []string{
		systemPasswordDigest([]byte("current")),
		systemPasswordDigest([]byte("next")),
		systemPasswordDigest([]byte("previous")),
	}, hashes)
}

func TestSystemPasswordACLReadyRequiresEveryNode(t *testing.T) {
	data := map[string][]byte{
		operatorUser:                        []byte("current"),
		systemPasswordNextKey(operatorUser): []byte("next"),
		replicationUser:                     []byte("re"),
	}
	users := []string{operatorUser, replicationUser}
	acl, revision := testSystemPasswordACL(data, users)
	require.True(t, systemPasswordACLReady(acl, data, users, []string{revision, revision}))
	require.True(t, systemPasswordACLReady(acl, data, users, []string{revision}))
	require.False(t, systemPasswordACLReady(acl, data, users, nil))
	require.False(t, systemPasswordACLReady(acl, data, users, []string{revision, "stale"}))
	data[operatorUser] = []byte("someone-else")
	require.False(t, systemPasswordACLReady(acl, data, users, []string{revision}))
}

func TestExporterAndReplicationCaughtUp(t *testing.T) {
	cutover := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	running := func(started time.Time) corev1.Pod {
		return corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: metricsExporterContainerName,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{
				StartedAt: metav1.NewTime(started),
			}},
		}}}}
	}
	require.False(t, exporterPasswordCaughtUp([]corev1.Pod{running(cutover.Add(-time.Second))}, cutover))
	require.True(t, exporterPasswordCaughtUp([]corev1.Pod{running(cutover)}, cutover))
	require.True(t, exporterPasswordCaughtUp(nil, cutover))

	current := []byte("re-new")
	setAt := time.Date(2026, 10, 30, 12, 5, 0, 0, time.UTC)
	node := valkeyiov1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			annotationPrimaryAuthHash:  systemPasswordDigest(current),
			annotationPrimaryAuthSetAt: setAt.Format(time.RFC3339Nano),
		}},
		Status: valkeyiov1alpha1.ValkeyNodeStatus{PodName: "p"},
	}
	oldPod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: serverContainerName, Command: []string{"valkey-server", "--primaryauth", "$(PRIMARY_AUTH)"},
	}}}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name:  serverContainerName,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(setAt.Add(-time.Second))}},
	}}}}
	newPod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: serverContainerName, Command: valkeyServerCommand(nil),
	}}}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name:  serverContainerName,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(setAt.Add(-time.Second))}},
	}}}}
	restarted := newPod.DeepCopy()
	restarted.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(setAt.Add(time.Second))
	require.False(t, replicationPasswordCaughtUp([]valkeyiov1alpha1.ValkeyNode{node}, []corev1.Pod{oldPod}, current))
	require.True(t, replicationPasswordCaughtUp([]valkeyiov1alpha1.ValkeyNode{node}, []corev1.Pod{newPod}, current))
	require.False(t, replicationPasswordCaughtUp([]valkeyiov1alpha1.ValkeyNode{node}, []corev1.Pod{*restarted}, current))
	unstamped := node.DeepCopy()
	delete(unstamped.Annotations, annotationPrimaryAuthSetAt)
	require.False(t, replicationPasswordCaughtUp([]valkeyiov1alpha1.ValkeyNode{*unstamped}, []corev1.Pod{newPod}, current))
	require.False(t, replicationPasswordCaughtUp([]valkeyiov1alpha1.ValkeyNode{{}}, []corev1.Pod{newPod}, current))
	require.True(t, replicationPasswordCaughtUp(nil, nil, current))
}

func TestApplyReplicationPasswordSetsPrimaryAuthDuringCutover(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	node := &valkeyiov1alpha1.ValkeyNode{ObjectMeta: metav1.ObjectMeta{
		Name: "n0", Namespace: "ns", Labels: map[string]string{LabelCluster: "c1"},
	}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: getSystemPasswordSecretName("c1"), Namespace: "ns",
	}, Data: map[string][]byte{
		replicationUser: []byte("re-new"),
		systemPasswordPreviousKey(replicationUser): []byte("re-old"),
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1.Pod{}).WithObjects(node, secret).Build()
	valkeyClient := &fakeConfigClient{}
	r := &ValkeyNodeReconciler{
		Client:    c,
		APIReader: c,
		newConfigClient: func(context.Context, *ValkeyNodeReconciler, *valkeyiov1alpha1.ValkeyNode) (valkeyConfigClient, error) {
			return valkeyClient, nil
		},
	}
	require.NoError(t, r.applyReplicationPassword(context.Background(), node))
	require.Equal(t, map[string]string{"primaryauth": "re-new"}, valkeyClient.params)

	got := &valkeyiov1alpha1.ValkeyNode{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(node), got))
	require.Equal(t, systemPasswordDigest([]byte("re-new")), got.Annotations[annotationPrimaryAuthHash])

	valkeyClient.params = nil
	require.NoError(t, r.applyReplicationPassword(context.Background(), got))
	require.Nil(t, valkeyClient.params, "a node already on the current password must not CONFIG SET again")

	setAt := time.Now().Add(-time.Minute)
	got.Annotations[annotationPrimaryAuthSetAt] = setAt.UTC().Format(time.RFC3339Nano)
	got.Status.PodName = "n0-pod"
	require.NoError(t, c.Update(context.Background(), got))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "n0-pod", Namespace: "ns"}}
	require.NoError(t, c.Create(context.Background(), pod))
	pod.Status = corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name: serverContainerName,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{
			StartedAt: metav1.NewTime(setAt.Add(time.Second)),
		}},
	}}}
	require.NoError(t, c.Status().Update(context.Background(), pod))
	require.NoError(t, r.applyReplicationPassword(context.Background(), got))
	require.Equal(t, map[string]string{"primaryauth": "re-new"}, valkeyClient.params, "a restarted server may have read a stale volume")
}

func TestUpsertStagesTheNextPasswordWithoutReplacingTheCurrent(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	enabled := false
	cluster := &valkeyiov1alpha1.ValkeyCluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}, Spec: valkeyiov1alpha1.ValkeyClusterSpec{
		Exporter: valkeyiov1alpha1.ExporterSpec{Enabled: &enabled},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
	r := &ValkeyClusterReconciler{Client: c, Scheme: scheme}

	secret, err := r.upsertSystemUsersPasswordSecret(context.Background(), cluster)
	require.NoError(t, err)
	require.NotEmpty(t, secret.Data[operatorUser])
	require.NotEmpty(t, secret.Data[replicationUser])
	require.Empty(t, secret.Data[systemPasswordNextKey(operatorUser)])
	require.Empty(t, secret.Annotations[annotationSystemPasswordPhase])
	require.NotEmpty(t, secret.Annotations[annotationSystemPasswordRotatedAt])

	aged := secret.DeepCopy()
	aged.Annotations[annotationSystemPasswordRotatedAt] = time.Now().Add(-systemPasswordRotationPeriod - time.Second).Format(time.RFC3339Nano)
	require.NoError(t, c.Update(context.Background(), aged))
	current := append([]byte(nil), aged.Data[operatorUser]...)

	secret, err = r.upsertSystemUsersPasswordSecret(context.Background(), cluster)
	require.NoError(t, err)
	require.Equal(t, current, secret.Data[operatorUser])
	require.NotEmpty(t, secret.Data[systemPasswordNextKey(operatorUser)])
	require.NotEqual(t, current, secret.Data[systemPasswordNextKey(operatorUser)])
	require.Equal(t, systemPasswordPhasePending, secret.Annotations[annotationSystemPasswordPhase])
	require.Contains(t, systemUserPasswordHashes(secret.Data, operatorUser), systemPasswordDigest(current))
	require.Contains(t, systemUserPasswordHashes(secret.Data, operatorUser), systemPasswordDigest(secret.Data[systemPasswordNextKey(operatorUser)]))
}

func TestCreateSystemUsersAclRotatesAnExistingSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	enabled := false
	cluster := &valkeyiov1alpha1.ValkeyCluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}, Spec: valkeyiov1alpha1.ValkeyClusterSpec{
		Exporter: valkeyiov1alpha1.ExporterSpec{Enabled: &enabled},
	}}
	current := []byte("op-current")
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: getSystemPasswordSecretName("c1"), Namespace: "ns",
		Annotations: map[string]string{
			annotationSystemPasswordRotatedAt: time.Now().Add(-systemPasswordRotationPeriod - time.Second).Format(time.RFC3339Nano),
		},
	}, Data: map[string][]byte{
		operatorUser:    current,
		replicationUser: []byte("re-current"),
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, secret).Build()
	r := &ValkeyClusterReconciler{Client: c, Scheme: scheme, APIReader: c}

	acl, err := r.createSystemUsersAcl(context.Background(), cluster)
	require.NoError(t, err)
	require.Contains(t, acl, "user "+operatorUser)

	got := &corev1.Secret{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), got))
	require.Equal(t, current, got.Data[operatorUser])
	require.NotEmpty(t, got.Data[systemPasswordNextKey(operatorUser)])
	require.NotEqual(t, current, got.Data[systemPasswordNextKey(operatorUser)])
	require.Equal(t, systemPasswordPhasePending, got.Annotations[annotationSystemPasswordPhase])
}

func TestApplyReplicationPasswordSkipsASteadySecret(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	node := &valkeyiov1alpha1.ValkeyNode{ObjectMeta: metav1.ObjectMeta{
		Name: "n0", Namespace: "ns", Labels: map[string]string{LabelCluster: "c1"},
	}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: getSystemPasswordSecretName("c1"), Namespace: "ns",
	}, Data: map[string][]byte{replicationUser: []byte("re-current")}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, secret).Build()
	valkeyClient := &fakeConfigClient{}
	r := &ValkeyNodeReconciler{
		Client:    c,
		APIReader: c,
		newConfigClient: func(context.Context, *ValkeyNodeReconciler, *valkeyiov1alpha1.ValkeyNode) (valkeyConfigClient, error) {
			return valkeyClient, nil
		},
	}
	require.NoError(t, r.applyReplicationPassword(context.Background(), node))
	require.Nil(t, valkeyClient.params)
}

// testSystemPasswordACL builds the aclfile text the cluster controller would
// write for users and returns that text plus its revision hash.
func testSystemPasswordACL(data map[string][]byte, users []string) (string, string) {
	var body strings.Builder
	for _, user := range users {
		acl := buildUserAcl(valkeyiov1alpha1.UserAclSpec{
			Name: user, Enabled: true, RawAcl: systemUsersAcls[user],
		}, systemUserPasswordHashes(data, user))
		fmt.Fprintf(&body, "%s\n", acl)
	}
	managed := body.String()
	revision := systemPasswordDigest([]byte(managed))
	fmt.Fprintf(&body, "user %s off resetchannels -@all #%s\n", aclRevisionUser, revision)
	return body.String(), revision
}
