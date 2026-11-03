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
	"encoding/hex"
	"encoding/pem"
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

// trustBundleServerKey holds the server root's last good copy in the bundle
// Secret. It is fixed rather than named after the server secret, so renaming
// that secret to one that cannot be read yet does not lose the root the cluster
// bus depends on.
const trustBundleServerKey = "source-server"

// trustSourceKey names the bundle Secret key that holds a clientAuth.ca entry's
// last good copy. It is a hash because a key cannot hold a resource name and a
// key name in full.
func trustSourceKey(ref valkeyiov1alpha1.TrustSource) string {
	key := ref.Key
	if key == "" {
		key = tlsSecretKeyCA
	}
	sum := sha256.Sum256([]byte(ref.ConfigMapName + "\x00" + ref.SecretName + "\x00" + key))
	return "source-" + hex.EncodeToString(sum[:])
}

// resolveTrustSources returns what each source contributes to the bundle: the
// server secret's ca.crt and then each clientAuth.ca entry, in that order. A
// source that is missing or invalid contributes its last good copy from stored,
// or nothing if it has none, and the first such failure is returned as broken.
// An entry no longer in the spec is not read, so its roots leave the bundle even
// while another source is broken. The server root comes first and is always
// wanted, since peers verify each other's server certificates on the cluster bus
// and replication links against the same file. User Secrets and ConfigMaps are
// not in the operator's cache, so they are read through the APIReader. Any other
// read failure is transient and returned as err.
func (r *ValkeyClusterReconciler) resolveTrustSources(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, stored map[string][]byte) (sources []trustSource, broken, err error) {
	refs := append([]valkeyiov1alpha1.TrustSource{serverTrustSource(cluster)}, cluster.GetTLS().ClientAuthCA()...)
	sources = make([]trustSource, 0, len(refs))
	for i, ref := range refs {
		key := trustBundleServerKey
		if i > 0 {
			key = trustSourceKey(ref)
		}
		src, readErr := r.readTrustSource(ctx, cluster.Namespace, ref)
		if readErr == nil {
			if _, mergeErr := mergeTrustBundle([]trustSource{src}); mergeErr != nil {
				readErr = fmt.Errorf("%w: %w", errTrustSourceInvalid, mergeErr)
			}
		}
		if readErr == nil {
			src.key = key
			sources = append(sources, src)
			continue
		}
		if !errors.Is(readErr, errTrustSourceNotFound) && !errors.Is(readErr, errTrustSourceInvalid) {
			return nil, nil, readErr
		}
		if broken == nil {
			broken = readErr
		}
		last := trustSource{ref: "the last good copy", pem: stored[key], key: key}
		if _, mergeErr := mergeTrustBundle([]trustSource{last}); mergeErr == nil {
			sources = append(sources, last)
		}
	}
	return sources, broken, nil
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

// withoutNewRoots returns next without the certificates that are in neither
// current nor exempt, and how many it left out. A current bundle that does not
// parse counts as holding nothing, so every root in next that is not exempt is
// new.
func withoutNewRoots(current, next, exempt []byte) ([]byte, int) {
	have := map[string]struct{}{}
	for _, known := range [][]byte{current, exempt} {
		if blocks, err := decodePEMStrict(known); err == nil {
			for _, b := range blocks {
				have[string(b.Bytes)] = struct{}{}
			}
		}
	}
	blocks, err := decodePEMStrict(next)
	if err != nil {
		return next, 0
	}
	var kept []byte
	left := 0
	for _, b := range blocks {
		if _, ok := have[string(b.Bytes)]; !ok {
			left++
			continue
		}
		kept = append(kept, pem.EncodeToMemory(b)...)
	}
	return kept, left
}

// nodesBehindAppliedACL names the cluster's ValkeyNodes whose running server has
// not been confirmed to hold the current revision of the operator-managed
// aclfile, read through the APIReader so it reflects this reconcile's write.
// It is empty when there is no managed aclfile yet.
func (r *ValkeyClusterReconciler) nodesBehindAppliedACL(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) ([]string, error) {
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
		if nodes.Items[i].Status.AppliedACLRevision != revision {
			pending = append(pending, nodes.Items[i].Name)
		}
	}
	slices.Sort(pending)
	return pending, nil
}

// podMountsSecret reports whether a pod spec has a volume sourcing the Secret.
func podMountsSecret(spec *corev1.PodSpec, name string) bool {
	for i := range spec.Volumes {
		volume := &spec.Volumes[i]
		if volume.Secret != nil && volume.Secret.SecretName == name {
			return true
		}
		if volume.Projected == nil {
			continue
		}
		for _, source := range volume.Projected.Sources {
			if source.Secret != nil && source.Secret.Name == name {
				return true
			}
		}
	}
	return false
}

// trustBundleInUse reports whether anything could still need the named trust
// bundle Secret. A ValkeyNode's spec is not enough: it drops the reference as
// soon as the cluster stops asking for it, while its workload keeps the old
// template until its roll is authorised and its pod runs until replaced. A pod
// created from that template would never start without the Secret. Pods that
// have finished are not counted, since they never start again. Everything is
// read through the APIReader, because the answer decides a deletion.
func (r *ValkeyClusterReconciler) trustBundleInUse(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, name string) (bool, error) {
	inCluster := []client.ListOption{client.InNamespace(cluster.Namespace), client.MatchingLabels{LabelCluster: cluster.Name}}
	nodes := &valkeyiov1alpha1.ValkeyNodeList{}
	if err := r.APIReader.List(ctx, nodes, inCluster...); err != nil {
		return false, err
	}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if tls := node.Spec.TLS; tls != nil && tls.Certificates.TrustBundle != nil && tls.Certificates.TrustBundle.SecretName == name {
			return true, nil
		}
		template, err := livePodTemplate(ctx, r.APIReader, node)
		if err != nil {
			return false, err
		}
		if template != nil && podMountsSecret(&template.Spec, name) {
			return true, nil
		}
	}
	pods := &corev1.PodList{}
	if err := r.APIReader.List(ctx, pods, inCluster...); err != nil {
		return false, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if podMountsSecret(&pod.Spec, name) {
			return true, nil
		}
	}
	return false, nil
}

// retireTrustBundle handles the trust bundle Secret this cluster controls once
// clientAuth.ca is empty. While anything can still mount it, it holds only the
// server root, so a root removed from the spec stops being trusted before the
// nodes reach their rolls, and a renewed server root still reaches them. Then it
// is deleted. A Secret of that name it does not control is left alone.
func (r *ValkeyClusterReconciler) retireTrustBundle(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) error {
	name := getTrustBundleSecretName(cluster.Name)
	secret := &corev1.Secret{}
	// Through the cache: this runs on every reconcile of a cluster without
	// clientAuth.ca, and a bundle the operator wrote carries the label the
	// cache selects on.
	if err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: name}, secret); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(secret, cluster) {
		return nil
	}
	inUse, err := r.trustBundleInUse(ctx, cluster, name)
	if err != nil {
		return err
	}
	if !inUse {
		if err := r.Delete(ctx, secret); err != nil {
			return client.IgnoreNotFound(err)
		}
		logf.FromContext(ctx).Info("deleted the unused trust bundle", "secret", name)
		return nil
	}
	if cluster.GetTLS() == nil {
		return nil
	}
	sources, _, err := r.resolveTrustSources(ctx, cluster, secret.Data)
	if err != nil || len(sources) == 0 {
		return err
	}
	bundle, err := mergeTrustBundle(sources)
	if err != nil {
		return err
	}
	return r.writeTrustBundle(ctx, cluster, secret, true, bundle, sources)
}

// holdNewRoots returns the bundle to write now. A new clientAuth.ca root waits
// until every node runs the current ACL. The ACL and the bundle are separate
// mounted Secrets, so a container that restarts after its bundle mount refreshed
// but before its ACL mount did would otherwise trust the new root under the old
// ACL, for example a permissive default removed in the same update.
//
// Only those roots wait, so they are left out of what is written and everything
// else in the bundle takes effect at once. A removed root stops being trusted.
// The server secret's current root is added: holding it back would gain nothing,
// since a node without a bundle trusts the server secret's ca.crt as the kubelet
// refreshes it, and it would break the cluster bus while a node lags, because
// peers present certificates renewed onto that root.
//
// A bundle nothing mounts is not held, since nodes roll onto it with both mounts
// fresh. A Secret deleted while nodes still mount it is not that case: their pods
// keep running and pick up its replacement through the mount, so it holds only
// the server root until the ACL has caught up.
//
// It also returns how many roots wait and the nodes they wait for.
func (r *ValkeyClusterReconciler) holdNewRoots(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, name string, current, bundle, serverRoot []byte) ([]byte, int, []string, error) {
	kept, held := withoutNewRoots(current, bundle, serverRoot)
	if held == 0 {
		return bundle, 0, nil, nil
	}
	inUse, err := r.trustBundleInUse(ctx, cluster, name)
	if err != nil {
		return nil, 0, nil, err
	}
	if !inUse {
		return bundle, 0, nil, nil
	}
	behind, err := r.nodesBehindAppliedACL(ctx, cluster)
	if err != nil {
		return nil, 0, nil, err
	}
	if len(behind) == 0 {
		return bundle, 0, nil, nil
	}
	return kept, held, behind, nil
}

// writeTrustBundle creates the trust bundle Secret, or updates the existing
// one read through the APIReader, so that it holds bundle and carries the
// operator's labels, with each source's last good copy under its own key. Pods
// project only ca.crt. The caller has already refused a Secret this cluster
// does not control.
func (r *ValkeyClusterReconciler) writeTrustBundle(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, existing *corev1.Secret, exists bool, bundle []byte, sources []trustSource) error {
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
	for _, src := range sources {
		secret.Data[src.key] = src.pem
	}
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

// reconcileTrustBundle writes <cluster>-tls-trust when clientAuth.ca is set and
// returns the name of the Secret nodes should reference as their trust bundle,
// or "" when they should keep the server secret's ca.crt.
//
// When a source is missing or invalid, it keeps the last good roots stored for
// it and TLSConfigured=False reports why: dropping them would silently lock out
// every client they signed. The other sources stay current, so a root removed
// from one, or an entry removed from the spec, is not trusted any longer. An
// entry whose name or key is edited counts as removed, so a typo there refuses
// its clients until it is fixed. If nothing mounts the trust Secret yet, nodes
// keep the server root, so a typo in clientAuth.ca never leaves a pod waiting on
// a volume that does not exist, and a Secret left from an earlier clientAuth.ca
// is not mistaken for last good. Only a Secret this cluster controls counts as
// written: one created by anyone else under the same name is never trusted.
//
// When clientAuth.ca is emptied, see retireTrustBundle.
func (r *ValkeyClusterReconciler) reconcileTrustBundle(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) (string, error) {
	if len(cluster.GetTLS().ClientAuthCA()) == 0 {
		meta.RemoveStatusCondition(&cluster.Status.Conditions, valkeyiov1alpha1.ConditionTLSConfigured)
		return "", r.retireTrustBundle(ctx, cluster)
	}

	name := getTrustBundleSecretName(cluster.Name)
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

	sources, broken, err := r.resolveTrustSources(ctx, cluster, existing.Data)
	if err != nil {
		// A transient read failure: retry rather than report a bad source.
		return "", err
	}
	if broken != nil {
		reason := valkeyiov1alpha1.ReasonTrustSourceInvalid
		if errors.Is(broken, errTrustSourceNotFound) {
			reason = valkeyiov1alpha1.ReasonTrustSourceNotFound
		}
		r.reportTrustSourceError(ctx, cluster, reason, strings.TrimPrefix(broken.Error(), errTrustSourceInvalid.Error()+": "))
		inUse, err := r.trustBundleInUse(ctx, cluster, name)
		if err != nil {
			return "", err
		}
		if !inUse {
			return "", nil
		}
		// Without a server root the bundle cannot be rebuilt, and nodes cannot
		// usefully roll off it either: leave things as they are.
		if len(sources) == 0 || sources[0].key != trustBundleServerKey {
			return name, nil
		}
	}

	bundle, err := mergeTrustBundle(sources)
	if err != nil {
		return "", err
	}
	toWrite, held, behind, err := r.holdNewRoots(ctx, cluster, name, existing.Data[tlsSecretKeyCA], bundle, sources[0].pem)
	if err != nil {
		return "", err
	}
	// Write based on that authoritative read rather than CreateOrUpdate, which
	// would read through the cache again.
	if err := r.writeTrustBundle(ctx, cluster, existing, exists, toWrite, sources); err != nil {
		return "", err
	}
	if broken != nil {
		return name, nil
	}
	if held > 0 {
		setCondition(cluster, valkeyiov1alpha1.ConditionTLSConfigured, valkeyiov1alpha1.ReasonTrustBundlePending,
			fmt.Sprintf("holding %d new root(s) until node(s) %s confirm the current ACL is live", held, strings.Join(behind, ", ")),
			metav1.ConditionFalse)
		return name, nil
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
