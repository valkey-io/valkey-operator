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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// testRotationInterval matches the default spec.acl.systemUsers.passwordRotation.intervalDays.
const testRotationInterval = valkeyiov1alpha1.DefaultSystemPasswordRotationDays * 24 * time.Hour

func TestReplicationAuthShellIsNotExpandedByKubelet(t *testing.T) {
	for _, handoff := range []bool{false, true} {
		shell := replicationAuthShell(handoff)
		require.NotContains(t, shell, "$(")
		require.Contains(t, shell, systemPasswordMountPath+"/"+replicationUser)
	}
	command := serverCommand(&valkeyiov1alpha1.ValkeyNode{}, []string{"--cluster-announce-ip", "$(POD_IP)"})
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

func TestTurningRotationOnDoesNotRollPods(t *testing.T) {
	off := pinTestCluster(nil, false)
	on := off.DeepCopy()
	on.Spec.ACL = &valkeyiov1alpha1.ACLSpec{SystemUsers: &valkeyiov1alpha1.SystemUsersSpec{
		PasswordRotation: &valkeyiov1alpha1.SystemPasswordRotationSpec{IntervalDays: 30},
	}}
	nodeOff, nodeOn := buildClusterValkeyNode(off, 0, 0), buildClusterValkeyNode(on, 0, 0)
	require.Equal(t, nodeOff.Spec, nodeOn.Spec)
	stsOff, err := buildValkeyNodeStatefulSet(nodeOff)
	require.NoError(t, err)
	stsOn, err := buildValkeyNodeStatefulSet(nodeOn)
	require.NoError(t, err)
	require.Equal(t, stsOff.Spec.Template, stsOn.Spec.Template, "turning rotation on changes only the Secret")
	require.True(t, containerMounts(stsOff.Spec.Template.Spec.Containers[0], systemPasswordMountPath),
		"the password files are mounted with rotation off too")
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

// rotate runs one rotation step and returns the event it records.
func rotate(t *testing.T, secret *corev1.Secret, exporterEnabled bool, signals systemPasswordSignals) string {
	t.Helper()
	before := secret.DeepCopy()
	require.NoError(t, advanceSystemPasswordRotation(secret, exporterEnabled, signals))
	reason, _ := systemPasswordRotationEvent(before, secret)
	return reason
}

func TestAdvanceSystemPasswordRotationKeepsTheOldPasswordUntilClientsSwitch(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	secret := &corev1.Secret{Data: map[string][]byte{
		operatorUser:    []byte("op-old"),
		replicationUser: []byte("re-old"),
		exporterUser:    []byte("ex-old"),
	}}

	require.Empty(t, rotate(t, secret, true, systemPasswordSignals{
		Now:      now,
		Interval: testRotationInterval,
		SkipNext: map[string]bool{operatorUser: true, replicationUser: true, exporterUser: true},
	}))
	require.Empty(t, secret.Data[systemPasswordNextKey(operatorUser)])
	require.Equal(t, now.Format(time.RFC3339Nano), secret.Annotations[annotationSystemPasswordsCurrentSince])

	secret.Annotations[annotationSystemPasswordsCurrentSince] = now.Add(-testRotationInterval - time.Second).Format(time.RFC3339Nano)
	require.Equal(t, "SystemPasswordRotationStarted", rotate(t, secret, true, systemPasswordSignals{Now: now, Interval: testRotationInterval}))
	require.Equal(t, []byte("op-old"), secret.Data[operatorUser], "clients keep sending the current password")
	require.Equal(t, []byte("re-old"), secret.Data[replicationUser])
	require.Equal(t, []byte("ex-old"), secret.Data[exporterUser])
	nextOperator := append([]byte(nil), secret.Data[systemPasswordNextKey(operatorUser)]...)
	nextReplication := append([]byte(nil), secret.Data[systemPasswordNextKey(replicationUser)]...)
	nextExporter := append([]byte(nil), secret.Data[systemPasswordNextKey(exporterUser)]...)
	require.NotEmpty(t, nextOperator)
	require.NotEqual(t, []byte("op-old"), nextOperator)
	require.Equal(t, systemPasswordPhasePending, secret.Annotations[annotationSystemPasswordsRotationPhase])

	require.Empty(t, rotate(t, secret, true, systemPasswordSignals{Now: now}))
	require.Equal(t, []byte("op-old"), secret.Data[operatorUser], "a new password is not installed before the nodes load it")

	require.Equal(t, "SystemPasswordRotationCutover", rotate(t, secret, true, systemPasswordSignals{Now: now, PasswordsLive: true}))
	require.Equal(t, nextOperator, secret.Data[operatorUser])
	require.Equal(t, nextReplication, secret.Data[replicationUser])
	require.Equal(t, nextExporter, secret.Data[exporterUser])
	require.Equal(t, []byte("op-old"), secret.Data[systemPasswordPreviousKey(operatorUser)])
	require.Equal(t, []byte("re-old"), secret.Data[systemPasswordPreviousKey(replicationUser)])
	require.Equal(t, []byte("ex-old"), secret.Data[systemPasswordPreviousKey(exporterUser)])
	require.Empty(t, secret.Data[systemPasswordNextKey(operatorUser)])

	require.Empty(t, rotate(t, secret, true, systemPasswordSignals{
		Now: now.Add(time.Minute), ExporterCaughtUp: true, ReplicationCaughtUp: true,
	}))
	require.Equal(t, []byte("op-old"), secret.Data[systemPasswordPreviousKey(operatorUser)])

	require.Empty(t, rotate(t, secret, true, systemPasswordSignals{
		Now: now.Add(systemPasswordCutoverGrace + time.Second),
	}))
	require.Equal(t, []byte("op-old"), secret.Data[systemPasswordPreviousKey(operatorUser)], "probes on a pod without the mount still send the old password")

	require.Equal(t, "SystemPasswordRotationHolding", rotate(t, secret, true, systemPasswordSignals{
		Now: now.Add(systemPasswordCutoverGrace + time.Second), OperatorCaughtUp: true, ExporterCaughtUp: false, ReplicationCaughtUp: false,
	}))
	require.Empty(t, secret.Data[systemPasswordPreviousKey(operatorUser)])
	require.Equal(t, []byte("re-old"), secret.Data[systemPasswordPreviousKey(replicationUser)])
	require.Equal(t, []byte("ex-old"), secret.Data[systemPasswordPreviousKey(exporterUser)])

	require.Equal(t, "SystemPasswordRotationFinished", rotate(t, secret, true, systemPasswordSignals{
		Now: now.Add(systemPasswordCutoverGrace + time.Second), ExporterCaughtUp: true, ReplicationCaughtUp: true,
	}))
	require.False(t, systemPasswordHasSuffix(secret.Data, systemPasswordPreviousSuffix))
	require.Empty(t, secret.Annotations[annotationSystemPasswordsRotationPhase])
	require.Equal(t, nextOperator, secret.Data[operatorUser])
}

func TestSystemPasswordRotationStartsOnlyWhenTurnedOn(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	secret := &corev1.Secret{Data: map[string][]byte{
		operatorUser:    []byte("op-old"),
		replicationUser: []byte("re-old"),
	}}
	require.Empty(t, rotate(t, secret, false, systemPasswordSignals{Now: now}))
	require.Empty(t, secret.Annotations[annotationSystemPasswordsCurrentSince], "rotation off starts no clock")

	// A clock left by an earlier rotation is dropped while rotation is off.
	secret.Annotations[annotationSystemPasswordsCurrentSince] = now.Add(-testRotationInterval - time.Second).Format(time.RFC3339Nano)
	require.Empty(t, rotate(t, secret, false, systemPasswordSignals{Now: now}))
	require.Empty(t, secret.Annotations[annotationSystemPasswordsCurrentSince])
	require.False(t, systemPasswordHasSuffix(secret.Data, systemPasswordNextSuffix))

	// Turning it on again waits the configured interval.
	week := 7 * 24 * time.Hour
	require.Empty(t, rotate(t, secret, false, systemPasswordSignals{Now: now, Interval: week}))
	require.Equal(t, now.Format(time.RFC3339Nano), secret.Annotations[annotationSystemPasswordsCurrentSince])

	require.Empty(t, rotate(t, secret, false, systemPasswordSignals{Now: now.Add(week - time.Hour), Interval: week}))

	require.Equal(t, "SystemPasswordRotationStarted", rotate(t, secret, false, systemPasswordSignals{Now: now.Add(week + time.Second), Interval: week}))
}

func TestSystemPasswordRotationUnderWayFinishesAfterItIsTurnedOff(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			annotationSystemPasswordsRotationPhase: systemPasswordPhasePending,
			annotationSystemPasswordsCurrentSince:  now.Format(time.RFC3339Nano),
		}},
		Data: map[string][]byte{
			operatorUser:                           []byte("op-old"),
			systemPasswordNextKey(operatorUser):    []byte("op-new"),
			replicationUser:                        []byte("re-old"),
			systemPasswordNextKey(replicationUser): []byte("re-new"),
		},
	}
	require.Equal(t, "SystemPasswordRotationCutover", rotate(t, secret, false, systemPasswordSignals{Now: now, PasswordsLive: true}))
	require.Equal(t, []byte("op-new"), secret.Data[operatorUser])

	require.Equal(t, "SystemPasswordRotationFinished", rotate(t, secret, false, systemPasswordSignals{
		Now: now.Add(systemPasswordCutoverGrace + time.Second), OperatorCaughtUp: true, ReplicationCaughtUp: true,
	}))
	require.False(t, systemPasswordHasSuffix(secret.Data, systemPasswordPreviousSuffix))
}

func TestUpsertLeavesSystemPasswordsAloneWhileRotationIsOff(t *testing.T) {
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
	require.Empty(t, secret.Annotations[annotationSystemPasswordsCurrentSince])

	stored := &corev1.Secret{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), stored))
	again, err := r.upsertSystemUsersPasswordSecret(context.Background(), cluster)
	require.NoError(t, err)
	require.Equal(t, stored.ResourceVersion, again.ResourceVersion, "with rotation off the Secret is not written again")
	require.False(t, systemPasswordHasSuffix(again.Data, systemPasswordNextSuffix))
}

func TestExistingSecretStartsTheClockInsteadOfRotating(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	secret := &corev1.Secret{Data: map[string][]byte{
		operatorUser:    []byte("op-old"),
		replicationUser: []byte("re-old"),
		exporterUser:    []byte("ex-old"),
	}}
	require.Empty(t, rotate(t, secret, true, systemPasswordSignals{Now: now, Interval: testRotationInterval}))
	require.Empty(t, secret.Data[systemPasswordNextKey(operatorUser)])
	require.Equal(t, now.Format(time.RFC3339Nano), secret.Annotations[annotationSystemPasswordsCurrentSince])

	require.Empty(t, rotate(t, secret, true, systemPasswordSignals{Now: now.Add(time.Hour), Interval: testRotationInterval}))
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
			annotationSystemPasswordsRotationPhase: systemPasswordPhaseCutover,
			annotationSystemPasswordsCutoverAt:     now.Format(time.RFC3339Nano),
			annotationSystemPasswordsCurrentSince:  now.Format(time.RFC3339Nano),
		}},
	}
	after := now.Add(systemPasswordCutoverGrace + time.Second)
	require.Equal(t, "SystemPasswordRotationHolding", rotate(t, secret, true, systemPasswordSignals{
		Now: after, OperatorCaughtUp: true, ReplicationCaughtUp: true,
	}))
	require.Empty(t, secret.Annotations[annotationSystemPasswordsRotationPhase])
	require.Equal(t, []byte("ex-old"), secret.Data[systemPasswordPreviousKey(exporterUser)])
	require.Empty(t, secret.Data[systemPasswordPreviousKey(operatorUser)])
	require.Empty(t, secret.Data[systemPasswordPreviousKey(replicationUser)])

	later := after.Add(testRotationInterval + time.Second)
	require.Equal(t, "SystemPasswordRotationStarted", rotate(t, secret, true, systemPasswordSignals{Now: later, Interval: testRotationInterval}))
	require.Equal(t, []byte("op-new"), secret.Data[operatorUser])
	require.NotEmpty(t, secret.Data[systemPasswordNextKey(operatorUser)])
	require.Empty(t, secret.Data[systemPasswordNextKey(exporterUser)], "the held exporter password must not be promoted over")
	require.Equal(t, []byte("ex-old"), secret.Data[systemPasswordPreviousKey(exporterUser)])

	require.Empty(t, rotate(t, secret, true, systemPasswordSignals{Now: later, ExporterCaughtUp: true}))
	require.Empty(t, secret.Data[systemPasswordPreviousKey(exporterUser)])
}

func TestSystemUserPasswordHashesCoverEveryAcceptedPassword(t *testing.T) {
	data := map[string][]byte{
		operatorUser:                            []byte("current"),
		systemPasswordNextKey(operatorUser):     []byte("next"),
		systemPasswordPreviousKey(operatorUser): []byte("previous"),
	}
	hashes := systemUserPasswordHashes(data, operatorUser)
	require.ElementsMatch(t, []string{
		systemPasswordDigest([]byte("current")),
		systemPasswordDigest([]byte("next")),
		systemPasswordDigest([]byte("previous")),
	}, hashes)

	// Cutover moves the next password to current and the current one to
	// previous. The hashes, and so the aclfile, stay the same.
	pending := map[string][]byte{operatorUser: []byte("a"), systemPasswordNextKey(operatorUser): []byte("b")}
	cutover := map[string][]byte{operatorUser: []byte("b"), systemPasswordPreviousKey(operatorUser): []byte("a")}
	require.Equal(t, systemUserPasswordHashes(pending, operatorUser), systemUserPasswordHashes(cutover, operatorUser))
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
	// A start right after cutover can still read the old Secret from the kubelet cache.
	require.False(t, exporterPasswordCaughtUp([]corev1.Pod{running(cutover)}, cutover))
	require.False(t, exporterPasswordCaughtUp([]corev1.Pod{running(cutover.Add(systemPasswordCutoverGrace - time.Second))}, cutover))
	require.True(t, exporterPasswordCaughtUp([]corev1.Pod{running(cutover.Add(systemPasswordCutoverGrace))}, cutover))
	require.True(t, exporterPasswordCaughtUp(nil, cutover))

	current := []byte("re-new")
	setAt := time.Date(2026, 10, 30, 12, 5, 0, 0, time.UTC)
	node := valkeyiov1alpha1.ValkeyNode{Status: valkeyiov1alpha1.ValkeyNodeStatus{
		PodName:                        "p",
		AppliedReplicationPasswordHash: systemPasswordDigest(current),
		ReplicationPasswordAppliedTime: &metav1.Time{Time: setAt},
	}}
	oldPod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: serverContainerName, Command: []string{"valkey-server", "--primaryauth", "$(PRIMARY_AUTH)"},
	}}}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name:  serverContainerName,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(setAt.Add(-time.Second))}},
	}}}}
	newPod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: serverContainerName, Command: serverCommand(&valkeyiov1alpha1.ValkeyNode{}, nil),
	}}}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name:  serverContainerName,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(setAt.Add(-time.Second))}},
	}}}}
	restarted := newPod.DeepCopy()
	restarted.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(setAt.Add(time.Second))
	require.False(t, replicationPasswordCaughtUp([]valkeyiov1alpha1.ValkeyNode{node}, []corev1.Pod{oldPod}, current))
	require.True(t, replicationPasswordCaughtUp([]valkeyiov1alpha1.ValkeyNode{node}, []corev1.Pod{newPod}, current))
	require.False(t, replicationPasswordCaughtUp([]valkeyiov1alpha1.ValkeyNode{node}, []corev1.Pod{*restarted}, current))
	sameSecond := node.DeepCopy()
	sameSecond.Status.ReplicationPasswordAppliedTime = &metav1.Time{Time: setAt.Add(500 * time.Millisecond)}
	samePod := newPod.DeepCopy()
	samePod.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(setAt)
	require.False(t, replicationPasswordCaughtUp([]valkeyiov1alpha1.ValkeyNode{*sameSecond}, []corev1.Pod{*samePod}, current))
	require.True(t, serverStartedBeforeConfigSet(setAt.Add(-time.Second), setAt.Add(500*time.Millisecond)))
	require.False(t, serverStartedBeforeConfigSet(setAt, setAt.Add(500*time.Millisecond)))
	unstamped := node.DeepCopy()
	unstamped.Status.ReplicationPasswordAppliedTime = nil
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
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1.Pod{}, &valkeyiov1alpha1.ValkeyNode{}).WithObjects(node, secret).Build()
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
	require.Equal(t, systemPasswordDigest([]byte("re-new")), got.Status.AppliedReplicationPasswordHash)
	require.NotNil(t, got.Status.ReplicationPasswordAppliedTime)
	require.Empty(t, got.Annotations, "the record is status, not metadata")

	valkeyClient.params = nil
	require.NoError(t, r.applyReplicationPassword(context.Background(), got))
	require.Nil(t, valkeyClient.params, "a node already on the current password must not CONFIG SET again")

	setAt := time.Now().Add(-time.Minute).Truncate(time.Second)
	got.Status.ReplicationPasswordAppliedTime = &metav1.Time{Time: setAt}
	got.Status.PodName = "n0-pod"
	require.NoError(t, c.Status().Update(context.Background(), got))
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
		ACL: &valkeyiov1alpha1.ACLSpec{SystemUsers: &valkeyiov1alpha1.SystemUsersSpec{
			PasswordRotation: &valkeyiov1alpha1.SystemPasswordRotationSpec{IntervalDays: 30},
		}},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
	r := &ValkeyClusterReconciler{Client: c, Scheme: scheme}

	secret, err := r.upsertSystemUsersPasswordSecret(context.Background(), cluster)
	require.NoError(t, err)
	require.NotEmpty(t, secret.Data[operatorUser])
	require.NotEmpty(t, secret.Data[replicationUser])
	require.Empty(t, secret.Data[systemPasswordNextKey(operatorUser)])
	require.Empty(t, secret.Annotations[annotationSystemPasswordsRotationPhase])
	require.NotEmpty(t, secret.Annotations[annotationSystemPasswordsCurrentSince])

	aged := secret.DeepCopy()
	aged.Annotations[annotationSystemPasswordsCurrentSince] = time.Now().Add(-testRotationInterval - time.Second).Format(time.RFC3339Nano)
	require.NoError(t, c.Update(context.Background(), aged))
	current := append([]byte(nil), aged.Data[operatorUser]...)

	secret, err = r.upsertSystemUsersPasswordSecret(context.Background(), cluster)
	require.NoError(t, err)
	require.Equal(t, current, secret.Data[operatorUser])
	require.NotEmpty(t, secret.Data[systemPasswordNextKey(operatorUser)])
	require.NotEqual(t, current, secret.Data[systemPasswordNextKey(operatorUser)])
	require.Equal(t, systemPasswordPhasePending, secret.Annotations[annotationSystemPasswordsRotationPhase])
	require.Contains(t, systemUserPasswordHashes(secret.Data, operatorUser), systemPasswordDigest(current))
	require.Contains(t, systemUserPasswordHashes(secret.Data, operatorUser), systemPasswordDigest(secret.Data[systemPasswordNextKey(operatorUser)]))
}

func TestUpsertHoldsTheRotationWhenAStepFails(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	enabled := false
	cluster := &valkeyiov1alpha1.ValkeyCluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}, Spec: valkeyiov1alpha1.ValkeyClusterSpec{
		Exporter: valkeyiov1alpha1.ExporterSpec{Enabled: &enabled},
		ACL: &valkeyiov1alpha1.ACLSpec{SystemUsers: &valkeyiov1alpha1.SystemUsersSpec{
			PasswordRotation: &valkeyiov1alpha1.SystemPasswordRotationSpec{IntervalDays: 30},
		}},
	}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: getSystemPasswordSecretName("c1"), Namespace: "ns",
		Annotations: map[string]string{annotationSystemPasswordsRotationPhase: "bogus"},
	}, Data: map[string][]byte{operatorUser: []byte("op-current")}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, secret).Build()
	recorder := events.NewFakeRecorder(10)
	r := &ValkeyClusterReconciler{Client: c, Scheme: scheme, Recorder: recorder}

	got, err := r.upsertSystemUsersPasswordSecret(context.Background(), cluster)
	require.NoError(t, err, "a failed rotation step does not fail the system users")
	require.Contains(t, <-recorder.Events, "Warning SystemPasswordRotationHeld")
	require.NotEmpty(t, got.Data[replicationUser], "a missing password is still filled")
	require.Equal(t, []byte("op-current"), got.Data[operatorUser])
	require.Equal(t, "bogus", got.Annotations[annotationSystemPasswordsRotationPhase], "the rotation stays where it was")
	require.False(t, systemPasswordHasSuffix(got.Data, systemPasswordNextSuffix))

	again, err := r.upsertSystemUsersPasswordSecret(context.Background(), cluster)
	require.NoError(t, err)
	require.Contains(t, <-recorder.Events, "Warning SystemPasswordRotationHeld")
	require.Equal(t, got.ResourceVersion, again.ResourceVersion, "a held rotation does not write the Secret")
}

func TestExporterHoldConditionFollowsTheStoredSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	enabled := true
	cluster := &valkeyiov1alpha1.ValkeyCluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}, Spec: valkeyiov1alpha1.ValkeyClusterSpec{
		Exporter: valkeyiov1alpha1.ExporterSpec{Enabled: &enabled},
	}}
	now := time.Now()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: getSystemPasswordSecretName("c1"), Namespace: "ns",
		Annotations: map[string]string{
			annotationSystemPasswordsCurrentSince: now.Format(time.RFC3339Nano),
			annotationExporterPasswordHeldSince:   now.Add(-systemPasswordCutoverGrace - time.Minute).Format(time.RFC3339Nano),
		},
	}, Data: map[string][]byte{
		operatorUser:                            []byte("op-current"),
		replicationUser:                         []byte("re-current"),
		exporterUser:                            []byte("ex-current"),
		systemPasswordPreviousKey(exporterUser): []byte("ex-old"),
	}}
	failUpdate := true
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, secret).WithInterceptorFuncs(interceptor.Funcs{
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*corev1.Secret); ok && failUpdate {
				return apierrors.NewConflict(corev1.Resource("secrets"), obj.GetName(), fmt.Errorf("modified"))
			}
			return cl.Update(ctx, obj, opts...)
		},
	}).Build()
	r := &ValkeyClusterReconciler{Client: c, Scheme: scheme, APIReader: c}
	r.syncExporterPasswordHold(cluster, secret, systemPasswordSignals{Now: now, ExporterPodsOnOldPassword: []string{"c1-0-0"}})
	require.True(t, meta.IsStatusConditionTrue(cluster.Status.Conditions, valkeyiov1alpha1.ConditionExporterPasswordHeld))

	// No exporter is running, so the release is due, but the write fails.
	_, err := r.upsertSystemUsersPasswordSecret(context.Background(), cluster)
	require.Error(t, err)
	stored := &corev1.Secret{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), stored))
	require.Equal(t, []byte("ex-old"), stored.Data[systemPasswordPreviousKey(exporterUser)])
	require.True(t, meta.IsStatusConditionTrue(cluster.Status.Conditions, valkeyiov1alpha1.ConditionExporterPasswordHeld))

	failUpdate = false
	_, err = r.upsertSystemUsersPasswordSecret(context.Background(), cluster)
	require.NoError(t, err)
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), stored))
	require.Empty(t, stored.Data[systemPasswordPreviousKey(exporterUser)])
	require.Nil(t, meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionExporterPasswordHeld))
}

func TestExporterHoldConditionWaitsOutTheGrace(t *testing.T) {
	cutover := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			annotationSystemPasswordsRotationPhase: systemPasswordPhaseCutover,
			annotationSystemPasswordsCutoverAt:     cutover.Format(time.RFC3339Nano),
		}},
		Data: map[string][]byte{
			exporterUser:                            []byte("ex-new"),
			systemPasswordPreviousKey(exporterUser): []byte("ex-old"),
		},
	}
	held := func(now time.Time, pods ...string) *metav1.Condition {
		cluster := &valkeyiov1alpha1.ValkeyCluster{}
		r := &ValkeyClusterReconciler{}
		r.syncExporterPasswordHold(cluster, secret, systemPasswordSignals{Now: now, ExporterPodsOnOldPassword: pods})
		return meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionExporterPasswordHeld)
	}
	require.Nil(t, held(cutover.Add(time.Minute), "c1-0-0"), "within the grace every rotation holds the exporter")
	cond := held(cutover.Add(systemPasswordCutoverGrace), "c1-0-0", "c1-1-0")
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionTrue, cond.Status)
	require.True(t, strings.HasSuffix(cond.Message, "to finish its rotation: c1-0-0, c1-1-0"), cond.Message)
	require.Nil(t, held(cutover.Add(systemPasswordCutoverGrace)), "no exporter is left on the old password")

	running := func(name string, started time.Time) corev1.Pod {
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  metricsExporterContainerName,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(started)}},
		}}}}
	}
	pods := []corev1.Pod{
		running("c1-1-0", cutover.Add(-time.Hour)),
		running("c1-0-1", cutover.Add(systemPasswordCutoverGrace)),
		running("c1-0-0", cutover.Add(time.Minute)),
		{ObjectMeta: metav1.ObjectMeta{Name: "c1-2-0"}},
	}
	require.Equal(t, []string{"c1-0-0", "c1-1-0"}, exporterPodsOnOldPassword(pods, cutover))
}

func TestExporterHoldIsReportedWithRotationOffUntilThePodsRestart(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	enabled := true
	cluster := &valkeyiov1alpha1.ValkeyCluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}, Spec: valkeyiov1alpha1.ValkeyClusterSpec{
		Exporter: valkeyiov1alpha1.ExporterSpec{Enabled: &enabled},
	}}
	heldAt := time.Now().Add(-systemPasswordCutoverGrace - time.Minute)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: getSystemPasswordSecretName("c1"), Namespace: "ns",
		Annotations: map[string]string{annotationExporterPasswordHeldSince: heldAt.Format(time.RFC3339Nano)},
	}, Data: map[string][]byte{
		operatorUser:                            []byte("op-current"),
		replicationUser:                         []byte("re-current"),
		exporterUser:                            []byte("ex-current"),
		systemPasswordPreviousKey(exporterUser): []byte("ex-old"),
	}}
	exporterPod := func(name string, started time.Time) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{LabelCluster: "c1"}},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name:  metricsExporterContainerName,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(started)}},
			}}},
		}
	}
	early := exporterPod("c1-0-0", heldAt.Add(-time.Hour))
	restarted := exporterPod("c1-0-1", heldAt.Add(systemPasswordCutoverGrace+time.Second))
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1.Pod{}).
		WithObjects(cluster, secret, early, restarted).Build()
	r := &ValkeyClusterReconciler{Client: c, Scheme: scheme, APIReader: c}

	_, err := r.upsertSystemUsersPasswordSecret(context.Background(), cluster)
	require.NoError(t, err)
	cond := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionExporterPasswordHeld)
	require.NotNil(t, cond, "the previous exporter password is still accepted with rotation off")
	require.True(t, strings.HasSuffix(cond.Message, ": c1-0-0"), cond.Message)

	pod := &corev1.Pod{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(early), pod))
	pod.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(time.Now())
	require.NoError(t, c.Status().Update(context.Background(), pod))
	_, err = r.upsertSystemUsersPasswordSecret(context.Background(), cluster)
	require.NoError(t, err)
	require.Nil(t, meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionExporterPasswordHeld))
	stored := &corev1.Secret{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), stored))
	require.Empty(t, stored.Data[systemPasswordPreviousKey(exporterUser)])
}

func TestCreateSystemUsersAclRotatesAnExistingSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	enabled := false
	cluster := &valkeyiov1alpha1.ValkeyCluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}, Spec: valkeyiov1alpha1.ValkeyClusterSpec{
		Exporter: valkeyiov1alpha1.ExporterSpec{Enabled: &enabled},
		ACL: &valkeyiov1alpha1.ACLSpec{SystemUsers: &valkeyiov1alpha1.SystemUsersSpec{
			PasswordRotation: &valkeyiov1alpha1.SystemPasswordRotationSpec{IntervalDays: 30},
		}},
	}}
	current := []byte("op-current")
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: getSystemPasswordSecretName("c1"), Namespace: "ns",
		Annotations: map[string]string{
			annotationSystemPasswordsCurrentSince: time.Now().Add(-testRotationInterval - time.Second).Format(time.RFC3339Nano),
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
	require.Equal(t, systemPasswordPhasePending, got.Annotations[annotationSystemPasswordsRotationPhase])
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
		acl := buildUserAcl(valkeyiov1alpha1.ACLUserSpec{
			Name:           user,
			ACLPermissions: valkeyiov1alpha1.ACLPermissions{RawAcl: systemUsersAcls[user]},
		}, systemUserPasswordHashes(data, user), false)
		fmt.Fprintf(&body, "%s\n", acl)
	}
	managed := body.String()
	revision := systemPasswordDigest([]byte(managed))
	fmt.Fprintf(&body, "user %s off resetchannels -@all #%s\n", aclRevisionUser, revision)
	return body.String(), revision
}
