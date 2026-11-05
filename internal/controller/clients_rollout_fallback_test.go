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
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	vclient "github.com/valkey-io/valkey-go"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

// rolloutTLSDialFixture holds a CA and a server certificate, plus a live TLS
// listener whose handshake demands a client certificate -- the posture of a
// Valkey node still enforcing clientAuth.mode: Required from before a
// clientAuth roll.
type rolloutTLSDialFixture struct {
	listener net.Listener
	addr     string
	refused  chan struct{}
	accepted chan struct{}
	caPool   *x509.CertPool
	caPEM    []byte
	certPEM  []byte
	keyPEM   []byte
	cert     tls.Certificate

	// dialTrace records each handshake outcome for failure diagnostics.
	// It is owned by the fixture: handler goroutines only send to it
	// (non-blocking, so a full buffer drops a trace line instead of
	// blocking the handshake), and only the test goroutine reads from
	// it. It is never closed, so a late handler send cannot panic the
	// test binary.
	dialTrace  chan string
	traceStart time.Time
}

func newRolloutTLSDialFixture(t *testing.T, maxVersion ...uint16) *rolloutTLSDialFixture {
	t.Helper()
	f := &rolloutTLSDialFixture{
		refused:    make(chan struct{}, 8),
		accepted:   make(chan struct{}, 8),
		caPool:     x509.NewCertPool(),
		dialTrace:  make(chan string, 64),
		traceStart: time.Now(),
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "rollout-dial-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	f.caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	require.True(t, f.caPool.AppendCertsFromPEM(f.caPEM), "failed to add CA to pool")
	caCert, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serverTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "rollout-dial-server"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTmpl, caCert, &serverKey.PublicKey, caKey)
	require.NoError(t, err)
	serverKeyDER, err := x509.MarshalECPrivateKey(serverKey)
	require.NoError(t, err)
	f.certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})
	f.keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: serverKeyDER})
	f.cert, err = tls.X509KeyPair(f.certPEM, f.keyPEM)
	require.NoError(t, err)

	listenerCfg := &tls.Config{
		Certificates: []tls.Certificate{f.cert},
		ClientAuth:   tls.RequireAnyClientCert,
		MinVersion:   tls.VersionTLS12,
	}
	if len(maxVersion) > 0 {
		listenerCfg.MaxVersion = maxVersion[0]
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", listenerCfg)
	require.NoError(t, err)
	f.listener = ln
	f.addr = ln.Addr().String()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				tc := c.(*tls.Conn)
				_ = tc.SetReadDeadline(time.Now().Add(5 * time.Second))
				if err := tc.Handshake(); err != nil {
					select {
					case f.dialTrace <- fmt.Sprintf("t=%d REFUSED: %v", time.Since(f.traceStart).Milliseconds(), err):
					default:
					}
					select {
					case f.refused <- struct{}{}:
					default:
					}
				} else {
					select {
					case f.dialTrace <- fmt.Sprintf("t=%d ACCEPTED", time.Since(f.traceStart).Milliseconds()):
					default:
					}
					select {
					case f.accepted <- struct{}{}:
					default:
					}
				}
				// Give the fatal alert time to reach the client before
				// Close can turn into a TCP reset, which on some platforms
				// surfaces as a generic connection-aborted read error on the
				// client instead of the certificate_required alert itself.
				time.Sleep(100 * time.Millisecond)
				_ = c.Close()
			}(conn)
		}
	}()

	t.Cleanup(func() { _ = ln.Close() })
	return f
}

// drainDialTrace empties the fixture-owned trace without closing it: a
// handler goroutine may still be mid-handshake and sending to it, and a
// send on a closed channel would panic the test binary.
func (f *rolloutTLSDialFixture) drainDialTrace(t *testing.T) {
	t.Helper()
	defer func() { _ = recover() }()
	for {
		select {
		case msg := <-f.dialTrace:
			t.Logf("TRACE %s", msg)
		default:
			return
		}
	}
}

// rolloutDialClusterSecret returns the TLS secret holding the fixture's CA
// and server certificate, named for a cluster that reads "vc-tls" in
// namespace "ns".
func rolloutDialClusterSecret(f *rolloutTLSDialFixture) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vc-tls", Namespace: "ns"},
		Data: map[string][]byte{
			tlsSecretKeyCA:   f.caPEM,
			tlsSecretKeyCert: f.certPEM,
			tlsSecretKeyKey:  f.keyPEM,
		},
	}
}

// rolloutDialCluster returns cluster "vc" in namespace "ns" with TLS from
// secret "vc-tls". A nil clientAuth models a roll to Optional; the Required
// form models a cluster that has not been rolled.
func rolloutDialCluster(clientAuth *valkeyiov1alpha1.TLSClientAuthSpec) *valkeyiov1alpha1.ValkeyCluster {
	return &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "vc", Namespace: "ns"},
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{
			Networking: &valkeyiov1alpha1.NetworkingSpec{
				TLS: &valkeyiov1alpha1.TLSSpec{
					ServerName:   "localhost",
					Certificates: valkeyiov1alpha1.TLSCertificates{Server: valkeyiov1alpha1.CertificateSource{SecretName: "vc-tls"}},
					ClientAuth:   clientAuth,
				},
			},
		},
	}
}

// TestForClusterClientAuthRollFallback drives a real provider against the
// live RequireAnyClientCert fixture: in Optional mode the primary dial is
// refused and the fallback retry completes the handshake; in Required mode
// the primary presents the certificate and is accepted on the first dial.

// rolloutNewClientScript answers each dial by whether it presents a client
// certificate: a dial presenting none is refused with certRefused -- the
// posture of a pod still enforcing clientAuth.mode: Required from before a
// clientAuth roll -- while a dial presenting the node's server certificate
// is answered with stub. It records every option it sees.
func rolloutNewClientScript(got *[]vclient.ClientOption, stub *stubClient, certRefused error) func(vclient.ClientOption) (vclient.Client, error) {
	return func(opt vclient.ClientOption) (vclient.Client, error) {
		*got = append(*got, opt)
		if opt.TLSConfig == nil || len(opt.TLSConfig.Certificates) == 0 {
			return nil, certRefused
		}
		return stub, nil
	}
}

// TestForClusterClientAuthRollFallback drives a real provider backed by a
// real client pool against the live RequireAnyClientCert fixture: in
// Optional mode the primary dial is refused and the fallback retry
// completes the handshake; in Required mode the primary presents the
// certificate and is accepted on the first dial. Both configurations come
// from a single secret read.
func TestForClusterClientAuthRollFallback(t *testing.T) {
	ctx := context.Background()

	t.Run("Optional retries a refused scrape dial with the fallback", func(t *testing.T) {
		f := newRolloutTLSDialFixture(t)
		defer f.drainDialTrace(t)
		c := providerTestClient(t, operatorPasswordSecret(), rolloutDialClusterSecret(f))
		p := NewClientProvider(c, c, valkey.NewPool(valkey.DefaultIdleTTL, vclient.NewClient))
		dial, err := p.ForCluster(ctx, rolloutDialCluster(nil))
		require.NoError(t, err)
		require.NotNil(t, dial)

		_, _ = dial(ctx, f.addr)
		select {
		case <-f.accepted:
		case <-time.After(3 * time.Second):
			t.Fatal("fallback dial never completed its handshake")
		}
		select {
		case <-f.refused:
		default:
			t.Fatal("expected the primary dial to be refused before the fallback retry")
		}
	})

	t.Run("Required presents the certificate on the first dial", func(t *testing.T) {
		f := newRolloutTLSDialFixture(t)
		defer f.drainDialTrace(t)
		c := providerTestClient(t, operatorPasswordSecret(), rolloutDialClusterSecret(f))
		p := NewClientProvider(c, c, valkey.NewPool(valkey.DefaultIdleTTL, vclient.NewClient))
		dial, err := p.ForCluster(ctx, rolloutDialCluster(&valkeyiov1alpha1.TLSClientAuthSpec{Mode: valkeyiov1alpha1.TLSAuthClientsRequired}))
		require.NoError(t, err)
		require.NotNil(t, dial)

		_, _ = dial(ctx, f.addr)
		select {
		case <-f.accepted:
		case <-time.After(3 * time.Second):
			t.Fatal("Required-mode dial never completed its handshake")
		}
		select {
		case <-f.refused:
			t.Fatal("Required-mode primary dial must present a certificate and be accepted, not refused")
		default:
		}
	})

	t.Run("Optional builds primary and fallback from a single secret read", func(t *testing.T) {
		f := newRolloutTLSDialFixture(t)
		c := providerTestClient(t, operatorPasswordSecret(), rolloutDialClusterSecret(f))
		cr := &countingReader{Reader: c}
		var got []vclient.ClientOption
		p := NewClientProvider(c, cr, valkey.NewPool(valkey.DefaultIdleTTL, rolloutNewClientScript(&got, &stubClient{}, errors.New("remote error: tls: certificate required"))))
		dial, err := p.ForCluster(ctx, rolloutDialCluster(nil))
		require.NoError(t, err)
		assert.Equal(t, 1, cr.secretGets, "primary and fallback must be built from a single secret read")
		require.NotNil(t, dial)

		_, err = dial(ctx, "10.0.0.1:6379")
		require.NoError(t, err)
		require.NotEmpty(t, got)
		require.NotNil(t, got[0].TLSConfig)
		assert.Empty(t, got[0].TLSConfig.Certificates, "Optional-mode primary must present no certificate")
	})
}

// TestForClusterTLS12HandshakeFailureFallback pins the TLS 1.2 form of the
// refusal. The certificate_required alert does not exist in TLS 1.2, so a
// pod still enforcing clientAuth.mode: Required on a tls-protocols:
// TLSv1.2 port refuses the primary dial with a generic "handshake failure"
// instead; the fallback must arm for that spelling too, or a
// Required-to-Optional roll stalls on every TLS 1.2 cluster.
func TestForClusterTLS12HandshakeFailureFallback(t *testing.T) {
	ctx := context.Background()
	f := newRolloutTLSDialFixture(t, tls.VersionTLS12)
	defer f.drainDialTrace(t)
	c := providerTestClient(t, operatorPasswordSecret(), rolloutDialClusterSecret(f))
	p := NewClientProvider(c, c, valkey.NewPool(valkey.DefaultIdleTTL, vclient.NewClient))
	dial, err := p.ForCluster(ctx, rolloutDialCluster(nil))
	require.NoError(t, err)

	_, _ = dial(ctx, f.addr)
	select {
	case <-f.accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("fallback dial never completed its handshake")
	}
}

// TestForClusterFallbackRetrySemantics pins the retry rules at the pooled
// provider: only a client-cert refusal retries, only once, and only when a
// fallback was armed.
func TestForClusterFallbackRetrySemantics(t *testing.T) {
	ctx := context.Background()
	certRefused := errors.New("remote error: tls: certificate required")
	connRefused := errors.New("dial tcp 10.0.0.1:6379: connect: connection refused")

	t.Run("retries a refused dial with the fallback TLS config", func(t *testing.T) {
		f := newRolloutTLSDialFixture(t)
		c := providerTestClient(t, operatorPasswordSecret(), rolloutDialClusterSecret(f))
		var got []vclient.ClientOption
		p := NewClientProvider(c, c, valkey.NewPool(valkey.DefaultIdleTTL, rolloutNewClientScript(&got, &stubClient{}, certRefused)))
		dial, err := p.ForCluster(ctx, rolloutDialCluster(nil))
		require.NoError(t, err)

		cl, err := dial(ctx, "10.0.0.1:6379")
		require.NoError(t, err)
		require.Len(t, got, 2, "the refused primary dial must be retried with the fallback")
		require.NotNil(t, got[0].TLSConfig)
		assert.Empty(t, got[0].TLSConfig.Certificates, "Optional-mode primary must present no client certificate")
		require.NotNil(t, got[1].TLSConfig)
		assert.Len(t, got[1].TLSConfig.Certificates, 1, "the retry must present the node server certificate")
		require.NotNil(t, cl)
	})

	t.Run("does not retry when the error is not a client-cert refusal", func(t *testing.T) {
		f := newRolloutTLSDialFixture(t)
		c := providerTestClient(t, operatorPasswordSecret(), rolloutDialClusterSecret(f))
		var got []vclient.ClientOption
		script := func(opt vclient.ClientOption) (vclient.Client, error) {
			got = append(got, opt)
			return nil, connRefused
		}
		p := NewClientProvider(c, c, valkey.NewPool(valkey.DefaultIdleTTL, script))
		dial, err := p.ForCluster(ctx, rolloutDialCluster(nil))
		require.NoError(t, err)

		cl, err := dial(ctx, "10.0.0.1:6379")
		require.ErrorIs(t, err, connRefused)
		assert.Len(t, got, 1)
		assert.Nil(t, cl)
	})

	t.Run("does not retry without a fallback config", func(t *testing.T) {
		// A secret without cert/key entries cannot arm a fallback, so the
		// refusal surfaces unchanged.
		f := newRolloutTLSDialFixture(t)
		noCert := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "vc-tls", Namespace: "ns"},
			Data:       map[string][]byte{tlsSecretKeyCA: f.caPEM},
		}
		c := providerTestClient(t, operatorPasswordSecret(), noCert)
		var got []vclient.ClientOption
		p := NewClientProvider(c, c, valkey.NewPool(valkey.DefaultIdleTTL, rolloutNewClientScript(&got, &stubClient{}, certRefused)))
		dial, err := p.ForCluster(ctx, rolloutDialCluster(nil))
		require.NoError(t, err)

		cl, err := dial(ctx, "10.0.0.1:6379")
		require.ErrorIs(t, err, certRefused)
		assert.Len(t, got, 1, "without a fallback the refused dial must not be retried")
		assert.Nil(t, cl)
	})

	t.Run("reuses the pooled fallback client on later dials", func(t *testing.T) {
		f := newRolloutTLSDialFixture(t)
		c := providerTestClient(t, operatorPasswordSecret(), rolloutDialClusterSecret(f))
		var got []vclient.ClientOption
		p := NewClientProvider(c, c, valkey.NewPool(valkey.DefaultIdleTTL, rolloutNewClientScript(&got, &stubClient{}, certRefused)))
		dial, err := p.ForCluster(ctx, rolloutDialCluster(nil))
		require.NoError(t, err)

		first, err := dial(ctx, "10.0.0.1:6379")
		require.NoError(t, err)
		require.Len(t, got, 2)

		// The second dial repeats the refused primary handshake -- the pod
		// has not restarted -- but must reuse the pooled fallback client
		// instead of dialling one.
		second, err := dial(ctx, "10.0.0.1:6379")
		require.NoError(t, err)
		require.Len(t, got, 3, "only the primary handshake repeats; the fallback client is reused")
		assert.Same(t, first, second, "later dials must reuse the pooled fallback client")
		require.NotNil(t, got[2].TLSConfig)
		assert.Empty(t, got[2].TLSConfig.Certificates)
	})
}

// countingReader wraps a client.Reader and counts Secret reads, so the
// tests can pin how many API-server requests a TLS configuration build
// issues. The production APIReader is uncached: every extra read is an
// extra request on the dial path, which the cluster poller repeats
// every few seconds.
type countingReader struct {
	client.Reader
	secretGets int
}

func (r *countingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Secret); ok {
		r.secretGets++
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}
