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
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strconv"
)

// trustSource is one input to a merged trust bundle: PEM certificates, where
// they were read from (for example `secret "ca" key ca.crt`) so errors name
// the source, and the format they were read as.
type trustSource struct {
	ref    string
	pem    []byte
	format string
}

// pemBlockCertificate is the PEM block type of an X.509 certificate.
const pemBlockCertificate = "CERTIFICATE"

// isSPIFFEBundle reports whether data is a SPIFFE trust bundle rather than
// PEM. The two cannot be confused: PEM begins with "-----BEGIN" and a bundle is
// a JSON object.
func isSPIFFEBundle(data []byte) bool {
	return bytes.HasPrefix(bytes.TrimSpace(data), []byte("{"))
}

// spiffeBundleToPEM converts a SPIFFE trust bundle, a JWK Set, to PEM: the
// x5c certificate of every key whose use is x509-svid, in order. Keys for
// other uses (jwt-svid) carry no certificate and are skipped. As the X509-SVID
// specification (section 6.2) requires, an x509-svid key whose x5c is missing
// or empty is ignored, and of several x5c values only the first is used. A
// bundle with no X.509 authority is an error, not an empty result.
func spiffeBundleToPEM(data []byte) ([]byte, error) {
	var bundle struct {
		Keys []struct {
			Use string   `json:"use"`
			X5C []string `json:"x5c"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(data, &bundle); err != nil {
		return nil, fmt.Errorf("not a SPIFFE bundle: %w", err)
	}
	var out bytes.Buffer
	jwtKeys, noX5C := 0, 0
	for i, key := range bundle.Keys {
		if key.Use == "jwt-svid" {
			jwtKeys++
		}
		if key.Use != "x509-svid" {
			continue
		}
		if len(key.X5C) == 0 {
			noX5C++
			continue
		}
		der, err := base64.StdEncoding.DecodeString(key.X5C[0])
		if err != nil {
			return nil, fmt.Errorf("x509-svid key %d: x5c: %w", i, err)
		}
		if err := pem.Encode(&out, &pem.Block{Type: pemBlockCertificate, Bytes: der}); err != nil {
			return nil, err
		}
	}
	if out.Len() == 0 {
		// Say what was found, so pointing at a JWT-only bundle by mistake is
		// recognisable from the error alone.
		return nil, fmt.Errorf("SPIFFE bundle has no x509-svid authority (it has %d key(s), %d of them jwt-svid and %d x509-svid without an x5c certificate): "+
			"only x509-svid keys with a certificate can verify client certificates", len(bundle.Keys), jwtKeys, noX5C)
	}
	return out.Bytes(), nil
}

// mergeTrustBundle concatenates the certificates of every source, in source
// order, dropping any certificate already emitted. Every source must hold at
// least one parseable certificate and nothing but certificates and whitespace:
// a source that does not is an error, never skipped, because a bundle silently
// missing a root locks out every client that root signed.
func mergeTrustBundle(sources []trustSource) ([]byte, error) {
	var out bytes.Buffer
	seen := map[string]struct{}{}
	for _, src := range sources {
		blocks, err := decodePEMStrict(src.pem)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", src.ref, err)
		}
		if len(blocks) == 0 {
			return nil, fmt.Errorf("%s: no certificates", src.ref)
		}
		for _, block := range blocks {
			if block.Type != pemBlockCertificate {
				return nil, fmt.Errorf("%s: unexpected PEM block %q", src.ref, block.Type)
			}
			if _, err := x509.ParseCertificate(block.Bytes); err != nil {
				return nil, fmt.Errorf("%s: %w", src.ref, err)
			}
			if _, dup := seen[string(block.Bytes)]; dup {
				continue
			}
			seen[string(block.Bytes)] = struct{}{}
			if err := pem.Encode(&out, &pem.Block{Type: pemBlockCertificate, Bytes: block.Bytes}); err != nil {
				return nil, err
			}
		}
	}
	return out.Bytes(), nil
}

// decodePEMStrict decodes every PEM block in data. Lines outside a block that
// carry no PEM marker are skipped, as OpenSSL skips them when Valkey loads a
// CA file, so a bundle with comments or CA names between its certificates
// (curl's cacert.pem) or an openssl x509 -text dump is accepted. Every line
// that does carry a marker must open or close a block that decodes; anything
// else is an error. pem.Decode alone is not enough: it skips any malformed
// block to reach the next one, so a corrupted root would vanish from the
// bundle without an error. A root whose BEGIN line is damaged still leaves its
// END line outside a block, and one whose END line is damaged runs into the
// next BEGIN line or the end of the data, so either is reported. Each line of
// a block is trimmed before it is decoded, so an indented block is accepted:
// the merge re-encodes every certificate, so the indentation never reaches
// Valkey.
func decodePEMStrict(data []byte) ([]*pem.Block, error) {
	var (
		blocks    []*pem.Block
		block     bytes.Buffer
		beginLine int // the line the open block began on, or 0 outside a block
		beginText string
	)
	lines := bytes.SplitAfter(data, []byte("\n"))
	for i, raw := range lines {
		n := i + 1
		line := bytes.TrimSpace(raw)
		hasBegin := bytes.Contains(line, []byte("-----BEGIN"))
		hasEnd := bytes.Contains(line, []byte("-----END"))
		if beginLine == 0 {
			switch {
			case hasEnd:
				return nil, fmt.Errorf("PEM END line outside a block at line %d: %s", n, quotePEMLine(line))
			case !hasBegin:
				continue
			case !bytes.HasPrefix(line, []byte("-----BEGIN ")) || !bytes.HasSuffix(line, []byte("-----")):
				return nil, fmt.Errorf("malformed PEM BEGIN line at line %d: %s", n, quotePEMLine(line))
			}
			beginLine, beginText = n, string(line)
			block.Reset()
			block.Write(line)
			block.WriteByte('\n')
			continue
		}
		if hasBegin {
			return nil, fmt.Errorf("unterminated PEM block %s at line %d: another BEGIN at line %d", quotePEMLine([]byte(beginText)), beginLine, n)
		}
		block.Write(line)
		block.WriteByte('\n')
		if !hasEnd {
			continue
		}
		decoded, rest := pem.Decode(block.Bytes())
		if decoded == nil || len(bytes.TrimSpace(rest)) > 0 {
			return nil, fmt.Errorf("malformed PEM block %s at lines %d-%d", quotePEMLine([]byte(beginText)), beginLine, n)
		}
		blocks = append(blocks, decoded)
		beginLine = 0
	}
	if beginLine != 0 {
		return nil, fmt.Errorf("unterminated PEM block %s at line %d: no END line", quotePEMLine([]byte(beginText)), beginLine)
	}
	return blocks, nil
}

// quotePEMLine quotes a line for an error message, shortened so that a long
// line of base64 or binary data does not swamp the message.
func quotePEMLine(line []byte) string {
	const maxLen = 64
	if len(line) > maxLen {
		return strconv.Quote(string(line[:maxLen]) + "...")
	}
	return strconv.Quote(string(line))
}
