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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// A ValkeyNode created by v0.6.0 has no spec.tls.serverName until the cluster
// controller updates it. The node client must still verify against the
// headless Service name, or a cert with DNS SANs only fails against the pod IP.
func TestBuildNodeClientOptionServerNameFallback(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "c-server", Namespace: "ns"},
		Data:       map[string][]byte{tlsSecretKeyCA: selfSignedCAPEM(t)},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	r := &ValkeyNodeReconciler{Client: c, APIReader: c, Scheme: scheme}

	node := newTestValkeyNode("c-0-0", "ns")
	node.Labels = map[string]string{LabelCluster: "c"}
	node.Spec.TLS = &valkeyiov1alpha1.NodeTLSSpec{
		Certificates: valkeyiov1alpha1.NodeTLSCertificates{
			Server: valkeyiov1alpha1.NodeCertificateRef{SecretName: "c-server"},
		},
	}

	opt := r.buildNodeClientOption(context.Background(), node)
	require.NotNil(t, opt.TLSConfig)
	assert.Equal(t, "valkey-c.ns.svc.cluster.local", opt.TLSConfig.ServerName)
}

func selfSignedCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
