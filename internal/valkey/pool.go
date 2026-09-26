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
	client vclient.Client
	sig    string
	// fallback marks a client dialled as the default user after WRONGPASS.
	fallback bool
	lastUsed time.Time
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

// signature fingerprints the inputs a client was built from. Each field is
// length-prefixed so none can run into the next, and the result is hashed so
// the password is not kept in the pool.
func signature(fields ...string) string {
	h := sha256.New()
	for _, f := range fields {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(f)))
		h.Write(n[:])
		h.Write([]byte(f))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func isWrongPass(err error) bool {
	return err != nil && strings.Contains(err.Error(), "WRONGPASS")
}

// Get returns the pooled client for opt's single address, dialling a new one
// when there is none or when opt's credentials or tlsToken no longer match.
// tlsToken must change whenever opt.TLSConfig does; the pool does not inspect
// TLSConfig. On WRONGPASS it dials as the default user and pools that client
// as a fallback, which each later Get tries to replace with one dialled from
// opt.
func (p *Pool) Get(ctx context.Context, opt vclient.ClientOption, tlsToken string) (vclient.Client, error) {
	if len(opt.InitAddress) != 1 {
		return nil, fmt.Errorf("pool needs exactly one address, got %d", len(opt.InitAddress))
	}
	address := opt.InitAddress[0]
	sig := signature(opt.Username, opt.Password, tlsToken)

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrPoolClosed
	}
	if e := p.entries[address]; e != nil && e.sig == sig && !e.fallback {
		e.lastUsed = p.now()
		c := e.client
		p.mu.Unlock()
		return c, nil
	}
	p.mu.Unlock()

	c, err := p.newClient(opt)
	fallback := false
	if isWrongPass(err) {
		// Still locked out: keep handing out the client we already have.
		if kept := p.lookup(address, sig); kept != nil {
			return kept, nil
		}
		logf.FromContext(ctx).Info("fall back to unauthenticated default user on WRONGPASS error", "address", address)
		opt.Username = ""
		opt.Password = ""
		c, err = p.newClient(opt)
		fallback = true
	}
	if err != nil {
		return nil, err
	}
	return p.store(address, sig, c, fallback)
}

// lookup returns the pooled client for address if it was built from sig.
func (p *Pool) lookup(address, sig string) vclient.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.entries[address]; e != nil && e.sig == sig {
		e.lastUsed = p.now()
		return e.client
	}
	return nil
}

// store pools c for address and closes the client it replaces. If another
// caller already pooled a client for the same inputs, and c would not upgrade
// a fallback, store keeps that one and closes c.
func (p *Pool) store(address, sig string, c vclient.Client, fallback bool) (vclient.Client, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		c.Close()
		return nil, ErrPoolClosed
	}
	var replaced vclient.Client
	if e := p.entries[address]; e != nil {
		if e.sig == sig && (!e.fallback || fallback) {
			e.lastUsed = p.now()
			kept := e.client
			p.mu.Unlock()
			c.Close()
			return kept, nil
		}
		replaced = e.client
	}
	p.entries[address] = &entry{client: c, sig: sig, fallback: fallback, lastUsed: p.now()}
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

// NeedLeaderElection reports that only the elected leader sweeps. Its
// callers, the reconcilers and the role poller, only run there.
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
