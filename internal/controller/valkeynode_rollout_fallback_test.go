/*
Copyright 2025 Valkey Contributors.

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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	vclient "github.com/valkey-io/valkey-go"
	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// rolloutTLSFixture holds a CA, a server certificate signed by it, and the
// secret the operator reads them from.
type rolloutTLSFixture struct {
	caPEM   []byte
	certPEM []byte
	keyPEM  []byte
	secret  *corev1.Secret
}

func newRolloutTLSFixture(t *testing.T) *rolloutTLSFixture {
	t.Helper()
	f := &rolloutTLSFixture{}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "rollout-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	f.caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	caCert, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serverTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "valkey-c.ns.svc.cluster.local"},
		DNSNames:     []string{"valkey-c.ns.svc.cluster.local", "valkey-c-headless.ns.svc.cluster.local"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTmpl, caCert, &serverKey.PublicKey, caKey)
	require.NoError(t, err)
	f.certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})
	serverKeyDER, err := x509.MarshalECPrivateKey(serverKey)
	require.NoError(t, err)
	f.keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: serverKeyDER})

	f.secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "c-server", Namespace: "ns"},
		Data: map[string][]byte{
			tlsSecretKeyCA:   f.caPEM,
			tlsSecretKeyCert: f.certPEM,
			tlsSecretKeyKey:  f.keyPEM,
		},
	}
	return f
}

func rolloutTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	return scheme
}

func rolloutTestReconciler(t *testing.T, objs ...*corev1.Secret) (*ValkeyNodeReconciler, *rolloutTLSFixture) {
	t.Helper()
	f := newRolloutTLSFixture(t)
	builder := fake.NewClientBuilder().WithScheme(rolloutTestScheme(t))
	if len(objs) == 0 {
		builder = builder.WithObjects(f.secret)
	} else {
		builder = builder.WithObjects(objs[0])
	}
	c := builder.Build()
	return &ValkeyNodeReconciler{Client: c, APIReader: c, Scheme: rolloutTestScheme(t)}, f
}

// rolloutDialNode returns node "c-0-0" in namespace "ns" with TLS from
// secret "c-server" and a pod IP, shaped for ClientProvider.ForNode. A nil
// clientAuth models a roll to Optional; the Required form models a node
// that has not been rolled yet.
func rolloutDialNode(clientAuth *valkeyiov1alpha1.TLSClientAuthSpec) *valkeyiov1alpha1.ValkeyNode {
	node := newTestValkeyNode("c-0-0", "ns")
	node.Status.PodIP = "10.0.0.1"
	node.Spec.TLS = &valkeyiov1alpha1.NodeTLSSpec{
		ServerName: "valkey-c.ns.svc.cluster.local",
		Certificates: valkeyiov1alpha1.NodeTLSCertificates{
			Server: valkeyiov1alpha1.NodeCertificateRef{SecretName: "c-server"},
		},
		ClientAuth: clientAuth,
	}
	return node
}

// TestGetTLSConfigWithFallback_OptionalBuildsFallback: the primary config
// under clientAuth Optional presents no certificate while the fallback
// presents the server certificate, and building the fallback leaves the
// primary untouched.
func TestGetTLSConfigWithFallback_OptionalBuildsFallback(t *testing.T) {
	_, f := rolloutTestReconciler(t)
	c := fake.NewClientBuilder().WithScheme(rolloutTestScheme(t)).WithObjects(f.secret).Build()

	primary, fallback, _, err := getTLSConfigWithFallback(context.Background(), c, "c-server", "valkey-c.ns.svc.cluster.local", "ns", false, true)
	require.NoError(t, err)
	require.NotNil(t, primary)
	assert.Empty(t, primary.Certificates, "primary config must not present a client certificate under Optional")
	require.NotNil(t, fallback, "fallback must be built under Optional")
	assert.Len(t, fallback.Certificates, 1, "fallback presents the server certificate")
	assert.Equal(t, "valkey-c.ns.svc.cluster.local", primary.ServerName)
	assert.NotSame(t, primary, fallback, "fallback must be a clone, not a mutation of the primary")
}

// TestGetTLSConfigWithFallback_RequiredNoFallback: under Required mode the
// primary already presents the certificate, so no fallback is built.
func TestGetTLSConfigWithFallback_RequiredNoFallback(t *testing.T) {
	_, f := rolloutTestReconciler(t)
	c := fake.NewClientBuilder().WithScheme(rolloutTestScheme(t)).WithObjects(f.secret).Build()

	primary, fallback, _, err := getTLSConfigWithFallback(context.Background(), c, "c-server", "valkey-c.ns.svc.cluster.local", "ns", true, false)
	require.NoError(t, err)
	require.NotNil(t, primary)
	assert.Len(t, primary.Certificates, 1, "primary presents the certificate under Required")
	assert.Nil(t, fallback)
}

// TestGetTLSConfigWithFallback_MissingCertKeepsPrimary: a secret without
// cert/key entries degrades to the primary config alone instead of failing
// the whole dial path.
func TestGetTLSConfigWithFallback_MissingCertKeepsPrimary(t *testing.T) {
	_, f := rolloutTestReconciler(t)
	broken := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "c-server", Namespace: "ns"},
		Data:       map[string][]byte{tlsSecretKeyCA: f.caPEM},
	}
	c := fake.NewClientBuilder().WithScheme(rolloutTestScheme(t)).WithObjects(broken).Build()

	primary, fallback, _, err := getTLSConfigWithFallback(context.Background(), c, "c-server", "valkey-c.ns.svc.cluster.local", "ns", false, true)
	require.NoError(t, err)
	require.NotNil(t, primary)
	assert.Nil(t, fallback, "fallback is best-effort: an unusable secret leaves the primary alone")
}

// TestForNodeClientAuthRollFallback pins the node side of the
// ClientProvider dial path: ForNode must arm the client-cert fallback
// exactly like ForCluster, so a clientAuth roll cannot stall node dials on
// pods that have not restarted yet.
func TestForNodeClientAuthRollFallback(t *testing.T) {
	ctx := context.Background()
	certRefused := errors.New("remote error: tls: certificate required")

	t.Run("Optional retries a refused node dial with the fallback", func(t *testing.T) {
		_, f := rolloutTestReconciler(t)
		c := fake.NewClientBuilder().WithScheme(rolloutTestScheme(t)).WithObjects(f.secret).Build()
		var got []vclient.ClientOption
		p := NewClientProvider(c, c, valkey.NewPool(valkey.DefaultIdleTTL, rolloutNewClientScript(&got, &stubClient{}, certRefused)))

		c1, err := p.ForNode(ctx, rolloutDialNode(nil))
		require.NoError(t, err)
		require.Len(t, got, 2, "the refused primary dial must be retried with the fallback")
		require.NotNil(t, got[0].TLSConfig)
		assert.Empty(t, got[0].TLSConfig.Certificates, "Optional-mode primary must present no client certificate")
		require.NotNil(t, got[1].TLSConfig)
		assert.Len(t, got[1].TLSConfig.Certificates, 1, "the retry must present the node server certificate")
		require.NotNil(t, c1)
	})

	t.Run("Required presents the certificate on the first dial", func(t *testing.T) {
		_, f := rolloutTestReconciler(t)
		c := fake.NewClientBuilder().WithScheme(rolloutTestScheme(t)).WithObjects(f.secret).Build()
		var got []vclient.ClientOption
		p := NewClientProvider(c, c, valkey.NewPool(valkey.DefaultIdleTTL, rolloutNewClientScript(&got, &stubClient{}, certRefused)))

		_, err := p.ForNode(ctx, rolloutDialNode(&valkeyiov1alpha1.TLSClientAuthSpec{Mode: valkeyiov1alpha1.TLSAuthClientsRequired}))
		require.NoError(t, err)
		require.Len(t, got, 1, "Required mode presents the certificate on the primary dial and needs no retry")
		require.NotNil(t, got[0].TLSConfig)
		assert.Len(t, got[0].TLSConfig.Certificates, 1)
	})

	t.Run("Optional builds primary and fallback from a single secret read", func(t *testing.T) {
		_, f := rolloutTestReconciler(t)
		c := fake.NewClientBuilder().WithScheme(rolloutTestScheme(t)).WithObjects(f.secret).Build()
		cr := &countingReader{Reader: c}
		var got []vclient.ClientOption
		p := NewClientProvider(c, cr, valkey.NewPool(valkey.DefaultIdleTTL, rolloutNewClientScript(&got, &stubClient{}, certRefused)))

		_, err := p.ForNode(ctx, rolloutDialNode(nil))
		require.NoError(t, err)
		assert.Equal(t, 1, cr.secretGets, "node dials must build primary and fallback from a single secret read")
		require.NotEmpty(t, got)
		require.NotNil(t, got[0].TLSConfig)
		assert.Empty(t, got[0].TLSConfig.Certificates)
	})
}

// TestClientCertificateRefused pins the refusal spellings that arm the
// clientAuth-roll fallback: the TLS 1.3 "certificate required" alert and
// the TLS 1.2 "handshake failure" form, and nothing else.
func TestClientCertificateRefused(t *testing.T) {
	cases := []struct {
		name string
		err  string
		want bool
	}{
		{"tls13 certificate required", "remote error: tls: certificate required", true},
		{"tls12 handshake failure", "remote error: tls: handshake failure", true},
		{"wrongpass is not a cert refusal", "WRONGPASS invalid username-password pair", false},
		{"dial refused is not a cert refusal", "dial tcp 127.0.0.1:6379: connect: connection refused", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		if got := clientCertificateRefused(errors.New(tc.err)); got != tc.want {
			t.Errorf("%s: clientCertificateRefused(%q) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
	if clientCertificateRefused(nil) {
		t.Error("nil error must not be a cert refusal")
	}
}

// TestGetTLSConfigWithFallback_FallbackReadFailureNotSilent: when the
// single secret read fails, the caller learns about it instead of
// silently losing the fallback. Before the helper owned the read, a
// failed wrapper-side read left a no-op loader in place and the helper
// returned a working primary with no fallback and no error, so dials to
// not-yet-rolled pods kept failing with nothing in the logs explaining
// why.
func TestGetTLSConfigWithFallback_FallbackReadFailureNotSilent(t *testing.T) {
	_, f := rolloutTestReconciler(t)
	missing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "nope", Namespace: "ns"},
		Data:       map[string][]byte{tlsSecretKeyCA: f.caPEM},
	}
	c := fake.NewClientBuilder().WithScheme(rolloutTestScheme(t)).WithObjects(missing).Build()

	_, _, _, err := getTLSConfigWithFallback(context.Background(), c, "c-server", "valkey-c.ns.svc.cluster.local", "ns", false, true)
	require.Error(t, err, "a failed secret read must surface, not silently drop the fallback")
}
