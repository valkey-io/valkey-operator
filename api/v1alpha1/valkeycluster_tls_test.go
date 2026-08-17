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

package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetTLS(t *testing.T) {
	t.Run("nil cluster", func(t *testing.T) {
		var c *ValkeyCluster
		assert.Nil(t, c.GetTLS())
	})

	t.Run("neither set", func(t *testing.T) {
		c := &ValkeyCluster{}
		assert.Nil(t, c.GetTLS())
	})

	t.Run("networking without tls", func(t *testing.T) {
		c := &ValkeyCluster{
			Spec: ValkeyClusterSpec{
				Networking: &NetworkingSpec{},
			},
		}
		assert.Nil(t, c.GetTLS())
	})

	t.Run("networking.tls", func(t *testing.T) {
		c := &ValkeyCluster{
			Spec: ValkeyClusterSpec{
				Networking: &NetworkingSpec{
					TLS: &TLSConfig{Certificate: CertificateRef{SecretName: "net-tls"}},
				},
			},
		}
		tls := c.GetTLS()
		require.NotNil(t, tls)
		assert.Equal(t, "net-tls", tls.Certificate.SecretName)
	})
}

func TestGetPreferredEndpointTypeAndClusterDomain(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		c := &ValkeyCluster{}
		assert.Equal(t, PreferredEndpointTypeIP, c.GetPreferredEndpointType())
		assert.Equal(t, DefaultClusterDomain, c.GetClusterDomain())
		assert.False(t, c.PrefersHostnameAnnounce())
	})
	t.Run("hostname and custom domain", func(t *testing.T) {
		c := &ValkeyCluster{
			Spec: ValkeyClusterSpec{
				Networking: &NetworkingSpec{
					ClusterDomain: "corp.local",
					Discovery: &DiscoverySpec{
						PreferredEndpointType: PreferredEndpointTypeHostname,
					},
				},
			},
		}
		assert.Equal(t, PreferredEndpointTypeHostname, c.GetPreferredEndpointType())
		assert.Equal(t, "corp.local", c.GetClusterDomain())
		assert.True(t, c.PrefersHostnameAnnounce())
	})
}
