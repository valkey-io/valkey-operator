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

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// systemPasswordHolders reports whether a server container and an exporter
// container of the cluster are running. A running server holds an ACL built
// from the current _operator and _replication passwords, and a running
// exporter read _exporter at start. A container that starts later reads the
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

// lockedSystemPasswordKeys returns the current system password keys the
// Secret lacks while a running container holds them. Generating any of them
// would lock that container out.
func lockedSystemPasswordKeys(secret *corev1.Secret, serverRunning, exporterRunning bool) []string {
	var locked []string
	for _, user := range systemUsers {
		held := serverRunning
		if user == exporterUser {
			held = exporterRunning
		}
		if _, ok := secret.Data[user]; !ok && held {
			locked = append(locked, user)
		}
	}
	return locked
}

// refuseLockedSystemPasswords returns errSystemPasswordSecretMissing, with a
// warning event, when the Secret lacks a current password a running container
// holds, or when it lacks one and the pods cannot be listed. It lists the
// pods only when a key is missing.
func (r *ValkeyClusterReconciler) refuseLockedSystemPasswords(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, secret *corev1.Secret) error {
	missing := lockedSystemPasswordKeys(secret, true, cluster.Spec.ExporterEnabled())
	if len(missing) == 0 {
		return nil
	}
	serverRunning, exporterRunning, err := r.systemPasswordHolders(ctx, cluster)
	var msg string
	if err != nil {
		logf.FromContext(ctx).Error(err, "failed to list the cluster's pods")
		msg = fmt.Sprintf("%s is missing %s and the pod state could not be checked; not generating system user passwords until the pods can be listed",
			secret.Name, strings.Join(missing, ", "))
	} else {
		locked := lockedSystemPasswordKeys(secret, serverRunning, exporterRunning)
		if len(locked) == 0 {
			return nil
		}
		msg = fmt.Sprintf("%s is missing %s while the cluster's servers run with them; restore the Secret to recover the cluster",
			secret.Name, strings.Join(locked, ", "))
	}
	r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, valkeyiov1alpha1.ReasonSystemPasswordMissing, "ReconcileUsers", "%s", msg)
	return fmt.Errorf("%w: %s", errSystemPasswordSecretMissing, msg)
}
