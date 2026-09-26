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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	vclient "github.com/valkey-io/valkey-go"
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
