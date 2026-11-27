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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// A cluster created by a release that named its Secrets internal-<cluster>-acl
// and internal-<cluster>-system-passwords. These run the whole Reconcile, so
// they catch the copy, the update of the legacy Secrets and their deletion
// happening at the wrong point of it.
var _ = Describe("Secrets created under their legacy names", func() {
	const ns = "default"
	ctx := context.Background()

	secret := func(name string) *corev1.Secret {
		s := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, s)).To(Succeed())
		return s
	}
	exists := func(name string) bool {
		err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &corev1.Secret{})
		if apierrors.IsNotFound(err) {
			return false
		}
		Expect(err).NotTo(HaveOccurred())
		return true
	}
	// createLegacySecrets creates the two Secrets the way an earlier release
	// did: labelled, controlled by the cluster, and with the passwords the
	// running nodes hold.
	createLegacySecrets := func(cluster *valkeyiov1alpha1.ValkeyCluster, aclFile string) {
		for _, s := range []*corev1.Secret{
			{
				ObjectMeta: metav1.ObjectMeta{Name: legacySystemPasswordSecretName(cluster.Name), Namespace: ns, Labels: labels(cluster)},
				Type:       AclSecretType,
				Data: map[string][]byte{
					operatorUser:    []byte("legacy-operator-pw"),
					replicationUser: []byte("legacy-replication-pw"),
					exporterUser:    []byte("legacy-exporter-pw"),
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: legacyInternalSecretName(cluster.Name), Namespace: ns, Labels: labels(cluster),
					Annotations: map[string]string{hashAnnotationKey: "legacy-hash"}},
				Type: AclSecretType,
				Data: map[string][]byte{aclFilename: []byte(aclFile)},
			},
		} {
			Expect(controllerutil.SetControllerReference(cluster, s, k8sClient.Scheme())).To(Succeed())
			Expect(k8sClient.Create(ctx, s)).To(Succeed())
		}
	}
	newReconciler := func() *ValkeyClusterReconciler {
		return &ValkeyClusterReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(), Recorder: events.NewFakeRecorder(100), ValkeyClients: testValkeyClients(k8sClient)}
	}
	reconcileOnce := func(r *ValkeyClusterReconciler, cluster *valkeyiov1alpha1.ValkeyCluster) error {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
		return err
	}
	cleanup := func(cluster *valkeyiov1alpha1.ValkeyCluster) {
		nodes := &valkeyiov1alpha1.ValkeyNodeList{}
		_ = k8sClient.List(ctx, nodes, client.InNamespace(ns), client.MatchingLabels{LabelCluster: cluster.Name})
		for i := range nodes.Items {
			_ = k8sClient.Delete(ctx, &nodes.Items[i])
		}
		_ = k8sClient.Delete(ctx, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: resourcePrefix + cluster.Name + "-0-0", Namespace: ns}})
		_ = k8sClient.Delete(ctx, cluster)
		for _, s := range renamedSecrets(cluster.Name) {
			_ = k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: ns}})
			_ = k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: s.legacyName, Namespace: ns}})
		}
		_ = k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: GetSharedConfigMapName(cluster.Name), Namespace: ns}})
		_ = k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: headlessServiceName(cluster.Name), Namespace: ns}})
	}

	It("copies them, keeps them equal while a workload reads them, and deletes them after the roll", func() {
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "legacy-names", Namespace: ns},
			Spec:       valkeyiov1alpha1.ValkeyClusterSpec{Shards: 1},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		defer cleanup(cluster)
		createLegacySecrets(cluster, "user default off ~* &* +@all\n")

		By("standing in for the node and its workload, still on the legacy names")
		node := buildClusterValkeyNode(cluster, 0, 0)
		node.Spec.ConfigRevision = testConfigRevision
		node.Spec.UsersACLSecretName = "internal-legacy-names-acl"
		Expect(controllerutil.SetControllerReference(cluster, node, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		podLabels := map[string]string{"app": "legacy-names"}
		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "valkey-legacy-names-0-0", Namespace: ns},
			Spec: appsv1.StatefulSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: podLabels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  serverContainerName,
							Image: DefaultImage,
							Env: []corev1.EnvVar{{Name: "PRIMARY_AUTH", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: "internal-legacy-names-system-passwords"}, Key: replicationUser,
							}}}},
						}},
						Volumes: []corev1.Volume{{Name: "users-acl", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "internal-legacy-names-acl"}}}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, sts)).To(Succeed())

		r := newReconciler()
		Expect(reconcileOnce(r, cluster)).To(Succeed())

		By("copying the passwords the nodes hold instead of generating new ones")
		passwords := secret("valkey-legacy-names-system-passwords")
		Expect(passwords.Data).To(Equal(secret("internal-legacy-names-system-passwords").Data))
		Expect(metav1.IsControlledBy(passwords, cluster)).To(BeTrue())
		acl := secret("valkey-legacy-names-acl")
		Expect(string(acl.Data[aclFilename])).To(ContainSubstring("user _operator on #" + hashOf("legacy-operator-pw") + " "))

		By("moving the ValkeyNode to the new ACL Secret")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(node), node)).To(Succeed())
		Expect(node.Spec.UsersACLSecretName).To(Equal("valkey-legacy-names-acl"))

		By("keeping the legacy Secrets, equal to the new ones, while the workload reads them")
		for _, s := range renamedSecrets(cluster.Name) {
			Expect(secret(s.legacyName).Data).To(Equal(secret(s.name).Data), s.legacyName)
			Expect(secret(s.legacyName).Annotations).To(Equal(secret(s.name).Annotations), s.legacyName)
		}

		By("deleting them once the workload has rolled onto the new names")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(sts), sts)).To(Succeed())
		sts.Spec.Template.Spec.Containers[0].Env[0].ValueFrom.SecretKeyRef.Name = "valkey-legacy-names-system-passwords"
		sts.Spec.Template.Spec.Volumes[0].Secret.SecretName = "valkey-legacy-names-acl"
		Expect(k8sClient.Update(ctx, sts)).To(Succeed())
		Expect(reconcileOnce(r, cluster)).To(Succeed())
		Expect(exists("internal-legacy-names-system-passwords")).To(BeFalse())
		Expect(exists("internal-legacy-names-acl")).To(BeFalse())
		Expect(secret("valkey-legacy-names-system-passwords").Data).To(Equal(passwords.Data))
	})

	It("keeps running on the legacy aclfile while a user Secret is missing", func() {
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "legacy-names-unresolved", Namespace: ns},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards: 1,
				ACL: &valkeyiov1alpha1.ACLSpec{Users: []valkeyiov1alpha1.ACLUserSpec{{
					Name:     "alice",
					Password: &valkeyiov1alpha1.ACLUserPassword{Secret: &valkeyiov1alpha1.PasswordSecretSpec{Name: "legacy-names-unresolved-alice"}},
				}}},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		defer cleanup(cluster)
		const lastGood = "user alice on #abc +@read\nuser default off ~* &* +@all\n"
		createLegacySecrets(cluster, lastGood)

		r := newReconciler()
		Expect(reconcileOnce(r, cluster)).To(Succeed(), "an upgraded cluster has an aclfile to fall back to")

		Expect(string(secret("valkey-legacy-names-unresolved-acl").Data[aclFilename])).To(Equal(lastGood))
		node := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "legacy-names-unresolved-0-0"}, node)).To(Succeed())
		Expect(node.Spec.UsersACLSecretName).To(Equal("valkey-legacy-names-unresolved-acl"))
		stored := &valkeyiov1alpha1.ValkeyCluster{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), stored)).To(Succeed())
		degraded := meta.FindStatusCondition(stored.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Reason).To(Equal(valkeyiov1alpha1.ReasonUsersACLUnresolved))
	})
})
