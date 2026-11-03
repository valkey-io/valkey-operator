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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	require.NoError(t, appsv1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	rec := events.NewFakeRecorder(10)
	return &ValkeyClusterReconciler{Client: c, APIReader: c, Scheme: scheme, Recorder: rec}, c, rec
}

// nodeOnTrustBundle is a ValkeyNode of cluster c that references c's trust bundle.
func nodeOnTrustBundle(name string) *valkeyiov1alpha1.ValkeyNode {
	return &valkeyiov1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{LabelCluster: "c"}},
		Spec: valkeyiov1alpha1.ValkeyNodeSpec{TLS: &valkeyiov1alpha1.NodeTLSSpec{Certificates: valkeyiov1alpha1.NodeTLSCertificates{
			TrustBundle: &valkeyiov1alpha1.NodeTrustBundleRef{SecretName: "c-tls-trust"},
		}}},
	}
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

func TestRetireTrustBundle(t *testing.T) {
	ctx := context.Background()
	serverCA := selfSignedCAPEM(t)
	clientCA := selfSignedCAPEM(t)
	projected := []corev1.Volume{{Name: "tls", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
		Sources: []corev1.VolumeProjection{{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "c-tls-trust"}}}},
	}}}}
	rolledNode := func(workloadType valkeyiov1alpha1.WorkloadType) *valkeyiov1alpha1.ValkeyNode {
		return &valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{Name: "c-0-0", Namespace: "ns", Labels: map[string]string{LabelCluster: "c"}},
			Spec:       valkeyiov1alpha1.ValkeyNodeSpec{WorkloadType: workloadType},
		}
	}
	podWith := func(phase corev1.PodPhase, volumes []corev1.Volume) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", Labels: map[string]string{LabelCluster: "c"}},
			Spec:       corev1.PodSpec{Volumes: volumes},
			Status:     corev1.PodStatus{Phase: phase},
		}
	}
	template := corev1.PodTemplateSpec{Spec: corev1.PodSpec{Volumes: projected}}

	for desc, tc := range map[string]struct {
		objs []client.Object
		kept bool
	}{
		"nothing is left using it":   {objs: []client.Object{rolledNode(valkeyiov1alpha1.WorkloadTypeStatefulSet)}},
		"a node still references it": {objs: []client.Object{nodeOnTrustBundle("c-0-0")}, kept: true},
		"a StatefulSet template still mounts it": {kept: true, objs: []client.Object{
			rolledNode(valkeyiov1alpha1.WorkloadTypeStatefulSet),
			&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "valkey-c-0-0", Namespace: "ns"}, Spec: appsv1.StatefulSetSpec{Template: template}},
		}},
		"a Deployment template still mounts it": {kept: true, objs: []client.Object{
			rolledNode(valkeyiov1alpha1.WorkloadTypeDeployment),
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "valkey-c-0-0", Namespace: "ns"}, Spec: appsv1.DeploymentSpec{Template: template}},
		}},
		"a running pod still mounts it": {objs: []client.Object{podWith(corev1.PodRunning, projected)}, kept: true},
		"a pending pod still mounts it": {objs: []client.Object{podWith(corev1.PodPending, projected)}, kept: true},
		"only a finished pod mounts it": {objs: []client.Object{podWith(corev1.PodFailed, projected)}},
		"a running pod mounts it directly": {kept: true, objs: []client.Object{podWith(corev1.PodRunning, []corev1.Volume{
			{Name: "v", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "c-tls-trust"}}},
		})}},
	} {
		t.Run(desc, func(t *testing.T) {
			cluster := trustTestCluster("client-ca")
			r, c, _ := newTrustReconciler(t, append(tc.objs, caSecret("server-tls", serverCA), caSecret("client-ca", clientCA))...)
			_, err := r.reconcileTrustBundle(ctx, cluster)
			require.NoError(t, err)

			cluster.Spec.Networking.TLS.ClientAuth.CA = nil
			name, err := r.reconcileTrustBundle(ctx, cluster)
			require.NoError(t, err)
			assert.Empty(t, name)
			s, exists := trustSecret(t, c)
			assert.Equal(t, tc.kept, exists)
			if tc.kept {
				assert.Equal(t, [][]byte{derOf(t, serverCA)}, bundleDERs(t, s.Data[tlsSecretKeyCA]), "only the server root is trusted while nodes roll")
				assert.Len(t, s.Data, 2, "ca.crt and the server root's copy")
			}
		})
	}

	t.Run("writes a renewed server root while nodes roll off the bundle", func(t *testing.T) {
		cluster := trustTestCluster("client-ca")
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("client-ca", clientCA), nodeOnTrustBundle("c-0-0"))
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)

		renewed := selfSignedCAPEM(t)
		require.NoError(t, c.Update(ctx, caSecret("server-tls", renewed)))
		cluster.Spec.Networking.TLS.ClientAuth.CA = nil
		_, err = r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		s, _ := trustSecret(t, c)
		assert.Equal(t, [][]byte{derOf(t, renewed)}, bundleDERs(t, s.Data[tlsSecretKeyCA]))
	})

	t.Run("keeps the server root's copy while nodes roll off and the server secret is broken", func(t *testing.T) {
		cluster := trustTestCluster("client-ca")
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("client-ca", clientCA), nodeOnTrustBundle("c-0-0"))
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)

		require.NoError(t, c.Delete(ctx, caSecret("server-tls", nil)))
		cluster.Spec.Networking.TLS.ClientAuth.CA = nil
		_, err = r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		s, _ := trustSecret(t, c)
		assert.Equal(t, [][]byte{derOf(t, serverCA)}, bundleDERs(t, s.Data[tlsSecretKeyCA]))
	})

	t.Run("leaves the bundle as it is when TLS is removed while nodes roll off it", func(t *testing.T) {
		cluster := trustTestCluster("client-ca")
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("client-ca", clientCA), nodeOnTrustBundle("c-0-0"))
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		before, _ := trustSecret(t, c)

		cluster.Spec.Networking = nil
		_, err = r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		after, ok := trustSecret(t, c)
		require.True(t, ok)
		assert.Equal(t, before.Data, after.Data)
	})

	t.Run("leaves a Secret of that name the cluster does not control", func(t *testing.T) {
		theirs := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "c-tls-trust", Namespace: "ns"}}
		r, c, _ := newTrustReconciler(t, theirs)
		_, err := r.reconcileTrustBundle(ctx, trustTestCluster())
		require.NoError(t, err)
		_, exists := trustSecret(t, c)
		assert.True(t, exists)
	})
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
			r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("client-ca", clientCA), nodeOnTrustBundle("c-0-0"))
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

	t.Run("does not fall back on a bundle no node references", func(t *testing.T) {
		cluster := trustTestCluster("old-ca")
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("old-ca", clientCA))
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		left, _ := trustSecret(t, c)

		cluster.Spec.Networking.TLS.ClientAuth.CA = []valkeyiov1alpha1.TrustSource{{SecretName: "missing-ca"}}
		name, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		assert.Empty(t, name, "nodes must not start trusting the roots of an earlier clientAuth.ca")
		after, _ := trustSecret(t, c)
		assert.Equal(t, left.Data, after.Data)
		cond := tlsConfigured(cluster)
		require.NotNil(t, cond)
		assert.Equal(t, valkeyiov1alpha1.ReasonTrustSourceNotFound, cond.Reason)
	})

	aclAt := func(revision string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "internal-c-acl", Namespace: "ns"},
			Data:       map[string][]byte{aclFilename: []byte("user " + aclRevisionUser + " off resetchannels -@all #" + revision + "\n")},
		}
	}
	otherCA := selfSignedCAPEM(t)
	entries := func(names ...string) []valkeyiov1alpha1.TrustSource {
		out := make([]valkeyiov1alpha1.TrustSource, 0, len(names))
		for _, n := range names {
			out = append(out, valkeyiov1alpha1.TrustSource{SecretName: n})
		}
		return out
	}
	// brokenB starts from a bundle holding ca-a and ca-b, on a mounted node, with ca-b then deleted.
	brokenB := func(t *testing.T) (*ValkeyClusterReconciler, client.Client, *valkeyiov1alpha1.ValkeyCluster) {
		t.Helper()
		cluster := trustTestCluster("ca-a", "ca-b")
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("ca-a", clientCA), caSecret("ca-b", otherCA), nodeOnTrustBundle("c-0-0"))
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		require.NoError(t, c.Delete(ctx, caSecret("ca-b", nil)))
		return r, c, cluster
	}
	roots := func(t *testing.T, c client.Client) [][]byte {
		t.Helper()
		s, ok := trustSecret(t, c)
		require.True(t, ok)
		return bundleDERs(t, s.Data[tlsSecretKeyCA])
	}

	t.Run("revokes an entry removed from the spec while another source is broken", func(t *testing.T) {
		r, c, cluster := brokenB(t)
		cluster.Spec.Networking.TLS.ClientAuth.CA = entries("ca-b")
		name, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		assert.Equal(t, "c-tls-trust", name)
		assert.Equal(t, [][]byte{derOf(t, serverCA), derOf(t, otherCA)}, roots(t, c))
		assert.Equal(t, valkeyiov1alpha1.ReasonTrustSourceNotFound, tlsConfigured(cluster).Reason)
	})

	t.Run("drops a broken entry once it leaves the spec", func(t *testing.T) {
		r, c, cluster := brokenB(t)
		cluster.Spec.Networking.TLS.ClientAuth.CA = entries("ca-a")
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		assert.Equal(t, [][]byte{derOf(t, serverCA), derOf(t, clientCA)}, roots(t, c))
		s, _ := trustSecret(t, c)
		assert.NotContains(t, s.Data, trustSourceKey(valkeyiov1alpha1.TrustSource{SecretName: "ca-b"}), "its copy goes with it")
		assert.Equal(t, valkeyiov1alpha1.ReasonTrustBundleReady, tlsConfigured(cluster).Reason)
	})

	t.Run("revokes a root removed from a readable source while another is broken", func(t *testing.T) {
		cluster := trustTestCluster("ca-a", "ca-b")
		revoked := selfSignedCAPEM(t)
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("ca-a", append(append([]byte{}, clientCA...), revoked...)), caSecret("ca-b", otherCA), nodeOnTrustBundle("c-0-0"))
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)

		require.NoError(t, c.Delete(ctx, caSecret("ca-b", nil)))
		require.NoError(t, c.Update(ctx, caSecret("ca-a", clientCA)))
		_, err = r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		assert.Equal(t, [][]byte{derOf(t, serverCA), derOf(t, clientCA), derOf(t, otherCA)}, roots(t, c))
	})

	t.Run("follows a readable source's new root while another is broken", func(t *testing.T) {
		r, c, cluster := brokenB(t)
		added := selfSignedCAPEM(t)
		require.NoError(t, c.Update(ctx, caSecret("ca-a", append(append([]byte{}, clientCA...), added...))))
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		assert.Equal(t, [][]byte{derOf(t, serverCA), derOf(t, clientCA), derOf(t, added), derOf(t, otherCA)}, roots(t, c))
	})

	t.Run("holds that new root while a node is behind, and reports the broken source", func(t *testing.T) {
		cluster := trustTestCluster("ca-a", "ca-b")
		node := nodeOnTrustBundle("c-0-0")
		node.Status.AppliedACLRevision = "rev1"
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("ca-a", clientCA), caSecret("ca-b", otherCA), aclAt("rev1"), node)
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)

		node.Status.AppliedACLRevision = "rev0"
		require.NoError(t, c.Update(ctx, node)) // no status subresource is registered on the fake
		require.NoError(t, c.Delete(ctx, caSecret("ca-b", nil)))
		require.NoError(t, c.Update(ctx, caSecret("ca-a", append(append([]byte{}, clientCA...), selfSignedCAPEM(t)...))))
		_, err = r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		assert.Equal(t, [][]byte{derOf(t, serverCA), derOf(t, clientCA), derOf(t, otherCA)}, roots(t, c))
		assert.Equal(t, valkeyiov1alpha1.ReasonTrustSourceNotFound, tlsConfigured(cluster).Reason)
	})

	t.Run("keeps a root moved to another entry while a source is broken", func(t *testing.T) {
		r, c, cluster := brokenB(t)
		require.NoError(t, c.Create(ctx, caSecret("ca-moved", clientCA)))
		cluster.Spec.Networking.TLS.ClientAuth.CA = entries("ca-moved", "ca-b")
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		assert.Equal(t, [][]byte{derOf(t, serverCA), derOf(t, clientCA), derOf(t, otherCA)}, roots(t, c))
	})

	t.Run("drops the roots of an entry edited to a name that does not exist", func(t *testing.T) {
		cluster := trustTestCluster("ca-a")
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("ca-a", clientCA), nodeOnTrustBundle("c-0-0"))
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)

		cluster.Spec.Networking.TLS.ClientAuth.CA = entries("ca-typo")
		name, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		assert.Equal(t, "c-tls-trust", name)
		assert.Equal(t, [][]byte{derOf(t, serverCA)}, roots(t, c))
		cond := tlsConfigured(cluster)
		require.NotNil(t, cond)
		assert.Equal(t, valkeyiov1alpha1.ReasonTrustSourceNotFound, cond.Reason)
		assert.Contains(t, cond.Message, "ca-typo")
	})

	t.Run("keeps the server root when the server secret is renamed to one that cannot be read", func(t *testing.T) {
		cluster := trustTestCluster("ca-a")
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("ca-a", clientCA), nodeOnTrustBundle("c-0-0"))
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)

		cluster.Spec.Networking.TLS.Certificates.Server.SecretName = "server-tls-next"
		_, err = r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		assert.Equal(t, [][]byte{derOf(t, serverCA), derOf(t, clientCA)}, roots(t, c))
		assert.Equal(t, valkeyiov1alpha1.ReasonTrustSourceNotFound, tlsConfigured(cluster).Reason)
	})

	t.Run("recreates a bundle deleted while nodes mount it and a source is broken", func(t *testing.T) {
		cluster := trustTestCluster("ca-a")
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), nodeOnTrustBundle("c-0-0"))
		name, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		assert.Equal(t, "c-tls-trust", name, "nodes must not roll off the bundle because of this")
		assert.Equal(t, [][]byte{derOf(t, serverCA)}, roots(t, c))
		assert.Equal(t, valkeyiov1alpha1.ReasonTrustSourceNotFound, tlsConfigured(cluster).Reason)
	})

	t.Run("keeps nodes on the bundle when it cannot be rebuilt without a server root", func(t *testing.T) {
		cluster := trustTestCluster("ca-a")
		r, c, _ := newTrustReconciler(t, caSecret("ca-a", clientCA), nodeOnTrustBundle("c-0-0"))
		name, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		assert.Equal(t, "c-tls-trust", name)
		_, exists := trustSecret(t, c)
		assert.False(t, exists)
	})

	t.Run("treats a damaged copy as no copy", func(t *testing.T) {
		r, c, cluster := brokenB(t)
		s, _ := trustSecret(t, c)
		s.Data[trustSourceKey(valkeyiov1alpha1.TrustSource{SecretName: "ca-b"})] = []byte("garbage")
		require.NoError(t, c.Update(ctx, s))
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		assert.Equal(t, [][]byte{derOf(t, serverCA), derOf(t, clientCA)}, roots(t, c))
	})

	t.Run("replaces the server root with a renewed one while a source is broken", func(t *testing.T) {
		cluster := trustTestCluster("client-ca")
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("client-ca", clientCA), nodeOnTrustBundle("c-0-0"))
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)

		require.NoError(t, c.Delete(ctx, caSecret("client-ca", nil)))
		renewed := selfSignedCAPEM(t)
		require.NoError(t, c.Update(ctx, caSecret("server-tls", renewed)))

		name, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		assert.Equal(t, "c-tls-trust", name)
		s, _ := trustSecret(t, c)
		assert.Equal(t, [][]byte{derOf(t, renewed), derOf(t, clientCA)}, bundleDERs(t, s.Data[tlsSecretKeyCA]),
			"the broken source keeps its last good root and the old server root goes, as on a healthy reconcile")
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
		t.Run("leaves the Secret alone when "+field+" names the trust bundle", func(t *testing.T) {
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
			assert.Equal(t, valkeyiov1alpha1.ReasonTrustBundleConflict, cond.Reason, "a Secret the cluster does not control is refused, should admission have been bypassed")
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
	nodeAt := func(name, revision string) *valkeyiov1alpha1.ValkeyNode {
		return &valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{LabelCluster: "c"}},
			Status:     valkeyiov1alpha1.ValkeyNodeStatus{AppliedACLRevision: revision},
		}
	}

	// mountBundle moves a node onto the bundle, as the rolls after its first write do.
	mountBundle := func(t *testing.T, c client.Client, node *valkeyiov1alpha1.ValkeyNode) {
		t.Helper()
		node.Spec.TLS = nodeOnTrustBundle(node.Name).Spec.TLS
		require.NoError(t, c.Update(ctx, node))
	}

	t.Run("holds new roots until every node confirms the current ACL", func(t *testing.T) {
		cluster := trustTestCluster("client-ca")
		behind := nodeAt("c-0-1", "rev0")
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("client-ca", clientCA),
			aclAt("rev1"), nodeAt("c-0-0", "rev1"), behind)
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err, "a bundle nothing mounts is never held: nodes roll onto it with both mounts fresh")
		first, _ := trustSecret(t, c)
		assert.Equal(t, [][]byte{derOf(t, serverCA), derOf(t, clientCA)}, bundleDERs(t, first.Data[tlsSecretKeyCA]))
		mountBundle(t, c, behind)

		added := selfSignedCAPEM(t)
		require.NoError(t, c.Update(ctx, caSecret("client-ca", append(append([]byte{}, clientCA...), added...))))
		name, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		assert.Equal(t, "c-tls-trust", name, "nodes keep the current bundle")
		held, _ := trustSecret(t, c)
		assert.Equal(t, first.Data[tlsSecretKeyCA], held.Data[tlsSecretKeyCA], "the new root is not written yet")
		cond := tlsConfigured(cluster)
		require.NotNil(t, cond)
		assert.Equal(t, valkeyiov1alpha1.ReasonTrustBundlePending, cond.Reason)
		assert.Contains(t, cond.Message, "c-0-1")
		assert.NotContains(t, cond.Message, "c-0-0")

		behind.Status.AppliedACLRevision = "rev1"
		require.NoError(t, c.Update(ctx, behind)) // no status subresource is registered on the fake
		_, err = r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		widened, _ := trustSecret(t, c)
		assert.Equal(t, [][]byte{derOf(t, serverCA), derOf(t, clientCA), derOf(t, added)}, bundleDERs(t, widened.Data[tlsSecretKeyCA]))
		assert.Equal(t, valkeyiov1alpha1.ReasonTrustBundleReady, tlsConfigured(cluster).Reason)
	})

	t.Run("does not hold new roots for a bundle nothing mounts", func(t *testing.T) {
		cluster := trustTestCluster("client-ca")
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("client-ca", clientCA),
			aclAt("rev1"), nodeAt("c-0-0", "rev0"))
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)

		added := selfSignedCAPEM(t)
		require.NoError(t, c.Update(ctx, caSecret("client-ca", append(append([]byte{}, clientCA...), added...))))
		_, err = r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		s, _ := trustSecret(t, c)
		assert.Equal(t, [][]byte{derOf(t, serverCA), derOf(t, clientCA), derOf(t, added)}, bundleDERs(t, s.Data[tlsSecretKeyCA]))
		assert.Equal(t, valkeyiov1alpha1.ReasonTrustBundleReady, tlsConfigured(cluster).Reason)
	})

	t.Run("writes a renewed server root at once while a node is behind", func(t *testing.T) {
		cluster := trustTestCluster("client-ca")
		behind := nodeAt("c-0-0", "rev0")
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("client-ca", clientCA), aclAt("rev1"), behind)
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		mountBundle(t, c, behind)

		renewed := selfSignedCAPEM(t)
		require.NoError(t, c.Update(ctx, caSecret("server-tls", renewed)))
		_, err = r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		s, _ := trustSecret(t, c)
		assert.Equal(t, [][]byte{derOf(t, renewed), derOf(t, clientCA)}, bundleDERs(t, s.Data[tlsSecretKeyCA]),
			"peers present certificates from the renewed root, so it is never held back")
		assert.Equal(t, valkeyiov1alpha1.ReasonTrustBundleReady, tlsConfigured(cluster).Reason)

		added := selfSignedCAPEM(t)
		newer := selfSignedCAPEM(t)
		require.NoError(t, c.Update(ctx, caSecret("server-tls", newer)))
		require.NoError(t, c.Update(ctx, caSecret("client-ca", append(append([]byte{}, clientCA...), added...))))
		_, err = r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		held, _ := trustSecret(t, c)
		assert.Equal(t, [][]byte{derOf(t, newer), derOf(t, clientCA)}, bundleDERs(t, held.Data[tlsSecretKeyCA]),
			"the renewed root is written while a client root arriving with it waits")
		assert.Equal(t, valkeyiov1alpha1.ReasonTrustBundlePending, tlsConfigured(cluster).Reason)
	})

	t.Run("drops a removed root at once while a root added with it waits", func(t *testing.T) {
		cluster := trustTestCluster("client-ca")
		revoked := selfSignedCAPEM(t)
		behind := nodeAt("c-0-0", "rev0")
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("client-ca", append(append([]byte{}, clientCA...), revoked...)), aclAt("rev1"), behind)
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		mountBundle(t, c, behind)

		added := selfSignedCAPEM(t)
		require.NoError(t, c.Update(ctx, caSecret("client-ca", append(append([]byte{}, clientCA...), added...))))
		_, err = r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		s, _ := trustSecret(t, c)
		assert.Equal(t, [][]byte{derOf(t, serverCA), derOf(t, clientCA)}, bundleDERs(t, s.Data[tlsSecretKeyCA]))
		assert.Equal(t, valkeyiov1alpha1.ReasonTrustBundlePending, tlsConfigured(cluster).Reason)
	})

	t.Run("holds client roots when the bundle is deleted while nodes mount it", func(t *testing.T) {
		cluster := trustTestCluster("client-ca")
		mounting := nodeOnTrustBundle("c-0-0")
		mounting.Status.AppliedACLRevision = "rev0"
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("client-ca", clientCA), aclAt("rev1"), mounting)
		name, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		assert.Equal(t, "c-tls-trust", name)
		s, ok := trustSecret(t, c)
		require.True(t, ok, "pods mount it, so it has to exist")
		assert.Equal(t, [][]byte{derOf(t, serverCA)}, bundleDERs(t, s.Data[tlsSecretKeyCA]))
		assert.Equal(t, valkeyiov1alpha1.ReasonTrustBundlePending, tlsConfigured(cluster).Reason)

		mounting.Status.AppliedACLRevision = "rev1"
		require.NoError(t, c.Update(ctx, mounting)) // no status subresource is registered on the fake
		_, err = r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		s, _ = trustSecret(t, c)
		assert.Equal(t, [][]byte{derOf(t, serverCA), derOf(t, clientCA)}, bundleDERs(t, s.Data[tlsSecretKeyCA]))
		assert.Equal(t, valkeyiov1alpha1.ReasonTrustBundleReady, tlsConfigured(cluster).Reason)
	})

	t.Run("writes a renewed server root while a source is broken and a node is behind", func(t *testing.T) {
		cluster := trustTestCluster("client-ca")
		node := nodeOnTrustBundle("c-0-0")
		node.Status.AppliedACLRevision = "rev1"
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("client-ca", clientCA), aclAt("rev1"), node)
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)

		node.Status.AppliedACLRevision = "rev0"
		require.NoError(t, c.Update(ctx, node)) // no status subresource is registered on the fake
		require.NoError(t, c.Delete(ctx, caSecret("client-ca", nil)))
		renewed := selfSignedCAPEM(t)
		require.NoError(t, c.Update(ctx, caSecret("server-tls", renewed)))
		_, err = r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		s, _ := trustSecret(t, c)
		assert.Equal(t, [][]byte{derOf(t, renewed), derOf(t, clientCA)}, bundleDERs(t, s.Data[tlsSecretKeyCA]))
	})

	t.Run("writes a bundle that only loses roots at once", func(t *testing.T) {
		cluster := trustTestCluster("client-ca")
		extra := selfSignedCAPEM(t)
		behind := nodeAt("c-0-0", "rev0")
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("client-ca", append(append([]byte{}, clientCA...), extra...)), aclAt("rev1"), behind)
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		mountBundle(t, c, behind)

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

	t.Run("leaves the bundle and the condition alone when reading a source fails transiently", func(t *testing.T) {
		cluster := trustTestCluster("client-ca")
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("client-ca", clientCA), nodeOnTrustBundle("c-0-0"))
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		before, _ := trustSecret(t, c)

		r.APIReader = sourceErrorReader{Reader: c, name: "client-ca", err: errors.New("connection refused")}
		_, err = r.reconcileTrustBundle(ctx, cluster)
		require.Error(t, err)
		after, _ := trustSecret(t, c)
		assert.Equal(t, before.Data, after.Data)
		assert.Equal(t, valkeyiov1alpha1.ReasonTrustBundleReady, tlsConfigured(cluster).Reason)
	})
}

// refusingClient fails every write with err and passes reads through.
type refusingClient struct {
	client.Client
	err error
}

func (c refusingClient) Create(context.Context, client.Object, ...client.CreateOption) error {
	return c.err
}

func (c refusingClient) Update(context.Context, client.Object, ...client.UpdateOption) error {
	return c.err
}

func (c refusingClient) Delete(context.Context, client.Object, ...client.DeleteOption) error {
	return c.err
}

func TestTrustBundleWriteRefused(t *testing.T) {
	ctx := context.Background()
	serverCA := selfSignedCAPEM(t)
	clientCA := selfSignedCAPEM(t)
	otherCA := selfSignedCAPEM(t)
	secrets := schema.GroupResource{Resource: "secrets"}
	refusals := map[string]error{
		"invalid":   apierrors.NewInvalid(schema.GroupKind{Kind: "Secret"}, "c-tls-trust", nil),
		"too large": apierrors.NewRequestEntityTooLargeError("too large"),
		"forbidden": apierrors.NewForbidden(secrets, "c-tls-trust", errors.New("denied by policy")),
	}

	for desc, refusal := range refusals {
		t.Run("keeps the bundle in use and carries on when "+desc, func(t *testing.T) {
			cluster := trustTestCluster("client-ca")
			r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("client-ca", clientCA), nodeOnTrustBundle("c-0-0"))
			_, err := r.reconcileTrustBundle(ctx, cluster)
			require.NoError(t, err)
			before, _ := trustSecret(t, c)

			require.NoError(t, c.Update(ctx, caSecret("client-ca", append(append([]byte{}, clientCA...), otherCA...))))
			r.Client = refusingClient{Client: c, err: refusal}
			name, err := r.reconcileTrustBundle(ctx, cluster)
			require.NoError(t, err, "a refusal must not stop the reconcile")
			assert.Equal(t, "c-tls-trust", name)
			after, _ := trustSecret(t, c)
			assert.Equal(t, before.Data, after.Data)
			cond := tlsConfigured(cluster)
			require.NotNil(t, cond)
			assert.Equal(t, metav1.ConditionFalse, cond.Status)
			assert.Equal(t, valkeyiov1alpha1.ReasonTrustBundleRejected, cond.Reason)
		})
	}

	t.Run("keeps nodes on the server root when nothing mounts the bundle", func(t *testing.T) {
		cluster := trustTestCluster("client-ca")
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("client-ca", clientCA))
		r.Client = refusingClient{Client: c, err: refusals["invalid"]}
		name, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)
		assert.Empty(t, name)
		assert.Equal(t, valkeyiov1alpha1.ReasonTrustBundleRejected, tlsConfigured(cluster).Reason)
	})

	t.Run("keeps reporting a broken source, with one event", func(t *testing.T) {
		cluster := trustTestCluster("ca-a", "ca-b")
		r, c, rec := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("ca-a", clientCA), caSecret("ca-b", otherCA), nodeOnTrustBundle("c-0-0"))
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.NoError(t, err)

		require.NoError(t, c.Delete(ctx, caSecret("ca-b", nil)))
		require.NoError(t, c.Update(ctx, caSecret("ca-a", append(append([]byte{}, clientCA...), selfSignedCAPEM(t)...))))
		r.Client = refusingClient{Client: c, err: refusals["invalid"]}
		for range 2 {
			_, err = r.reconcileTrustBundle(ctx, cluster)
			require.NoError(t, err)
		}
		assert.Equal(t, valkeyiov1alpha1.ReasonTrustSourceNotFound, tlsConfigured(cluster).Reason)
		assert.Len(t, rec.Events, 1)
	})

	t.Run("returns any other write failure so it is retried", func(t *testing.T) {
		cluster := trustTestCluster("client-ca")
		r, c, _ := newTrustReconciler(t, caSecret("server-tls", serverCA), caSecret("client-ca", clientCA))
		r.Client = refusingClient{Client: c, err: apierrors.NewConflict(secrets, "c-tls-trust", errors.New("modified"))}
		_, err := r.reconcileTrustBundle(ctx, cluster)
		require.Error(t, err)
	})

	for desc, nodes := range map[string][]client.Object{
		"rewriting": {nodeOnTrustBundle("c-0-0")},
		"deleting":  nil,
	} {
		t.Run("reports a failure "+desc+" a retired bundle and carries on", func(t *testing.T) {
			cluster := trustTestCluster("client-ca")
			r, c, rec := newTrustReconciler(t, append(nodes, caSecret("server-tls", serverCA), caSecret("client-ca", clientCA))...)
			_, err := r.reconcileTrustBundle(ctx, cluster)
			require.NoError(t, err)
			before, _ := trustSecret(t, c)

			cluster.Spec.Networking.TLS.ClientAuth.CA = nil
			r.Client = refusingClient{Client: c, err: errors.New("connection refused")}
			name, err := r.reconcileTrustBundle(ctx, cluster)
			require.NoError(t, err)
			assert.Empty(t, name, "nodes still roll off it")
			after, ok := trustSecret(t, c)
			require.True(t, ok)
			assert.Equal(t, before.Data, after.Data)
			require.Len(t, rec.Events, 1)
			assert.Contains(t, <-rec.Events, "TrustBundleRetireFailed")
		})
	}
}

// sourceErrorReader fails reads of one object and passes the rest through.
type sourceErrorReader struct {
	client.Reader
	name string
	err  error
}

func (r sourceErrorReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if key.Name == r.name {
		return r.err
	}
	return r.Reader.Get(ctx, key, obj, opts...)
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
		"content with no certificate that is not a bundle": {[]client.Object{configMap("spire-bundle", "bundle.spiffe", []byte("garbage"))},
			valkeyiov1alpha1.ReasonTrustSourceInvalid, `configmap "spire-bundle" key bundle.spiffe: no certificates`},
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

func TestWithoutNewRoots(t *testing.T) {
	a, b, c := selfSignedCAPEM(t), selfSignedCAPEM(t), selfSignedCAPEM(t)
	join := func(ps ...[]byte) []byte { return bytes.Join(ps, nil) }
	for _, tc := range []struct {
		desc                  string
		current, next, exempt []byte
		kept                  []byte
		left                  int
	}{
		{desc: "unchanged", current: join(a, b), next: join(a, b), kept: join(a, b)},
		{desc: "only removed", current: join(a, b), next: a, kept: a},
		{desc: "one replaced leaves out the new one and drops the old", current: join(a, b), next: join(a, c), kept: a, left: 1},
		{desc: "nothing before", next: join(a, b), left: 2},
		{desc: "an unreadable current bundle counts as empty", current: []byte("garbage"), next: join(a, b), left: 2},
		{desc: "an exempt root is kept", current: a, next: join(c, a), exempt: c, kept: join(c, a)},
		{desc: "the rest are still left out", current: a, next: join(c, a, b), exempt: c, kept: join(c, a), left: 1},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			kept, left := withoutNewRoots(tc.current, tc.next, tc.exempt)
			assert.Equal(t, string(tc.kept), string(kept))
			assert.Equal(t, tc.left, left)
		})
	}
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
