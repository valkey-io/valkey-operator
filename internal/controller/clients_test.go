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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// stubClient satisfies vclient.Client for tests that never issue commands.
// Only Close is implemented; any other method panics on the nil embed.
type stubClient struct {
	vclient.Client
	closed int
}

func (s *stubClient) Close() { s.closed++ }

// recordNewClient returns a newClient func that records each option and
// answers with the next error from errs, or stub once errs runs out.
func recordNewClient(got *[]vclient.ClientOption, stub *stubClient, errs ...error) func(vclient.ClientOption) (vclient.Client, error) {
	return func(opt vclient.ClientOption) (vclient.Client, error) {
		*got = append(*got, opt)
		if len(errs) > 0 {
			err := errs[0]
			errs = errs[1:]
			return nil, err
		}
		return stub, nil
	}
}

func TestDialValkey(t *testing.T) {
	ctx := context.Background()
	cfg := connConfig{username: operatorUser, password: "pw"}
	wrongpass := errors.New("WRONGPASS invalid username-password pair or user is disabled.")

	t.Run("builds a single-node option with the tuned buffers", func(t *testing.T) {
		var got []vclient.ClientOption
		stub := &stubClient{}
		c, release, err := dialValkey(ctx, recordNewClient(&got, stub), "10.0.0.1:6379", cfg)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, []string{"10.0.0.1:6379"}, got[0].InitAddress)
		assert.True(t, got[0].ForceSingleClient)
		assert.Equal(t, operatorUser, got[0].Username)
		assert.Equal(t, "pw", got[0].Password)
		assert.Nil(t, got[0].TLSConfig)
		assert.Equal(t, -1, got[0].PipelineMultiplex)
		assert.Equal(t, 16*1024, got[0].ReadBufferEachConn)
		assert.Equal(t, 8*1024, got[0].WriteBufferEachConn)
		assert.Equal(t, 4, got[0].RingScaleEachConn)
		assert.Same(t, stub, c)

		release()
		assert.Equal(t, 1, stub.closed)
	})

	t.Run("retries once as the default user on WRONGPASS", func(t *testing.T) {
		var got []vclient.ClientOption
		stub := &stubClient{}
		c, _, err := dialValkey(ctx, recordNewClient(&got, stub, wrongpass), "10.0.0.1:6379", cfg)
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Empty(t, got[1].Username)
		assert.Empty(t, got[1].Password)
		assert.Same(t, stub, c)
	})

	t.Run("returns the retry's error when the fallback also fails", func(t *testing.T) {
		var got []vclient.ClientOption
		noauth := errors.New("NOAUTH Authentication required.")
		c, release, err := dialValkey(ctx, recordNewClient(&got, &stubClient{}, wrongpass, noauth), "10.0.0.1:6379", cfg)
		require.ErrorIs(t, err, noauth)
		assert.Len(t, got, 2)
		assert.Nil(t, c)
		require.NotNil(t, release)
		release()
	})

	t.Run("returns other errors without retrying", func(t *testing.T) {
		var got []vclient.ClientOption
		refused := errors.New("dial tcp 10.0.0.1:6379: connect: connection refused")
		c, release, err := dialValkey(ctx, recordNewClient(&got, &stubClient{}, refused), "10.0.0.1:6379", cfg)
		require.ErrorIs(t, err, refused)
		assert.Len(t, got, 1)
		assert.Nil(t, c)
		require.NotNil(t, release)
		release()
	})
}

func providerTestClient(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// operatorPasswordSecret returns the operator password secret for cluster
// "vc" in namespace "ns", holding password "pw".
func operatorPasswordSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: getSystemPasswordSecretName("vc"), Namespace: "ns"},
		Data:       map[string][]byte{operatorUser: []byte("pw")},
	}
}

// testTLSSecret returns a server certificate secret named "vc-tls" in
// namespace "ns", holding a self-signed CA that doubles as the certificate
// and key.
func testTLSSecret(t *testing.T) *corev1.Secret {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vc-tls", Namespace: "ns"},
		Data: map[string][]byte{
			tlsSecretKeyCA:   certPEM,
			tlsSecretKeyCert: certPEM,
			tlsSecretKeyKey:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		},
	}
}

func TestForCluster(t *testing.T) {
	ctx := context.Background()
	newCluster := func(tlsSpec *valkeyiov1alpha1.TLSSpec) *valkeyiov1alpha1.ValkeyCluster {
		return &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "vc", Namespace: "ns"},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Networking: &valkeyiov1alpha1.NetworkingSpec{TLS: tlsSpec},
			},
		}
	}
	tlsOn := &valkeyiov1alpha1.TLSSpec{
		Certificates: valkeyiov1alpha1.TLSCertificates{
			Server: valkeyiov1alpha1.CertificateSource{SecretName: "vc-tls"},
		},
	}
	mTLS := tlsOn.DeepCopy()
	mTLS.ClientAuth = &valkeyiov1alpha1.TLSClientAuthSpec{Mode: valkeyiov1alpha1.TLSAuthClientsRequired}

	provider := func(c client.Client, got *[]vclient.ClientOption) *unpooledProvider {
		return &unpooledProvider{client: c, apiReader: c, newClient: recordNewClient(got, &stubClient{})}
	}

	t.Run("TLS off dials with operator credentials", func(t *testing.T) {
		var got []vclient.ClientOption
		p := provider(providerTestClient(t, operatorPasswordSecret()), &got)
		dial, err := p.ForCluster(ctx, newCluster(nil))
		require.NoError(t, err)

		_, release, err := dial(ctx, "10.0.0.1:6379")
		require.NoError(t, err)
		defer release()
		require.Len(t, got, 1)
		assert.Equal(t, []string{"10.0.0.1:6379"}, got[0].InitAddress)
		assert.Equal(t, operatorUser, got[0].Username)
		assert.Equal(t, "pw", got[0].Password)
		assert.Nil(t, got[0].TLSConfig)
	})

	t.Run("TLS on sets the CA and server name", func(t *testing.T) {
		var got []vclient.ClientOption
		c := providerTestClient(t, operatorPasswordSecret(), testTLSSecret(t))
		cluster := newCluster(tlsOn)
		dial, err := provider(c, &got).ForCluster(ctx, cluster)
		require.NoError(t, err)

		_, release, err := dial(ctx, "10.0.0.1:6379")
		require.NoError(t, err)
		defer release()
		require.Len(t, got, 1)
		require.NotNil(t, got[0].TLSConfig)
		assert.NotNil(t, got[0].TLSConfig.RootCAs)
		assert.Equal(t, nodeTLSFromCluster(cluster).ServerName, got[0].TLSConfig.ServerName)
		assert.Empty(t, got[0].TLSConfig.Certificates)
	})

	t.Run("mTLS presents the client certificate", func(t *testing.T) {
		var got []vclient.ClientOption
		c := providerTestClient(t, operatorPasswordSecret(), testTLSSecret(t))
		dial, err := provider(c, &got).ForCluster(ctx, newCluster(mTLS))
		require.NoError(t, err)

		_, release, err := dial(ctx, "10.0.0.1:6379")
		require.NoError(t, err)
		defer release()
		require.Len(t, got, 1)
		require.NotNil(t, got[0].TLSConfig)
		assert.Len(t, got[0].TLSConfig.Certificates, 1)
	})

	t.Run("missing TLS secret fails every dial without dialling", func(t *testing.T) {
		var got []vclient.ClientOption
		c := providerTestClient(t, operatorPasswordSecret())
		dial, err := provider(c, &got).ForCluster(ctx, newCluster(tlsOn))
		require.NoError(t, err, "a missing TLS secret must not stop the cluster reconcile")
		require.NotNil(t, dial)

		for _, address := range []string{"10.0.0.1:6379", "10.0.0.2:6379"} {
			client, release, err := dial(ctx, address)
			require.ErrorContains(t, err, "TLS config")
			assert.Nil(t, client)
			require.NotNil(t, release)
			release()
		}
		assert.Empty(t, got)
	})

	t.Run("missing operator password secret is an error", func(t *testing.T) {
		var got []vclient.ClientOption
		dial, err := provider(providerTestClient(t), &got).ForCluster(ctx, newCluster(nil))
		require.Error(t, err)
		assert.True(t, apierrors.IsNotFound(err))
		assert.Nil(t, dial)
		assert.Empty(t, got)
	})
}

func TestForNode(t *testing.T) {
	ctx := context.Background()
	newNode := func(podIP string, labels map[string]string, tlsSpec *valkeyiov1alpha1.NodeTLSSpec) *valkeyiov1alpha1.ValkeyNode {
		return &valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{Name: "vc-0-0", Namespace: "ns", Labels: labels},
			Spec:       valkeyiov1alpha1.ValkeyNodeSpec{TLS: tlsSpec},
			Status:     valkeyiov1alpha1.ValkeyNodeStatus{PodIP: podIP},
		}
	}
	inCluster := map[string]string{LabelCluster: "vc"}
	tlsOn := &valkeyiov1alpha1.NodeTLSSpec{
		ServerName: "vc.ns.svc",
		Certificates: valkeyiov1alpha1.NodeTLSCertificates{
			Server: valkeyiov1alpha1.NodeCertificateRef{SecretName: "vc-tls"},
		},
	}
	mTLS := tlsOn.DeepCopy()
	mTLS.ClientAuth = &valkeyiov1alpha1.TLSClientAuthSpec{Mode: valkeyiov1alpha1.TLSAuthClientsRequired}

	provider := func(c client.Client, got *[]vclient.ClientOption) *unpooledProvider {
		return &unpooledProvider{client: c, apiReader: c, newClient: recordNewClient(got, &stubClient{})}
	}

	t.Run("no pod IP is an error", func(t *testing.T) {
		var got []vclient.ClientOption
		c, release, err := provider(providerTestClient(t), &got).ForNode(ctx, newNode("", inCluster, nil))
		require.ErrorContains(t, err, "no pod IP")
		assert.Nil(t, c)
		require.NotNil(t, release)
		release()
		assert.Empty(t, got)
	})

	t.Run("cluster node dials its pod with operator credentials", func(t *testing.T) {
		var got []vclient.ClientOption
		p := provider(providerTestClient(t, operatorPasswordSecret()), &got)
		_, release, err := p.ForNode(ctx, newNode("10.0.0.5", inCluster, nil))
		require.NoError(t, err)
		defer release()
		require.Len(t, got, 1)
		assert.Equal(t, []string{"10.0.0.5:6379"}, got[0].InitAddress)
		assert.Equal(t, operatorUser, got[0].Username)
		assert.Equal(t, "pw", got[0].Password)
		assert.Nil(t, got[0].TLSConfig)
	})

	t.Run("node outside a cluster dials as the default user", func(t *testing.T) {
		var got []vclient.ClientOption
		_, release, err := provider(providerTestClient(t), &got).ForNode(ctx, newNode("10.0.0.5", nil, nil))
		require.NoError(t, err)
		defer release()
		require.Len(t, got, 1)
		assert.Empty(t, got[0].Username)
		assert.Empty(t, got[0].Password)
	})

	t.Run("missing password secret dials as the default user", func(t *testing.T) {
		var got []vclient.ClientOption
		_, release, err := provider(providerTestClient(t), &got).ForNode(ctx, newNode("10.0.0.5", inCluster, nil))
		require.NoError(t, err)
		defer release()
		require.Len(t, got, 1)
		assert.Empty(t, got[0].Username)
		assert.Empty(t, got[0].Password)
	})

	t.Run("password secret without the operator key dials as the default user", func(t *testing.T) {
		var got []vclient.ClientOption
		secret := operatorPasswordSecret()
		secret.Data = map[string][]byte{"_exporter": []byte("other")}
		_, release, err := provider(providerTestClient(t, secret), &got).ForNode(ctx, newNode("10.0.0.5", inCluster, nil))
		require.NoError(t, err)
		defer release()
		require.Len(t, got, 1)
		assert.Empty(t, got[0].Username)
		assert.Empty(t, got[0].Password)
	})

	t.Run("other password lookup errors are returned", func(t *testing.T) {
		var got []vclient.ClientOption
		timeout := errors.New("etcdserver: request timed out")
		scheme := runtime.NewScheme()
		require.NoError(t, corev1.AddToScheme(scheme))
		c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return timeout
			},
		}).Build()
		_, release, err := provider(c, &got).ForNode(ctx, newNode("10.0.0.5", inCluster, nil))
		require.ErrorIs(t, err, timeout)
		require.NotNil(t, release)
		release()
		assert.Empty(t, got)
	})

	t.Run("TLS on sets the CA and server name", func(t *testing.T) {
		var got []vclient.ClientOption
		c := providerTestClient(t, operatorPasswordSecret(), testTLSSecret(t))
		_, release, err := provider(c, &got).ForNode(ctx, newNode("10.0.0.5", inCluster, tlsOn))
		require.NoError(t, err)
		defer release()
		require.Len(t, got, 1)
		require.NotNil(t, got[0].TLSConfig)
		assert.NotNil(t, got[0].TLSConfig.RootCAs)
		assert.Equal(t, "vc.ns.svc", got[0].TLSConfig.ServerName)
		assert.Empty(t, got[0].TLSConfig.Certificates)
	})

	t.Run("mTLS presents the client certificate", func(t *testing.T) {
		var got []vclient.ClientOption
		c := providerTestClient(t, operatorPasswordSecret(), testTLSSecret(t))
		_, release, err := provider(c, &got).ForNode(ctx, newNode("10.0.0.5", inCluster, mTLS))
		require.NoError(t, err)
		defer release()
		require.Len(t, got, 1)
		require.NotNil(t, got[0].TLSConfig)
		assert.Len(t, got[0].TLSConfig.Certificates, 1)
	})

	// A ValkeyNode created by v0.6.0 has no spec.tls.serverName until the
	// cluster controller updates it. Verifying against the pod IP would fail a
	// certificate with DNS SANs only.
	t.Run("cluster node without a server name verifies against the cluster default", func(t *testing.T) {
		var got []vclient.ClientOption
		c := providerTestClient(t, operatorPasswordSecret(), testTLSSecret(t))
		noServerName := tlsOn.DeepCopy()
		noServerName.ServerName = ""
		_, release, err := provider(c, &got).ForNode(ctx, newNode("10.0.0.5", inCluster, noServerName))
		require.NoError(t, err)
		defer release()
		require.Len(t, got, 1)
		require.NotNil(t, got[0].TLSConfig)
		assert.Equal(t, "valkey-vc.ns.svc.cluster.local", got[0].TLSConfig.ServerName)
	})

	t.Run("missing TLS secret is an error", func(t *testing.T) {
		var got []vclient.ClientOption
		c := providerTestClient(t, operatorPasswordSecret())
		_, release, err := provider(c, &got).ForNode(ctx, newNode("10.0.0.5", inCluster, tlsOn))
		require.ErrorContains(t, err, "TLS config")
		require.NotNil(t, release)
		release()
		assert.Empty(t, got)
	})
}
