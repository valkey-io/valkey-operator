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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

var _ = Describe("per-node Services", func() {
	var (
		r            *ValkeyNodeReconciler
		ctx          context.Context
		fakeRecorder *events.FakeRecorder
	)

	BeforeEach(func() {
		ctx = context.Background()
		fakeRecorder = events.NewFakeRecorder(100)
		r = &ValkeyNodeReconciler{
			Client:    k8sClient,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  fakeRecorder,
		}
	})

	It("creates a ClusterIP Service owned by the ValkeyNode", func() {
		node := newServiceNode("sample-0-1")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, node) }()
		stored := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(node), stored)).To(Succeed())

		Expect(r.ensureNodeService(ctx, stored)).To(Succeed())

		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      valkeyNodeResourceName(stored),
			Namespace: stored.Namespace,
		}, svc)).To(Succeed())
		Expect(svc.Name).To(Equal("valkey-sample-0-1"))
		Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
		Expect(svc.Spec.ClusterIP).NotTo(BeEmpty())
		Expect(svc.Spec.ClusterIP).NotTo(Equal(corev1.ClusterIPNone))
		Expect(svc.Spec.Selector).To(Equal(map[string]string{
			"app.kubernetes.io/name":       appName,
			"app.kubernetes.io/instance":   stored.Name,
			"app.kubernetes.io/component":  "valkey-node",
			"app.kubernetes.io/part-of":    appName,
			"app.kubernetes.io/managed-by": "valkey-operator",
			LabelCluster:                   "sample",
			LabelShardIndex:                "0",
			LabelNodeIndex:                 "1",
		}))
		Expect(svc.Spec.Ports).To(Equal([]corev1.ServicePort{{
			Name:       appName,
			Port:       DefaultPort,
			Protocol:   corev1.ProtocolTCP,
			TargetPort: intstr.FromInt32(DefaultPort),
		}}))
		Expect(svc.OwnerReferences).To(HaveLen(1))
		Expect(svc.OwnerReferences[0].Name).To(Equal(stored.Name))
		Expect(svc.OwnerReferences[0].UID).To(Equal(stored.UID))
	})

	It("does not update the Service on a second reconcile", func() {
		node := newServiceNode("noop-0-0")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, node) }()
		stored := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(node), stored)).To(Succeed())

		Expect(r.ensureNodeService(ctx, stored)).To(Succeed())
		svc := &corev1.Service{}
		key := types.NamespacedName{Name: valkeyNodeResourceName(stored), Namespace: stored.Namespace}
		Expect(k8sClient.Get(ctx, key, svc)).To(Succeed())
		created := svc.ResourceVersion

		Expect(r.ensureNodeService(ctx, stored)).To(Succeed())
		Expect(k8sClient.Get(ctx, key, svc)).To(Succeed())
		Expect(svc.ResourceVersion).To(Equal(created))
	})

	It("deletes the Service when Service is cleared", func() {
		node := newServiceNode("clear-0-0")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, node) }()
		stored := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(node), stored)).To(Succeed())
		Expect(r.ensureNodeService(ctx, stored)).To(Succeed())

		stored.Spec.Service = nil
		Expect(r.ensureNodeService(ctx, stored)).To(Succeed())
		err := k8sClient.Get(ctx, types.NamespacedName{
			Name:      valkeyNodeResourceName(stored),
			Namespace: stored.Namespace,
		}, &corev1.Service{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("does not delete a Service it does not own", func() {
		node := newBareNode("foreign-0-0")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, node) }()
		stored := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(node), stored)).To(Succeed())

		foreign := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      valkeyNodeResourceName(stored),
				Namespace: stored.Namespace,
			},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{"app": "mine"},
				Ports:    []corev1.ServicePort{{Name: appName, Port: DefaultPort, TargetPort: intstr.FromInt32(DefaultPort)}},
			},
		}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, foreign) }()

		Expect(r.ensureNodeService(ctx, stored)).To(Succeed())
		got := &corev1.Service{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(foreign), got)).To(Succeed())
		Expect(got.OwnerReferences).To(BeEmpty())
	})

	It("does not adopt a Service owned by another ValkeyNode", func() {
		node := newServiceNode("adopt-0-0")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, node) }()
		stored := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(node), stored)).To(Succeed())

		other := newServiceNode("adopt-other-0-0")
		Expect(k8sClient.Create(ctx, other)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, other) }()
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(other), other)).To(Succeed())

		taken := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      valkeyNodeResourceName(stored),
				Namespace: stored.Namespace,
				Labels:    valkeyNodeLabels(other),
			},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{"app": "mine"},
				Ports:    []corev1.ServicePort{{Name: appName, Port: DefaultPort, TargetPort: intstr.FromInt32(DefaultPort)}},
			},
		}
		Expect(controllerutil.SetControllerReference(other, taken, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, taken)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, taken) }()

		r.Client = cachedServiceClient{k8sClient}
		err := r.ensureNodeService(ctx, stored)
		Expect(err).To(MatchError(ContainSubstring("is not owned by ValkeyNode")))
		got := &corev1.Service{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(taken), got)).To(Succeed())
		Expect(got.OwnerReferences).To(HaveLen(1))
		Expect(got.OwnerReferences[0].Name).To(Equal(other.Name))
		Expect(got.Spec.Selector).To(Equal(map[string]string{"app": "mine"}))
	})

	It("reports a user Service that the cache cannot see", func() {
		node := newServiceNode("user-0-0")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, node) }()
		stored := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(node), stored)).To(Succeed())

		foreign := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      valkeyNodeResourceName(stored),
				Namespace: stored.Namespace,
			},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{"app": "mine"},
				Ports:    []corev1.ServicePort{{Name: appName, Port: DefaultPort, TargetPort: intstr.FromInt32(DefaultPort)}},
			},
		}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, foreign) }()

		r.Client = cachedServiceClient{k8sClient}
		err := r.ensureNodeService(ctx, stored)
		Expect(err).To(MatchError(ContainSubstring("is not owned by ValkeyNode")))
		got := &corev1.Service{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(foreign), got)).To(Succeed())
		Expect(got.OwnerReferences).To(BeEmpty())
		Expect(got.Spec.Selector).To(Equal(map[string]string{"app": "mine"}))
	})

	It("reconciles the Deployment for a long name when Service is unset", func() {
		node := newBareNode(strings.Repeat("d", 57))
		node.Spec.WorkloadType = valkeyiov1alpha1.WorkloadTypeDeployment
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		key := types.NamespacedName{Name: valkeyNodeResourceName(node), Namespace: node.Namespace}
		defer func() {
			_ = k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}})
			_ = k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: GetServerConfigMapName(node.Name), Namespace: node.Namespace}})
			_ = k8sClient.Delete(ctx, node)
		}()

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(node)})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, &appsv1.Deployment{})).To(Succeed())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &corev1.Service{}))).To(BeTrue())
	})

	It("creates a per-node Service when another cluster only shares the name", func() {
		node := newServiceNode("other")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, node) }()
		stored := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(node), stored)).To(Succeed())

		other := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace},
			Spec:       valkeyiov1alpha1.ValkeyClusterSpec{Shards: 1},
		}
		Expect(k8sClient.Create(ctx, other)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, other) }()

		Expect(r.ensureNodeService(ctx, stored)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      valkeyNodeResourceName(stored),
			Namespace: stored.Namespace,
		}, &corev1.Service{})).To(Succeed())
	})

	It("creates the headless Service when the node has not created one yet", func() {
		node := newServiceNode("clash")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, node) }()

		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace},
			Spec:       valkeyiov1alpha1.ValkeyClusterSpec{Shards: 1},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, cluster) }()

		cr := &ValkeyClusterReconciler{
			Client:    k8sClient,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  fakeRecorder,
		}
		Expect(cr.upsertService(ctx, cluster)).To(Succeed())
		got := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      headlessServiceName(cluster.Name),
			Namespace: cluster.Namespace,
		}, got)).To(Succeed())
		Expect(got.OwnerReferences[0].Name).To(Equal(cluster.Name))
	})

	It("does not replace a per-node Service with a headless Service", func() {
		node := newServiceNode("taken")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, node) }()
		stored := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(node), stored)).To(Succeed())
		Expect(r.ensureNodeService(ctx, stored)).To(Succeed())

		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace},
			Spec:       valkeyiov1alpha1.ValkeyClusterSpec{Shards: 1},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, cluster) }()

		cr := &ValkeyClusterReconciler{
			Client:    k8sClient,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  fakeRecorder,
		}
		err := cr.upsertService(ctx, cluster)
		Expect(err).To(HaveOccurred())
		got := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      valkeyNodeResourceName(stored),
			Namespace: stored.Namespace,
		}, got)).To(Succeed())
		Expect(got.OwnerReferences[0].Name).To(Equal(stored.Name))
		Expect(got.Spec.ClusterIP).NotTo(Equal(corev1.ClusterIPNone))
	})

	It("rejects a Service name longer than 63 characters", func() {
		node := newServiceNode(strings.Repeat("a", 57))
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, node) }()

		err := r.ensureNodeService(ctx, node)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("63"))
		getErr := k8sClient.Get(ctx, types.NamespacedName{
			Name:      valkeyNodeResourceName(node),
			Namespace: node.Namespace,
		}, &corev1.Service{})
		Expect(apierrors.IsNotFound(getErr)).To(BeTrue())
	})
})

func newServiceNode(name string) *valkeyiov1alpha1.ValkeyNode {
	node := newBareNode(name)
	node.Spec.Service = &valkeyiov1alpha1.ValkeyNodeServiceSpec{}
	return node
}

func newBareNode(name string) *valkeyiov1alpha1.ValkeyNode {
	return &valkeyiov1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				LabelCluster:    "sample",
				LabelShardIndex: "0",
				LabelNodeIndex:  "1",
			},
		},
	}
}

// cachedServiceClient returns NotFound for a Service without the operator
// managed-by label, the same as the manager cache.
type cachedServiceClient struct {
	client.Client
}

func (c cachedServiceClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if _, ok := obj.(*corev1.Service); ok && obj.GetLabels()["app.kubernetes.io/managed-by"] != "valkey-operator" {
		return apierrors.NewNotFound(corev1.Resource("services"), key.Name)
	}
	return nil
}
