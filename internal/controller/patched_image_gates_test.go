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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	valkeyv1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPatchedServerImageGates(t *testing.T) {
	server := func(image string) corev1.Container { return corev1.Container{Name: serverContainerName, Image: image} }
	for _, tc := range []struct {
		name, image, expected string
		patches               []corev1.Container
	}{
		{"default", "", DefaultImage, nil},
		{"patch overrides default", "", "valkey/valkey:8.0.12", []corev1.Container{server("valkey/valkey:8.0.12")}},
		{"patch downgrades spec", "valkey/valkey:9.1.0", "valkey/valkey:8.0.12", []corev1.Container{server("valkey/valkey:8.0.12")}},
		{"patch upgrades spec", "valkey/valkey:8.0.12", "valkey/valkey:9.1.0", []corev1.Container{server("valkey/valkey:9.1.0")}},
		{"sidecar does not override server", "valkey/valkey:8.1.11", "valkey/valkey:8.1.11", []corev1.Container{{Name: "sidecar", Image: "valkey/valkey:8.0.12"}}},
		{"empty image keeps spec", "valkey/valkey:8.1.11", "valkey/valkey:8.1.11", []corev1.Container{server("")}},
		{"last patch wins", "valkey/valkey:9.1.0", "valkey/valkey:8.0.12", []corev1.Container{server("valkey/valkey:8.1.11"), server("valkey/valkey:8.0.12")}},
		{"last empty patch keeps spec", "valkey/valkey:9.1.0", "valkey/valkey:9.1.0", []corev1.Container{server("valkey/valkey:8.0.12"), server("")}},
		{"unknown version keeps existing policy", "valkey/valkey:8.0.12", "custom:unknown", []corev1.Container{server("custom:unknown")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := &valkeyv1.ValkeyCluster{ObjectMeta: metav1.ObjectMeta{Name: "image-gates", Namespace: "default"},
				Spec: valkeyv1.ValkeyClusterSpec{Image: tc.image, Containers: tc.patches, Config: map[string]string{"tls-auto-reload-interval": "60"}}}
			reference := cluster.DeepCopy()
			reference.Spec.Image, reference.Spec.Containers = tc.expected, nil
			node := buildClusterValkeyNode(cluster, 0, 0)
			referenceNode := buildClusterValkeyNode(reference, 0, 0)
			containers, err := buildContainersDef(node)
			require.NoError(t, err)
			assert.Equal(t, tc.expected, containers[0].Image)
			assert.Equal(t, renderPinnedConfig(referenceNode), renderPinnedConfig(node), "config must match the image the server runs")
			wantRevision := pinnedConfigRevision(renderPinnedConfig(referenceNode))
			assert.Equal(t, wantRevision, pinnedConfigRevision(renderPinnedConfig(node)))
			warnings := versionGateConfigWarnings(cluster)
			assert.Len(t, warnings, len(versionGateConfigWarnings(reference)))
			for _, warning := range warnings {
				assert.Contains(t, warning.message, tc.expected)
			}

			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			passwords := map[string][]byte{}
			for _, user := range systemUsers {
				passwords[user] = []byte("test-password")
			}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: getSystemPasswordSecretName(cluster.Name), Namespace: cluster.Namespace}, Data: passwords}
			r := &ValkeyClusterReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()}
			acl, err := r.createSystemUsersAcl(context.Background(), cluster)
			require.NoError(t, err)
			expectedACL, err := r.createSystemUsersAcl(context.Background(), reference)
			require.NoError(t, err)
			assert.Equal(t, expectedACL, acl, "system ACL rules must match the patched server version")
		})
	}
}
