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

package valkey

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"testing"
	"time"
)

// rolloutTLSDialFixture holds a CA and a server certificate, plus a live TLS
// listener whose handshake demands a client certificate -- the posture of a
// Valkey node still enforcing clientAuth.mode: Required from before a
// clientAuth roll.
type rolloutTLSDialFixture struct {
	listener net.Listener
	addr     string
	port     int
	refused  chan struct{}
	accepted chan struct{}
	caPool   *x509.CertPool
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
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	if !f.caPool.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to add CA to pool")
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	serverKeyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		t.Fatal(err)
	}
	serverCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})
	serverKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: serverKeyDER})
	f.cert, err = tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		t.Fatal(err)
	}

	listenerCfg := &tls.Config{
		Certificates: []tls.Certificate{f.cert},
		ClientAuth:   tls.RequireAnyClientCert,
		MinVersion:   tls.VersionTLS12,
	}
	if len(maxVersion) > 0 {
		listenerCfg.MaxVersion = maxVersion[0]
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", listenerCfg)
	if err != nil {
		t.Fatal(err)
	}
	f.listener = ln
	f.addr = ln.Addr().String()
	_, portStr, err := net.SplitHostPort(f.addr)
	if err != nil {
		t.Fatal(err)
	}
	f.port = 0
	for _, c := range portStr {
		f.port = f.port*10 + int(c-'0')
	}

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

// TestGetClusterStateWithFallback_CertificateRequiredRetry is the dial-level
// control for the clientAuth rollout: the fixture's node still enforces
// clientAuth.mode: Required (its handshake demands a client certificate),
// while the primary TLS configuration -- following the desired Optional or
// Disabled spec -- presents none. The primary dial fails with "certificate
// required", and the fallback retry presenting the node's own server
// certificate completes the handshake. Without the fallback argument this
// dial is never retried, which is what pristine main does.
func TestGetClusterStateWithFallback_CertificateRequiredRetry(t *testing.T) {
	f := newRolloutTLSDialFixture(t)
	defer func() {
		// Drain the fixture-owned trace without closing it: a handler
		// goroutine may still be mid-handshake and sending to it, and
		// a send on a closed channel would panic the test binary.
		for {
			select {
			case msg := <-f.dialTrace:
				t.Logf("TRACE %s", msg)
			default:
				return
			}
		}
	}()

	primary := &tls.Config{
		RootCAs:    f.caPool,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS12,
	}
	// The fallback presents the node's own server certificate as a client
	// certificate, mirroring loadClientCertificateFallback.
	fallback := primary.Clone()
	fallback.Certificates = []tls.Certificate{f.cert}

	// Sanity: the primary dial alone is refused with "certificate required".
	// This pins the fixture itself; if the primary were accepted the test
	// would prove nothing about the retry.
	primaryOnly := GetClusterStateWithFallback(context.Background(),
		[]string{"127.0.0.1"}, f.port, "", "", primary, nil)
	if primaryOnly != nil && len(primaryOnly.Shards) > 0 {
		t.Fatal("primary dial without a client certificate unexpectedly succeeded; fixture is not enforcing RequireAnyClientCert")
	}
	select {
	case <-f.refused:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the primary dial's handshake to be refused")
	}

	// The refused dial must be retried with the fallback and succeed: the
	// handshake completes, so the scrape proceeds past the TLS layer. The
	// fixture speaks no RESP, so the scrape cannot return a live node; the
	// assertion is that the client constructor stopped failing with
	// "certificate required" and the handshake completed (f.accepted).
	refusedBefore := len(f.refused)
	GetClusterStateWithFallback(context.Background(),
		[]string{"127.0.0.1"}, f.port, "", "", primary, fallback)

	select {
	case <-f.accepted:
	case <-time.After(2 * time.Second):
		t.Fatalf("fallback dial never completed its handshake; refusals so far: %d", refusedBefore)
	}
}

// TestGetClusterStateWithFallback_TLS12HandshakeFailureRetry pins the TLS
// 1.2 form of the refusal. The certificate_required alert does not exist
// in TLS 1.2, so a pod still enforcing clientAuth.mode: Required on a
// tls-protocols: TLSv1.2 port refuses the primary dial with a generic
// "handshake failure" instead. ClientCertificateRefused must arm the
// fallback retry for that spelling too, or a Required-to-Optional roll
// stalls on every TLS 1.2 cluster. The fixture speaks no RESP, so the
// assertion is the same as the TLS 1.3 case: the primary dial is
// refused, the fallback retry completes its handshake.
func TestGetClusterStateWithFallback_TLS12HandshakeFailureRetry(t *testing.T) {
	f := newRolloutTLSDialFixture(t, tls.VersionTLS12)
	defer func() {
		for {
			select {
			case msg := <-f.dialTrace:
				t.Logf("TRACE %s", msg)
			default:
				return
			}
		}
	}()

	primary := &tls.Config{
		RootCAs:    f.caPool,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS12,
		MaxVersion: tls.VersionTLS12,
	}
	fallback := primary.Clone()
	fallback.Certificates = []tls.Certificate{f.cert}

	// Sanity: the primary dial alone is refused under TLS 1.2 as well. A
	// TLS 1.2 server demanding a client certificate reports the refusal
	// as a handshake failure, not certificate required.
	primaryOnly := GetClusterStateWithFallback(context.Background(),
		[]string{"127.0.0.1"}, f.port, "", "", primary, nil)
	if primaryOnly != nil && len(primaryOnly.Shards) > 0 {
		t.Fatal("primary dial without a client certificate unexpectedly succeeded; fixture is not enforcing RequireAnyClientCert")
	}
	select {
	case <-f.refused:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the primary dial's handshake to be refused")
	}

	// The refused dial must be retried with the fallback and complete the
	// TLS 1.2 handshake.
	GetClusterStateWithFallback(context.Background(),
		[]string{"127.0.0.1"}, f.port, "", "", primary, fallback)

	select {
	case <-f.accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("fallback dial never completed its handshake under TLS 1.2")
	}
}
