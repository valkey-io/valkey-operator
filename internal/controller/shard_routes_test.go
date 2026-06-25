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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

var _ = Describe("reconcileShardRoutes", func() {
	ctx := context.Background()

	// The Gateway API CRDs are not loaded into envtest, so these specs exercise the
	// gated path: with GatewayAPIEnabled false the controller must never touch
	// TCPRoutes, even when the user configures a Gateway. Creation behaviour with the
	// CRDs present is covered by the GatewayAPI-labelled e2e suite.
	It("is a no-op and warns when a Gateway is configured but the Gateway API is absent", func() {
		recorder := events.NewFakeRecorder(10)
		r := &ValkeyClusterReconciler{
			Client:            k8sClient,
			APIReader:         k8sClient,
			Scheme:            k8sClient.Scheme(),
			Recorder:          recorder,
			GatewayAPIEnabled: false,
		}
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "gw-absent", Namespace: "default"},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:   1,
				Replicas: 1,
				ExternalAccess: &valkeyiov1alpha1.ExternalAccessSpec{
					Enabled: true,
					Gateway: &valkeyiov1alpha1.GatewayExposureSpec{
						GatewayRef: valkeyiov1alpha1.GatewayReference{Name: "public-gw"},
						BasePort:   30000,
					},
				},
			},
		}

		// Must not error even though TCPRoute is not served by the API server.
		Expect(r.reconcileShardRoutes(ctx, cluster)).To(Succeed())

		Expect(collectEvents(recorder)).To(ContainElement(ContainSubstring("GatewayAPIUnavailable")))
	})

	It("is a no-op when no Gateway is configured", func() {
		r := &ValkeyClusterReconciler{
			Client:            k8sClient,
			APIReader:         k8sClient,
			Scheme:            k8sClient.Scheme(),
			Recorder:          events.NewFakeRecorder(10),
			GatewayAPIEnabled: false,
		}
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "gw-none", Namespace: "default"},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:         1,
				Replicas:       0,
				ExternalAccess: &valkeyiov1alpha1.ExternalAccessSpec{Enabled: true},
			},
		}
		Expect(r.reconcileShardRoutes(ctx, cluster)).To(Succeed())
	})
})
