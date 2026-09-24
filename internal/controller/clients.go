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
	"fmt"
	"strings"

	vclient "github.com/valkey-io/valkey-go"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
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

// ClientProvider hands out connected Valkey clients. Callers call the release
// func they are given when done, and never call Close on a client.
type ClientProvider interface {
	// ForCluster resolves the cluster's TLS config and operator credentials
	// once and returns a DialFunc that connects to any of its nodes with them.
	ForCluster(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) (valkey.DialFunc, error)
}

// NewClientProvider returns a ClientProvider that dials a new client per call.
// c reads the operator password secret, apiReader the TLS secret.
func NewClientProvider(c client.Client, apiReader client.Reader) ClientProvider {
	return &nodeClientProvider{client: c, apiReader: apiReader, newClient: vclient.NewClient}
}

type nodeClientProvider struct {
	client    client.Client
	apiReader client.Reader
	// newClient is vclient.NewClient; tests replace it.
	newClient func(vclient.ClientOption) (vclient.Client, error)
}

// tlsConfig returns nil when spec names no server certificate secret.
func (p *nodeClientProvider) tlsConfig(ctx context.Context, namespace string, spec *valkeyiov1alpha1.NodeTLSSpec) (*tls.Config, error) {
	if spec == nil || spec.Certificates.Server.SecretName == "" {
		return nil, nil
	}
	return getTLSConfig(ctx, p.apiReader, spec.Certificates.Server.SecretName, spec.ServerName, namespace, spec.RequiresClientCertificate())
}

func (p *nodeClientProvider) ForCluster(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) (valkey.DialFunc, error) {
	password, err := fetchSystemUserPassword(ctx, operatorUser, p.client, cluster.Name, cluster.Namespace)
	if err != nil {
		return nil, fmt.Errorf("operator password: %w", err)
	}
	cfg := connConfig{username: operatorUser, password: password}

	tlsSpec := nodeTLSFromCluster(cluster)
	tlsCfg, err := p.tlsConfig(ctx, cluster.Namespace, tlsSpec)
	if err != nil {
		// Fail each dial, not the caller: the cluster reconcile still has
		// ValkeyNodes to create while the secret is being issued.
		logf.FromContext(ctx).Error(err, "failed to build TLS config for cluster state",
			"secretName", tlsSpec.Certificates.Server.SecretName)
		tlsErr := fmt.Errorf("TLS config: %w", err)
		return func(context.Context, string) (vclient.Client, func(), error) {
			return nil, func() {}, tlsErr
		}, nil
	}
	cfg.tls = tlsCfg

	return func(ctx context.Context, address string) (vclient.Client, func(), error) {
		return dialValkey(ctx, p.newClient, address, cfg)
	}, nil
}

// valkeyClients returns r.ValkeyClients, or a provider built from the
// reconciler's own readers when it is unset.
func (r *ValkeyClusterReconciler) valkeyClients() ClientProvider {
	if r.ValkeyClients != nil {
		return r.ValkeyClients
	}
	return NewClientProvider(r.Client, r.APIReader)
}
