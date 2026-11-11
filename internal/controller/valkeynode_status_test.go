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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// staleCacheClient stands in for an informer cache that has not seen the
// latest writes: every read of a ValkeyNode returns the copy it holds. Writes
// go to the API server.
type staleCacheClient struct {
	client.Client
	node *valkeyiov1alpha1.ValkeyNode
}

func (c staleCacheClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if n, ok := obj.(*valkeyiov1alpha1.ValkeyNode); ok && key == client.ObjectKeyFromObject(c.node) {
		c.node.DeepCopyInto(n)
		return nil
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

var _ = Describe("ValkeyNode status writes from a stale read", func() {
	var (
		ctx  context.Context
		node *valkeyiov1alpha1.ValkeyNode
	)

	BeforeEach(func() {
		ctx = context.Background()
		node = &valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{Name: "stale-status-test", Namespace: "default"},
			Spec:       valkeyiov1alpha1.ValkeyNodeSpec{WorkloadType: valkeyiov1alpha1.WorkloadTypeStatefulSet},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, node)).To(Succeed())
	})

	read := func() *valkeyiov1alpha1.ValkeyNode {
		n := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(node), n)).To(Succeed())
		return n
	}
	// write changes the node's status on the API server, behind the stale copy.
	write := func(change func(*valkeyiov1alpha1.ValkeyNode)) {
		n := read()
		change(n)
		Expect(k8sClient.Status().Update(ctx, n)).To(Succeed())
	}
	rollPending := func(n *valkeyiov1alpha1.ValkeyNode) {
		meta.SetStatusCondition(&n.Status.Conditions, metav1.Condition{
			Type:   valkeyiov1alpha1.ValkeyNodeConditionWorkloadRollPending,
			Status: metav1.ConditionTrue,
			Reason: valkeyiov1alpha1.ValkeyNodeReasonAwaitingWorkloadRevision,
		})
	}
	// reconcilerOn returns a reconciler whose cached reads return the node as
	// it is now, while its API reads and writes see every later change.
	reconcilerOn := func() *ValkeyNodeReconciler {
		return &ValkeyNodeReconciler{
			Client:    staleCacheClient{Client: k8sClient, node: read()},
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  events.NewFakeRecorder(100),
		}
	}
	conditionTypes := func() []string {
		conditions := read().Status.Conditions
		types := make([]string, 0, len(conditions))
		for _, c := range conditions {
			types = append(types, c.Type)
		}
		return types
	}

	It("keeps a condition written after the read when setting another", func() {
		r := reconcilerOn()
		write(rollPending)

		Expect(r.setACLCondition(ctx, node, metav1.ConditionTrue, valkeyiov1alpha1.ValkeyNodeReasonApplied, "applied")).To(Succeed())

		Expect(conditionTypes()).To(ConsistOf(
			valkeyiov1alpha1.ValkeyNodeConditionWorkloadRollPending,
			valkeyiov1alpha1.ValkeyNodeConditionACLApplied,
		))
	})

	It("does not bring back a condition removed after the read", func() {
		write(rollPending)
		r := reconcilerOn()
		write(func(n *valkeyiov1alpha1.ValkeyNode) {
			meta.RemoveStatusCondition(&n.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionWorkloadRollPending)
		})

		Expect(r.setACLCondition(ctx, node, metav1.ConditionTrue, valkeyiov1alpha1.ValkeyNodeReasonApplied, "applied")).To(Succeed())

		Expect(conditionTypes()).To(ConsistOf(valkeyiov1alpha1.ValkeyNodeConditionACLApplied))
	})

	It("keeps a condition written after the read when removing another", func() {
		Expect(reconcilerOn().setTrustBundleCondition(ctx, node, metav1.ConditionTrue, valkeyiov1alpha1.ValkeyNodeReasonApplied, "reloaded")).To(Succeed())
		r := reconcilerOn()
		write(rollPending)

		Expect(r.clearTrustBundleCondition(ctx, node)).To(Succeed())

		Expect(conditionTypes()).To(ConsistOf(valkeyiov1alpha1.ValkeyNodeConditionWorkloadRollPending))
	})

	It("keeps a condition written after the read when updating status", func() {
		r := reconcilerOn()
		write(rollPending)

		_, err := r.updateStatus(ctx, node, nil)
		Expect(err).NotTo(HaveOccurred())

		Expect(conditionTypes()).To(ConsistOf(
			valkeyiov1alpha1.ValkeyNodeConditionWorkloadRollPending,
			valkeyiov1alpha1.ValkeyNodeConditionReady,
		))
	})

	It("judges a roll pending from the node it writes, not the stale read", func() {
		r := reconcilerOn()
		write(rollPending)

		newly, err := r.markWorkloadRollPending(ctx, node, "hash")
		Expect(err).NotTo(HaveOccurred())
		Expect(newly).To(BeFalse(), "the roll was already pending on the API server")
	})

	It("records the applied ACL revision without dropping a condition", func() {
		r := reconcilerOn()
		write(rollPending)

		Expect(r.setAppliedACLRevision(ctx, node, "rev1")).To(Succeed())

		n := read()
		Expect(n.Status.AppliedACLRevision).To(Equal("rev1"))
		Expect(meta.IsStatusConditionTrue(n.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionWorkloadRollPending)).To(BeTrue())
	})
})
