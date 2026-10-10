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
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
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

func rolloutTestNode() *valkeyiov1alpha1.ValkeyNode {
	node := newTestValkeyNode("c-0-0", "ns")
	node.Labels = map[string]string{LabelCluster: "c"}
	node.Spec.TLS = &valkeyiov1alpha1.NodeTLSSpec{
		Certificates: valkeyiov1alpha1.NodeTLSCertificates{
			Server: valkeyiov1alpha1.NodeCertificateRef{SecretName: "c-server"},
		},
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

	primary, fallback, err := getTLSConfigWithFallback(context.Background(), c, "c-server", "valkey-c.ns.svc.cluster.local", "ns", false, loadClientCertificateFallback(f.secret))
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

	primary, fallback, err := getTLSConfigWithFallback(context.Background(), c, "c-server", "valkey-c.ns.svc.cluster.local", "ns", true, nil)
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

	primary, fallback, err := getTLSConfigWithFallback(context.Background(), c, "c-server", "valkey-c.ns.svc.cluster.local", "ns", false, loadClientCertificateFallback(broken))
	require.NoError(t, err)
	require.NotNil(t, primary)
	assert.Nil(t, fallback, "fallback is best-effort: an unusable secret leaves the primary alone")
}

// TestBuildNodeClientOptionWithFallback_Optional: the node-controller side
// of the pair arms the fallback when clientAuth is Optional.
func TestBuildNodeClientOptionWithFallback_Optional(t *testing.T) {
	r, _ := rolloutTestReconciler(t)
	node := rolloutTestNode()

	opt, fallback := r.buildNodeClientOptionWithFallback(context.Background(), node)
	require.NotNil(t, opt.TLSConfig)
	assert.Empty(t, opt.TLSConfig.Certificates)
	require.NotNil(t, fallback, "Optional mode must arm the fallback")
	assert.Len(t, fallback.TLSConfig.Certificates, 1)
}

// TestBuildNodeClientOptionWithFallback_Required: Required mode presents
// the certificate on the primary and arms no fallback.
func TestBuildNodeClientOptionWithFallback_Required(t *testing.T) {
	r, _ := rolloutTestReconciler(t)
	node := rolloutTestNode()
	node.Spec.TLS.ClientAuth = &valkeyiov1alpha1.TLSClientAuthSpec{Mode: valkeyiov1alpha1.TLSAuthClientsRequired}

	opt, fallback := r.buildNodeClientOptionWithFallback(context.Background(), node)
	require.NotNil(t, opt.TLSConfig)
	assert.Len(t, opt.TLSConfig.Certificates, 1, "Required mode presents the certificate on the primary")
	assert.Nil(t, fallback)
}

// TestGetTLSConfigForScrape_OptionalBuildsFallback: the cluster-scrape side
// of the pair arms the fallback under Optional.
func TestGetTLSConfigForScrape_OptionalBuildsFallback(t *testing.T) {
	r, _ := rolloutTestReconciler(t)
	cluster := &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
	}
	tlsSpec := &valkeyiov1alpha1.NodeTLSSpec{
		Certificates: valkeyiov1alpha1.NodeTLSCertificates{
			Server: valkeyiov1alpha1.NodeCertificateRef{SecretName: "c-server"},
		},
	}

	primary, fallback, err := getTLSConfigForScrape(context.Background(), r.APIReader, tlsSpec, cluster)
	require.NoError(t, err)
	require.NotNil(t, primary)
	assert.Empty(t, primary.Certificates)
	require.NotNil(t, fallback)
	assert.Len(t, fallback.Certificates, 1)
}

// TestGetTLSConfigForScrape_RequiredNoFallback: under Required the scrape
// primary presents the certificate and no fallback is armed.
func TestGetTLSConfigForScrape_RequiredNoFallback(t *testing.T) {
	r, _ := rolloutTestReconciler(t)
	cluster := &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
	}
	tlsSpec := &valkeyiov1alpha1.NodeTLSSpec{
		Certificates: valkeyiov1alpha1.NodeTLSCertificates{
			Server: valkeyiov1alpha1.NodeCertificateRef{SecretName: "c-server"},
		},
		ClientAuth: &valkeyiov1alpha1.TLSClientAuthSpec{Mode: valkeyiov1alpha1.TLSAuthClientsRequired},
	}

	primary, fallback, err := getTLSConfigForScrape(context.Background(), r.APIReader, tlsSpec, cluster)
	require.NoError(t, err)
	require.NotNil(t, primary)
	assert.Len(t, primary.Certificates, 1)
	assert.Nil(t, fallback)
}
