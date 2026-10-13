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
	// clientCertFallback presents the node's own server certificate as a
	// client certificate. During a clientAuth roll (Required to Optional or
	// Disabled) pods that have not restarted yet still demand a client
	// certificate the primary configuration no longer presents; a dial
	// refused at the handshake is retried once with this configuration so
	// the roll keeps moving instead of stalling in Reconciling. It is nil
	// unless clientAuth is Optional or Disabled, and is only ever used for
	// that retry, keeping the certificateUser ACL posture documented in
	// docs/mtls.md intact.
	clientCertFallback *tls.Config
}

// clientCertRefusedErrors lists the handshake-refusal spellings that arm the
// clientAuth-roll fallback retry. TLS 1.3 surfaces the server's demand as
// "certificate required"; the certificate_required alert does not exist in
// TLS 1.2, so a node enforcing clientAuth.mode: Required on a
// tls-protocols: TLSv1.2 port instead refuses the dial with a generic
// "handshake failure". Both spellings mean the same pod state during a
// clientAuth roll, so both arm the retry.
var clientCertRefusedErrors = []string{"certificate required", "handshake failure"}

// clientCertificateRefused reports whether err is a dial refusal caused by
// the server demanding a client certificate the configuration did not
// present -- the posture of a pod still enforcing clientAuth.mode:
// Required from before a clientAuth roll. It matches the TLS 1.3
// "certificate required" alert and the TLS 1.2 "handshake failure" form.
func clientCertificateRefused(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, clientCertRefusedErrors[0]) || strings.Contains(msg, clientCertRefusedErrors[1])
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
	if err != nil && cfg.clientCertFallback != nil && clientCertificateRefused(err) {
		// The node has not been rolled yet and still demands a client
		// certificate; retry presenting the node's own server certificate.
		logf.FromContext(ctx).Info("node still requires a client certificate, retrying with one",
			"address", address)
		opt.TLSConfig = cfg.clientCertFallback
		c, err = newClient(opt)
	}
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

// tlsConfig returns nil when spec names no server certificate secret. Unless
// client certificates are required -- the primary configuration already
// presents one -- it also returns the clientAuth-roll fallback described in
// docs/mtls.md: a configuration presenting the node's own server certificate,
// used only to retry dials refused by pods still enforcing the previous
// clientAuth.mode: Required from before a roll.
func (p *unpooledProvider) tlsConfig(ctx context.Context, namespace string, spec *valkeyiov1alpha1.NodeTLSSpec) (*tls.Config, *tls.Config, error) {
	if spec == nil || spec.Certificates.Server.SecretName == "" {
		return nil, nil, nil
	}
	present := spec.RequiresClientCertificate()
	return getTLSConfigWithFallback(ctx, p.apiReader, spec.Certificates.Server.SecretName, spec.ServerName, namespace, present, !present)
}

func (p *unpooledProvider) ForCluster(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) (valkey.DialFunc, error) {
	password, err := fetchSystemUserPassword(ctx, operatorUser, p.client, cluster.Name, cluster.Namespace)
	if err != nil {
		return nil, fmt.Errorf("operator password: %w", err)
	}
	cfg := connConfig{username: operatorUser, password: password}

	tlsSpec := nodeTLSFromCluster(cluster)
	tlsCfg, clientCertFallback, err := p.tlsConfig(ctx, cluster.Namespace, tlsSpec)
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
	cfg.clientCertFallback = clientCertFallback

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
	tlsCfg, clientCertFallback, err := p.tlsConfig(ctx, node.Namespace, tlsSpec)
	if err != nil {
		return nil, func() {}, fmt.Errorf("TLS config: %w", err)
	}
	// Arm the client-cert fallback for the node dial exactly like ForCluster:
	// during a clientAuth roll, pods that have not restarted yet still demand
	// a client certificate, and dialValkey retries such refusals once with it.
	cfg := connConfig{tls: tlsCfg, clientCertFallback: clientCertFallback}

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
