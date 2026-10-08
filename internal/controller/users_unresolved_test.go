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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// A user whose password Secret cannot be read must not stop topology work.
// Once the cluster has an aclfile, the nodes keep that one, the failure goes
// on Degraded, and scaling carries on. Before the first aclfile exists the
// reconcile still blocks, because there is nothing to fall back to.
var _ = Describe("Users ACL with an unresolvable password Secret", func() {
	const ns = "default"

	user := func(name, secret string) valkeyiov1alpha1.UserAclSpec {
		return valkeyiov1alpha1.UserAclSpec{
			Name:           name,
			Enabled:        true,
			PasswordSecret: valkeyiov1alpha1.PasswordSecretSpec{Name: secret, Keys: []string{"current"}},
			RawAcl:         "+@read",
		}
	}
	passwordSecret := func(name string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			StringData: map[string]string{"current": name + "-pw"},
		}
	}
	newReconciler := func() (*ValkeyClusterReconciler, *events.FakeRecorder) {
		recorder := events.NewFakeRecorder(100)
		return &ValkeyClusterReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder}, recorder
	}
	reconcileOnce := func(r *ValkeyClusterReconciler, cluster *valkeyiov1alpha1.ValkeyCluster) error {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
		return err
	}
	nodeNames := func(cluster *valkeyiov1alpha1.ValkeyCluster) []string {
		nodes := &valkeyiov1alpha1.ValkeyNodeList{}
		Expect(k8sClient.List(ctx, nodes, client.InNamespace(ns), client.MatchingLabels{LabelCluster: cluster.Name})).To(Succeed())
		names := make([]string, 0, len(nodes.Items))
		for i := range nodes.Items {
			names = append(names, nodes.Items[i].Name)
		}
		return names
	}
	aclFile := func(cluster *valkeyiov1alpha1.ValkeyCluster) (string, error) {
		secret := &corev1.Secret{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: getInternalSecretName(cluster.Name), Namespace: ns}, secret); err != nil {
			return "", err
		}
		return string(secret.Data[aclFilename]), nil
	}
	degraded := func(cluster *valkeyiov1alpha1.ValkeyCluster) *metav1.Condition {
		stored := &valkeyiov1alpha1.ValkeyCluster{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), stored)).To(Succeed())
		return meta.FindStatusCondition(stored.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)
	}
	drainEvents := func(recorder *events.FakeRecorder) []string {
		var got []string
		for {
			select {
			case e := <-recorder.Events:
				got = append(got, e)
			default:
				return got
			}
		}
	}
	cleanup := func(cluster *valkeyiov1alpha1.ValkeyCluster, secrets ...string) {
		nodes := &valkeyiov1alpha1.ValkeyNodeList{}
		_ = k8sClient.List(ctx, nodes, client.InNamespace(ns), client.MatchingLabels{LabelCluster: cluster.Name})
		for i := range nodes.Items {
			_ = k8sClient.Delete(ctx, &nodes.Items[i])
		}
		_ = k8sClient.Delete(ctx, cluster)
		for _, name := range append(secrets, getInternalSecretName(cluster.Name), getSystemPasswordSecretName(cluster.Name)) {
			_ = k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		}
		_ = k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: GetServerConfigMapName(cluster.Name), Namespace: ns}})
	}

	It("keeps the last aclfile and still scales out while a user Secret is missing", func() {
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "acl-unresolved", Namespace: ns},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards: 2, Replicas: 0,
				Users: []valkeyiov1alpha1.UserAclSpec{user("alice", "acl-unresolved-alice")},
			},
		}
		defer cleanup(cluster, "acl-unresolved-alice", "acl-unresolved-bob")
		Expect(k8sClient.Create(ctx, passwordSecret("acl-unresolved-alice"))).To(Succeed())
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		r, recorder := newReconciler()

		By("reconciling a cluster whose only user resolves")
		Expect(reconcileOnce(r, cluster)).To(Succeed())
		Expect(nodeNames(cluster)).To(ConsistOf("acl-unresolved-0-0", "acl-unresolved-1-0"))
		before, err := aclFile(cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(before).To(ContainSubstring("user alice "))
		_ = drainEvents(recorder)

		// envtest runs no pods, so mark the existing nodes Ready the way a
		// running cluster would report them; otherwise the node loop waits on
		// the first node and never reaches the new shard.
		for _, name := range nodeNames(cluster) {
			node := &valkeyiov1alpha1.ValkeyNode{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, node)).To(Succeed())
			node.Status.Ready = true
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
		}

		By("adding a shard and a user whose Secret does not exist")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
		cluster.Spec.Shards = 3
		cluster.Spec.Users = append(cluster.Spec.Users, user("bob", "acl-unresolved-bob"))
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
		Expect(reconcileOnce(r, cluster)).To(Succeed())

		By("creating the new shard's ValkeyNode anyway")
		Expect(nodeNames(cluster)).To(ContainElement("acl-unresolved-2-0"))

		By("leaving the internal ACL Secret at its last good aclfile")
		after, err := aclFile(cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before))

		By("reporting the user, the Secret and the failure on Degraded, with an event")
		cond := degraded(cluster)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(valkeyiov1alpha1.ReasonUsersACLUnresolved))
		Expect(cond.Message).To(Equal("user bob: Secret acl-unresolved-bob not found"))
		Expect(drainEvents(recorder)).To(ContainElement(ContainSubstring("UsersACLUnresolved")))

		By("repeating the failure without a second event")
		Expect(reconcileOnce(r, cluster)).To(Succeed())
		Expect(drainEvents(recorder)).NotTo(ContainElement(ContainSubstring("UsersACLUnresolved")))

		By("naming the missing key once the Secret exists without it")
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "acl-unresolved-bob", Namespace: ns},
			StringData: map[string]string{"previous": "old"},
		})).To(Succeed())
		Expect(reconcileOnce(r, cluster)).To(Succeed())
		cond = degraded(cluster)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Message).To(Equal("user bob: Secret acl-unresolved-bob has no key current"))
		Expect(drainEvents(recorder)).To(ContainElement(ContainSubstring("has no key current")))

		By("rebuilding the aclfile and clearing Degraded once the key appears")
		bobSecret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "acl-unresolved-bob", Namespace: ns}, bobSecret)).To(Succeed())
		bobSecret.Data["current"] = []byte("bob-pw")
		Expect(k8sClient.Update(ctx, bobSecret)).To(Succeed())
		Expect(reconcileOnce(r, cluster)).To(Succeed())
		rebuilt, err := aclFile(cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(rebuilt).To(ContainSubstring("user bob "))
		Expect(degraded(cluster)).To(BeNil())
		Expect(drainEvents(recorder)).To(ContainElement(ContainSubstring("UsersACLResolved")))
	})

	It("lands system user changes in the system-passwords Secret while a user Secret is missing", func() {
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "acl-system-first", Namespace: ns},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards: 1, Replicas: 0,
				Exporter: valkeyiov1alpha1.ExporterSpec{Enabled: boolPtr(false)},
				Users:    []valkeyiov1alpha1.UserAclSpec{user("alice", "acl-system-first-alice")},
			},
		}
		defer cleanup(cluster, "acl-system-first-alice", "acl-system-first-bob")
		Expect(k8sClient.Create(ctx, passwordSecret("acl-system-first-alice"))).To(Succeed())
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		r, _ := newReconciler()
		systemPasswords := func() map[string][]byte {
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: getSystemPasswordSecretName(cluster.Name), Namespace: ns}, secret)).To(Succeed())
			return secret.Data
		}

		By("reconciling with the exporter disabled, so its system user has no password yet")
		Expect(reconcileOnce(r, cluster)).To(Succeed())
		Expect(systemPasswords()).NotTo(HaveKey(exporterUser))

		By("enabling the exporter while adding a user whose Secret does not exist")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
		cluster.Spec.Exporter.Enabled = boolPtr(true)
		cluster.Spec.Users = append(cluster.Spec.Users, user("bob", "acl-system-first-bob"))
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
		Expect(reconcileOnce(r, cluster)).To(Succeed())

		By("writing the exporter password even though the aclfile could not be rebuilt")
		Expect(systemPasswords()).To(HaveKey(exporterUser))
		acl, err := aclFile(cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(acl).NotTo(ContainSubstring("user bob "))
		Expect(degraded(cluster)).NotTo(BeNil())

		By("adding the exporter to the aclfile once the user Secret resolves")
		Expect(k8sClient.Create(ctx, passwordSecret("acl-system-first-bob"))).To(Succeed())
		Expect(reconcileOnce(r, cluster)).To(Succeed())
		acl, err = aclFile(cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(acl).To(ContainSubstring("user bob "))
		Expect(acl).To(ContainSubstring("user " + exporterUser + " "))
		Expect(degraded(cluster)).To(BeNil())
	})

	It("still blocks the first reconcile until every user Secret resolves", func() {
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "acl-first", Namespace: ns},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards: 1, Replicas: 0,
				Users: []valkeyiov1alpha1.UserAclSpec{user("carol", "acl-first-carol")},
			},
		}
		defer cleanup(cluster, "acl-first-carol")
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		r, _ := newReconciler()

		By("returning the error and creating nothing while the Secret is missing")
		err := reconcileOnce(r, cluster)
		Expect(err).To(MatchError("user carol: Secret acl-first-carol not found"))
		Expect(nodeNames(cluster)).To(BeEmpty())
		_, err = aclFile(cluster)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), fmt.Sprintf("internal ACL Secret must not exist yet, got %v", err))
		stored := &valkeyiov1alpha1.ValkeyCluster{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), stored)).To(Succeed())
		ready := meta.FindStatusCondition(stored.Status.Conditions, valkeyiov1alpha1.ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(valkeyiov1alpha1.ReasonUsersAclError))
		Expect(meta.FindStatusCondition(stored.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)).To(BeNil())

		By("starting the cluster once the Secret exists")
		Expect(k8sClient.Create(ctx, passwordSecret("acl-first-carol"))).To(Succeed())
		Expect(reconcileOnce(r, cluster)).To(Succeed())
		Expect(nodeNames(cluster)).To(ConsistOf("acl-first-0-0"))
	})
})
