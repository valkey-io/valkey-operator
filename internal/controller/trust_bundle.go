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
// other uses (jwt-svid) carry no certificate and are skipped. A bundle with
// no X.509 authority is an error, not an empty result.
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
	jwtKeys := 0
	for i, key := range bundle.Keys {
		if key.Use == "jwt-svid" {
			jwtKeys++
		}
		if key.Use != "x509-svid" {
			continue
		}
		if len(key.X5C) == 0 {
			return nil, fmt.Errorf("x509-svid key %d has no x5c certificate", i)
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
		return nil, fmt.Errorf("SPIFFE bundle has no x509-svid authority (it has %d key(s), %d of them jwt-svid): "+
			"only x509-svid keys can verify client certificates", len(bundle.Keys), jwtKeys)
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

// decodePEMStrict decodes every PEM block in data, allowing nothing but
// whitespace between them. pem.Decode alone is not enough: it skips any text
// before a block, and any malformed block, to reach the next one, so a
// corrupted root would vanish from the bundle without an error.
func decodePEMStrict(data []byte) ([]*pem.Block, error) {
	var blocks []*pem.Block
	rest := data
	for {
		trimmed := bytes.TrimLeft(rest, " \t\r\n")
		if len(trimmed) == 0 {
			return blocks, nil
		}
		offset := len(data) - len(trimmed)
		if !bytes.HasPrefix(trimmed, []byte("-----BEGIN ")) {
			return nil, fmt.Errorf("data that is not PEM at byte %d", offset)
		}
		// Decode exactly one block: up to the end of the first END line, so a
		// malformed block cannot be skipped in favour of a later one.
		end := bytes.Index(trimmed, []byte("-----END "))
		// A second BEGIN before the first END means this block never ended:
		// the END belongs to a later block.
		if end < 0 || bytes.Contains(trimmed[len("-----BEGIN "):end], []byte("-----BEGIN ")) {
			return nil, fmt.Errorf("unterminated PEM block at byte %d", offset)
		}
		segment, next := trimmed, []byte(nil)
		if nl := bytes.IndexByte(trimmed[end:], '\n'); nl >= 0 {
			segment, next = trimmed[:end+nl+1], trimmed[end+nl+1:]
		}
		block, after := pem.Decode(segment)
		if block == nil || len(bytes.TrimSpace(after)) > 0 {
			return nil, fmt.Errorf("malformed PEM block at byte %d", offset)
		}
		blocks = append(blocks, block)
		rest = next
	}
}
