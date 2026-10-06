/*
Copyright 2026 Valkey Contributors.

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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyv1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

func trustTestNode(trustBundle string) *valkeyv1.ValkeyNode {
	node := newTestValkeyNode("mynode", "test-ns")
	enabled := true
	node.Spec.Exporter.Enabled = &enabled
	node.Spec.TLS = &valkeyv1.NodeTLSSpec{
		Certificates: valkeyv1.NodeTLSCertificates{
			Server: valkeyv1.NodeCertificateRef{SecretName: "server-tls"},
		},
		ClientAuth: &valkeyv1.NodeTLSClientAuthSpec{Mode: valkeyv1.TLSAuthClientsRequired},
	}
	withTrustBundle(node, trustBundle)
	return node
}

func envValue(t *testing.T, c corev1.Container, name string) string {
	t.Helper()
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value
		}
	}
	t.Fatalf("container %s has no env %s", c.Name, name)
	return ""
}

func tlsCertsVolume(t *testing.T, pts corev1.PodTemplateSpec) corev1.Volume {
	t.Helper()
	for _, v := range pts.Spec.Volumes {
		if v.Name == tlsVolumeName {
			return v
		}
	}
	t.Fatal("no tls-certs volume")
	return corev1.Volume{}
}

func TestBuildValkeyNodePodTemplateSpec_TrustBundle(t *testing.T) {
	t.Run("without a trust bundle the server secret is mounted as is", func(t *testing.T) {
		node := trustTestNode("")
		pts, err := buildValkeyNodePodTemplateSpec(node, valkeyNodeLabels(node))
		require.NoError(t, err)

		vol := tlsCertsVolume(t, pts)
		require.NotNil(t, vol.Secret)
		assert.Nil(t, vol.Projected)
		assert.Equal(t, "server-tls", vol.Secret.SecretName)
		assert.Equal(t, "/tls/ca.crt", envValue(t, pts.Spec.Containers[0], "VALKEY_TLS_CA_FILE"))
		assert.Equal(t, "/tls/ca.crt", envValue(t, pts.Spec.Containers[1], "REDIS_EXPORTER_TLS_CA_CERT_FILE"))
	})

	t.Run("with a trust bundle ca.crt comes from the bundle", func(t *testing.T) {
		node := trustTestNode("c-tls-trust")
		pts, err := buildValkeyNodePodTemplateSpec(node, valkeyNodeLabels(node))
		require.NoError(t, err)

		vol := tlsCertsVolume(t, pts)
		assert.Nil(t, vol.Secret)
		require.NotNil(t, vol.Projected)
		require.NotNil(t, vol.Projected.DefaultMode, "API-server default is mirrored so the template does not diff")
		assert.Equal(t, corev1.ProjectedVolumeSourceDefaultMode, *vol.Projected.DefaultMode)

		paths := map[string]string{}
		for _, src := range vol.Projected.Sources {
			require.NotNil(t, src.Secret)
			for _, item := range src.Secret.Items {
				paths[item.Path] = src.Secret.Name + "/" + item.Key
			}
		}
		assert.Equal(t, map[string]string{
			"tls.crt":       "server-tls/tls.crt",
			"tls.key":       "server-tls/tls.key",
			"ca.crt":        "c-tls-trust/ca.crt",
			"server-ca.crt": "server-tls/ca.crt",
		}, paths)
	})

	t.Run("with a trust bundle probes and the exporter still verify the server root", func(t *testing.T) {
		node := trustTestNode("c-tls-trust")
		pts, err := buildValkeyNodePodTemplateSpec(node, valkeyNodeLabels(node))
		require.NoError(t, err)

		server := pts.Spec.Containers[0]
		assert.Equal(t, "/tls/server-ca.crt", envValue(t, server, "VALKEY_TLS_CA_FILE"))
		assert.Contains(t, envValue(t, server, "VALKEY_TLS_ARGS"), "--cacert /tls/server-ca.crt")
		assert.Contains(t, envValue(t, server, "VALKEY_TLS_ARGS"), "--cert /tls/tls.crt --key /tls/tls.key")
		assert.Equal(t, "/tls/server-ca.crt", envValue(t, pts.Spec.Containers[1], "REDIS_EXPORTER_TLS_CA_CERT_FILE"))
	})

	t.Run("valkey.conf and the config roll hash do not change", func(t *testing.T) {
		without, with := trustTestNode(""), trustTestNode("c-tls-trust")

		cmWithout, err := buildValkeyNodeConfigMap(without)
		require.NoError(t, err)
		cmWith, err := buildValkeyNodeConfigMap(with)
		require.NoError(t, err)
		assert.Equal(t, cmWithout.Data["valkey.conf"], cmWith.Data["valkey.conf"])
		assert.Contains(t, cmWith.Data["valkey.conf"], "tls-ca-cert-file /tls/ca.crt")

		assert.Equal(t, nodeServerConfigRollHash(without), nodeServerConfigRollHash(with))
	})
}

func TestReloadTrustBundle(t *testing.T) {
	ctx := context.Background()
	reconciler := func(cfg *fakeConfigClient, opened *int) *ValkeyNodeReconciler {
		return &ValkeyNodeReconciler{
			newConfigClient: func(context.Context, *ValkeyNodeReconciler, *valkeyv1.ValkeyNode) (valkeyConfigClient, error) {
				*opened++
				return cfg, nil
			},
		}
	}

	t.Run("re-sets tls-ca-cert-file to its unchanged path and closes the client", func(t *testing.T) {
		cfg, opened := &fakeConfigClient{}, 0
		require.NoError(t, reconciler(cfg, &opened).reloadTrustBundle(ctx, trustTestNode("c-tls-trust")))
		assert.Equal(t, map[string]string{"tls-ca-cert-file": "/tls/ca.crt"}, cfg.params)
		assert.True(t, cfg.closed)
	})

	t.Run("leaves a node without a trust bundle alone", func(t *testing.T) {
		cfg, opened := &fakeConfigClient{}, 0
		require.NoError(t, reconciler(cfg, &opened).reloadTrustBundle(ctx, trustTestNode("")))
		plain := trustTestNode("")
		plain.Spec.TLS = nil
		require.NoError(t, reconciler(cfg, &opened).reloadTrustBundle(ctx, plain))
		assert.Zero(t, opened, "no connection is opened")
	})

	t.Run("returns a CONFIG SET failure", func(t *testing.T) {
		cfg, opened := &fakeConfigClient{err: errors.New("boom")}, 0
		require.Error(t, reconciler(cfg, &opened).reloadTrustBundle(ctx, trustTestNode("c-tls-trust")))
		assert.True(t, cfg.closed)
	})
}

func TestApplyLiveACLReturnsTheConfirmedRevision(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	acl := "user alice on #aaa ~* +@all\nuser " + aclRevisionUser + " off resetchannels -@all #rev1\n"
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "acl", Namespace: "ns"},
		Data:       map[string][]byte{aclFilename: []byte(acl)},
	}).Build()
	node := &valkeyv1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "ns"},
		Spec:       valkeyv1.ValkeyNodeSpec{UsersACLSecretName: "acl"},
	}
	live := map[string][]string{"alice": {"aaa"}, aclRevisionUser: {"rev1"}}
	reconciler := func(c *fakeConfigClient) *ValkeyNodeReconciler {
		return &ValkeyNodeReconciler{
			APIReader: k,
			newConfigClient: func(context.Context, *ValkeyNodeReconciler, *valkeyv1.ValkeyNode) (valkeyConfigClient, error) {
				return c, nil
			},
		}
	}

	synced, revision, err := reconciler(&fakeConfigClient{aclHashes: map[string][]string{"alice": {"old"}}, aclOnLoad: live}).applyLiveACL(ctx, node)
	require.NoError(t, err)
	assert.True(t, synced)
	assert.Equal(t, "rev1", revision, "the revision the server was confirmed to hold")

	synced, revision, err = reconciler(&fakeConfigClient{aclHashes: map[string][]string{"alice": {"old"}}}).applyLiveACL(ctx, node)
	require.NoError(t, err)
	assert.False(t, synced)
	assert.Empty(t, revision, "no revision is reported until it is live")
}
