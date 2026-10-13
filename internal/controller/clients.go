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
	"crypto/tls"
	"strings"

	vclient "github.com/valkey-io/valkey-go"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// connConfig is what a connection to a Valkey node needs beyond its address.
type connConfig struct {
	tls      *tls.Config
	username string
	password string
}

// dialValkey connects to the Valkey node at address (host:port), retrying once
// as the default user on WRONGPASS. The returned release closes the client; on
// error it is a no-op.
func dialValkey(ctx context.Context, newClient func(vclient.ClientOption) (vclient.Client, error), address string, cfg connConfig) (vclient.Client, func(), error) {
	opt := vclient.ClientOption{
		InitAddress:       []string{address},
		ForceSingleClient: true, // Don't connect to another cluster node.
		Username:          cfg.username,
		Password:          cfg.password,
		TLSConfig:         cfg.tls,
		// valkey-go defaults to data-plane sizes: up to 4 connections per
		// client, each with 0.5 MiB buffers either way and a 1024-entry ring.
		// Tuned to this controller's usage: one connection issuing a few
		// commands with no concurrency, CLUSTER NODES the largest response and
		// CLUSTER MIGRATESLOTS the largest request. Exceeding a buffer costs a
		// flush, no error.
		PipelineMultiplex:   -1, // at most 1 connection, not the default 4
		ReadBufferEachConn:  16 * 1024,
		WriteBufferEachConn: 8 * 1024,
		RingScaleEachConn:   4, // 2^4 slots, used by concurrent ops only
	}
	c, err := newClient(opt)
	if err != nil && strings.Contains(err.Error(), "WRONGPASS") {
		logf.FromContext(ctx).Info("fall back to unauthenticated default user on WRONGPASS error", "address", address)
		opt.Username = ""
		opt.Password = ""
		c, err = newClient(opt)
	}
	if err != nil {
		return nil, func() {}, err
	}
	return c, c.Close, nil
}
