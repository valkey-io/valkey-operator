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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

func trustTestCluster(ca ...string) *valkeyiov1alpha1.ValkeyCluster {
	sources := make([]valkeyiov1alpha1.TrustSource, 0, len(ca))
	for _, name := range ca {
		sources = append(sources, valkeyiov1alpha1.TrustSource{SecretName: name})
	}
	return &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns", UID: "cluster-uid"},
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{
			Networking: &valkeyiov1alpha1.NetworkingSpec{
				TLS: &valkeyiov1alpha1.TLSSpec{
					Certificates: valkeyiov1alpha1.TLSCertificates{
						Server: valkeyiov1alpha1.CertificateSource{SecretName: "server-tls"},
					},
					ClientAuth: &valkeyiov1alpha1.TLSClientAuthSpec{
						Mode: valkeyiov1alpha1.TLSAuthClientsRequired,
						CA:   sources,
					},
				},
			},
		},
	}
}

func caSecret(name string, data []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Data:       map[string][]byte{tlsSecretKeyCA: data},
	}
}

func newTrustReconciler(t *testing.T, objs ...client.Object) (*ValkeyClusterReconciler, client.Client, *events.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	rec := events.NewFakeRecorder(10)
	return &ValkeyClusterReconciler{Client: c, APIReader: c, Scheme: scheme, Recorder: rec}, c, rec
}

func trustSecret(t *testing.T, c client.Client) (*corev1.Secret, bool) {
	t.Helper()
	s := &corev1.Secret{}
	err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "c-tls-trust"}, s)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	require.NoError(t, err)
	return s, true
}

func tlsConfigured(cluster *valkeyiov1alpha1.ValkeyCluster) *metav1.Condition {
	return meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionTLSConfigured)
}

// labelFilteredClient models the manager's cache, which holds only Secrets
// labelled managed-by valkey-operator: Get reports any other Secret as not
// found, while writes go to the real store.
type labelFilteredClient struct{ client.Client }

func (c labelFilteredClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if _, isSecret := obj.(*corev1.Secret); isSecret && obj.GetLabels()["app.kubernetes.io/managed-by"] != "valkey-operator" {
		return apierrors.NewNotFound(corev1.Resource("secrets"), key.Name)
	}
	return nil
}

func TestReconcileTrustBundle(t *testing.T) {
	ctx := context.Background()
	serverCA := selfSignedCAPEM(t)
	clientCA := selfSignedCAPEM(t)

	t.Run("does nothing without clientAuth.ca", func(t *testing.T) {
		cluster := trustTestCluster()
		setCondition(cluster, valkeyiov1alpha1.ConditionTLSConfigured, valkeyiov1alpha1.ReasonTrustBundleReady, "stale", metav1.ConditionTrue)
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA))

		name, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		assert.Empty(t, name)
		assert.Nil(t, tlsConfigured(cluster), "a stale condition from an earlier clientAuth.ca is removed")
		_, exists := trustSecret(t, c)
		assert.False(t, exists)
	})

	t.Run("does nothing without TLS", func(t *testing.T) {
		cluster := trustTestCluster()
		cluster.Spec.Networking = nil
		r, _, _ := newTrustReconciler(t)
		name, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		assert.Empty(t, name)
	})

	t.Run("writes the server root followed by each client root", func(t *testing.T) {
		cluster := trustTestCluster("client-ca")
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("client-ca", clientCA))

		name, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		assert.Equal(t, "c-tls-trust", name)

		s, exists := trustSecret(t, c)
		require.True(t, exists)
		assert.Equal(t, [][]byte{derOf(t, serverCA), derOf(t, clientCA)}, bundleDERs(t, s.Data[tlsSecretKeyCA]))
		assert.Equal(t, "valkey-operator", s.Labels["app.kubernetes.io/managed-by"], "the operator's cache only sees managed-by Secrets")
		require.Len(t, s.OwnerReferences, 1)
		assert.Equal(t, types.UID("cluster-uid"), s.OwnerReferences[0].UID)

		cond := tlsConfigured(cluster)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		assert.Equal(t, valkeyiov1alpha1.ReasonTrustBundleReady, cond.Reason)
	})

	t.Run("follows a rotated source", func(t *testing.T) {
		cluster := trustTestCluster("client-ca")
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("client-ca", clientCA))
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)

		rotated := selfSignedCAPEM(t)
		both := append(append([]byte{}, clientCA...), rotated...)
		require.NoError(t, c.Update(ctx, caSecret("client-ca", both)))
		_, err = r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)

		s, _ := trustSecret(t, c)
		assert.Equal(t, [][]byte{derOf(t, serverCA), derOf(t, clientCA), derOf(t, rotated)}, bundleDERs(t, s.Data[tlsSecretKeyCA]))
	})

	t.Run("writes nothing and keeps nodes on the server root when a source is missing on first write", func(t *testing.T) {
		cluster := trustTestCluster("missing-ca")
		r, c, rec := newTrustReconciler(t, caSecret("server-tls", serverCA))

		name, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		assert.Empty(t, name, "nodes must not reference a trust Secret that does not exist")
		_, exists := trustSecret(t, c)
		assert.False(t, exists)

		cond := tlsConfigured(cluster)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, valkeyiov1alpha1.ReasonTrustSourceNotFound, cond.Reason)
		assert.Contains(t, cond.Message, `"missing-ca"`)
		assert.Len(t, rec.Events, 1)

		_, err = r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		assert.Len(t, rec.Events, 1, "a lasting error is reported once, not on every reconcile")
	})

	for desc, tc := range map[string]struct {
		broken     *corev1.Secret
		wantReason string
	}{
		"deleted":         {nil, valkeyiov1alpha1.ReasonTrustSourceNotFound},
		"missing ca.crt":  {&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "client-ca", Namespace: "ns"}, Data: map[string][]byte{"other": []byte("x")}}, valkeyiov1alpha1.ReasonTrustSourceNotFound},
		"not certificate": {caSecret("client-ca", []byte("garbage")), valkeyiov1alpha1.ReasonTrustSourceInvalid},
	} {
		t.Run("keeps the last good bundle when a source is "+desc, func(t *testing.T) {
			cluster := trustTestCluster("client-ca")
			r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("client-ca", clientCA))
			_, err := r.reconcileTrustBundle(ctx, cluster)
			require.NoError(t, err)
			good, _ := trustSecret(t, c)

			if tc.broken == nil {
				require.NoError(t, c.Delete(ctx, caSecret("client-ca", nil)))
			} else {
				require.NoError(t, c.Update(ctx, tc.broken))
			}
			name, err := r.reconcileTrustBundle(ctx, cluster)
			require.NoError(t, err)
			assert.Equal(t, "c-tls-trust", name, "nodes keep the last good bundle")

			after, _ := trustSecret(t, c)
			assert.Equal(t, good.Data, after.Data, "a partial bundle is never written")
			cond := tlsConfigured(cluster)
			require.NotNil(t, cond)
			assert.Equal(t, metav1.ConditionFalse, cond.Status)
			assert.Equal(t, tc.wantReason, cond.Reason)
		})
	}

	t.Run("adds a renewed server root to the last good bundle while a source is broken", func(t *testing.T) {
		cluster := trustTestCluster("client-ca")
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("client-ca", clientCA))
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)

		require.NoError(t, c.Delete(ctx, caSecret("client-ca", nil)))
		renewed := selfSignedCAPEM(t)
		require.NoError(t, c.Update(ctx, caSecret("server-tls", renewed)))

		name, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		assert.Equal(t, "c-tls-trust", name)
		s, _ := trustSecret(t, c)
		assert.Equal(t, [][]byte{derOf(t, renewed), derOf(t, serverCA), derOf(t, clientCA)}, bundleDERs(t, s.Data[tlsSecretKeyCA]),
			"the renewed server root is added and no last-good root is dropped")
		cond := tlsConfigured(cluster)
		require.NotNil(t, cond)
		assert.Equal(t, valkeyiov1alpha1.ReasonTrustSourceNotFound, cond.Reason, "the broken source is still reported")
	})

	attackerCA := selfSignedCAPEM(t)
	squat := func(controller *metav1.OwnerReference) *corev1.Secret {
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "c-tls-trust", Namespace: "ns",
				Labels: map[string]string{"app.kubernetes.io/managed-by": "valkey-operator"}},
			Data: map[string][]byte{tlsSecretKeyCA: attackerCA},
		}
		if controller != nil {
			s.OwnerReferences = []metav1.OwnerReference{*controller}
		}
		return s
	}

	for desc, tc := range map[string]struct {
		sources []client.Object
		squat   func() *corev1.Secret
	}{
		"unowned, while a source is broken": {nil, func() *corev1.Secret { return squat(nil) }},
		"unowned, with every source good":   {[]client.Object{caSecret("client-ca", clientCA)}, func() *corev1.Secret { return squat(nil) }},
		"unlabeled, so the cache cannot see it": {[]client.Object{caSecret("client-ca", clientCA)}, func() *corev1.Secret {
			s := squat(nil)
			s.Labels = nil
			return s
		}},
	} {
		t.Run("never adopts or trusts a same-named Secret it does not control: "+desc, func(t *testing.T) {
			cluster := trustTestCluster("client-ca")
			objs := append([]client.Object{caSecret("server-tls", serverCA), tc.squat()}, tc.sources...)
			r, c, _ := newTrustReconciler(t, objs...)
			r.Client = labelFilteredClient{c} // the APIReader stays unfiltered, as in the manager

			name, err := r.reconcileTrustBundle(ctx, cluster)
			require.NoError(t, err, "a name conflict must not block the rest of the reconcile")
			assert.Empty(t, name, "nodes stay on the server root")
			s, _ := trustSecret(t, c)
			assert.Equal(t, attackerCA, s.Data[tlsSecretKeyCA], "the Secret is left untouched")
			assert.Empty(t, s.OwnerReferences, "and not adopted")
			cond := tlsConfigured(cluster)
			require.NotNil(t, cond)
			assert.Equal(t, valkeyiov1alpha1.ReasonTrustBundleConflict, cond.Reason)
			assert.Contains(t, cond.Message, "controlled by nothing")
		})
	}

	for field, mutate := range map[string]func(*valkeyiov1alpha1.ValkeyCluster){
		"certificates.server.secretName": func(c *valkeyiov1alpha1.ValkeyCluster) {
			c.Spec.Networking.TLS.Certificates.Server.SecretName = "c-tls-trust"
		},
		"clientAuth.ca[0].secretName": func(c *valkeyiov1alpha1.ValkeyCluster) {
			c.Spec.Networking.TLS.ClientAuth.CA[0].SecretName = "c-tls-trust"
		},
	} {
		t.Run("refuses "+field+" naming the trust bundle", func(t *testing.T) {
			cluster := trustTestCluster("client-ca")
			mutate(cluster)
			serverSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "c-tls-trust", Namespace: "ns"},
				Type:       corev1.SecretTypeOpaque,
				Data:       map[string][]byte{tlsSecretKeyCA: serverCA, tlsSecretKeyCert: []byte("cert"), tlsSecretKeyKey: []byte("key")},
			}
			r, c, _ := newTrustReconciler(t, serverSecret, caSecret("server-tls", serverCA), caSecret("client-ca", clientCA))

			name, err := r.reconcileTrustBundle(ctx, cluster)
			require.NoError(t, err)
			assert.Empty(t, name)
			s, _ := trustSecret(t, c)
			assert.Equal(t, []byte("key"), s.Data[tlsSecretKeyKey], "the certificate and key must survive")
			cond := tlsConfigured(cluster)
			require.NotNil(t, cond)
			assert.Equal(t, valkeyiov1alpha1.ReasonTrustBundleConflict, cond.Reason)
			assert.Contains(t, cond.Message, field)
		})
	}

	t.Run("reports a same-named Secret controlled by something else", func(t *testing.T) {
		cluster := trustTestCluster("client-ca")
		yes := true
		other := &metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "other", UID: "other-uid", Controller: &yes}
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("client-ca", clientCA), squat(other))

		name, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err, "a name conflict must not block the rest of the reconcile")
		assert.Empty(t, name)
		s, _ := trustSecret(t, c)
		assert.Equal(t, attackerCA, s.Data[tlsSecretKeyCA])
		cond := tlsConfigured(cluster)
		require.NotNil(t, cond)
		assert.Equal(t, valkeyiov1alpha1.ReasonTrustBundleConflict, cond.Reason)
	})

	// A cluster whose aclfile is at revision rev1, with one node that has
	// confirmed it and one that has not.
	aclAt := func(revision string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "internal-c-acl", Namespace: "ns"},
			Data:       map[string][]byte{aclFilename: []byte("user " + aclRevisionUser + " off resetchannels -@all #" + revision + "\n")},
		}
	}
	nodeAt := func(name, revision string) *valkeyiov1alpha1.ValkeyNode {
		return &valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{LabelCluster: "c"}},
			Status:     valkeyiov1alpha1.ValkeyNodeStatus{LiveACLRevision: revision},
		}
	}

	t.Run("holds new roots until every node confirms the current ACL", func(t *testing.T) {
		cluster := trustTestCluster("client-ca")
		behind := nodeAt("c-0-1", "rev0")
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("client-ca", clientCA),
			aclAt("rev1"), nodeAt("c-0-0", "rev1"), behind)
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err, "the first write is never held: nodes roll onto it with both mounts fresh")
		first, _ := trustSecret(t, c)

		added := selfSignedCAPEM(t)
		require.NoError(t, c.Update(ctx, caSecret("client-ca", append(append([]byte{}, clientCA...), added...))))
		name, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		assert.Equal(t, "c-tls-trust", name, "nodes keep the current bundle")
		held, _ := trustSecret(t, c)
		assert.Equal(t, first.Data, held.Data, "the new root is not written yet")
		cond := tlsConfigured(cluster)
		require.NotNil(t, cond)
		assert.Equal(t, valkeyiov1alpha1.ReasonTrustBundlePending, cond.Reason)
		assert.Contains(t, cond.Message, "c-0-1")
		assert.NotContains(t, cond.Message, "c-0-0")

		behind.Status.LiveACLRevision = "rev1"
		require.NoError(t, c.Update(ctx, behind)) // no status subresource is registered on the fake
		_, err = r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		widened, _ := trustSecret(t, c)
		assert.Equal(t, [][]byte{derOf(t, serverCA), derOf(t, clientCA), derOf(t, added)}, bundleDERs(t, widened.Data[tlsSecretKeyCA]))
		assert.Equal(t, valkeyiov1alpha1.ReasonTrustBundleReady, tlsConfigured(cluster).Reason)
	})

	t.Run("writes a bundle that only loses roots at once", func(t *testing.T) {
		cluster := trustTestCluster("client-ca")
		extra := selfSignedCAPEM(t)
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("client-ca", append(append([]byte{}, clientCA...), extra...)),
			aclAt("rev1"), nodeAt("c-0-0", "rev0"))
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)

		require.NoError(t, c.Update(ctx, caSecret("client-ca", clientCA)))
		_, err = r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		s, _ := trustSecret(t, c)
		assert.Equal(t, [][]byte{derOf(t, serverCA), derOf(t, clientCA)}, bundleDERs(t, s.Data[tlsSecretKeyCA]), "removing a root never waits")
	})

	t.Run("returns a transient read error instead of reporting a bad source", func(t *testing.T) {
		cluster := trustTestCluster("client-ca")
		r, _, _ := newTrustReconciler(t)
		r.APIReader = errorReader{err: errors.New("connection refused")}

		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.Error(t, err)
		assert.Nil(t, tlsConfigured(cluster))
	})
}

func TestReconcileTrustBundleConfigMapSources(t *testing.T) {
	ctx := context.Background()
	serverCA := selfSignedCAPEM(t)
	spireRoot := selfSignedCAPEM(t)
	pemRoot := selfSignedCAPEM(t)
	configMap := func(name, key string, data []byte) *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
			Data:       map[string]string{key: string(data)},
		}
	}
	cluster := func(sources ...valkeyiov1alpha1.TrustSource) *valkeyiov1alpha1.ValkeyCluster {
		c := trustTestCluster()
		c.Spec.Networking.TLS.ClientAuth.CA = sources
		return c
	}

	t.Run("detects a SPIFFE bundle and PEM under custom keys", func(t *testing.T) {
		c := cluster(
			valkeyiov1alpha1.TrustSource{ConfigMapName: "spire-bundle", Key: "bundle.spiffe"},
			valkeyiov1alpha1.TrustSource{ConfigMapName: "spire-pem", Key: "bundle.crt"},
		)
		r, k, _ := newTrustReconciler(t, caSecret("server-tls", serverCA),
			configMap("spire-bundle", "bundle.spiffe", spiffeBundleJSON(t, spireRoot)),
			configMap("spire-pem", "bundle.crt", pemRoot))

		name, err := r.reconcileTrustBundle(ctx, c)
		require.NoError(t, err)
		assert.Equal(t, "c-tls-trust", name)
		s, _ := trustSecret(t, k)
		assert.Equal(t, [][]byte{derOf(t, serverCA), derOf(t, spireRoot), derOf(t, pemRoot)}, bundleDERs(t, s.Data[tlsSecretKeyCA]))

		cond := tlsConfigured(c)
		require.NotNil(t, cond)
		assert.Contains(t, cond.Message, `configmap "spire-bundle" key bundle.spiffe (SPIFFE bundle)`)
		assert.Contains(t, cond.Message, `configmap "spire-pem" key bundle.crt (PEM)`)
	})

	t.Run("defaults the key to ca.crt", func(t *testing.T) {
		c := cluster(valkeyiov1alpha1.TrustSource{ConfigMapName: "pem"})
		r, k, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), configMap("pem", "ca.crt", pemRoot))
		_, err := r.reconcileTrustBundle(ctx, c)
		require.NoError(t, err)
		s, _ := trustSecret(t, k)
		assert.Equal(t, [][]byte{derOf(t, serverCA), derOf(t, pemRoot)}, bundleDERs(t, s.Data[tlsSecretKeyCA]))
	})

	for desc, tc := range map[string]struct {
		objs       []client.Object
		wantReason string
		wantMsg    string
	}{
		"a missing ConfigMap": {nil, valkeyiov1alpha1.ReasonTrustSourceNotFound, `configmap "spire-bundle" does not exist`},
		"a missing key": {[]client.Object{configMap("spire-bundle", "other", pemRoot)},
			valkeyiov1alpha1.ReasonTrustSourceNotFound, `configmap "spire-bundle" has no bundle.spiffe key`},
		"a SPIFFE bundle with no X.509 authority": {[]client.Object{configMap("spire-bundle", "bundle.spiffe", []byte(`{"keys":[{"use":"jwt-svid"}]}`))},
			valkeyiov1alpha1.ReasonTrustSourceInvalid, "no x509-svid authority"},
		"content that is neither PEM nor a bundle": {[]client.Object{configMap("spire-bundle", "bundle.spiffe", []byte("garbage"))},
			valkeyiov1alpha1.ReasonTrustSourceInvalid, "data that is not PEM at byte 0"},
	} {
		t.Run("reports "+desc, func(t *testing.T) {
			c := cluster(valkeyiov1alpha1.TrustSource{ConfigMapName: "spire-bundle", Key: "bundle.spiffe"})
			r, _, _ := newTrustReconciler(t, append([]client.Object{caSecret("server-tls", serverCA)}, tc.objs...)...)
			name, err := r.reconcileTrustBundle(ctx, c)
			require.NoError(t, err)
			assert.Empty(t, name)
			cond := tlsConfigured(c)
			require.NotNil(t, cond)
			assert.Equal(t, tc.wantReason, cond.Reason)
			assert.Contains(t, cond.Message, tc.wantMsg)
			assert.NotContains(t, cond.Message, "trust source invalid", "the sentinel prefix is not shown to users")
		})
	}
}

func TestAddedRoots(t *testing.T) {
	a, b, c := selfSignedCAPEM(t), selfSignedCAPEM(t), selfSignedCAPEM(t)
	join := func(ps ...[]byte) []byte { return bytes.Join(ps, nil) }
	assert.Equal(t, 0, addedRoots(join(a, b), join(a, b)), "unchanged")
	assert.Equal(t, 0, addedRoots(join(a, b), a), "only removed")
	assert.Equal(t, 1, addedRoots(join(a, b), join(a, c)), "one replaced counts as one added")
	assert.Equal(t, 2, addedRoots(nil, join(a, b)), "nothing before")
	assert.Equal(t, 2, addedRoots([]byte("garbage"), join(a, b)), "an unreadable current bundle counts as empty")
}

func TestWithTrustBundle(t *testing.T) {
	node := buildClusterValkeyNode(trustTestCluster("client-ca"), 0, 0)
	require.NotNil(t, node.Spec.TLS)
	assert.Nil(t, node.Spec.TLS.Certificates.TrustBundle, "buildClusterValkeyNode leaves the bundle to the reconciler")

	withTrustBundle(node, "c-tls-trust")
	require.NotNil(t, node.Spec.TLS.Certificates.TrustBundle)
	assert.Equal(t, "c-tls-trust", node.Spec.TLS.Certificates.TrustBundle.SecretName)

	withTrustBundle(node, "")
	assert.Nil(t, node.Spec.TLS.Certificates.TrustBundle)

	plain := buildClusterValkeyNode(&valkeyiov1alpha1.ValkeyCluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"}}, 0, 0)
	withTrustBundle(plain, "c-tls-trust")
	assert.Nil(t, plain.Spec.TLS, "a non-TLS node is left alone")
}
