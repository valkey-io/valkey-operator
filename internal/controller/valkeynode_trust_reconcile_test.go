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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// The trust bundle is reloaded, and the live ACL revision published, only once
// the node's ACL is confirmed live: never under a stale or failed ACL load, and
// the revision never ahead of a failed reload. These run the whole Reconcile,
// so moving either step ahead of the ACL check fails them.
var _ = Describe("ValkeyNode trust bundle reload ordering", func() {
	const (
		aclSecret = "trust-order-acl"
		revision  = "aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000"
	)
	var (
		ctx  context.Context
		node *valkeyiov1alpha1.ValkeyNode
	)
	desired := map[string][]string{"alice": {"aaa"}, aclRevisionUser: {revision}}

	// reconcileToLive runs Reconcile against a node whose workload envtest
	// reports as rolled out and Ready, so the pass reaches the live steps, with
	// cfg standing in for the Valkey server. It returns the reconcile error.
	reconcileToLive := func(cfg *fakeConfigClient) error {
		r := &ValkeyNodeReconciler{
			Client:    k8sClient,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  events.NewFakeRecorder(100),
			newConfigClient: func(context.Context, *ValkeyNodeReconciler, *valkeyiov1alpha1.ValkeyNode) (valkeyConfigClient, error) {
				return cfg, nil
			},
			resolveRoleFunc: func(context.Context, *valkeyiov1alpha1.ValkeyNode) string { return RolePrimary },
			nodeInfoFunc:    func(context.Context, *valkeyiov1alpha1.ValkeyNode) (string, error) { return "", nil },
		}
		req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(node)}

		// The first pass creates the StatefulSet; mark it rolled out and give it
		// a Ready pod, as the StatefulSet controller and kubelet would.
		_, _ = r.Reconcile(ctx, req)
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: node.Namespace, Name: valkeyNodeResourceName(node)}, sts)).To(Succeed())
		sts.Status.ObservedGeneration = sts.Generation
		sts.Status.Replicas, sts.Status.ReadyReplicas, sts.Status.UpdatedReplicas, sts.Status.AvailableReplicas = 1, 1, 1, 1
		sts.Status.CurrentRevision, sts.Status.UpdateRevision = "rev-1", "rev-1"
		Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())
		labels := valkeyNodeLabels(node)
		labels[appsv1.StatefulSetRevisionLabel] = "rev-1"
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: sts.Name + "-0", Namespace: node.Namespace, Labels: labels},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "server", Image: "valkey/valkey:9.0.0"}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.PodIP = "10.0.0.9"
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		_, err := r.Reconcile(ctx, req)
		got := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, req.NamespacedName, got)).To(Succeed())
		Expect(got.Status.Ready).To(BeTrue(), "the pass must reach the live steps for this test to mean anything")
		return err
	}
	reloaded := func(cfg *fakeConfigClient) bool { return cfg.params["tls-ca-cert-file"] != "" }
	liveRevision := func() string {
		got := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(node), got)).To(Succeed())
		return got.Status.LiveACLRevision
	}

	BeforeEach(func() {
		ctx = context.Background()
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: aclSecret, Namespace: "default"},
			Data: map[string][]byte{aclFilename: fmt.Appendf(nil,
				"user alice on #aaa ~* +@all\nuser %s off resetchannels -@all #%s\n", aclRevisionUser, revision)},
		}))).To(Succeed())
		node = &valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "trust-order-",
				Namespace:    "default",
			},
			Spec: valkeyiov1alpha1.ValkeyNodeSpec{
				WorkloadType:       valkeyiov1alpha1.WorkloadTypeStatefulSet,
				UsersACLSecretName: aclSecret,
				TLS: &valkeyiov1alpha1.NodeTLSSpec{
					ServerName: "localhost",
					Certificates: valkeyiov1alpha1.NodeTLSCertificates{
						Server:      valkeyiov1alpha1.NodeCertificateRef{SecretName: "trust-order-server"},
						TrustBundle: &valkeyiov1alpha1.NodeTrustBundleRef{SecretName: "trust-order-bundle"},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
	})

	AfterEach(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, node))).To(Succeed())
		_ = k8sClient.Delete(ctx, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: valkeyNodeResourceName(node), Namespace: node.Namespace}})
		_ = k8sClient.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: valkeyNodeResourceName(node) + "-0", Namespace: node.Namespace}})
	})

	It("neither reloads nor publishes while the mounted ACL is stale", func() {
		cfg := &fakeConfigClient{aclHashes: map[string][]string{"alice": {"old"}}}
		Expect(reconcileToLive(cfg)).To(Succeed())
		Expect(cfg.aclLoads).To(BeNumerically(">", 0), "the ACL load ran")
		Expect(reloaded(cfg)).To(BeFalse(), "no reload under a stale ACL")
		Expect(liveRevision()).To(BeEmpty())
	})

	It("neither reloads nor publishes when the ACL load fails", func() {
		cfg := &fakeConfigClient{aclErr: fmt.Errorf("boom")}
		Expect(reconcileToLive(cfg)).NotTo(Succeed())
		Expect(reloaded(cfg)).To(BeFalse())
		Expect(liveRevision()).To(BeEmpty())
	})

	It("reloads and then publishes once the ACL is confirmed live", func() {
		cfg := &fakeConfigClient{aclHashes: map[string][]string{"alice": {"old"}}, aclOnLoad: desired}
		Expect(reconcileToLive(cfg)).To(Succeed())
		Expect(cfg.params).To(HaveKeyWithValue("tls-ca-cert-file", "/tls/ca.crt"))
		Expect(liveRevision()).To(Equal(revision))
	})

	It("does not publish the revision when the reload fails", func() {
		cfg := &fakeConfigClient{aclHashes: map[string][]string{"alice": {"old"}}, aclOnLoad: desired, err: fmt.Errorf("CONFIG SET refused")}
		Expect(reconcileToLive(cfg)).NotTo(Succeed())
		Expect(liveRevision()).To(BeEmpty(), "a failed reload must not advance the published revision")
	})
})
