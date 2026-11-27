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
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

const (
	// systemPasswordCutoverGrace is how long the previous password stays
	// accepted after the new one becomes the value clients read. Probes read
	// the mounted file on every check, and kubelet refreshes a Secret volume
	// about once a minute, so the grace has to cover that refresh plus
	// connections already open with the old password.
	systemPasswordCutoverGrace = 5 * time.Minute

	systemPasswordNextSuffix     = ".next"
	systemPasswordPreviousSuffix = ".previous"

	// Certificate login for _operator, _exporter, and _replication is the
	// longer-term replacement for this rotation. Each client needs its own
	// certificate (valkey-operator#360) so Valkey can map the certificate
	// name to that one ACL user, and the Valkey process has to reload a
	// renewed leaf without a restart (tls-auto-reload-interval). Until that
	// exists, overlapping passwords are what retires a password that would
	// otherwise never change.

	// annotationSystemPasswordsCurrentSince is when the current passwords
	// became current, or when the rotation clock started.
	annotationSystemPasswordsCurrentSince  = "valkey.io/system-passwords-current-since"
	annotationSystemPasswordsRotationPhase = "valkey.io/system-passwords-rotation-phase"
	annotationSystemPasswordsCutoverAt     = "valkey.io/system-passwords-cutover-at"
	// annotationExporterPasswordHeldSince is when _exporter.previous was left
	// behind. The exporter is not restarted by a rotation. That key stays
	// until the container starts again, and it does not keep the other users
	// in cutover.
	annotationExporterPasswordHeldSince = "valkey.io/exporter-password-held-since"

	systemPasswordPhasePending = "pending"
	systemPasswordPhaseCutover = "cutover"

	metricsExporterContainerName = "metrics-exporter"
)

// replicationAuthShell is the script the server container's shell runs.
// Kubelet expands $(VAR) in the pod command, which would freeze PRIMARY_AUTH
// at container start, so this string must not contain "$(". The shell reads
// the mounted file when it is there and otherwise uses the environment value.
// With handoff it starts the server through primary-handoff.sh, which execs
// the server itself. $0 is the server binary and $@ its arguments.
func replicationAuthShell(handoff bool) string {
	start := `exec "$0" "$@" --primaryauth "$PRIMARY_AUTH"`
	if handoff {
		start = `exec /bin/sh /scripts/` + primaryHandoffScriptKey + ` "$@" --primaryauth "$PRIMARY_AUTH"`
	}
	return fmt.Sprintf(`pw="%s/%s"
if [ -f "$pw" ]; then IFS= read -r PRIMARY_AUTH < "$pw" || true; fi
if [ -z "${PRIMARY_AUTH:-}" ]; then echo "replication password is empty" >&2; exit 1; fi
%s`, systemPasswordMountPath, replicationUser, start)
}

func systemPasswordNextKey(user string) string {
	return user + systemPasswordNextSuffix
}

func systemPasswordPreviousKey(user string) string {
	return user + systemPasswordPreviousSuffix
}

func isSystemPasswordKey(key string) bool {
	for _, user := range systemUsers {
		if key == user || key == systemPasswordNextKey(user) || key == systemPasswordPreviousKey(user) {
			return true
		}
	}
	return false
}

func systemPasswordUsers(exporterEnabled bool) []string {
	users := []string{operatorUser, replicationUser}
	if exporterEnabled {
		users = append(users, exporterUser)
	}
	return users
}

func systemPasswordDigest(password []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(password))
}

// systemUserPasswordHashes returns the sha256 hex of every password the ACL
// must accept for user: the current, next and previous keys, skipping a
// missing one. The server accepts a login that matches any of them. They are
// sorted, so a cutover that only moves passwords between keys renders the same
// aclfile and does not cause an ACL LOAD.
func systemUserPasswordHashes(data map[string][]byte, user string) []string {
	var hashes []string
	for _, key := range []string{user, systemPasswordNextKey(user), systemPasswordPreviousKey(user)} {
		password, ok := data[key]
		if !ok || len(password) == 0 {
			continue
		}
		hashes = append(hashes, systemPasswordDigest(password))
	}
	return normalizeHashes(hashes)
}

// systemPasswordACLReady reports whether every node's running ACL is the
// aclfile that contains exactly the passwords currently stored for users.
// revisions are the nodes' status.appliedACLRevision values. An empty node
// list is not ready: there is no observation that the new hash was loaded.
func systemPasswordACLReady(aclFile string, data map[string][]byte, users []string, revisions []string) bool {
	if aclFile == "" || len(revisions) == 0 {
		return false
	}
	desired := desiredUserPasswordHashes(aclFile)
	for _, user := range users {
		want := systemUserPasswordHashes(data, user)
		if len(want) == 0 || !equalHashes(desired[user], want) {
			return false
		}
	}
	revision := aclRevision(desired)
	if revision == "" {
		return false
	}
	for _, got := range revisions {
		if got != revision {
			return false
		}
	}
	return true
}

func equalHashes(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// systemPasswordSignals is what advanceSystemPasswordRotation needs from the
// cluster. SkipNext users were given their current password in this call, so
// they must not be rotated in the same call.
type systemPasswordSignals struct {
	Now time.Time
	// Interval is how long a password stays current before the next one is
	// generated. Zero starts no rotation; one already under way still finishes.
	Interval            time.Duration
	PasswordsLive       bool
	OperatorCaughtUp    bool
	ExporterCaughtUp    bool
	ReplicationCaughtUp bool
	SkipNext            map[string]bool
	// ExporterPodsOnOldPassword are the pods whose exporter started too early
	// to read the current password, while _exporter.previous is held.
	ExporterPodsOnOldPassword []string
}

// advanceSystemPasswordRotation moves the system-password Secret one step.
// It never replaces the only password a running server accepts.
//
// pending: <user>.next holds a new password and <user> is unchanged, so
// clients keep sending the old one. The ACL lists both hashes. Promotion
// waits until every node has loaded that ACL.
//
// cutover: <user> is the new password and <user>.previous is the old one.
// The old hash is removed only after the clients that captured the old
// password have switched. _exporter.previous can outlive that phase. The
// exporter process is not restarted, and the other users rotate again
// while its old password stays accepted.
func advanceSystemPasswordRotation(secret *corev1.Secret, exporterEnabled bool, signals systemPasswordSignals) error {
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	users := systemPasswordUsers(exporterEnabled)
	switch secret.Annotations[annotationSystemPasswordsRotationPhase] {
	case "":
		if err := startSystemPasswordRotation(secret, users, signals); err != nil {
			return err
		}
	case systemPasswordPhasePending:
		promoteSystemPasswordRotation(secret, users, exporterEnabled, signals)
	case systemPasswordPhaseCutover:
		finishSystemPasswordRotation(secret, exporterEnabled, signals)
	default:
		return fmt.Errorf("unknown system password rotation phase %q", secret.Annotations[annotationSystemPasswordsRotationPhase])
	}
	releaseHeldExporter(secret, signals)
	return nil
}

func startSystemPasswordRotation(secret *corev1.Secret, users []string, signals systemPasswordSignals) error {
	if signals.Interval <= 0 {
		// Drop the clock so turning rotation on again waits a full interval.
		delete(secret.Annotations, annotationSystemPasswordsCurrentSince)
		return nil
	}
	// A Secret that has not rotated before has no clock. Stamp it and wait
	// out the interval, so turning rotation on does not rotate at once, and an
	// upgrade does not rotate during the one-time pod roll that installs the
	// volume and the new command.
	if secret.Annotations[annotationSystemPasswordsCurrentSince] == "" && !systemPasswordHasSuffix(secret.Data, systemPasswordNextSuffix) {
		secret.Annotations[annotationSystemPasswordsCurrentSince] = signals.Now.Format(time.RFC3339Nano)
		return nil
	}
	if rotatedAt, ok := secret.Annotations[annotationSystemPasswordsCurrentSince]; ok && rotatedAt != "" {
		t, err := time.Parse(time.RFC3339Nano, rotatedAt)
		if err == nil && signals.Now.Sub(t) < signals.Interval {
			return nil
		}
	}
	for _, user := range users {
		// The exporter's previous password is still the one its running
		// process captured. Promoting over it would erase that hash.
		if user == exporterUser && len(secret.Data[systemPasswordPreviousKey(exporterUser)]) > 0 {
			continue
		}
		if signals.SkipNext[user] || len(secret.Data[systemPasswordNextKey(user)]) > 0 {
			continue
		}
		if len(secret.Data[user]) == 0 {
			continue
		}
		password, err := generatePassword(passwordLength)
		if err != nil {
			return err
		}
		secret.Data[systemPasswordNextKey(user)] = password
	}
	if !systemPasswordHasSuffix(secret.Data, systemPasswordNextSuffix) {
		if secret.Annotations[annotationSystemPasswordsCurrentSince] == "" {
			secret.Annotations[annotationSystemPasswordsCurrentSince] = signals.Now.Format(time.RFC3339Nano)
		}
		return nil
	}
	secret.Annotations[annotationSystemPasswordsRotationPhase] = systemPasswordPhasePending
	return nil
}

func promoteSystemPasswordRotation(secret *corev1.Secret, users []string, exporterEnabled bool, signals systemPasswordSignals) {
	if !signals.PasswordsLive {
		return
	}
	for _, user := range users {
		next := secret.Data[systemPasswordNextKey(user)]
		if len(next) == 0 {
			continue
		}
		if current := secret.Data[user]; len(current) > 0 {
			secret.Data[systemPasswordPreviousKey(user)] = current
		}
		secret.Data[user] = append([]byte(nil), next...)
		delete(secret.Data, systemPasswordNextKey(user))
	}
	if !exporterEnabled {
		delete(secret.Data, systemPasswordNextKey(exporterUser))
	}
	secret.Annotations[annotationSystemPasswordsRotationPhase] = systemPasswordPhaseCutover
	stamp := signals.Now.Format(time.RFC3339Nano)
	secret.Annotations[annotationSystemPasswordsCutoverAt] = stamp
	secret.Annotations[annotationSystemPasswordsCurrentSince] = stamp
}

func finishSystemPasswordRotation(secret *corev1.Secret, exporterEnabled bool, signals systemPasswordSignals) {
	cutoverAt, err := time.Parse(time.RFC3339Nano, secret.Annotations[annotationSystemPasswordsCutoverAt])
	if err != nil || signals.Now.Sub(cutoverAt) < systemPasswordCutoverGrace {
		return
	}
	drop := func(user string, caughtUp bool) {
		key := systemPasswordPreviousKey(user)
		if len(secret.Data[key]) == 0 || !caughtUp {
			return
		}
		delete(secret.Data, key)
	}
	drop(operatorUser, signals.OperatorCaughtUp)
	drop(replicationUser, signals.ReplicationCaughtUp)
	drop(exporterUser, !exporterEnabled || signals.ExporterCaughtUp)
	if exporterPasswordHeld(secret) && secret.Annotations[annotationExporterPasswordHeldSince] == "" {
		secret.Annotations[annotationExporterPasswordHeldSince] = secret.Annotations[annotationSystemPasswordsCutoverAt]
	}
	// _operator and _replication have switched. Leave the phase so their next
	// rotation is not stuck behind an exporter that has not restarted.
	if !systemPasswordHeldExceptExporter(secret) && exporterPasswordHeld(secret) {
		delete(secret.Annotations, annotationSystemPasswordsRotationPhase)
		delete(secret.Annotations, annotationSystemPasswordsCutoverAt)
		return
	}
	if systemPasswordHasSuffix(secret.Data, systemPasswordPreviousSuffix) || systemPasswordHasSuffix(secret.Data, systemPasswordNextSuffix) {
		return
	}
	delete(secret.Annotations, annotationSystemPasswordsRotationPhase)
	delete(secret.Annotations, annotationSystemPasswordsCutoverAt)
	delete(secret.Annotations, annotationExporterPasswordHeldSince)
}

func exporterPasswordHeld(secret *corev1.Secret) bool {
	return len(secret.Data[systemPasswordPreviousKey(exporterUser)]) > 0
}

func systemPasswordHeldExceptExporter(secret *corev1.Secret) bool {
	if len(secret.Data[systemPasswordPreviousKey(operatorUser)]) > 0 || len(secret.Data[systemPasswordPreviousKey(replicationUser)]) > 0 {
		return true
	}
	for _, user := range []string{operatorUser, replicationUser} {
		if len(secret.Data[systemPasswordNextKey(user)]) > 0 {
			return true
		}
	}
	return false
}

// releaseHeldExporter drops _exporter.previous once that container has started
// after the hold. It runs in every phase, so a later rotation of the other
// users does not have to finish first.
func releaseHeldExporter(secret *corev1.Secret, signals systemPasswordSignals) {
	if !exporterPasswordHeld(secret) {
		delete(secret.Annotations, annotationExporterPasswordHeldSince)
		return
	}
	if !exporterHeldPastGrace(secret, signals.Now) || !signals.ExporterCaughtUp {
		return
	}
	delete(secret.Data, systemPasswordPreviousKey(exporterUser))
	delete(secret.Annotations, annotationExporterPasswordHeldSince)
}

// exporterHeldSince is when _exporter.previous was left behind: the hold
// annotation, or the cutover while the rotation is still in that phase.
func exporterHeldSince(secret *corev1.Secret) (time.Time, bool) {
	since := secret.Annotations[annotationExporterPasswordHeldSince]
	if since == "" {
		since = secret.Annotations[annotationSystemPasswordsCutoverAt]
	}
	heldAt, err := time.Parse(time.RFC3339Nano, since)
	return heldAt, err == nil
}

// exporterHeldPastGrace reports whether _exporter.previous is still accepted
// more than systemPasswordCutoverGrace after it was left behind. Within the
// grace, holding it is the normal end of a rotation.
func exporterHeldPastGrace(secret *corev1.Secret, now time.Time) bool {
	if !exporterPasswordHeld(secret) {
		return false
	}
	heldAt, ok := exporterHeldSince(secret)
	return ok && now.Sub(heldAt) >= systemPasswordCutoverGrace
}

func systemPasswordHasSuffix(data map[string][]byte, suffix string) bool {
	for key, value := range data {
		if strings.HasSuffix(key, suffix) && len(value) > 0 && isSystemPasswordKey(key) {
			return true
		}
	}
	return false
}

// exporterPasswordCaughtUp reports whether every running exporter container
// started at least systemPasswordCutoverGrace after cutover.
func exporterPasswordCaughtUp(pods []corev1.Pod, cutover time.Time) bool {
	return len(exporterPodsOnOldPassword(pods, cutover)) == 0
}

// exporterPodsOnOldPassword returns, sorted, the pods whose running exporter
// container started less than systemPasswordCutoverGrace after cutover.
// redis_exporter reads REDIS_PASSWORD once, at process start, and its image
// has no shell, so a container that is already running keeps the old password
// until it starts again. The kubelet fills that variable from its Secret
// cache, so a start shortly after cutover can still get the old password. A
// container that is not running reads the current key when it starts.
func exporterPodsOnOldPassword(pods []corev1.Pod, cutover time.Time) []string {
	readsCurrent := cutover.Add(systemPasswordCutoverGrace)
	var names []string
	for i := range pods {
		for _, status := range pods[i].Status.ContainerStatuses {
			if status.Name == metricsExporterContainerName && status.State.Running != nil &&
				status.State.Running.StartedAt.Time.Before(readsCurrent) {
				names = append(names, pods[i].Name)
			}
		}
	}
	slices.Sort(names)
	return names
}

// podsMountSystemPasswords reports whether every server container has the
// system-password volume. Probes on a pod without that mount still send the
// password from the environment captured at container start.
func podsMountSystemPasswords(nodes []valkeyiov1alpha1.ValkeyNode, pods []corev1.Pod) bool {
	if len(nodes) == 0 {
		return true
	}
	if len(pods) == 0 {
		return false
	}
	sawServer := false
	for i := range pods {
		for _, container := range pods[i].Spec.Containers {
			if container.Name != serverContainerName {
				continue
			}
			sawServer = true
			if !containerMounts(container, systemPasswordMountPath) {
				return false
			}
		}
	}
	return sawServer
}

func containerMounts(container corev1.Container, path string) bool {
	for _, mount := range container.VolumeMounts {
		if mount.MountPath == path {
			return true
		}
	}
	return false
}

// replicationPasswordCaughtUp reports whether every node has CONFIG SET the
// current replication password, that process is still the one that received
// it, and every server container will read the mounted file the next time it
// starts. A container that started after the CONFIG SET may have read a stale
// volume, and dropping the previous password would leave no way to repair it.
func replicationPasswordCaughtUp(nodes []valkeyiov1alpha1.ValkeyNode, pods []corev1.Pod, current []byte) bool {
	if len(current) == 0 {
		return false
	}
	if len(nodes) == 0 {
		return true
	}
	byName := make(map[string]*corev1.Pod, len(pods))
	for i := range pods {
		byName[pods[i].Name] = &pods[i]
	}
	want := systemPasswordDigest(current)
	for i := range nodes {
		if nodes[i].Status.AppliedReplicationPasswordHash != want || !primaryAuthSurvivedRestart(&nodes[i], byName) {
			return false
		}
	}
	sawServer := false
	for i := range pods {
		for _, container := range pods[i].Spec.Containers {
			if container.Name != serverContainerName {
				continue
			}
			sawServer = true
			if !strings.Contains(strings.Join(container.Command, "\n"), systemPasswordMountPath) {
				return false
			}
		}
	}
	return sawServer
}

// primaryAuthSurvivedRestart reports whether the running server started in
// an earlier second than the recorded CONFIG SET. StartedAt is whole
// seconds, so the same second counts as a restart.
func primaryAuthSurvivedRestart(node *valkeyiov1alpha1.ValkeyNode, pods map[string]*corev1.Pod) bool {
	if node.Status.ReplicationPasswordAppliedTime == nil {
		return false
	}
	recorded := node.Status.ReplicationPasswordAppliedTime.Time
	pod := pods[node.Status.PodName]
	if pod == nil {
		return false
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != serverContainerName || status.State.Running == nil {
			continue
		}
		return serverStartedBeforeConfigSet(status.State.Running.StartedAt.Time, recorded)
	}
	return false
}

// serverStartedBeforeConfigSet reports whether the container started in an
// earlier second than the CONFIG SET. The API stores StartedAt at whole
// seconds, so a restart later in the same second counts as not before.
func serverStartedBeforeConfigSet(started, recorded time.Time) bool {
	return started.Before(recorded.Truncate(time.Second))
}

func (r *ValkeyClusterReconciler) uncachedReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *ValkeyClusterReconciler) systemPasswordSignals(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, secret *corev1.Secret) (systemPasswordSignals, error) {
	signals := systemPasswordSignals{Now: time.Now(), Interval: cluster.Spec.SystemPasswordRotationInterval()}
	phase := secret.Annotations[annotationSystemPasswordsRotationPhase]
	holdSince := secret.Annotations[annotationExporterPasswordHeldSince]
	if holdSince == "" && exporterPasswordHeld(secret) {
		holdSince = secret.Annotations[annotationSystemPasswordsCutoverAt]
	}
	var nodes []valkeyiov1alpha1.ValkeyNode
	var pods []corev1.Pod
	fetched := false
	fetch := func() error {
		if fetched {
			return nil
		}
		var err error
		nodes, pods, err = r.systemPasswordWorkloads(ctx, cluster)
		fetched = err == nil
		return err
	}
	if holdSince != "" {
		if heldAt, err := time.Parse(time.RFC3339Nano, holdSince); err == nil {
			if err := fetch(); err != nil {
				return signals, err
			}
			signals.ExporterPodsOnOldPassword = exporterPodsOnOldPassword(pods, heldAt)
			signals.ExporterCaughtUp = len(signals.ExporterPodsOnOldPassword) == 0
		}
	}
	if phase == "" {
		return signals, nil
	}
	users := systemPasswordUsers(cluster.Spec.ExporterEnabled())
	if phase == systemPasswordPhasePending {
		live, err := r.systemPasswordsLive(ctx, cluster, secret.Data, users)
		if err != nil {
			return signals, err
		}
		signals.PasswordsLive = live
		return signals, nil
	}
	if err := fetch(); err != nil {
		return signals, err
	}
	cutoverAt, err := time.Parse(time.RFC3339Nano, secret.Annotations[annotationSystemPasswordsCutoverAt])
	if err != nil {
		return signals, nil
	}
	signals.OperatorCaughtUp = podsMountSystemPasswords(nodes, pods)
	if holdSince == "" {
		signals.ExporterCaughtUp = exporterPasswordCaughtUp(pods, cutoverAt)
	}
	signals.ReplicationCaughtUp = replicationPasswordCaughtUp(nodes, pods, secret.Data[replicationUser])
	return signals, nil
}

func (r *ValkeyClusterReconciler) systemPasswordsLive(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, data map[string][]byte, users []string) (bool, error) {
	aclSecret := &corev1.Secret{}
	err := r.uncachedReader().Get(ctx, types.NamespacedName{
		Name:      getInternalSecretName(cluster.Name),
		Namespace: cluster.Namespace,
	}, aclSecret)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	nodes := &valkeyiov1alpha1.ValkeyNodeList{}
	if err := r.uncachedReader().List(ctx, nodes, client.InNamespace(cluster.Namespace), client.MatchingLabels(map[string]string{LabelCluster: cluster.Name})); err != nil {
		return false, err
	}
	revisions := make([]string, 0, len(nodes.Items))
	for i := range nodes.Items {
		revisions = append(revisions, nodes.Items[i].Status.AppliedACLRevision)
	}
	return systemPasswordACLReady(string(aclSecret.Data[aclFilename]), data, users, revisions), nil
}

func (r *ValkeyClusterReconciler) systemPasswordWorkloads(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) ([]valkeyiov1alpha1.ValkeyNode, []corev1.Pod, error) {
	nodes := &valkeyiov1alpha1.ValkeyNodeList{}
	if err := r.uncachedReader().List(ctx, nodes, client.InNamespace(cluster.Namespace), client.MatchingLabels(map[string]string{LabelCluster: cluster.Name})); err != nil {
		return nil, nil, err
	}
	pods := &corev1.PodList{}
	if err := r.uncachedReader().List(ctx, pods, client.InNamespace(cluster.Namespace), client.MatchingLabels(map[string]string{LabelCluster: cluster.Name})); err != nil {
		return nil, nil, err
	}
	return nodes.Items, pods.Items, nil
}

// applyReplicationPassword points a running server at the current replication
// password while the previous one is still accepted. It does nothing when
// there is no previous password, so a steady cluster does not CONFIG SET on
// every reconcile. The primary already accepts the current password because
// this runs only after the ACL is live.
func (r *ValkeyNodeReconciler) applyReplicationPassword(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) error {
	clusterName := node.Labels[LabelCluster]
	if clusterName == "" || r.newConfigClient == nil {
		return nil
	}
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{Name: getSystemPasswordSecretName(clusterName), Namespace: node.Namespace}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get system password secret: %w", err)
	}
	if len(secret.Data[systemPasswordPreviousKey(replicationUser)]) == 0 || len(secret.Data[replicationUser]) == 0 {
		return nil
	}
	sum := systemPasswordDigest(secret.Data[replicationUser])
	if node.Status.AppliedReplicationPasswordHash == sum &&
		node.Status.ReplicationPasswordAppliedTime != nil &&
		!r.serverRestartedSincePrimaryAuth(ctx, node) {
		return nil
	}
	configClient, err := r.newConfigClient(ctx, r, node)
	if err != nil {
		return err
	}
	// Stamp the time before CONFIG SET. A restart between the SET and a
	// later stamp would look older than the stamp and the old password
	// could be dropped while that process still has the stale file. The
	// status keeps whole seconds, which is all a container start time has.
	appliedAt := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
	if err := configClient.SetConfig(ctx, map[string]string{"primaryauth": string(secret.Data[replicationUser])}); err != nil {
		return err
	}
	if _, err := r.patchStatus(ctx, node, r.Client, func(current *valkeyiov1alpha1.ValkeyNode) bool {
		if current.Status.AppliedReplicationPasswordHash == sum && appliedAt.Equal(current.Status.ReplicationPasswordAppliedTime) {
			return false
		}
		current.Status.AppliedReplicationPasswordHash = sum
		current.Status.ReplicationPasswordAppliedTime = &appliedAt
		return true
	}); err != nil {
		return fmt.Errorf("record replication password: %w", err)
	}
	return nil
}

// serverRestartedSincePrimaryAuth reports whether the server container started
// after the last CONFIG SET. That process may have read the mounted
// replication password before kubelet refreshed the Secret volume.
func (r *ValkeyNodeReconciler) serverRestartedSincePrimaryAuth(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) bool {
	if node.Status.ReplicationPasswordAppliedTime == nil || node.Status.PodName == "" {
		return false
	}
	recorded := node.Status.ReplicationPasswordAppliedTime.Time
	pod := &corev1.Pod{}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.Get(ctx, types.NamespacedName{Name: node.Status.PodName, Namespace: node.Namespace}, pod); err != nil {
		return false
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != serverContainerName || status.State.Running == nil {
			continue
		}
		return !serverStartedBeforeConfigSet(status.State.Running.StartedAt.Time, recorded)
	}
	return false
}

// syncExporterPasswordHold keeps ExporterPasswordHeld set while
// _exporter.previous is held past the grace, naming the pods whose exporter
// still started too early to read the current password. One event is not
// enough: the hold lasts until those pods restart, with rotation on or off.
func (r *ValkeyClusterReconciler) syncExporterPasswordHold(cluster *valkeyiov1alpha1.ValkeyCluster, secret *corev1.Secret, signals systemPasswordSignals) {
	if secret != nil && exporterHeldPastGrace(secret, signals.Now) && len(signals.ExporterPodsOnOldPassword) > 0 {
		setCondition(cluster, valkeyiov1alpha1.ConditionExporterPasswordHeld, valkeyiov1alpha1.ReasonExporterNotRestarted,
			"The metrics-exporter has not restarted since the system passwords changed, so its previous password is still accepted. "+
				"Restart these Valkey pods one at a time to finish its rotation: "+strings.Join(signals.ExporterPodsOnOldPassword, ", "),
			metav1.ConditionTrue)
		return
	}
	meta.RemoveStatusCondition(&cluster.Status.Conditions, valkeyiov1alpha1.ConditionExporterPasswordHeld)
}

func (r *ValkeyClusterReconciler) recordSystemPasswordRotation(cluster *valkeyiov1alpha1.ValkeyCluster, before, after *corev1.Secret) {
	reason, message := systemPasswordRotationEvent(before, after)
	if r.Recorder == nil || reason == "" {
		return
	}
	r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, reason, "ReconcileUsers", "%s", message)
}

// systemPasswordRotationEvent names the rotation step that turned before into
// after, read from the phase and the previous passwords left in the Secret. A
// step that only moves the clock or releases the exporter's old password has
// no event.
func systemPasswordRotationEvent(before, after *corev1.Secret) (reason, message string) {
	was := before.Annotations[annotationSystemPasswordsRotationPhase]
	now := after.Annotations[annotationSystemPasswordsRotationPhase]
	switch {
	case was == "" && now == systemPasswordPhasePending:
		return "SystemPasswordRotationStarted",
			"Generated the next system passwords. The passwords already in use stay valid until every node has loaded both."
	case was == systemPasswordPhasePending && now == systemPasswordPhaseCutover:
		return "SystemPasswordRotationCutover",
			"Nodes have loaded both system passwords. Clients are moving to the new ones. The old passwords stay valid until they have switched."
	case was == systemPasswordPhaseCutover && now == "" && !exporterPasswordHeld(after):
		return "SystemPasswordRotationFinished",
			"Previous system passwords have been removed. Clients are using the current ones."
	case was == systemPasswordPhaseCutover && (now == "" || previousPasswordCount(after) < previousPasswordCount(before)):
		return "SystemPasswordRotationHolding",
			"A client is still using a previous system password. That password stays accepted until the client has switched."
	}
	return "", ""
}

// previousPasswordCount returns how many <user>.previous passwords the Secret
// still holds.
func previousPasswordCount(secret *corev1.Secret) int {
	n := 0
	for key, value := range secret.Data {
		if strings.HasSuffix(key, systemPasswordPreviousSuffix) && len(value) > 0 && isSystemPasswordKey(key) {
			n++
		}
	}
	return n
}
