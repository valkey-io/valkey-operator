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

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// setDesiredConfigRevision sets desired.Spec.ConfigRevision. current is the
// existing node, nil when the node is new, and liveRev is the revision its
// live workload template references. A new node, a node without a workload,
// or a live revision whose content is unknown gets the desired revision.
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

// liveRevisionContent returns the content of the pinned config revision rev
// that node's live workload references. found is false when the content is
// not known, and the parent then uses the desired revision.
//
// Each config layout replaces this with a read from where it stores pinned
// revisions.
func (r *ValkeyClusterReconciler) liveRevisionContent(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster,
	node *valkeyiov1alpha1.ValkeyNode, rev string) (content string, found bool, err error) {
	return "", false, nil
}
