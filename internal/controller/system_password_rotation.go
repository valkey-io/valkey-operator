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
	// systemPasswordRotationPeriod is how long a system password stays current
	// before the operator generates the next one. The password already in use
	// is not removed in that step.
	systemPasswordRotationPeriod = 30 * 24 * time.Hour
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

	annotationSystemPasswordRotatedAt = "valkey.operator/system-password-rotated-at"
	annotationSystemPasswordPhase     = "valkey.operator/system-password-rotation"
	annotationSystemPasswordCutoverAt = "valkey.operator/system-password-cutover-at"
	// annotationExporterHoldSince is when _exporter.previous was left behind.
	// The exporter is not restarted by a rotation. That key stays until the
	// container starts again, and it does not keep the other users in cutover.
	annotationExporterHoldSince = "valkey.operator/system-password-exporter-hold"
	// annotationPrimaryAuthHash is the sha256 of the replication password
	// last applied with CONFIG SET primaryauth. The cluster drops the
	// previous replication password only after every node records the hash
	// of the current one.
	annotationPrimaryAuthHash = "valkey.operator/primaryauth-sha256"
	// annotationPrimaryAuthSetAt is when that CONFIG SET was recorded. A
	// server container that starts later may have read a Secret volume that
	// kubelet had not refreshed yet, so the hash alone is not enough.
	annotationPrimaryAuthSetAt = "valkey.operator/primaryauth-set-at"

	systemPasswordPhasePending = "pending"
	systemPasswordPhaseCutover = "cutover"
	systemPasswordEventHolding = "holding"

	metricsExporterContainerName = "metrics-exporter"
)

// replicationAuthShell is the container command that starts valkey-server.
// Kubelet expands $(VAR) in the pod command, which would freeze PRIMARY_AUTH
// at container start, so this string must not contain "$(". The shell reads
// the mounted file when it is there and otherwise uses the environment value.
func replicationAuthShell() string {
	return fmt.Sprintf(`pw="%s/%s"
if [ -f "$pw" ]; then IFS= read -r PRIMARY_AUTH < "$pw" || true; fi
if [ -z "${PRIMARY_AUTH:-}" ]; then echo "replication password is empty" >&2; exit 1; fi
exec "$0" "$@" --primaryauth "$PRIMARY_AUTH"`, systemPasswordMountPath, replicationUser)
}

func valkeyServerCommand(announceArgs []string) []string {
	cmd := make([]string, 0, 5+len(announceArgs)+2)
	cmd = append(cmd, shellPath, "-c", replicationAuthShell(), "valkey-server", "/config/valkey.conf")
	cmd = append(cmd, announceArgs...)
	cmd = append(cmd, "--primaryuser", replicationUser)
	return cmd
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
// must accept for user, in stable order: the current password, then the next
// one, then the previous one. A missing key is skipped. The server accepts a
// login that matches any of them.
func systemUserPasswordHashes(data map[string][]byte, user string) []string {
	var hashes []string
	for _, key := range []string{user, systemPasswordNextKey(user), systemPasswordPreviousKey(user)} {
		password, ok := data[key]
		if !ok || len(password) == 0 {
			continue
		}
		hashes = append(hashes, systemPasswordDigest(password))
	}
	return hashes
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
		want := normalizeHashes(systemUserPasswordHashes(data, user))
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
	Now                 time.Time
	PasswordsLive       bool
	OperatorCaughtUp    bool
	ExporterCaughtUp    bool
	ReplicationCaughtUp bool
	SkipNext            map[string]bool
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
func advanceSystemPasswordRotation(secret *corev1.Secret, exporterEnabled bool, signals systemPasswordSignals) (string, error) {
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	users := systemPasswordUsers(exporterEnabled)
	var event string
	var err error
	switch secret.Annotations[annotationSystemPasswordPhase] {
	case "":
		event, err = startSystemPasswordRotation(secret, users, signals)
	case systemPasswordPhasePending:
		event = promoteSystemPasswordRotation(secret, users, exporterEnabled, signals)
	case systemPasswordPhaseCutover:
		event = finishSystemPasswordRotation(secret, exporterEnabled, signals)
	default:
		return "", fmt.Errorf("unknown system password rotation phase %q", secret.Annotations[annotationSystemPasswordPhase])
	}
	if err != nil {
		return "", err
	}
	if released := releaseHeldExporter(secret, signals); event == "" {
		event = released
	}
	return event, nil
}

func startSystemPasswordRotation(secret *corev1.Secret, users []string, signals systemPasswordSignals) (string, error) {
	// An existing Secret from before this feature has no clock. Stamp it and
	// wait out the period. Rotating on the upgrade reconcile would overlap
	// the one-time pod roll that installs the volume and the new command.
	if secret.Annotations[annotationSystemPasswordRotatedAt] == "" && !systemPasswordHasSuffix(secret.Data, systemPasswordNextSuffix) {
		secret.Annotations[annotationSystemPasswordRotatedAt] = signals.Now.Format(time.RFC3339Nano)
		return "clock", nil
	}
	if rotatedAt, ok := secret.Annotations[annotationSystemPasswordRotatedAt]; ok && rotatedAt != "" {
		t, err := time.Parse(time.RFC3339Nano, rotatedAt)
		if err == nil && signals.Now.Sub(t) < systemPasswordRotationPeriod {
			return "", nil
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
			return "", err
		}
		secret.Data[systemPasswordNextKey(user)] = password
	}
	if !systemPasswordHasSuffix(secret.Data, systemPasswordNextSuffix) {
		if secret.Annotations[annotationSystemPasswordRotatedAt] == "" {
			secret.Annotations[annotationSystemPasswordRotatedAt] = signals.Now.Format(time.RFC3339Nano)
			return "clock", nil
		}
		return "", nil
	}
	secret.Annotations[annotationSystemPasswordPhase] = systemPasswordPhasePending
	return "started", nil
}

func promoteSystemPasswordRotation(secret *corev1.Secret, users []string, exporterEnabled bool, signals systemPasswordSignals) string {
	if !signals.PasswordsLive {
		return ""
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
	secret.Annotations[annotationSystemPasswordPhase] = systemPasswordPhaseCutover
	stamp := signals.Now.Format(time.RFC3339Nano)
	secret.Annotations[annotationSystemPasswordCutoverAt] = stamp
	secret.Annotations[annotationSystemPasswordRotatedAt] = stamp
	return "cutover"
}

func finishSystemPasswordRotation(secret *corev1.Secret, exporterEnabled bool, signals systemPasswordSignals) string {
	cutoverAt, err := time.Parse(time.RFC3339Nano, secret.Annotations[annotationSystemPasswordCutoverAt])
	if err != nil || signals.Now.Sub(cutoverAt) < systemPasswordCutoverGrace {
		return ""
	}
	deleted := false
	drop := func(user string, caughtUp bool) {
		key := systemPasswordPreviousKey(user)
		if len(secret.Data[key]) == 0 || !caughtUp {
			return
		}
		delete(secret.Data, key)
		deleted = true
	}
	drop(operatorUser, signals.OperatorCaughtUp)
	drop(replicationUser, signals.ReplicationCaughtUp)
	drop(exporterUser, !exporterEnabled || signals.ExporterCaughtUp)
	if exporterPasswordHeld(secret) && secret.Annotations[annotationExporterHoldSince] == "" {
		secret.Annotations[annotationExporterHoldSince] = secret.Annotations[annotationSystemPasswordCutoverAt]
	}
	// _operator and _replication have switched. Leave the phase so their next
	// rotation is not stuck behind an exporter that has not restarted.
	if !systemPasswordHeldExceptExporter(secret) && exporterPasswordHeld(secret) {
		delete(secret.Annotations, annotationSystemPasswordPhase)
		delete(secret.Annotations, annotationSystemPasswordCutoverAt)
		return systemPasswordEventHolding
	}
	if systemPasswordHasSuffix(secret.Data, systemPasswordPreviousSuffix) || systemPasswordHasSuffix(secret.Data, systemPasswordNextSuffix) {
		if deleted {
			return systemPasswordEventHolding
		}
		return ""
	}
	delete(secret.Annotations, annotationSystemPasswordPhase)
	delete(secret.Annotations, annotationSystemPasswordCutoverAt)
	delete(secret.Annotations, annotationExporterHoldSince)
	return "complete"
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
func releaseHeldExporter(secret *corev1.Secret, signals systemPasswordSignals) string {
	if !exporterPasswordHeld(secret) {
		delete(secret.Annotations, annotationExporterHoldSince)
		return ""
	}
	since := secret.Annotations[annotationExporterHoldSince]
	if since == "" {
		since = secret.Annotations[annotationSystemPasswordCutoverAt]
	}
	heldAt, err := time.Parse(time.RFC3339Nano, since)
	if err != nil || signals.Now.Sub(heldAt) < systemPasswordCutoverGrace || !signals.ExporterCaughtUp {
		return ""
	}
	delete(secret.Data, systemPasswordPreviousKey(exporterUser))
	delete(secret.Annotations, annotationExporterHoldSince)
	return "exporter-released"
}

func systemPasswordHasSuffix(data map[string][]byte, suffix string) bool {
	for key, value := range data {
		if strings.HasSuffix(key, suffix) && len(value) > 0 && isSystemPasswordKey(key) {
			return true
		}
	}
	return false
}

// exporterPasswordCaughtUp reports whether no running exporter container
// started before cutover. redis_exporter reads REDIS_PASSWORD once, at
// process start, and its image has no shell, so a container that is already
// running keeps the old password until it starts again. A container that is
// not running will read the current key when it starts.
func exporterPasswordCaughtUp(pods []corev1.Pod, cutover time.Time) bool {
	for i := range pods {
		for _, status := range pods[i].Status.ContainerStatuses {
			if status.Name != metricsExporterContainerName || status.State.Running == nil {
				continue
			}
			if status.State.Running.StartedAt.Time.Before(cutover) {
				return false
			}
		}
	}
	return true
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
		if nodes[i].Annotations[annotationPrimaryAuthHash] != want || !primaryAuthSurvivedRestart(&nodes[i], byName) {
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
	recorded, err := time.Parse(time.RFC3339Nano, node.Annotations[annotationPrimaryAuthSetAt])
	if err != nil {
		return false
	}
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
	signals := systemPasswordSignals{Now: time.Now()}
	phase := secret.Annotations[annotationSystemPasswordPhase]
	holdSince := secret.Annotations[annotationExporterHoldSince]
	if holdSince == "" && exporterPasswordHeld(secret) {
		holdSince = secret.Annotations[annotationSystemPasswordCutoverAt]
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
			signals.ExporterCaughtUp = exporterPasswordCaughtUp(pods, heldAt)
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
	cutoverAt, err := time.Parse(time.RFC3339Nano, secret.Annotations[annotationSystemPasswordCutoverAt])
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
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get system password secret: %w", err)
	}
	if len(secret.Data[systemPasswordPreviousKey(replicationUser)]) == 0 || len(secret.Data[replicationUser]) == 0 {
		return nil
	}
	sum := systemPasswordDigest(secret.Data[replicationUser])
	if node.Annotations[annotationPrimaryAuthHash] == sum &&
		node.Annotations[annotationPrimaryAuthSetAt] != "" &&
		!r.serverRestartedSincePrimaryAuth(ctx, node) {
		return nil
	}
	configClient, err := r.newConfigClient(ctx, r, node)
	if err != nil {
		return err
	}
	// Stamp the time before CONFIG SET. A restart between the SET and a
	// later stamp would look older than the stamp and the old password
	// could be dropped while that process still has the stale file.
	setAt := time.Now().UTC()
	if err := configClient.SetConfig(ctx, map[string]string{"primaryauth": string(secret.Data[replicationUser])}); err != nil {
		return err
	}
	current := &valkeyiov1alpha1.ValkeyNode{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(node), current); err != nil {
		return fmt.Errorf("get ValkeyNode: %w", err)
	}
	base := current.DeepCopy()
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[annotationPrimaryAuthHash] = sum
	current.Annotations[annotationPrimaryAuthSetAt] = setAt.Format(time.RFC3339Nano)
	if err := r.Patch(ctx, current, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("record replication password: %w", err)
	}
	return nil
}

// serverRestartedSincePrimaryAuth reports whether the server container started
// after the last CONFIG SET. That process may have read the mounted
// replication password before kubelet refreshed the Secret volume.
func (r *ValkeyNodeReconciler) serverRestartedSincePrimaryAuth(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) bool {
	setAt := node.Annotations[annotationPrimaryAuthSetAt]
	if setAt == "" || node.Status.PodName == "" {
		return false
	}
	recorded, err := time.Parse(time.RFC3339Nano, setAt)
	if err != nil {
		return false
	}
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

// syncExporterPasswordHold keeps ExporterPasswordHeld set for as long as
// _exporter.previous remains. One event is not enough: the hold can last
// until some other change restarts the exporter.
func (r *ValkeyClusterReconciler) syncExporterPasswordHold(cluster *valkeyiov1alpha1.ValkeyCluster, secret *corev1.Secret) {
	if secret != nil && exporterPasswordHeld(secret) {
		setCondition(cluster, valkeyiov1alpha1.ConditionExporterPasswordHeld, valkeyiov1alpha1.ReasonExporterNotRestarted,
			"The metrics-exporter keeps the previous password until that container starts again. The other system passwords keep rotating.",
			metav1.ConditionTrue)
		return
	}
	meta.RemoveStatusCondition(&cluster.Status.Conditions, valkeyiov1alpha1.ConditionExporterPasswordHeld)
}

func (r *ValkeyClusterReconciler) recordSystemPasswordRotation(cluster *valkeyiov1alpha1.ValkeyCluster, event string) {
	if r.Recorder == nil {
		return
	}
	switch event {
	case "started":
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "SystemPasswordRotationStarted", "ReconcileUsers",
			"Generated the next system passwords. The passwords already in use stay valid until every node has loaded both.")
	case "cutover":
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "SystemPasswordRotationCutover", "ReconcileUsers",
			"Nodes have loaded both system passwords. Clients are moving to the new ones. The old passwords stay valid until they have switched.")
	case systemPasswordEventHolding:
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "SystemPasswordRotationHolding", "ReconcileUsers",
			"A client is still using a previous system password. That password stays accepted until the client has switched.")
	case "complete":
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "SystemPasswordRotationFinished", "ReconcileUsers",
			"Previous system passwords have been removed. Clients are using the current ones.")
	}
}
