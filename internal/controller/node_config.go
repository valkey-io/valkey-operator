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
	"maps"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

const (
	// Ready reasons for a node whose config cannot be written.
	reasonMissingConfigRevision  = "MissingConfigRevision"
	reasonConfigRevisionMismatch = "ConfigRevisionMismatch"

	configRevisionKeyPrefix = "valkey-"
	configRevisionKeySuffix = ".conf"
)

// errMissingConfigRevision means the node has no spec.configRevision, so
// there is no pinned file to mount.
var errMissingConfigRevision = errors.New("spec.configRevision is empty; the parent sets it")

// configRevisionMismatchError means the pinned file the node renders from its
// spec does not hash to spec.configRevision, and the key is not there yet.
type configRevisionMismatchError struct {
	want, rendered string
}

func (e *configRevisionMismatchError) Error() string {
	return fmt.Sprintf("config revision %s is not in the ConfigMap and the spec renders revision %s", e.want, e.rendered)
}

// nodeConfigMapName names the ConfigMap holding a node's pinned revisions and
// live.conf.
func nodeConfigMapName(nodeName string) string {
	return resourcePrefix + nodeName + "-config"
}

// configRevisionFileKey is the ConfigMap key of pinned revision rev.
func configRevisionFileKey(rev string) string {
	return configRevisionKeyPrefix + rev + configRevisionKeySuffix
}

// configRevisionFromKey returns the revision a ConfigMap key holds, or false
// when the key is not a pinned revision.
func configRevisionFromKey(key string) (string, bool) {
	rev, ok := strings.CutPrefix(key, configRevisionKeyPrefix)
	if !ok {
		return "", false
	}
	rev, ok = strings.CutSuffix(rev, configRevisionKeySuffix)
	if !ok || len(rev) != pinnedConfigRevisionLength {
		return "", false
	}
	return rev, true
}

// ensureNodeConfig writes the node's ConfigMap before the workload: the
// pinned file for spec.configRevision when it is missing, and live.conf. A
// revision key is written once and never rewritten. A missing one is written
// only when the spec renders to that revision. Other keys are left alone.
func (r *ValkeyNodeReconciler) ensureNodeConfig(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) error {
	rev := node.Spec.ConfigRevision
	if rev == "" {
		return errMissingConfigRevision
	}
	revKey := configRevisionFileKey(rev)

	cm := &corev1.ConfigMap{}
	key := client.ObjectKey{Name: nodeConfigMapName(node.Name), Namespace: node.Namespace}
	exists := true
	if err := r.Get(ctx, key, cm); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		exists = false
	}

	data := map[string]string{}
	if _, ok := cm.Data[revKey]; !ok {
		pinned := renderPinnedConfig(node)
		if rendered := pinnedConfigRevision(pinned); rendered != rev {
			return &configRevisionMismatchError{want: rev, rendered: rendered}
		}
		data[revKey] = pinned
	}
	if live := renderLiveConfig(node); !exists || cm.Data[liveConfigFileKey] != live {
		data[liveConfigFileKey] = live
	}
	if len(data) == 0 {
		return nil
	}

	log := logf.FromContext(ctx)
	if !exists {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: valkeyNodeLabels(node)},
			Data:       data,
		}
		if err := controllerutil.SetControllerReference(node, cm, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, cm); err != nil {
			return err
		}
		log.V(1).Info("created node ConfigMap", "name", cm.Name, "revision", rev)
		return nil
	}
	// A merge patch of the changed keys only: it never drops a key another
	// revision's pods still mount.
	base := cm.DeepCopy()
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	maps.Copy(cm.Data, data)
	if err := r.Patch(ctx, cm, client.MergeFrom(base)); err != nil {
		return err
	}
	log.V(1).Info("updated node ConfigMap", "name", cm.Name, "keys", slices.Sorted(maps.Keys(data)))
	return nil
}

// pruneConfigRevisions deletes the node's pinned revision keys that nothing
// uses: not spec.configRevision, not referenced by the workload template, not
// mounted by any pod of the node (terminating pods included), and only once
// the workload's roll has finished. Pods and the workload are read from the
// API server, and only when the ConfigMap holds more than one revision.
func (r *ValkeyNodeReconciler) pruneConfigRevisions(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) error {
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKey{Name: nodeConfigMapName(node.Name), Namespace: node.Namespace}, cm); err != nil {
		return client.IgnoreNotFound(err)
	}
	var stale []string
	revisions := 0
	for key := range cm.Data {
		rev, ok := configRevisionFromKey(key)
		if !ok {
			continue
		}
		revisions++
		if rev != node.Spec.ConfigRevision {
			stale = append(stale, key)
		}
	}
	if revisions < 2 || len(stale) == 0 {
		return nil
	}

	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	template, finished, err := r.workloadRollState(ctx, reader, node)
	if err != nil || !finished {
		return err
	}
	inUse := map[string]struct{}{}
	if template != nil {
		for _, k := range mountedConfigRevisionKeys(&template.Spec) {
			inUse[k] = struct{}{}
		}
	}
	pods := &corev1.PodList{}
	if err := reader.List(ctx, pods, client.InNamespace(node.Namespace), client.MatchingLabels(valkeyNodeLabels(node))); err != nil {
		return fmt.Errorf("listing pods for ValkeyNode %s: %w", node.Name, err)
	}
	for i := range pods.Items {
		for _, k := range mountedConfigRevisionKeys(&pods.Items[i].Spec) {
			inUse[k] = struct{}{}
		}
	}
	stale = slices.DeleteFunc(stale, func(key string) bool {
		_, used := inUse[key]
		return used
	})
	if len(stale) == 0 {
		return nil
	}

	base := cm.DeepCopy()
	for _, key := range stale {
		delete(cm.Data, key)
	}
	// Optimistic lock: a key written since the read must not be dropped.
	if err := r.Patch(ctx, cm, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	slices.Sort(stale)
	logf.FromContext(ctx).Info("deleted unused config revisions", "name", cm.Name, "keys", stale)
	return nil
}

// workloadRollState returns the node's workload pod template and whether its
// roll has finished: the controller has observed the latest spec and every
// pod runs the current template. A missing workload has no template and
// counts as finished.
func (r *ValkeyNodeReconciler) workloadRollState(ctx context.Context, reader client.Reader,
	node *valkeyiov1alpha1.ValkeyNode) (*corev1.PodTemplateSpec, bool, error) {
	key := client.ObjectKey{Name: valkeyNodeResourceName(node), Namespace: node.Namespace}
	switch node.Spec.WorkloadType {
	case valkeyiov1alpha1.WorkloadTypeStatefulSet:
		sts := &appsv1.StatefulSet{}
		if err := reader.Get(ctx, key, sts); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, true, nil
			}
			return nil, false, err
		}
		finished := sts.Status.ObservedGeneration >= sts.Generation &&
			sts.Status.UpdateRevision != "" && sts.Status.UpdateRevision == sts.Status.CurrentRevision
		return &sts.Spec.Template, finished, nil
	case valkeyiov1alpha1.WorkloadTypeDeployment:
		dep := &appsv1.Deployment{}
		if err := reader.Get(ctx, key, dep); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, true, nil
			}
			return nil, false, err
		}
		finished := dep.Status.ObservedGeneration >= dep.Generation &&
			dep.Status.Replicas == dep.Status.UpdatedReplicas
		return &dep.Spec.Template, finished, nil
	default:
		return nil, false, fmt.Errorf("unsupported workload type: %q", node.Spec.WorkloadType)
	}
}

// mountedConfigRevisionKeys returns the pinned revision keys a pod spec's
// config volume mounts.
func mountedConfigRevisionKeys(spec *corev1.PodSpec) []string {
	var out []string
	for _, v := range spec.Volumes {
		if v.Name != configVolumeName || v.ConfigMap == nil {
			continue
		}
		for _, item := range v.ConfigMap.Items {
			if _, ok := configRevisionFromKey(item.Key); ok {
				out = append(out, item.Key)
			}
		}
	}
	return out
}
