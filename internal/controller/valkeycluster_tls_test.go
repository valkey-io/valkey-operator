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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyv1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

func TestNodeTLSFromCluster(t *testing.T) {
	t.Run("nil cluster TLS", func(t *testing.T) {
		assert.Nil(t, nodeTLSFromCluster(nil))
	})

	t.Run("server certificate", func(t *testing.T) {
		got := nodeTLSFromCluster(&valkeyv1.TLSSpec{
			Certificates: valkeyv1.TLSCertificates{
				Server: valkeyv1.CertificateSource{SecretName: "valkey-server-tls"},
			},
		})
		require.NotNil(t, got)
		assert.Equal(t, "valkey-server-tls", got.Certificates.Server.SecretName)
	})
}

func TestBuildClusterValkeyNodeTLS(t *testing.T) {
	cluster := &valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "mycluster", Namespace: "default"},
		Spec: valkeyv1.ValkeyClusterSpec{
			Networking: &valkeyv1.NetworkingSpec{
				TLS: &valkeyv1.TLSSpec{
					Certificates: valkeyv1.TLSCertificates{
						Server: valkeyv1.CertificateSource{SecretName: "valkey-server-tls"},
					},
				},
			},
		},
	}

	node := buildClusterValkeyNode(cluster, 0, 0)
	require.NotNil(t, node.Spec.TLS)
	assert.Equal(t, "valkey-server-tls", node.Spec.TLS.Certificates.Server.SecretName)
}

func TestBuildClusterValkeyNodeWithoutTLS(t *testing.T) {
	cluster := &valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "mycluster", Namespace: "default"},
	}

	node := buildClusterValkeyNode(cluster, 0, 0)
	assert.Nil(t, node.Spec.TLS)
}
