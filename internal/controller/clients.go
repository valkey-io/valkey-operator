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
	"net"
	"strconv"
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
// clientAuth-roll fallback retry. TLS 1.3 surfaces the server demand as
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

// clientOption is the option for a single-node client to address (host:port).
func clientOption(address string, cfg connConfig) vclient.ClientOption {
	return vclient.ClientOption{
		InitAddress:       []string{address},
		ForceSingleClient: true,              // Don't connect to another cluster node.
		ClientName:        "valkey-operator", // Names the connection in CLIENT LIST.
		Username:          cfg.username,
		Password:          cfg.password,
		TLSConfig:         cfg.tls,
		// valkey-go defaults to data-plane sizes: up to 4 connections per
		// client, each with 0.5 MiB buffers either way and a 1024-entry ring.
		// Tuned to this controller's usage: one connection shared by the
		// poller and both reconcilers, each issuing a few commands, CLUSTER
		// NODES the largest response and CLUSTER MIGRATESLOTS the largest
		// request. Exceeding a buffer costs a flush, no error.
		PipelineMultiplex:   -1, // at most 1 connection, not the default 4
		ReadBufferEachConn:  16 * 1024,
		WriteBufferEachConn: 8 * 1024,
		RingScaleEachConn:   4, // 2^4 slots, used by concurrent ops only
	}
}

// ClientProvider hands out pooled Valkey clients. The provider owns every
// client it returns; callers never call Close on one.
type ClientProvider interface {
	// ForCluster resolves the cluster's TLS config and operator credentials
	// once and returns a DialFunc that returns the pooled client for any of
	// its nodes. It returns an error only when the operator password cannot be
	// read. A TLS config that cannot be built fails each dial instead.
	ForCluster(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) (valkey.DialFunc, error)

	// ForNode returns the pooled client for the node's pod. It returns an
	// error when the node has no pod IP, the TLS config cannot be built, the
	// operator password cannot be read for a reason other than the secret not
	// existing, or the dial fails. On error the client is nil.
	ForNode(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) (vclient.Client, error)
}

// NewClientProvider returns a ClientProvider that takes its clients from
// pool. c reads the operator password secret, apiReader the TLS secret.
func NewClientProvider(c client.Client, apiReader client.Reader, pool *valkey.Pool) ClientProvider {
	return &provider{client: c, apiReader: apiReader, pool: pool}
}

type provider struct {
	client    client.Client
	apiReader client.Reader
	pool      *valkey.Pool
}

// tlsConfig returns the TLS config for spec and a token that changes whenever
// the config would. Both are empty when spec names no server certificate
// secret.
func (p *provider) tlsConfig(ctx context.Context, namespace string, spec *valkeyiov1alpha1.NodeTLSSpec) (*tls.Config, *tls.Config, string, error) {
	if spec == nil || spec.Certificates.Server.SecretName == "" {
		return nil, nil, "", nil
	}
	name := spec.Certificates.Server.SecretName
	clientCert := spec.RequiresClientCertificate()
	cfg, fallback, resourceVersion, err := getTLSConfigWithFallback(ctx, p.apiReader, name, spec.ServerName, namespace, clientCert, !clientCert)
	if err != nil {
		return nil, nil, "", err
	}
	return cfg, fallback, fmt.Sprintf("%s/%s/%s/%t", name, resourceVersion, spec.ServerName, clientCert), nil
}

// dialPooled returns the pooled client for address built from cfg, retrying
// once with the clientAuth-roll fallback when the dial is refused by a pod
// that still demands a client certificate. The retry dials under the token
// plus a client-cert suffix, so it pools separately: later dials to the
// same not-yet-rolled pod reuse the fallback client instead of repeating
// the refused handshake, and the pool drops it when the secret changes or
// it goes idle.
func (p *provider) dialPooled(ctx context.Context, address string, cfg connConfig, token string) (vclient.Client, error) {
	c, err := p.pool.Get(ctx, clientOption(address, cfg), token)
	if err != nil && cfg.clientCertFallback != nil && clientCertificateRefused(err) {
		logf.FromContext(ctx).Info("node still requires a client certificate, retrying with one",
			"address", address)
		retry := cfg
		retry.tls = cfg.clientCertFallback
		retry.clientCertFallback = nil
		return p.pool.Get(ctx, clientOption(address, retry), token+"/client-cert")
	}
	return c, err
}

func (p *provider) ForCluster(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) (valkey.DialFunc, error) {
	password, err := fetchSystemUserPassword(ctx, operatorUser, p.client, cluster.Name, cluster.Namespace)
	if err != nil {
		return nil, fmt.Errorf("operator password: %w", err)
	}
	cfg := connConfig{username: operatorUser, password: password}

	tlsSpec := nodeTLSFromCluster(cluster)
	tlsCfg, fallbackCfg, token, err := p.tlsConfig(ctx, cluster.Namespace, tlsSpec)
	if err != nil {
		// Fail each dial, not the caller: the cluster reconcile still has
		// ValkeyNodes to create while the secret is being issued.
		logf.FromContext(ctx).Error(err, "failed to build TLS config for cluster state",
			"secretName", tlsSpec.Certificates.Server.SecretName)
		tlsErr := fmt.Errorf("TLS config: %w", err)
		return func(context.Context, string) (vclient.Client, error) {
			return nil, tlsErr
		}, nil
	}
	cfg.tls = tlsCfg
	cfg.clientCertFallback = fallbackCfg

	return func(ctx context.Context, address string) (vclient.Client, error) {
		return p.dialPooled(ctx, address, cfg, token)
	}, nil
}

func (p *provider) ForNode(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) (vclient.Client, error) {
	if node.Status.PodIP == "" {
		return nil, fmt.Errorf("node %s has no pod IP", node.Name)
	}
	tlsSpec := node.Spec.TLS
	if tlsSpec != nil {
		tlsSpec = tlsSpec.DeepCopy()
		tlsSpec.ServerName = nodeTLSServerName(node)
	}
	tlsCfg, fallbackCfg, token, err := p.tlsConfig(ctx, node.Namespace, tlsSpec)
	if err != nil {
		return nil, fmt.Errorf("TLS config: %w", err)
	}
	cfg := connConfig{tls: tlsCfg, clientCertFallback: fallbackCfg}

	// A node outside a cluster has no operator user, and a cluster's password
	// secret does not exist until the cluster controller writes it. Both dial
	// as the default user.
	if clusterName, ok := node.Labels[LabelCluster]; ok {
		password, err := fetchSystemUserPassword(ctx, operatorUser, p.client, clusterName, node.Namespace)
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("operator password: %w", err)
		}
		if password != "" {
			cfg.username = operatorUser
			cfg.password = password
		}
	}

	return p.dialPooled(ctx, net.JoinHostPort(node.Status.PodIP, strconv.Itoa(DefaultPort)), cfg, token)
}
