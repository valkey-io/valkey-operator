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
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	vclient "github.com/valkey-io/valkey-go"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// DefaultIdleTTL is how long a pooled client may go unused before the sweep
// closes it. Well above the role poller interval, so clients in use stay open.
const DefaultIdleTTL = 2 * time.Minute

// sweepInterval is how often Start looks for idle clients.
const sweepInterval = time.Minute

// ErrPoolClosed is returned by Get after Close.
var ErrPoolClosed = errors.New("valkey client pool is closed")

// Pool keeps one long-lived client per address, shared by every caller. It
// replaces a client when its credentials or TLS inputs change and closes one
// that goes unused for the idle TTL. Callers never close a pooled client.
type Pool struct {
	mu      sync.Mutex
	entries map[string]*entry
	closed  bool
	idleTTL time.Duration
	// newClient is vclient.NewClient outside tests.
	newClient func(vclient.ClientOption) (vclient.Client, error)
	// now is time.Now; tests replace it.
	now func() time.Time
}

type entry struct {
	client    vclient.Client
	signature string
	lastUsed  time.Time
}

// NewPool returns an empty pool that dials with newClient and closes clients
// unused for idleTTL once Start is running.
func NewPool(idleTTL time.Duration, newClient func(vclient.ClientOption) (vclient.Client, error)) *Pool {
	return &Pool{
		entries:   map[string]*entry{},
		idleTTL:   idleTTL,
		newClient: newClient,
		now:       time.Now,
	}
}

// signatureOf fingerprints the inputs a client was built from. Each field is
// length-prefixed so none can run into the next, and the result is hashed so
// the password is not kept in the pool.
func signatureOf(fields ...string) string {
	h := sha256.New()
	for _, f := range fields {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(f)))
		h.Write(n[:])
		h.Write([]byte(f))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// IsWrongPass reports whether err, or an error it wraps, is Valkey rejecting
// the client's username or password: a server error whose leading token is
// WRONGPASS.
func IsWrongPass(err error) bool {
	for err != nil {
		if msg := err.Error(); msg == "WRONGPASS" || strings.HasPrefix(msg, "WRONGPASS ") {
			return true
		}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			return slices.ContainsFunc(joined.Unwrap(), IsWrongPass)
		}
		err = errors.Unwrap(err)
	}
	return false
}

// Get returns the pooled client for opt's single address, dialling a new one
// when there is none or when opt's credentials or tlsToken no longer match.
// tlsToken must change whenever opt.TLSConfig does; the pool does not inspect
// TLSConfig. A failed dial, WRONGPASS included, returns its error and leaves
// any pooled client in place.
func (p *Pool) Get(ctx context.Context, opt vclient.ClientOption, tlsToken string) (vclient.Client, error) {
	if len(opt.InitAddress) != 1 {
		return nil, fmt.Errorf("pool needs exactly one address, got %d", len(opt.InitAddress))
	}
	address := opt.InitAddress[0]
	signature := signatureOf(opt.Username, opt.Password, tlsToken)

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrPoolClosed
	}
	if e := p.entries[address]; e != nil && e.signature == signature {
		e.lastUsed = p.now()
		c := e.client
		p.mu.Unlock()
		return c, nil
	}
	p.mu.Unlock()

	c, err := p.newClient(opt)
	if err != nil {
		return nil, err
	}
	return p.store(address, signature, c)
}

// store pools c for address and closes the client it replaces. If another
// caller already pooled a client for the same inputs, store keeps that one and
// closes c.
func (p *Pool) store(address, signature string, c vclient.Client) (vclient.Client, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		c.Close()
		return nil, ErrPoolClosed
	}
	var replaced vclient.Client
	if e := p.entries[address]; e != nil {
		if e.signature == signature {
			e.lastUsed = p.now()
			kept := e.client
			p.mu.Unlock()
			c.Close()
			return kept, nil
		}
		replaced = e.client
	}
	p.entries[address] = &entry{client: c, signature: signature, lastUsed: p.now()}
	p.mu.Unlock()
	if replaced != nil {
		replaced.Close()
	}
	return c, nil
}

// Start closes idle clients every sweepInterval until ctx is done. It leaves
// the rest open; Close shuts them.
func (p *Pool) Start(ctx context.Context) error {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if n := p.sweep(); n > 0 {
				logf.FromContext(ctx).V(1).Info("closed idle Valkey clients", "count", n)
			}
		}
	}
}

// NeedLeaderElection makes the pool a leader-election Runnable: added to the
// manager, it sweeps only on the elected leader. Its callers, the reconcilers
// and the role poller, only run there.
func (p *Pool) NeedLeaderElection() bool {
	return true
}

// sweep closes every client unused for idleTTL and returns how many.
func (p *Pool) sweep() int {
	p.mu.Lock()
	cutoff := p.now().Add(-p.idleTTL)
	var idle []vclient.Client
	for address, e := range p.entries {
		if e.lastUsed.Before(cutoff) {
			idle = append(idle, e.client)
			delete(p.entries, address)
		}
	}
	p.mu.Unlock()
	for _, c := range idle {
		c.Close()
	}
	return len(idle)
}

// Close closes every pooled client. Get fails with ErrPoolClosed afterwards.
func (p *Pool) Close() {
	p.mu.Lock()
	p.closed = true
	all := make([]vclient.Client, 0, len(p.entries))
	for _, e := range p.entries {
		all = append(all, e.client)
	}
	p.entries = map[string]*entry{}
	p.mu.Unlock()
	for _, c := range all {
		c.Close()
	}
}
