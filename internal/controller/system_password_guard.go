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
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// systemPasswordHolders reports whether a server container and an exporter
// container of the cluster are running. A running server holds an ACL built
// from the current _operator and _replication passwords, and a running
// exporter read _exporter at start and keeps it. A container that starts later reads the
// Secret and the aclfile as they are then. Terminating pods still count. The
// pods are read uncached, so a server that has just started is not missed.
func (r *ValkeyClusterReconciler) systemPasswordHolders(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) (serverRunning, exporterRunning bool, err error) {
	pods := &corev1.PodList{}
	if err := r.uncachedReader().List(ctx, pods, client.InNamespace(cluster.Namespace), client.MatchingLabels{LabelCluster: cluster.Name}); err != nil {
		return false, false, err
	}
	for i := range pods.Items {
		for _, status := range pods.Items[i].Status.ContainerStatuses {
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

// lockedSystemPasswordKeys returns the _operator and _replication keys the
// Secret lacks while a server runs. Generating either would lock the operator
// or the replicas out of that server.
func lockedSystemPasswordKeys(secret *corev1.Secret, serverRunning bool) []string {
	if !serverRunning {
		return nil
	}
	var locked []string
	for _, user := range []string{operatorUser, replicationUser} {
		if _, ok := secret.Data[user]; !ok {
			locked = append(locked, user)
		}
	}
	return locked
}

// guardMissingSystemPasswords returns errSystemPasswordSecretMissing, with a
// warning event, when the Secret lacks _operator or _replication while a
// server runs, or lacks one and the pods cannot be listed. It lists the pods
// only when a key is missing.
//
// A lost _exporter is generated again: a running exporter's password cannot
// be read back, so a new one is the only recovery, and the exporter is the
// only client of it. A warning event says the running exporters fail until
// their pods restart.
func (r *ValkeyClusterReconciler) guardMissingSystemPasswords(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, secret *corev1.Secret) error {
	missing := lockedSystemPasswordKeys(secret, true)
	_, hasExporterKey := secret.Data[exporterUser]
	exporterLost := cluster.Spec.ExporterEnabled() && !hasExporterKey
	if len(missing) == 0 && !exporterLost {
		return nil
	}
	serverRunning, exporterRunning, err := r.systemPasswordHolders(ctx, cluster)
	var msg string
	switch {
	case err != nil && len(missing) == 0:
		logf.FromContext(ctx).Error(err, "failed to list the cluster's pods")
		return nil
	case err != nil:
		logf.FromContext(ctx).Error(err, "failed to list the cluster's pods")
		msg = fmt.Sprintf("%s is missing %s and the pod state could not be checked; not generating system user passwords until the pods can be listed",
			secret.Name, strings.Join(missing, ", "))
	default:
		locked := lockedSystemPasswordKeys(secret, serverRunning)
		if len(locked) == 0 {
			if exporterLost && exporterRunning {
				r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "ExporterPasswordRegenerated", "ReconcileUsers",
					"%s is missing %s, so a new password is generated; running exporters keep the old one and report redis_up 0 until their pods restart", secret.Name, exporterUser)
			}
			return nil
		}
		msg = fmt.Sprintf("%s is missing %s while the cluster's servers run with them; restore the Secret to recover the cluster",
			secret.Name, strings.Join(locked, ", "))
	}
	r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, valkeyiov1alpha1.ReasonSystemPasswordSecretMissing, "ReconcileUsers", "%s", msg)
	return fmt.Errorf("%w: %s", errSystemPasswordSecretMissing, msg)
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
