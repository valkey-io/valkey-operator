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
	"crypto/tls"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	vclient "github.com/valkey-io/valkey-go"
)

var errWrongPass = errors.New("WRONGPASS invalid username-password pair or user is disabled")

// fakeClient satisfies vclient.Client for pool tests. Only Close is
// implemented; any other method panics on the nil embed.
type fakeClient struct {
	vclient.Client
	mu     sync.Mutex
	closed int
}

func (f *fakeClient) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
}

func (f *fakeClient) closes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// fakeDialer stands in for vclient.NewClient. Each call records its option
// and takes the next error from errs; a nil error, or none left, returns a
// new fakeClient.
type fakeDialer struct {
	mu   sync.Mutex
	opts []vclient.ClientOption
	errs []error
	made []*fakeClient
	// gate, when set, holds every dial until it is closed.
	gate chan struct{}
}

func (d *fakeDialer) newClient(opt vclient.ClientOption) (vclient.Client, error) {
	if d.gate != nil {
		<-d.gate
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.opts = append(d.opts, opt)
	if len(d.errs) > 0 {
		err := d.errs[0]
		d.errs = d.errs[1:]
		if err != nil {
			return nil, err
		}
	}
	c := &fakeClient{}
	d.made = append(d.made, c)
	return c, nil
}

func (d *fakeDialer) dials() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.opts)
}

// testPool returns a pool that dials through d, and the clock it reads.
func testPool(d *fakeDialer) (*Pool, *time.Time) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	p := NewPool(DefaultIdleTTL, d.newClient)
	p.now = func() time.Time { return now }
	return p, &now
}

func option(address, password string) vclient.ClientOption {
	return vclient.ClientOption{InitAddress: []string{address}, Username: "_operator", Password: password}
}

func TestSignatureSeparatesFields(t *testing.T) {
	assert.NotEqual(t, signature("a", "bc", ""), signature("ab", "c", ""))
	assert.Equal(t, signature("_operator", "pw", "t"), signature("_operator", "pw", "t"))
	assert.NotContains(t, signature("_operator", "secret-password", ""), "secret-password")
}

func TestPoolGetReusesClient(t *testing.T) {
	d := &fakeDialer{}
	p, _ := testPool(d)
	ctx := context.Background()

	c1, err := p.Get(ctx, option("10.0.0.1:6379", "pw"), "")
	require.NoError(t, err)
	c2, err := p.Get(ctx, option("10.0.0.1:6379", "pw"), "")
	require.NoError(t, err)

	assert.Same(t, c1, c2)
	assert.Equal(t, 1, d.dials())
	assert.Zero(t, d.made[0].closes())
}

func TestPoolGetKeysByAddress(t *testing.T) {
	d := &fakeDialer{}
	p, _ := testPool(d)
	ctx := context.Background()

	c1, err := p.Get(ctx, option("10.0.0.1:6379", "pw"), "")
	require.NoError(t, err)
	c2, err := p.Get(ctx, option("10.0.0.2:6379", "pw"), "")
	require.NoError(t, err)

	assert.NotSame(t, c1, c2)
	assert.Equal(t, 2, d.dials())
}

func TestPoolGetNeedsOneAddress(t *testing.T) {
	d := &fakeDialer{}
	p, _ := testPool(d)

	c, err := p.Get(context.Background(), vclient.ClientOption{}, "")
	require.Error(t, err)
	assert.Nil(t, c)
	assert.Zero(t, d.dials())
}

func TestPoolGetRebuildsOnChangedInputs(t *testing.T) {
	ctx := context.Background()
	const token = "vc-tls/1/vc.ns.svc/false"
	for name, next := range map[string]struct{ password, token string }{
		"password":  {password: "new", token: token},
		"TLS token": {password: "pw", token: "vc-tls/2/vc.ns.svc/false"},
	} {
		t.Run(name, func(t *testing.T) {
			d := &fakeDialer{}
			p, _ := testPool(d)
			old, err := p.Get(ctx, option("10.0.0.1:6379", "pw"), token)
			require.NoError(t, err)

			c, err := p.Get(ctx, option("10.0.0.1:6379", next.password), next.token)
			require.NoError(t, err)
			assert.NotSame(t, old, c)
			assert.Equal(t, 2, d.dials())
			assert.Equal(t, 1, d.made[0].closes())

			again, err := p.Get(ctx, option("10.0.0.1:6379", next.password), next.token)
			require.NoError(t, err)
			assert.Same(t, c, again)
			assert.Equal(t, 2, d.dials())
		})
	}
}

func TestPoolGetDialErrorStoresNothing(t *testing.T) {
	ctx := context.Background()
	refused := errors.New("dial tcp 10.0.0.1:6379: connect: connection refused")

	t.Run("nothing pooled", func(t *testing.T) {
		d := &fakeDialer{errs: []error{refused}}
		p, _ := testPool(d)

		c, err := p.Get(ctx, option("10.0.0.1:6379", "pw"), "")
		require.ErrorIs(t, err, refused)
		assert.Nil(t, c)

		_, err = p.Get(ctx, option("10.0.0.1:6379", "pw"), "")
		require.NoError(t, err)
		assert.Equal(t, 2, d.dials())
	})

	t.Run("existing client kept", func(t *testing.T) {
		d := &fakeDialer{errs: []error{nil, refused}}
		p, _ := testPool(d)
		old, err := p.Get(ctx, option("10.0.0.1:6379", "pw"), "")
		require.NoError(t, err)

		_, err = p.Get(ctx, option("10.0.0.1:6379", "new"), "")
		require.ErrorIs(t, err, refused)

		c, err := p.Get(ctx, option("10.0.0.1:6379", "pw"), "")
		require.NoError(t, err)
		assert.Same(t, old, c)
		assert.Equal(t, 2, d.dials())
		assert.Zero(t, d.made[0].closes())
	})
}

func TestPoolGetWrongPassFallback(t *testing.T) {
	ctx := context.Background()
	opt := option("10.0.0.1:6379", "pw")

	t.Run("dials as the default user and pools the client", func(t *testing.T) {
		d := &fakeDialer{errs: []error{errWrongPass}}
		p, _ := testPool(d)

		c, err := p.Get(ctx, opt, "")
		require.NoError(t, err)
		require.Equal(t, 2, d.dials())
		assert.Equal(t, opt.InitAddress, d.opts[1].InitAddress)
		assert.Empty(t, d.opts[1].Username)
		assert.Empty(t, d.opts[1].Password)
		assert.Same(t, d.made[0], c)
	})

	t.Run("replaces the fallback once the operator user works", func(t *testing.T) {
		d := &fakeDialer{errs: []error{errWrongPass}}
		p, _ := testPool(d)
		fallback, err := p.Get(ctx, opt, "")
		require.NoError(t, err)

		c, err := p.Get(ctx, opt, "")
		require.NoError(t, err)
		assert.NotSame(t, fallback, c)
		assert.Equal(t, "pw", d.opts[2].Password)
		assert.Equal(t, 1, d.made[0].closes())

		again, err := p.Get(ctx, opt, "")
		require.NoError(t, err)
		assert.Same(t, c, again)
		assert.Equal(t, 3, d.dials())
	})

	t.Run("keeps the fallback while WRONGPASS persists", func(t *testing.T) {
		d := &fakeDialer{errs: []error{errWrongPass, nil, errWrongPass}}
		p, _ := testPool(d)
		fallback, err := p.Get(ctx, opt, "")
		require.NoError(t, err)

		c, err := p.Get(ctx, opt, "")
		require.NoError(t, err)
		assert.Same(t, fallback, c)
		assert.Equal(t, 3, d.dials(), "one operator attempt, no second fallback dial")
		assert.Zero(t, d.made[0].closes())
	})

	t.Run("returns other errors from the operator attempt", func(t *testing.T) {
		refused := errors.New("dial tcp 10.0.0.1:6379: connect: connection refused")
		d := &fakeDialer{errs: []error{errWrongPass, nil, refused}}
		p, _ := testPool(d)
		_, err := p.Get(ctx, opt, "")
		require.NoError(t, err)

		c, err := p.Get(ctx, opt, "")
		require.ErrorIs(t, err, refused)
		assert.Nil(t, c)
		assert.Zero(t, d.made[0].closes())
	})

	t.Run("returns the fallback dial's error and pools nothing", func(t *testing.T) {
		noauth := errors.New("NOAUTH Authentication required.")
		d := &fakeDialer{errs: []error{errWrongPass, noauth}}
		p, _ := testPool(d)

		c, err := p.Get(ctx, opt, "")
		require.ErrorIs(t, err, noauth)
		assert.Nil(t, c)

		_, err = p.Get(ctx, opt, "")
		require.NoError(t, err)
		assert.Equal(t, 3, d.dials())
	})

	t.Run("a fallback never replaces a working client for the same inputs", func(t *testing.T) {
		d := &fakeDialer{}
		p, _ := testPool(d)
		good, err := p.Get(ctx, opt, "")
		require.NoError(t, err)

		late := &fakeClient{}
		c, err := p.store("10.0.0.1:6379", signature(opt.Username, opt.Password, ""), late, true)
		require.NoError(t, err)
		assert.Same(t, good, c)
		assert.Equal(t, 1, late.closes())
	})
}

func TestPoolGetConcurrentCallersShareOneClient(t *testing.T) {
	d := &fakeDialer{gate: make(chan struct{})}
	p, _ := testPool(d)
	const callers = 8
	got := make([]vclient.Client, callers)

	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			c, err := p.Get(context.Background(), option("10.0.0.1:6379", "pw"), "")
			assert.NoError(t, err)
			got[i] = c
		})
	}
	time.Sleep(10 * time.Millisecond) // let most callers reach the dial
	close(d.gate)
	wg.Wait()

	for _, c := range got {
		assert.Same(t, got[0], c)
	}
	open := 0
	for _, c := range d.made {
		switch c.closes() {
		case 0:
			open++
		case 1:
		default:
			t.Errorf("client closed %d times", c.closes())
		}
	}
	assert.Equal(t, 1, open, "one client stays pooled and every duplicate is closed")
}

func TestPoolSweepClosesIdleClients(t *testing.T) {
	d := &fakeDialer{}
	p, clock := testPool(d)
	ctx := context.Background()

	idle, err := p.Get(ctx, option("10.0.0.1:6379", "pw"), "")
	require.NoError(t, err)
	*clock = clock.Add(90 * time.Second)
	fresh, err := p.Get(ctx, option("10.0.0.2:6379", "pw"), "")
	require.NoError(t, err)
	*clock = clock.Add(60 * time.Second)

	assert.Equal(t, 1, p.sweep())
	assert.Equal(t, 1, d.made[0].closes())
	assert.Zero(t, d.made[1].closes())

	c, err := p.Get(ctx, option("10.0.0.2:6379", "pw"), "")
	require.NoError(t, err)
	assert.Same(t, fresh, c)
	c, err = p.Get(ctx, option("10.0.0.1:6379", "pw"), "")
	require.NoError(t, err)
	assert.NotSame(t, idle, c)
	assert.Equal(t, 3, d.dials())
}

func TestPoolStartSweepsOnlyUntilCancelled(t *testing.T) {
	d := &fakeDialer{}
	p, _ := testPool(d)
	_, err := p.Get(context.Background(), option("10.0.0.1:6379", "pw"), "")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Start(ctx) }()
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
	assert.Zero(t, d.made[0].closes(), "Start leaves closing to Close")
	assert.True(t, p.NeedLeaderElection())
}

func TestPoolClose(t *testing.T) {
	d := &fakeDialer{}
	p, _ := testPool(d)
	ctx := context.Background()
	for _, address := range []string{"10.0.0.1:6379", "10.0.0.2:6379"} {
		_, err := p.Get(ctx, option(address, "pw"), "")
		require.NoError(t, err)
	}

	p.Close()
	for _, c := range d.made {
		assert.Equal(t, 1, c.closes())
	}

	c, err := p.Get(ctx, option("10.0.0.1:6379", "pw"), "")
	require.ErrorIs(t, err, ErrPoolClosed)
	assert.Nil(t, c)
	assert.Equal(t, 2, d.dials())
}

func TestPoolGetPassesTheOptionThrough(t *testing.T) {
	d := &fakeDialer{}
	p, _ := testPool(d)
	opt := option("10.0.0.1:6379", "pw")
	opt.TLSConfig = &tls.Config{ServerName: "vc.ns.svc", MinVersion: tls.VersionTLS12}
	opt.ForceSingleClient = true
	opt.PipelineMultiplex = -1
	opt.ReadBufferEachConn = 16 * 1024

	_, err := p.Get(context.Background(), opt, "vc-tls/1/vc.ns.svc/false")
	require.NoError(t, err)
	require.Len(t, d.opts, 1)
	assert.Equal(t, opt, d.opts[0])
}
