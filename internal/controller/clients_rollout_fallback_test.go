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

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
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

// TestDialValkey_CertificateRequiredFallbackRetry is the dial-level control
// for the clientAuth rollout under TLS 1.3: the fixture's node still enforces
// clientAuth.mode: Required (its handshake demands a client certificate),
// while the primary TLS configuration -- following the desired Optional or
// Disabled spec -- presents none. The primary dial fails with "certificate
// required", and the fallback retry presenting the node's own server
// certificate completes the handshake. Without the fallback in the config
// this dial is never retried, which is what pristine main does.
func TestDialValkey_CertificateRequiredFallbackRetry(t *testing.T) {
	f := newRolloutTLSDialFixture(t)
	defer f.drainDialTrace(t)

	primary := &tls.Config{
		RootCAs:    f.caPool,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS12,
	}
	// The fallback presents the node's own server certificate as a client
	// certificate.
	fallback := primary.Clone()
	fallback.Certificates = []tls.Certificate{f.cert}

	// Sanity: the primary dial alone is refused with "certificate required".
	// This pins the fixture itself; if the primary were accepted the test
	// would prove nothing about the retry.
	_, _, err := dialValkey(context.Background(), vclient.NewClient, f.addr, connConfig{tls: primary})
	require.Error(t, err)
	require.ErrorContains(t, err, "certificate required", "TLS 1.3 refusal must surface the certificate_required alert")
	select {
	case <-f.refused:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the primary dial's handshake to be refused")
	}

	// The refused dial must be retried with the fallback and complete its
	// handshake: the fixture speaks no RESP, so the client constructor
	// cannot return a live node; the assertion is that the dial stopped
	// failing with "certificate required" and the handshake completed.
	_, _, _ = dialValkey(context.Background(), vclient.NewClient, f.addr, connConfig{tls: primary, clientCertFallback: fallback})
	select {
	case <-f.accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("fallback dial never completed its handshake")
	}
}

// TestDialValkey_TLS12HandshakeFailureFallbackRetry pins the TLS 1.2 form of
// the refusal. The certificate_required alert does not exist in TLS 1.2, so
// a pod still enforcing clientAuth.mode: Required on a tls-protocols:
// TLSv1.2 port refuses the primary dial with a generic "handshake failure"
// instead. clientCertificateRefused must arm the fallback retry for that
// spelling too, or a Required-to-Optional roll stalls on every TLS 1.2
// cluster. The fixture speaks no RESP, so the assertion is the same as the
// TLS 1.3 case: the primary dial is refused, the fallback retry completes
// its handshake.
func TestDialValkey_TLS12HandshakeFailureFallbackRetry(t *testing.T) {
	f := newRolloutTLSDialFixture(t, tls.VersionTLS12)
	defer f.drainDialTrace(t)

	primary := &tls.Config{
		RootCAs:    f.caPool,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS12,
		MaxVersion: tls.VersionTLS12,
	}
	fallback := primary.Clone()
	fallback.Certificates = []tls.Certificate{f.cert}

	// Sanity: the primary dial alone is refused, and on a TLS 1.2 port the
	// refusal surfaces as the generic "handshake failure".
	_, _, err := dialValkey(context.Background(), vclient.NewClient, f.addr, connConfig{tls: primary})
	require.Error(t, err)
	require.ErrorContains(t, err, "handshake failure", "TLS 1.2 refusal must surface the generic handshake-failure form")
	select {
	case <-f.refused:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the primary dial's handshake to be refused")
	}

	_, _, _ = dialValkey(context.Background(), vclient.NewClient, f.addr, connConfig{tls: primary, clientCertFallback: fallback})
	select {
	case <-f.accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("fallback dial never completed its handshake")
	}
}

func TestDialValkeyFallbackRetry(t *testing.T) {
	ctx := context.Background()
	primary := &tls.Config{ServerName: "valkey-vc.ns.svc.cluster.local"}
	fallback := primary.Clone()
	fallback.Certificates = []tls.Certificate{{}}
	certRefused := errors.New("remote error: tls: certificate required")
	cfg := connConfig{username: operatorUser, password: "pw", tls: primary, clientCertFallback: fallback}

	t.Run("retries a refused dial with the fallback TLS config", func(t *testing.T) {
		var got []vclient.ClientOption
		stub := &stubClient{}
		c, release, err := dialValkey(ctx, recordNewClient(&got, stub, certRefused), "10.0.0.1:6379", cfg)
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Same(t, primary, got[0].TLSConfig)
		assert.Same(t, fallback, got[1].TLSConfig, "the retry must dial with the fallback configuration")
		assert.Same(t, stub, c)
		release()
		assert.Equal(t, 1, stub.closed)
	})

	t.Run("returns the retry's error when the fallback dial also fails", func(t *testing.T) {
		var got []vclient.ClientOption
		other := errors.New("dial tcp 10.0.0.1:6379: connect: connection refused")
		c, release, err := dialValkey(ctx, recordNewClient(&got, &stubClient{}, certRefused, other), "10.0.0.1:6379", cfg)
		require.ErrorIs(t, err, other)
		assert.Len(t, got, 2)
		assert.Nil(t, c)
		require.NotNil(t, release)
		release()
	})

	t.Run("does not retry when the error is not a refusal", func(t *testing.T) {
		var got []vclient.ClientOption
		refused := errors.New("dial tcp 10.0.0.1:6379: connect: connection refused")
		c, release, err := dialValkey(ctx, recordNewClient(&got, &stubClient{}, refused), "10.0.0.1:6379", cfg)
		require.ErrorIs(t, err, refused)
		assert.Len(t, got, 1)
		assert.Nil(t, c)
		require.NotNil(t, release)
		release()
	})

	t.Run("does not retry without a fallback config", func(t *testing.T) {
		var got []vclient.ClientOption
		noFallback := connConfig{username: operatorUser, password: "pw", tls: primary}
		c, release, err := dialValkey(ctx, recordNewClient(&got, &stubClient{}, certRefused), "10.0.0.1:6379", noFallback)
		require.ErrorIs(t, err, certRefused)
		assert.Len(t, got, 1)
		assert.Nil(t, c)
		require.NotNil(t, release)
		release()
	})
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
func TestForClusterClientAuthRollFallback(t *testing.T) {
	ctx := context.Background()

	t.Run("Optional retries a refused scrape dial with the fallback", func(t *testing.T) {
		f := newRolloutTLSDialFixture(t)
		defer f.drainDialTrace(t)
		c := providerTestClient(t, operatorPasswordSecret(), rolloutDialClusterSecret(f))
		p := &unpooledProvider{client: c, apiReader: c, newClient: vclient.NewClient}
		dial, err := p.ForCluster(ctx, rolloutDialCluster(nil))
		require.NoError(t, err)
		require.NotNil(t, dial)

		_, _, _ = dial(ctx, f.addr)
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
		p := &unpooledProvider{client: c, apiReader: c, newClient: vclient.NewClient}
		dial, err := p.ForCluster(ctx, rolloutDialCluster(&valkeyiov1alpha1.TLSClientAuthSpec{Mode: valkeyiov1alpha1.TLSAuthClientsRequired}))
		require.NoError(t, err)
		require.NotNil(t, dial)

		_, _, _ = dial(ctx, f.addr)
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
		p := &unpooledProvider{client: c, apiReader: cr, newClient: recordNewClient(&got, &stubClient{})}
		dial, err := p.ForCluster(ctx, rolloutDialCluster(nil))
		require.NoError(t, err)
		require.NotNil(t, dial)

		_, release, err := dial(ctx, "10.0.0.1:6379")
		require.NoError(t, err)
		release()
		assert.Equal(t, 1, cr.secretGets, "primary and fallback must be built from a single secret read")
		require.Len(t, got, 1)
		assert.NotNil(t, got[0].TLSConfig)
		assert.Empty(t, got[0].TLSConfig.Certificates, "Optional-mode primary must present no certificate")
	})
}
