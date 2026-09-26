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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	// It returns an error only when the operator password cannot be read. A
	// TLS config that cannot be built fails each dial instead.
	ForCluster(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) (valkey.DialFunc, error)

	// ForNode connects to the node's pod. It returns an error when the node
	// has no pod IP, the TLS config cannot be built, the operator password
	// cannot be read for a reason other than the secret not existing, or the
	// dial fails. On error the client is nil and release is a no-op.
	ForNode(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) (vclient.Client, func(), error)
}

// NewClientProvider returns a ClientProvider that dials a new client per call
// and closes it on release. c reads the operator password secret, apiReader
// the TLS secret.
func NewClientProvider(c client.Client, apiReader client.Reader) ClientProvider {
	return &unpooledProvider{client: c, apiReader: apiReader, newClient: vclient.NewClient}
}

type unpooledProvider struct {
	client    client.Client
	apiReader client.Reader
	// newClient is vclient.NewClient; tests replace it.
	newClient func(vclient.ClientOption) (vclient.Client, error)
}

// tlsConfig returns nil when spec names no server certificate secret.
func (p *unpooledProvider) tlsConfig(ctx context.Context, namespace string, spec *valkeyiov1alpha1.NodeTLSSpec) (*tls.Config, error) {
	if spec == nil || spec.Certificates.Server.SecretName == "" {
		return nil, nil
	}
	return getTLSConfig(ctx, p.apiReader, spec.Certificates.Server.SecretName, spec.ServerName, namespace, spec.RequiresClientCertificate())
}

func (p *unpooledProvider) ForCluster(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) (valkey.DialFunc, error) {
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

func (p *unpooledProvider) ForNode(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) (vclient.Client, func(), error) {
	if node.Status.PodIP == "" {
		return nil, func() {}, fmt.Errorf("node %s has no pod IP", node.Name)
	}
	tlsSpec := node.Spec.TLS
	if tlsSpec != nil {
		tlsSpec = tlsSpec.DeepCopy()
		tlsSpec.ServerName = nodeTLSServerName(node)
	}
	tlsCfg, err := p.tlsConfig(ctx, node.Namespace, tlsSpec)
	if err != nil {
		return nil, func() {}, fmt.Errorf("TLS config: %w", err)
	}
	cfg := connConfig{tls: tlsCfg}

	// A node outside a cluster has no operator user, and a cluster's password
	// secret does not exist until the cluster controller writes it. Both dial
	// as the default user.
	if clusterName, ok := node.Labels[LabelCluster]; ok {
		password, err := fetchSystemUserPassword(ctx, operatorUser, p.client, clusterName, node.Namespace)
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, func() {}, fmt.Errorf("operator password: %w", err)
		}
		if password != "" {
			cfg.username = operatorUser
			cfg.password = password
		}
	}

	return dialValkey(ctx, p.newClient, fmt.Sprintf("%s:%d", node.Status.PodIP, DefaultPort), cfg)
}

// valkeyClients returns r.ValkeyClients, or a provider built from the
// reconciler's own readers when it is unset.
func (r *ValkeyClusterReconciler) valkeyClients() ClientProvider {
	if r.ValkeyClients != nil {
		return r.ValkeyClients
	}
	return NewClientProvider(r.Client, r.APIReader)
}

// valkeyClients returns r.ValkeyClients, or a provider built from the
// reconciler's own readers when it is unset.
func (r *ValkeyNodeReconciler) valkeyClients() ClientProvider {
	if r.ValkeyClients != nil {
		return r.ValkeyClients
	}
	return NewClientProvider(r.Client, r.APIReader)
}
