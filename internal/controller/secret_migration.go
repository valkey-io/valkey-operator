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
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// legacyInternalSecretName is the name the ACL Secret had before it took the
// valkey- prefix.
func legacyInternalSecretName(clusterName string) string {
	return "internal-" + clusterName + "-acl"
}

// errOperatorSecretForeignController is an operator Secret that another
// object controls.
var errOperatorSecretForeignController = errors.New("operator secret controlled by another object")

// legacySystemPasswordSecretName is the name the system passwords Secret had
// before it took the valkey- prefix.
func legacySystemPasswordSecretName(clusterName string) string {
	return "internal-" + clusterName + "-system-passwords"
}

// renamedSecret pairs an operator-owned Secret with its legacy name.
type renamedSecret struct {
	name, legacyName string
}

func renamedSecrets(clusterName string) []renamedSecret {
	return []renamedSecret{
		{name: getInternalSecretName(clusterName), legacyName: legacyInternalSecretName(clusterName)},
		{name: getSystemPasswordSecretName(clusterName), legacyName: legacySystemPasswordSecretName(clusterName)},
	}
}

// operatorSecret returns the cluster's Secret name, or nil when it does not
// exist. When it does not exist but the Secret legacyName does, it creates name
// first as a copy of that one. The copy does not depend on who controls the
// legacy Secret: the running nodes hold its passwords, so new ones would lock
// the operator out.
//
// The cache holds only Secrets with the operator's managed-by label, and may
// not have seen one created moments ago, so a miss is read again through the
// APIReader before anything is copied or generated.
//
// A Secret under the new name that the cluster does not control is adopted,
// as one restored from a backup is, unless the cluster still controls the
// legacy Secret. Then the copy did not create it, and its passwords may not be
// the ones the nodes hold, so it is refused. See adoptOperatorSecret.
func (r *ValkeyClusterReconciler) operatorSecret(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, name, legacyName string) (*corev1.Secret, error) {
	key := client.ObjectKey{Namespace: cluster.Namespace, Name: name}
	secret := &corev1.Secret{}
	err := r.Get(ctx, key, secret)
	if apierrors.IsNotFound(err) {
		err = r.uncachedReader().Get(ctx, key, secret)
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	legacyKey := client.ObjectKey{Namespace: cluster.Namespace, Name: legacyName}
	legacy := &corev1.Secret{}
	if err == nil {
		if metav1.IsControlledBy(secret, cluster) {
			return secret, r.adoptOperatorSecret(ctx, cluster, secret)
		}
		// Uncached: a legacy Secret the cluster still controls may have lost
		// the managed-by label the cache selects on.
		if err := r.uncachedReader().Get(ctx, legacyKey, legacy); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, err
			}
		} else if metav1.IsControlledBy(legacy, cluster) {
			return nil, fmt.Errorf("secret %s exists but this cluster does not control it, while it still controls %s: delete %s so the operator can copy %s to it", name, legacyName, name, legacyName)
		}
		if err := r.adoptOperatorSecret(ctx, cluster, secret); err != nil {
			return nil, err
		}
		return secret, nil
	}

	if err := r.uncachedReader().Get(ctx, legacyKey, legacy); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	return r.copyLegacySecret(ctx, cluster, legacy, name)
}

// adoptOperatorSecret gives secret the operator's labels and the cluster as
// controller when it lacks them, as a Secret restored from a backup or by hand
// does. Without the managed-by label the cache never sees it, and without the
// controller reference it outlives the cluster. A Secret another object
// controls is refused and not written, except one an earlier cluster of the
// same name controls.
func (r *ValkeyClusterReconciler) adoptOperatorSecret(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, secret *corev1.Secret) error {
	if owner := metav1.GetControllerOf(secret); owner != nil && owner.UID != cluster.UID {
		// An earlier cluster of the same name: garbage collection removes
		// the Secret, and until then it is used as it is.
		if owner.Kind == "ValkeyCluster" && owner.Name == cluster.Name {
			return nil
		}
		return fmt.Errorf("%w: secret %s is controlled by %s %s; the operator manages this Secret itself", errOperatorSecretForeignController, secret.Name, owner.Kind, owner.Name)
	}
	changed := false
	if secret.Labels == nil {
		secret.Labels = map[string]string{}
	}
	for k, v := range labels(cluster) {
		if secret.Labels[k] != v {
			secret.Labels[k] = v
			changed = true
		}
	}
	if metav1.GetControllerOf(secret) == nil {
		if err := controllerutil.SetControllerReference(cluster, secret, r.Scheme); err != nil {
			return err
		}
		changed = true
	}
	if !changed {
		return nil
	}
	if err := r.Update(ctx, secret); err != nil {
		return fmt.Errorf("adopting secret %s: %w", secret.Name, err)
	}
	logf.FromContext(ctx).Info("adopted the Secret", "name", secret.Name)
	return nil
}

// copyLegacySecret creates name as a copy of the legacy Secret, with every key,
// annotation and label, and the operator's labels and controller reference.
func (r *ValkeyClusterReconciler) copyLegacySecret(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, legacy *corev1.Secret, name string) (*corev1.Secret, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   cluster.Namespace,
			Labels:      maps.Clone(legacy.Labels),
			Annotations: maps.Clone(legacy.Annotations),
		},
		Type: AclSecretType,
		Data: maps.Clone(legacy.Data),
	}
	if secret.Labels == nil {
		secret.Labels = map[string]string{}
	}
	maps.Copy(secret.Labels, labels(cluster))
	if err := controllerutil.SetControllerReference(cluster, secret, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, secret); err != nil {
		return nil, fmt.Errorf("copying secret %s to %s: %w", legacy.Name, name, err)
	}
	logf.FromContext(ctx).Info("copied the legacy Secret", "from", legacy.Name, "to", name)
	return secret, nil
}

// retireLegacySecrets handles the cluster's Secrets under their legacy names.
// While any of the cluster's ValkeyNodes, workload templates or pods still
// reads one, it is kept equal to its replacement: a node that has not rolled
// yet keeps loading ACL changes from it, and a container that restarts in place
// reads the current passwords. After that it is deleted. One the cluster does
// not control, or whose replacement does not exist, is left alone.
func (r *ValkeyClusterReconciler) retireLegacySecrets(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) error {
	type retiring struct {
		renamedSecret
		old *corev1.Secret
	}
	var found []retiring
	var legacyNames []string
	for _, s := range renamedSecrets(cluster.Name) {
		old := &corev1.Secret{}
		// Through the cache: this runs on every reconcile, and a Secret the
		// operator created carries the label the cache selects on.
		if err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: s.legacyName}, old); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if !metav1.IsControlledBy(old, cluster) {
			continue
		}
		found = append(found, retiring{renamedSecret: s, old: old})
		legacyNames = append(legacyNames, s.legacyName)
	}
	if len(found) == 0 {
		return nil
	}
	inUse, err := r.secretsInUse(ctx, cluster, legacyNames...)
	if err != nil {
		return err
	}

	log := logf.FromContext(ctx)
	var errs []error
	for _, s := range found {
		old := s.old
		current := &corev1.Secret{}
		if err := r.uncachedReader().Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: s.name}, current); err != nil {
			if !apierrors.IsNotFound(err) {
				errs = append(errs, err)
			}
			continue
		}
		if !inUse[s.legacyName] {
			uid := old.UID
			if err := r.Delete(ctx, old, client.Preconditions{UID: &uid}); err != nil {
				if !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
					errs = append(errs, fmt.Errorf("deleting secret %s: %w", old.Name, err))
				}
				continue
			}
			log.Info("deleted the legacy Secret", "secret", old.Name, "replacedBy", s.name)
			continue
		}
		if maps.EqualFunc(old.Data, current.Data, bytes.Equal) && maps.Equal(old.Annotations, current.Annotations) {
			continue
		}
		mirrored := old.DeepCopy()
		mirrored.Data = maps.Clone(current.Data)
		mirrored.Annotations = maps.Clone(current.Annotations)
		// A conflict means the cache has not seen an earlier update yet; the
		// next reconcile reads it again.
		if err := r.Update(ctx, mirrored); err != nil {
			if !apierrors.IsConflict(err) {
				errs = append(errs, fmt.Errorf("updating secret %s: %w", old.Name, err))
			}
			continue
		}
		log.Info("updated the legacy Secret to match its replacement", "secret", old.Name, "replacedBy", s.name)
	}
	return errors.Join(errs...)
}
