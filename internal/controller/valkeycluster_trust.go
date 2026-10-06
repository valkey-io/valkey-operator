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
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// getTrustBundleSecretName is the operator-managed Secret holding the merged
// trust roots that nodes load as tls-ca-cert-file.
func getTrustBundleSecretName(clusterName string) string {
	return clusterName + "-tls-trust"
}

// errTrustSourceNotFound marks a source Secret, or its ca.crt key, that does
// not exist, as opposed to one whose contents do not parse.
var errTrustSourceNotFound = errors.New("trust source not found")

// errTrustSourceInvalid marks a source whose contents are not a valid bundle.
var errTrustSourceInvalid = errors.New("trust source invalid")

// readTrustSources reads the server secret's ca.crt and then each
// clientAuth.ca entry, in that order, converting a SPIFFE bundle to PEM. The
// server root comes first and is always present, since peers verify each
// other's server certificates on the cluster bus and replication links against
// the same file. User Secrets and ConfigMaps are not in the operator's cache,
// so they are read through the APIReader.
func (r *ValkeyClusterReconciler) readTrustSources(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) ([]trustSource, error) {
	refs := append([]valkeyiov1alpha1.TrustSource{serverTrustSource(cluster)}, cluster.GetTLS().ClientAuthCA()...)
	sources := make([]trustSource, 0, len(refs))
	for _, ref := range refs {
		src, err := r.readTrustSource(ctx, cluster.Namespace, ref)
		if err != nil {
			return nil, err
		}
		sources = append(sources, src)
	}
	return sources, nil
}

// serverTrustSource is the server secret's ca.crt, the first source of every
// bundle.
func serverTrustSource(cluster *valkeyiov1alpha1.ValkeyCluster) valkeyiov1alpha1.TrustSource {
	return valkeyiov1alpha1.TrustSource{SecretName: cluster.GetTLS().Certificates.Server.SecretName}
}

// readTrustSource reads one source's key, converting a SPIFFE bundle to PEM.
func (r *ValkeyClusterReconciler) readTrustSource(ctx context.Context, namespace string, ref valkeyiov1alpha1.TrustSource) (trustSource, error) {
	key := ref.Key
	if key == "" {
		key = tlsSecretKeyCA
	}
	var (
		kind, name string
		data       []byte
		found      bool
		err        error
	)
	if ref.ConfigMapName != "" {
		kind, name = "configmap", ref.ConfigMapName
		cm := &corev1.ConfigMap{}
		if err = r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cm); err == nil {
			var s string
			if s, found = cm.Data[key]; found {
				data = []byte(s)
			} else {
				data, found = cm.BinaryData[key]
			}
		}
	} else {
		kind, name = "secret", ref.SecretName
		secret := &corev1.Secret{}
		if err = r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret); err == nil {
			data, found = secret.Data[key]
		}
	}
	if err != nil {
		if apierrors.IsNotFound(err) {
			return trustSource{}, fmt.Errorf("%w: %s %q does not exist", errTrustSourceNotFound, kind, name)
		}
		return trustSource{}, fmt.Errorf("reading trust source %s %q: %w", kind, name, err)
	}
	if !found {
		return trustSource{}, fmt.Errorf("%w: %s %q has no %s key", errTrustSourceNotFound, kind, name, key)
	}
	src := trustSource{ref: fmt.Sprintf("%s %q key %s", kind, name, key), pem: data, format: "PEM"}
	if isSPIFFEBundle(data) {
		src.format = "SPIFFE bundle"
		converted, convErr := spiffeBundleToPEM(data)
		if convErr != nil {
			return trustSource{}, fmt.Errorf("%w: %s: %w", errTrustSourceInvalid, src.ref, convErr)
		}
		src.pem = converted
	}
	return src, nil
}

// reservedTrustBundleNameUse reports, as a condition message, any spec field
// that names the operator-managed trust bundle Secret as one of its own
// inputs, or "" when none does. Writing the bundle would replace that Secret's
// contents, deleting a server certificate and key, or would merge the bundle
// into itself.
func reservedTrustBundleNameUse(cluster *valkeyiov1alpha1.ValkeyCluster, name string) string {
	tls := cluster.GetTLS()
	if tls.Certificates.Server.SecretName == name {
		return fmt.Sprintf("certificates.server.secretName %q is reserved for the operator-managed trust bundle; use another name", name)
	}
	for i, src := range tls.ClientAuthCA() {
		if src.SecretName == name {
			return fmt.Sprintf("clientAuth.ca[%d].secretName %q is reserved for the operator-managed trust bundle; use another name", i, name)
		}
	}
	return ""
}

// addedRoots counts the certificates in next that are not in current. A
// current bundle that does not parse counts as holding nothing, so every root
// in next is new.
func addedRoots(current, next []byte) int {
	have := map[string]struct{}{}
	if blocks, err := decodePEMStrict(current); err == nil {
		for _, b := range blocks {
			have[string(b.Bytes)] = struct{}{}
		}
	}
	blocks, err := decodePEMStrict(next)
	if err != nil {
		return 0
	}
	added := 0
	for _, b := range blocks {
		if _, ok := have[string(b.Bytes)]; !ok {
			added++
		}
	}
	return added
}

// nodesBehindLiveACL names the cluster's ValkeyNodes whose running server has
// not been confirmed to hold the current revision of the operator-managed
// aclfile, read through the APIReader so it reflects this reconcile's write.
// It is empty when there is no managed aclfile yet.
func (r *ValkeyClusterReconciler) nodesBehindLiveACL(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) ([]string, error) {
	acl := &corev1.Secret{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: getInternalSecretName(cluster.Name)}, acl); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	revision := aclRevision(desiredUserPasswordHashes(string(acl.Data[aclFilename])))
	if revision == "" {
		return nil, nil
	}
	nodes := &valkeyiov1alpha1.ValkeyNodeList{}
	if err := r.List(ctx, nodes, client.InNamespace(cluster.Namespace), client.MatchingLabels{LabelCluster: cluster.Name}); err != nil {
		return nil, err
	}
	var pending []string
	for i := range nodes.Items {
		if nodes.Items[i].Status.LiveACLRevision != revision {
			pending = append(pending, nodes.Items[i].Name)
		}
	}
	slices.Sort(pending)
	return pending, nil
}

// writeTrustBundle creates the trust bundle Secret, or updates the existing
// one read through the APIReader, so that it holds bundle and carries the
// operator's labels. The caller has already refused a Secret this cluster
// does not control.
func (r *ValkeyClusterReconciler) writeTrustBundle(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, existing *corev1.Secret, exists bool, bundle []byte) error {
	name := getTrustBundleSecretName(cluster.Name)
	secret := existing
	if !exists {
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cluster.Namespace}}
	}
	before := secret.DeepCopy()
	if secret.Labels == nil {
		secret.Labels = map[string]string{}
	}
	maps.Copy(secret.Labels, baseLabels(cluster.Name, "tls-trust"))
	secret.Type = corev1.SecretTypeOpaque
	secret.Data = map[string][]byte{tlsSecretKeyCA: bundle}
	if err := controllerutil.SetControllerReference(cluster, secret, r.Scheme); err != nil {
		return fmt.Errorf("owning trust bundle secret %q: %w", name, err)
	}
	log := logf.FromContext(ctx)
	if !exists {
		if err := r.Create(ctx, secret); err != nil {
			return fmt.Errorf("creating trust bundle secret %q: %w", name, err)
		}
		log.Info("trust bundle written", "secret", name, "result", "created")
		return nil
	}
	if equality.Semantic.DeepEqual(before, secret) {
		return nil
	}
	if err := r.Update(ctx, secret); err != nil {
		return fmt.Errorf("updating trust bundle secret %q: %w", name, err)
	}
	log.Info("trust bundle written", "secret", name, "result", "updated")
	return nil
}

// refreshServerRootInFallback keeps a last-good bundle in step with the
// server certificate while a clientAuth.ca source is broken: nodes verify
// peers against the same file, so a server certificate renewed onto a new CA
// would otherwise break the cluster bus. It writes the current server root
// followed by every root already in the bundle, so nothing is dropped. It is
// best effort: if the server secret cannot be read either, the bundle is left
// as it is.
func (r *ValkeyClusterReconciler) refreshServerRootInFallback(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, existing *corev1.Secret) error {
	server, err := r.readTrustSource(ctx, cluster.Namespace, serverTrustSource(cluster))
	if err != nil {
		return nil
	}
	merged, err := mergeTrustBundle([]trustSource{server, {ref: "the last good bundle", pem: existing.Data[tlsSecretKeyCA]}})
	if err != nil || bytes.Equal(merged, existing.Data[tlsSecretKeyCA]) {
		return nil
	}
	// The renewed server root is a new root like any other: it waits for the ACL.
	if pending, err := r.nodesBehindLiveACL(ctx, cluster); err != nil || len(pending) > 0 {
		return err
	}
	existing.Data[tlsSecretKeyCA] = merged
	if err := r.Update(ctx, existing); err != nil {
		return fmt.Errorf("adding the current server root to trust bundle secret %q: %w", existing.Name, err)
	}
	logf.FromContext(ctx).Info("added the current server root to the last good trust bundle", "secret", existing.Name)
	return nil
}

// reconcileTrustBundle writes <cluster>-tls-trust when clientAuth.ca is set and
// returns the name of the Secret nodes should reference as their trust bundle,
// or "" when they should keep the server secret's ca.crt.
//
// When a source is missing or invalid, the trust Secret is left at its last
// good roots and TLSConfigured=False reports why: writing a partial bundle
// would silently lock out every client the dropped root signed. If no trust
// Secret has been written yet, nodes keep the server root, so a typo in
// clientAuth.ca never leaves a pod waiting on a volume that does not exist.
// Only a Secret this cluster controls counts as written: one created by
// anyone else under the same name is never trusted.
//
// The trust Secret is not deleted when clientAuth.ca is emptied: pods still
// mount it until they roll. It is garbage-collected with the cluster.
func (r *ValkeyClusterReconciler) reconcileTrustBundle(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) (string, error) {
	if len(cluster.GetTLS().ClientAuthCA()) == 0 {
		meta.RemoveStatusCondition(&cluster.Status.Conditions, valkeyiov1alpha1.ConditionTLSConfigured)
		return "", nil
	}

	name := getTrustBundleSecretName(cluster.Name)
	if msg := reservedTrustBundleNameUse(cluster, name); msg != "" {
		r.reportTrustSourceError(ctx, cluster, valkeyiov1alpha1.ReasonTrustBundleConflict, msg)
		return "", nil
	}
	existing := &corev1.Secret{}
	exists := true
	// Read through the APIReader, not the cache: the cache holds only Secrets
	// with the operator's managed-by label, so a same-named Secret without it
	// would look absent and every create would fail with AlreadyExists.
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: name}, existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return "", err
		}
		exists = false
	}
	// The operator only ever writes, reads as last good, or points nodes at a
	// Secret this cluster controls. A same-named Secret it does not control is
	// never adopted: adopting would replace whatever it holds (it may be
	// someone's certificate), and trusting it would let anyone able to create
	// Secrets in the namespace inject a root.
	if exists && !metav1.IsControlledBy(existing, cluster) {
		owner := "nothing"
		if controller := metav1.GetControllerOf(existing); controller != nil {
			owner = fmt.Sprintf("%s %q", controller.Kind, controller.Name)
		}
		r.reportTrustSourceError(ctx, cluster, valkeyiov1alpha1.ReasonTrustBundleConflict,
			fmt.Sprintf("secret %q already exists and is controlled by %s, not this cluster; it is left untouched and nodes stay on the server root until it is removed", name, owner))
		return "", nil
	}

	sources, err := r.readTrustSources(ctx, cluster)
	var bundle []byte
	if err == nil {
		if bundle, err = mergeTrustBundle(sources); err != nil {
			err = fmt.Errorf("%w: %w", errTrustSourceInvalid, err)
		}
	}
	if err != nil {
		var reason string
		switch {
		case errors.Is(err, errTrustSourceNotFound):
			reason = valkeyiov1alpha1.ReasonTrustSourceNotFound
		case errors.Is(err, errTrustSourceInvalid):
			reason = valkeyiov1alpha1.ReasonTrustSourceInvalid
		default:
			// A transient read failure: retry rather than report a bad source.
			return "", err
		}
		r.reportTrustSourceError(ctx, cluster, reason, strings.TrimPrefix(err.Error(), errTrustSourceInvalid.Error()+": "))
		if !exists {
			return "", nil
		}
		if err := r.refreshServerRootInFallback(ctx, cluster, existing); err != nil {
			return "", err
		}
		return name, nil
	}

	// A bundle that gains a root waits until every node runs the current ACL.
	// The ACL and the bundle are separate mounted Secrets, so a container that
	// restarts after its bundle mount refreshed but before its ACL mount did
	// would otherwise trust the new root under the old ACL, for example a
	// permissive default removed in the same update. Only additions wait: a
	// bundle that loses roots or stays the same is written at once.
	if exists {
		if added := addedRoots(existing.Data[tlsSecretKeyCA], bundle); added > 0 {
			pending, err := r.nodesBehindLiveACL(ctx, cluster)
			if err != nil {
				return "", err
			}
			if len(pending) > 0 {
				setCondition(cluster, valkeyiov1alpha1.ConditionTLSConfigured, valkeyiov1alpha1.ReasonTrustBundlePending,
					fmt.Sprintf("holding %d new root(s) until node(s) %s confirm the current ACL is live", added, strings.Join(pending, ", ")),
					metav1.ConditionFalse)
				return name, nil
			}
		}
	}

	// Write based on that authoritative read rather than CreateOrUpdate, which
	// would read through the cache again.
	if err := r.writeTrustBundle(ctx, cluster, existing, exists, bundle); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("%s holds the server root and %d clientAuth.ca source(s): %s", name, len(sources)-1, describeTrustSources(sources[1:]))
	setCondition(cluster, valkeyiov1alpha1.ConditionTLSConfigured, valkeyiov1alpha1.ReasonTrustBundleReady, msg, metav1.ConditionTrue)
	return name, nil
}

// describeTrustSources names each source and the format it was read as, so
// the detected format is visible in status rather than guessed at.
func describeTrustSources(sources []trustSource) string {
	parts := make([]string, 0, len(sources))
	for _, src := range sources {
		parts = append(parts, fmt.Sprintf("%s (%s)", src.ref, src.format))
	}
	return strings.Join(parts, ", ")
}

// reportTrustSourceError sets TLSConfigured=False and emits a warning event the
// first time a given message is reported, so a lasting error does not repeat
// the event on every reconcile.
func (r *ValkeyClusterReconciler) reportTrustSourceError(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, reason, message string) {
	prev := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionTLSConfigured)
	if prev == nil || prev.Status != metav1.ConditionFalse || prev.Message != message {
		logf.FromContext(ctx).Info("trust bundle not updated", "reason", reason, "detail", message)
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, reason, "ReconcileTrustBundle", "%s", message)
	}
	setCondition(cluster, valkeyiov1alpha1.ConditionTLSConfigured, reason, message, metav1.ConditionFalse)
}

// withTrustBundle points desired's TLS at the trust bundle Secret, or clears it
// when secretName is empty.
func withTrustBundle(desired *valkeyiov1alpha1.ValkeyNode, secretName string) {
	if desired.Spec.TLS == nil {
		return
	}
	if secretName == "" {
		desired.Spec.TLS.Certificates.TrustBundle = nil
		return
	}
	desired.Spec.TLS.Certificates.TrustBundle = &valkeyiov1alpha1.NodeTrustBundleRef{SecretName: secretName}
}
