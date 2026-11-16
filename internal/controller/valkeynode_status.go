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

	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// patchStatus changes the node's status and writes the change. It reads the
// node from first, calls mutate on it, and sends nothing when mutate reports
// no change. It returns the node as written, or as read when nothing changed.
//
// The patch carries the resourceVersion of the copy it was built from. A JSON
// merge patch replaces status.conditions as a whole, so a patch built on a
// stale read would otherwise send the stale list and erase a condition written
// since, or bring back one removed since. With the resourceVersion, such a
// patch fails with a conflict instead. On a conflict, patchStatus reads the
// node from the API server and calls mutate again, so mutate must derive every
// change from the node it is given.
func (r *ValkeyNodeReconciler) patchStatus(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode, first client.Reader,
	mutate func(current *valkeyiov1alpha1.ValkeyNode) bool) (*valkeyiov1alpha1.ValkeyNode, error) {
	fresh := client.Reader(r.Client)
	if r.APIReader != nil {
		fresh = r.APIReader
	}
	reader := first
	var current *valkeyiov1alpha1.ValkeyNode
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current = &valkeyiov1alpha1.ValkeyNode{}
		if err := reader.Get(ctx, client.ObjectKeyFromObject(node), current); err != nil {
			return err
		}
		reader = fresh
		patchBase := current.DeepCopy()
		if !mutate(current) {
			return nil
		}
		return r.Status().Patch(ctx, current, client.MergeFromWithOptions(patchBase, client.MergeFromWithOptimisticLock{}))
	})
	return current, err
}
