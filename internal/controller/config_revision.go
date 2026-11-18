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

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// setDesiredConfigRevision sets desired.Spec.ConfigRevision. current is the
// existing node, nil when the node is new, and liveRev is the revision its
// live workload template references. A new node, a node without a workload,
// or a live revision whose key is gone from the node's ConfigMap gets the
// desired revision.
func (r *ValkeyClusterReconciler) setDesiredConfigRevision(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster,
	desired, current *valkeyiov1alpha1.ValkeyNode, liveRev string) error {
	pinned := renderPinnedConfig(desired)
	desiredRev := pinnedConfigRevision(pinned)
	desired.Spec.ConfigRevision = desiredRev
	if current == nil || liveRev == "" || liveRev == desiredRev {
		return nil
	}
	liveData, found, err := r.liveRevisionContent(ctx, cluster, current, liveRev)
	if err != nil {
		return fmt.Errorf("read config revision %s for %s: %w", liveRev, current.Name, err)
	}
	if !found {
		return nil
	}
	desired.Spec.ConfigRevision = pinnedConfigRef(pinned, specLiveKeys(desired.Spec.Config), liveRev, liveData,
		appliedLiveConfigMap(current.Status.AppliedLiveConfig), liveConfigAllowlist)
	return nil
}

// errLiveConfigRevisionNotCached means the parent's cache does not hold the
// content of a node's live config revision yet, while the API server does.
// The parent requeues rather than treat it as unknown, which would roll the
// node to the desired revision.
var errLiveConfigRevisionNotCached = errors.New("live config revision not in cache")

// liveRevisionContent returns the content of the pinned config revision rev
// that node's live workload references, from key valkey-<rev>.conf of the
// node's ConfigMap. A cache miss is confirmed through the APIReader. If the
// API server has the key, the cache trails it and errLiveConfigRevisionNotCached
// is returned. If the ConfigMap or the key is gone, found is false and the
// caller uses the desired revision, so the node rolls through the usual gate.
func (r *ValkeyClusterReconciler) liveRevisionContent(ctx context.Context, _ *valkeyiov1alpha1.ValkeyCluster,
	node *valkeyiov1alpha1.ValkeyNode, rev string) (content string, found bool, err error) {
	content, found, err = readConfigRevision(ctx, r.Client, node, rev)
	if err != nil || found {
		return content, found, err
	}
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	if _, found, err = readConfigRevision(ctx, reader, node, rev); err != nil {
		return "", false, err
	}
	if found {
		return "", false, errLiveConfigRevisionNotCached
	}
	return "", false, nil
}

// readConfigRevision reads key valkey-<rev>.conf of the node's ConfigMap.
// A missing ConfigMap or key is not an error.
func readConfigRevision(ctx context.Context, reader client.Reader, node *valkeyiov1alpha1.ValkeyNode,
	rev string) (string, bool, error) {
	cm := &corev1.ConfigMap{}
	if err := reader.Get(ctx, client.ObjectKey{Name: nodeConfigMapName(node.Name), Namespace: node.Namespace}, cm); err != nil {
		return "", false, client.IgnoreNotFound(err)
	}
	content, ok := cm.Data[configRevisionFileKey(rev)]
	return content, ok, nil
}
