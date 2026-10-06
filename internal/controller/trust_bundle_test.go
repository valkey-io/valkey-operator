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
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bundleDERs returns the DER bytes of each certificate in bundle, in order.
func bundleDERs(t *testing.T, bundle []byte) [][]byte {
	t.Helper()
	var ders [][]byte
	for {
		var block *pem.Block
		block, bundle = pem.Decode(bundle)
		if block == nil {
			return ders
		}
		ders = append(ders, block.Bytes)
	}
}

func derOf(t *testing.T, p []byte) []byte {
	t.Helper()
	block, _ := pem.Decode(p)
	require.NotNil(t, block)
	return block.Bytes
}

func TestMergeTrustBundle(t *testing.T) {
	server := selfSignedCAPEM(t)
	clientA := selfSignedCAPEM(t)
	clientB := selfSignedCAPEM(t)

	t.Run("server root alone when there are no client sources", func(t *testing.T) {
		got, err := mergeTrustBundle([]trustSource{{"server", server, "PEM"}})
		require.NoError(t, err)
		assert.Equal(t, [][]byte{derOf(t, server)}, bundleDERs(t, got))
	})

	t.Run("keeps source order", func(t *testing.T) {
		got, err := mergeTrustBundle([]trustSource{{"server", server, "PEM"}, {"b", clientB, "PEM"}, {"a", clientA, "PEM"}})
		require.NoError(t, err)
		assert.Equal(t, [][]byte{derOf(t, server), derOf(t, clientB), derOf(t, clientA)}, bundleDERs(t, got))
	})

	t.Run("drops duplicates at their first position", func(t *testing.T) {
		both := append(append([]byte{}, clientA...), server...)
		got, err := mergeTrustBundle([]trustSource{{"server", server, "PEM"}, {"both", both, "PEM"}, {"a-again", clientA, "PEM"}})
		require.NoError(t, err)
		assert.Equal(t, [][]byte{derOf(t, server), derOf(t, clientA)}, bundleDERs(t, got))
	})

	t.Run("allows whitespace between blocks", func(t *testing.T) {
		spaced := append(append(append([]byte("\n  \n"), clientA...), []byte("\r\n\n")...), clientB...)
		got, err := mergeTrustBundle([]trustSource{{"server", server, "PEM"}, {"spaced", spaced, "PEM"}})
		require.NoError(t, err)
		assert.Equal(t, [][]byte{derOf(t, server), derOf(t, clientA), derOf(t, clientB)}, bundleDERs(t, got))
	})

	t.Run("keeps every certificate of a multi-root source", func(t *testing.T) {
		rotating := append(append([]byte{}, clientA...), clientB...)
		got, err := mergeTrustBundle([]trustSource{{"server", server, "PEM"}, {"rotating", rotating, "PEM"}})
		require.NoError(t, err)
		assert.Equal(t, [][]byte{derOf(t, server), derOf(t, clientA), derOf(t, clientB)}, bundleDERs(t, got))
	})

	t.Run("is byte-stable for the same input", func(t *testing.T) {
		in := []trustSource{{"server", server, "PEM"}, {"a", clientA, "PEM"}}
		first, err := mergeTrustBundle(in)
		require.NoError(t, err)
		second, err := mergeTrustBundle(in)
		require.NoError(t, err)
		assert.Equal(t, first, second)
	})

	for name, tc := range map[string]struct {
		pem     []byte
		wantErr string
	}{
		"empty":            {nil, "no certificates"},
		"not PEM":          {[]byte("not a certificate"), "data that is not PEM at byte 0"},
		"a private key":    {pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1}}), `unexpected PEM block "PRIVATE KEY"`},
		"garbage DER":      {pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1, 2, 3}}), "bad: x509"},
		"trailing garbage": {append(append([]byte{}, clientA...), []byte("junk")...), "data that is not PEM at byte"},
		// pem.Decode alone skips the first three to reach the next block, which
		// would drop a root without an error.
		"a malformed block before a good one":     {append(corruptPEM(t, clientB), clientA...), "malformed PEM block at byte 0"},
		"a malformed block between good ones":     {append(append(append([]byte{}, clientA...), corruptPEM(t, clientB)...), server...), "malformed PEM block at byte"},
		"text between good blocks":                {append(append(append([]byte{}, clientA...), []byte("not pem\n")...), server...), "data that is not PEM at byte"},
		"an unterminated block before a good one": {append([]byte("-----BEGIN CERTIFICATE-----\nAAAA\n"), clientA...), "unterminated PEM block at byte 0"},
		"an unterminated block":                   {bytes.TrimSuffix(clientA, []byte("-----END CERTIFICATE-----\n")), "unterminated PEM block at byte 0"},
	} {
		t.Run("rejects a source that is "+name, func(t *testing.T) {
			got, err := mergeTrustBundle([]trustSource{{"server", server, "PEM"}, {"bad", tc.pem, "PEM"}})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Nil(t, got, "a failed merge must not return a partial bundle")
		})
	}
}

// corruptPEM damages the base64 body of a PEM certificate so it no longer
// decodes, keeping its BEGIN and END lines.
func corruptPEM(t *testing.T, p []byte) []byte {
	t.Helper()
	lines := bytes.Split(p, []byte("\n"))
	require.Greater(t, len(lines), 2)
	lines[1] = append([]byte("!!!"), lines[1]...)
	return bytes.Join(lines, []byte("\n"))
}

// spiffeBundleJSON builds a SPIFFE trust bundle with one x509-svid key per
// certificate, plus a jwt-svid key that carries no certificate, as SPIRE
// publishes them.
func spiffeBundleJSON(t *testing.T, certs ...[]byte) []byte {
	t.Helper()
	keys := make([]map[string]any, 0, 1+len(certs))
	keys = append(keys, map[string]any{"use": "jwt-svid", "kty": "EC", "kid": "jwt"})
	for _, c := range certs {
		keys = append(keys, map[string]any{"use": "x509-svid", "kty": "EC", "x5c": []string{base64.StdEncoding.EncodeToString(derOf(t, c))}})
	}
	out, err := json.Marshal(map[string]any{"keys": keys, "spiffe_sequence": 1})
	require.NoError(t, err)
	return out
}

func TestSPIFFEBundleToPEM(t *testing.T) {
	a, b := selfSignedCAPEM(t), selfSignedCAPEM(t)

	t.Run("keeps every x509-svid authority in order and skips jwt-svid keys", func(t *testing.T) {
		got, err := spiffeBundleToPEM(spiffeBundleJSON(t, a, b))
		require.NoError(t, err)
		assert.Equal(t, [][]byte{derOf(t, a), derOf(t, b)}, bundleDERs(t, got))
	})

	for name, tc := range map[string]struct {
		data    []byte
		wantErr string
	}{
		"not JSON":                     {[]byte("-----BEGIN CERTIFICATE-----"), "not a SPIFFE bundle"},
		"a JWT-only bundle":            {[]byte(`{"keys":[{"use":"jwt-svid","kty":"EC"},{"use":"jwt-svid","kty":"RSA"}]}`), "no x509-svid authority (it has 2 key(s), 2 of them jwt-svid): clientAuth.ca needs a bundle with X.509 authorities"},
		"a bundle with no keys":        {[]byte(`{"keys":[]}`), "no x509-svid authority (it has 0 key(s), 0 of them jwt-svid)"},
		"an x509-svid key without x5c": {[]byte(`{"keys":[{"use":"x509-svid","kty":"EC"}]}`), "has no x5c certificate"},
		"bad base64":                   {[]byte(`{"keys":[{"use":"x509-svid","x5c":["%%%"]}]}`), "x5c"},
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			got, err := spiffeBundleToPEM(tc.data)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Nil(t, got)
		})
	}
}

func TestIsSPIFFEBundle(t *testing.T) {
	assert.True(t, isSPIFFEBundle(spiffeBundleJSON(t, selfSignedCAPEM(t))))
	assert.True(t, isSPIFFEBundle([]byte("\n  {\"keys\":[]}")), "leading whitespace is ignored")
	assert.False(t, isSPIFFEBundle(selfSignedCAPEM(t)))
	assert.False(t, isSPIFFEBundle([]byte("garbage")), "anything that is not a JSON object is left to the PEM parser to reject")
	assert.False(t, isSPIFFEBundle(nil))
}
