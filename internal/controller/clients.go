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

	vclient "github.com/valkey-io/valkey-go"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
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
func (p *provider) tlsConfig(ctx context.Context, namespace string, spec *valkeyiov1alpha1.NodeTLSSpec) (*tls.Config, string, error) {
	if spec == nil || spec.Certificates.Server.SecretName == "" {
		return nil, "", nil
	}
	name := spec.Certificates.Server.SecretName
	clientCert := spec.RequiresClientCertificate()
	cfg, resourceVersion, err := getTLSConfig(ctx, p.apiReader, name, spec.ServerName, namespace, clientCert)
	if err != nil {
		return nil, "", err
	}
	return cfg, fmt.Sprintf("%s/%s/%s/%t", name, resourceVersion, spec.ServerName, clientCert), nil
}

func (p *provider) ForCluster(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) (valkey.DialFunc, error) {
	password, err := fetchSystemUserPassword(ctx, operatorUser, p.client, cluster.Name, cluster.Namespace)
	if err != nil {
		return nil, fmt.Errorf("operator password: %w", err)
	}
	cfg := connConfig{username: operatorUser, password: password}

	tlsSpec := nodeTLSFromCluster(cluster)
	// A candidate that is not published yet left the nodes on their previous
	// TLS. Dialing the cluster spec would use TLS against servers that are
	// still on plaintext, or the reverse, and the scrape would fail.
	held, ok, lookupErr := p.unpublishedNodeTLS(ctx, cluster)
	if lookupErr != nil {
		return nil, fmt.Errorf("published server configuration: %w", lookupErr)
	}
	if ok {
		tlsSpec = held
	}
	tlsCfg, token, err := p.tlsConfig(ctx, cluster.Namespace, tlsSpec)
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

	return func(ctx context.Context, address string) (vclient.Client, error) {
		return p.pool.Get(ctx, clientOption(address, cfg), token)
	}, nil
}

// unpublishedNodeTLS is the TLS the nodes are serving while the candidate
// valkey.conf is not the published file. The bool is false when the file
// already matches or there is no node to copy. A nil spec with true means
// those nodes are on plaintext.
func (p *provider) unpublishedNodeTLS(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) (*valkeyiov1alpha1.NodeTLSSpec, bool, error) {
	cm := &corev1.ConfigMap{}
	err := p.client.Get(ctx, types.NamespacedName{Name: GetServerConfigMapName(cluster.Name), Namespace: cluster.Namespace}, cm)
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	published := cm.Data[configFileKey]
	if published == "" || published == buildServerConfig(cluster) {
		return nil, false, nil
	}
	nodes := &valkeyiov1alpha1.ValkeyNodeList{}
	if err := p.client.List(ctx, nodes, client.InNamespace(cluster.Namespace), client.MatchingLabels{LabelCluster: cluster.Name}); err != nil {
		return nil, false, err
	}
	if len(nodes.Items) == 0 {
		return nil, false, nil
	}
	spec := nodes.Items[0].Spec.TLS
	if spec != nil {
		spec = spec.DeepCopy()
		if spec.ServerName == "" {
			spec.ServerName = nodeTLSServerName(&nodes.Items[0])
		}
	}
	return spec, true, nil
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
	tlsCfg, token, err := p.tlsConfig(ctx, node.Namespace, tlsSpec)
	if err != nil {
		return nil, fmt.Errorf("TLS config: %w", err)
	}
	cfg := connConfig{tls: tlsCfg}

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

	return p.pool.Get(ctx, clientOption(net.JoinHostPort(node.Status.PodIP, strconv.Itoa(DefaultPort)), cfg), token)
}
