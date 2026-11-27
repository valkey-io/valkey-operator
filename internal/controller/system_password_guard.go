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
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// errSystemPasswordSecretMissing is a refused regeneration of a system
// password that a running server still holds.
var errSystemPasswordSecretMissing = errors.New("system password secret missing")

// systemPasswordHolders reports whether a server container and an exporter
// container of the cluster are running. A running server holds an ACL built
// from the current _operator and _replication passwords, and a running
// exporter read _exporter at start and keeps it. A container that starts later
// reads the Secret and the aclfile as they are then. Terminating pods still
// count.
func (r *ValkeyClusterReconciler) systemPasswordHolders(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) (serverRunning, exporterRunning bool, err error) {
	pods, err := r.uncachedClusterPods(ctx, cluster)
	if err != nil {
		return false, false, err
	}
	for i := range pods {
		for _, status := range pods[i].Status.ContainerStatuses {
			if status.State.Running == nil {
				continue
			}
			switch status.Name {
			case serverContainerName:
				serverRunning = true
			case metricsExporterContainerName:
				exporterRunning = true
			}
		}
	}
	return serverRunning, exporterRunning, nil
}

// missingServerPasswordKeys returns the _operator and _replication keys the
// Secret lacks or holds empty. Generating either while a server runs would
// lock the operator or the replicas out of that server.
func missingServerPasswordKeys(secret *corev1.Secret) []string {
	var missing []string
	for _, user := range []string{operatorUser, replicationUser} {
		if len(secret.Data[user]) == 0 {
			missing = append(missing, user)
		}
	}
	return missing
}

// unmatchedServerPasswordKeys returns the _operator and _replication keys
// whose password the cluster's aclfile does not accept: a backup from before
// a rotation, or a hand edit. The aclfile is what the servers load, and the
// operator adds a password to it before it becomes current, so a password
// the operator wrote always matches. A cluster with no aclfile yet has
// nothing to match.
func (r *ValkeyClusterReconciler) unmatchedServerPasswordKeys(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, secret *corev1.Secret) ([]string, error) {
	aclSecret, err := r.operatorSecret(ctx, cluster, getInternalSecretName(cluster.Name), legacyInternalSecretName(cluster.Name))
	if err != nil || aclSecret == nil || len(aclSecret.Data[aclFilename]) == 0 {
		return nil, err
	}
	accepted := desiredUserPasswordHashes(string(aclSecret.Data[aclFilename]))
	var unmatched []string
	for _, user := range []string{operatorUser, replicationUser} {
		hashes, ok := accepted[user]
		password := secret.Data[user]
		if ok && len(password) > 0 && !slices.Contains(hashes, systemPasswordDigest(password)) {
			unmatched = append(unmatched, user)
		}
	}
	return unmatched, nil
}

// guardSystemPasswords returns errSystemPasswordSecretMissing, with a warning
// event, when the Secret lacks _operator or _replication, or holds one the
// aclfile does not accept, while a server runs, or when the pods cannot be
// listed to tell. It lists the pods only when there is such a key.
//
// A lost _exporter is generated again: a running exporter's password cannot
// be read back, so a new one is the only recovery, and the exporter is the
// only client of it. exporterLost reports that an exporter runs with the lost
// password, for warnExporterPasswordRegenerated once the new one is written.
func (r *ValkeyClusterReconciler) guardSystemPasswords(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, secret *corev1.Secret) (exporterLost bool, err error) {
	missing := missingServerPasswordKeys(secret)
	unmatched, err := r.unmatchedServerPasswordKeys(ctx, cluster, secret)
	if err != nil {
		return false, err
	}
	exporterMissing := cluster.Spec.ExporterEnabled() && len(secret.Data[exporterUser]) == 0
	if len(missing) == 0 && len(unmatched) == 0 && !exporterMissing {
		return false, nil
	}
	serverRunning, exporterRunning, err := r.systemPasswordHolders(ctx, cluster)
	if err != nil {
		logf.FromContext(ctx).Error(err, "failed to list the cluster's pods")
		switch {
		case len(missing) > 0:
			return false, r.refuseSystemPasswords(cluster, fmt.Sprintf("%s is missing %s and the pod state could not be checked; not generating system user passwords until the pods can be listed",
				secret.Name, strings.Join(missing, ", ")))
		case len(unmatched) > 0:
			return false, r.refuseSystemPasswords(cluster, fmt.Sprintf("%s holds %s passwords that do not match the aclfile and the pod state could not be checked; not writing them until the pods can be listed",
				secret.Name, strings.Join(unmatched, ", ")))
		}
		return false, nil
	}
	if serverRunning {
		switch {
		case len(missing) > 0:
			return false, r.refuseSystemPasswords(cluster, fmt.Sprintf("%s is missing %s while the cluster's servers run with them; restore the Secret to recover the cluster",
				secret.Name, strings.Join(missing, ", ")))
		case len(unmatched) > 0:
			return false, r.refuseSystemPasswords(cluster, fmt.Sprintf("%s holds %s passwords that do not match the aclfile the servers run with; restore a copy taken after the last rotation, or rebuild the Secret from the files mounted in a running server",
				secret.Name, strings.Join(unmatched, ", ")))
		}
	}
	return exporterMissing && exporterRunning, nil
}

// warnExporterPasswordRegenerated says the running exporters fail until their
// pods restart, since they keep the password they started with.
func (r *ValkeyClusterReconciler) warnExporterPasswordRegenerated(cluster *valkeyiov1alpha1.ValkeyCluster, secretName string) {
	r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "ExporterPasswordRegenerated", "ReconcileUsers",
		"%s was missing %s, so a new password was generated; running exporters keep the old one and report redis_up 0 until their pods restart", secretName, exporterUser)
}

// refuseSystemPasswords returns the refusal, and emits its warning when the
// cluster does not already report it: the refusal retries every 10s.
func (r *ValkeyClusterReconciler) refuseSystemPasswords(cluster *valkeyiov1alpha1.ValkeyCluster, msg string) error {
	err := fmt.Errorf("%w: %s", errSystemPasswordSecretMissing, msg)
	c := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)
	if c == nil || c.Reason != valkeyiov1alpha1.ReasonSystemPasswordSecretMissing || c.Message != err.Error() {
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, valkeyiov1alpha1.ReasonSystemPasswordSecretMissing, "ReconcileUsers", "%s", msg)
	}
	return err
}

// reportSystemPasswordSecretMissing sets Degraded and Ready for a refused
// regeneration and returns true. Otherwise it clears the Degraded reason it
// set, and returns false.
func reportSystemPasswordSecretMissing(cluster *valkeyiov1alpha1.ValkeyCluster, aclErr error) bool {
	if !errors.Is(aclErr, errSystemPasswordSecretMissing) {
		removeConditionIfReason(&cluster.Status.Conditions, valkeyiov1alpha1.ConditionDegraded, valkeyiov1alpha1.ReasonSystemPasswordSecretMissing)
		return false
	}
	setCondition(cluster, valkeyiov1alpha1.ConditionDegraded, valkeyiov1alpha1.ReasonSystemPasswordSecretMissing, aclErr.Error(), metav1.ConditionTrue)
	setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonUsersAclError, aclErr.Error(), metav1.ConditionFalse)
	return true
}
