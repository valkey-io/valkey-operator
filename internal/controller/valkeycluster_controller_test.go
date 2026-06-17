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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
	testutils "valkey.io/valkey-operator/test/utils"
)

var _ = Describe("ValkeyCluster Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		valkeycluster := &valkeyiov1alpha1.ValkeyCluster{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind ValkeyCluster")
			err := k8sClient.Get(ctx, typeNamespacedName, valkeycluster)
			if err != nil && errors.IsNotFound(err) {
				resource := &valkeyiov1alpha1.ValkeyCluster{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: valkeyiov1alpha1.ValkeyClusterSpec{
						Shards:   3,
						Replicas: 1,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &valkeyiov1alpha1.ValkeyCluster{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance ValkeyCluster")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			fakeRecorder := events.NewFakeRecorder(100)
			controllerReconciler := &ValkeyClusterReconciler{
				Client:    k8sClient,
				APIReader: k8sClient,
				Scheme:    k8sClient.Scheme(),
				Recorder:  fakeRecorder,
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Check status conditions
			updatedValkeyCluster := &valkeyiov1alpha1.ValkeyCluster{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updatedValkeyCluster)).To(Succeed())
			Expect(updatedValkeyCluster.Status.Conditions).ToNot(BeEmpty())

			// Verify that the Ready condition is set to False initially
			readyCondition := testutils.FindCondition(updatedValkeyCluster.Status.Conditions, valkeyiov1alpha1.ConditionReady)
			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))

			// Verify that the Progressing condition is set to True
			progressingCondition := testutils.FindCondition(updatedValkeyCluster.Status.Conditions, valkeyiov1alpha1.ConditionProgressing)
			Expect(progressingCondition).NotTo(BeNil())
			Expect(progressingCondition.Status).To(Equal(metav1.ConditionTrue))

			// Verify that events were recorded
			By("Verifying that events were recorded")
			events := collectEvents(fakeRecorder)
			Expect(events).ToNot(BeEmpty())
			Expect(events).To(ContainElement(ContainSubstring("ServiceCreated")))
			Expect(events).To(ContainElement(ContainSubstring("ConfigMapCreated")))
			Expect(events).To(ContainElement(ContainSubstring("ValkeyNodeCreated")))

		})
	})
})

var _ = Describe("ValkeyCluster config hash propagation", func() {
	ctx := context.Background()

	It("should set configHashKey annotation on ValkeyNodes and update it when cluster config changes", func() {
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "config-hash-test",
				Namespace: "default",
			},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:   1,
				Replicas: 0,
				Config:   map[string]string{"maxmemory": "100mb"},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		defer func() {
			nodeList := &valkeyiov1alpha1.ValkeyNodeList{}
			_ = k8sClient.List(ctx, nodeList, client.InNamespace("default"), client.MatchingLabels{LabelCluster: cluster.Name})
			for i := range nodeList.Items {
				_ = k8sClient.Delete(ctx, &nodeList.Items[i])
			}
			_ = k8sClient.Delete(ctx, cluster)
			cm := &corev1.ConfigMap{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: GetServerConfigMapName(cluster.Name), Namespace: cluster.Namespace}, cm); err == nil {
				_ = k8sClient.Delete(ctx, cm)
			}
			secret := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: getInternalSecretName(cluster.Name), Namespace: cluster.Namespace}, secret); err == nil {
				_ = k8sClient.Delete(ctx, secret)
			}
		}()

		r := &ValkeyClusterReconciler{
			Client:    k8sClient,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  events.NewFakeRecorder(100),
		}

		By("reconciling to create the ConfigMap and ValkeyNodes")
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
		Expect(err).NotTo(HaveOccurred())

		By("reading the config hash from the cluster ConfigMap")
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: GetServerConfigMapName(cluster.Name), Namespace: cluster.Namespace}, cm)).To(Succeed())
		initialCMHash := cm.Annotations[configHashKey]
		Expect(initialCMHash).NotTo(BeEmpty())

		By("verifying each ValkeyNode carries the roll hash (not the CM full hash) in its spec")
		// ServerConfigHash is the roll hash: it excludes live-settable keys so that
		// changes to those keys do not trigger a pod roll.
		initialRollHash := serverConfigRollHash(cluster)
		Expect(initialRollHash).NotTo(BeEmpty())
		nodeList := &valkeyiov1alpha1.ValkeyNodeList{}
		Expect(k8sClient.List(ctx, nodeList, client.InNamespace("default"), client.MatchingLabels{LabelCluster: cluster.Name})).To(Succeed())
		Expect(nodeList.Items).NotTo(BeEmpty())
		for _, n := range nodeList.Items {
			Expect(n.Spec.ServerConfigHash).To(Equal(initialRollHash),
				"ValkeyNode %s must have ServerConfigHash set to the roll hash", n.Name)
		}

		By("updating the cluster config with a live-settable key (maxmemory)")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
		cluster.Spec.Config["maxmemory"] = "200mb"
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

		By("reconciling after the config change")
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
		Expect(err).NotTo(HaveOccurred())

		By("verifying the ConfigMap has a new full hash (all keys changed)")
		updatedCM := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: GetServerConfigMapName(cluster.Name), Namespace: cluster.Namespace}, updatedCM)).To(Succeed())
		newCMHash := updatedCM.Annotations[configHashKey]
		Expect(newCMHash).NotTo(BeEmpty())
		Expect(newCMHash).NotTo(Equal(initialCMHash), "CM full-config hash should change when cluster config changes")

		By("verifying ValkeyNode ServerConfigHash is unchanged (maxmemory is live-settable, no pod roll)")
		newRollHash := serverConfigRollHash(cluster)
		Expect(newRollHash).To(Equal(initialRollHash), "roll hash must not change for live-settable key changes")
		updatedNodeList := &valkeyiov1alpha1.ValkeyNodeList{}
		Expect(k8sClient.List(ctx, updatedNodeList, client.InNamespace("default"), client.MatchingLabels{LabelCluster: cluster.Name})).To(Succeed())
		Expect(updatedNodeList.Items).NotTo(BeEmpty())
		for _, n := range updatedNodeList.Items {
			Expect(n.Spec.ServerConfigHash).To(Equal(newRollHash),
				"ValkeyNode %s ServerConfigHash must remain the roll hash after a live-settable key change", n.Name)
		}
	})
})

// staleConfigMapClient wraps a client.Client and always reports ConfigMaps as
// NotFound on Get, simulating the read-after-write staleness of the cached
// controller-runtime client used in production (mgr.GetClient()): immediately
// after a ConfigMap is created, the informer cache has not yet observed it, so
// a Get returns NotFound. Writes and all non-ConfigMap reads pass through.
type staleConfigMapClient struct {
	client.Client
}

func (c staleConfigMapClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.ConfigMap); ok {
		return errors.NewNotFound(corev1.Resource("configmaps"), key.Name)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

var _ = Describe("ValkeyCluster config hash on first reconcile", func() {
	ctx := context.Background()

	It("creates ValkeyNodes with the config hash even when the ConfigMap is not yet visible to the client cache", func() {
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "config-hash-stale-test",
				Namespace: "default",
			},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:   1,
				Replicas: 0,
				Config:   map[string]string{"maxmemory": "100mb"},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		defer func() {
			nodeList := &valkeyiov1alpha1.ValkeyNodeList{}
			_ = k8sClient.List(ctx, nodeList, client.InNamespace("default"), client.MatchingLabels{LabelCluster: cluster.Name})
			for i := range nodeList.Items {
				_ = k8sClient.Delete(ctx, &nodeList.Items[i])
			}
			_ = k8sClient.Delete(ctx, cluster)
			cm := &corev1.ConfigMap{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: GetServerConfigMapName(cluster.Name), Namespace: cluster.Namespace}, cm); err == nil {
				_ = k8sClient.Delete(ctx, cm)
			}
			secret := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: getInternalSecretName(cluster.Name), Namespace: cluster.Namespace}, secret); err == nil {
				_ = k8sClient.Delete(ctx, secret)
			}
		}()

		r := &ValkeyClusterReconciler{
			Client:    staleConfigMapClient{Client: k8sClient},
			APIReader: staleConfigMapClient{Client: k8sClient},
			Scheme:    k8sClient.Scheme(),
			Recorder:  events.NewFakeRecorder(100),
		}

		By("reconciling with a client that cannot yet see the freshly created ConfigMap")
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
		Expect(err).NotTo(HaveOccurred())

		By("reading the config hash the operator persisted to the ConfigMap")
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: GetServerConfigMapName(cluster.Name), Namespace: cluster.Namespace}, cm)).To(Succeed())
		Expect(cm.Annotations[configHashKey]).NotTo(BeEmpty())

		By("verifying every ValkeyNode was created with the roll hash, not an empty one")
		rollHash := serverConfigRollHash(cluster)
		Expect(rollHash).NotTo(BeEmpty())
		nodeList := &valkeyiov1alpha1.ValkeyNodeList{}
		Expect(k8sClient.List(ctx, nodeList, client.InNamespace("default"), client.MatchingLabels{LabelCluster: cluster.Name})).To(Succeed())
		Expect(nodeList.Items).NotTo(BeEmpty())
		for _, n := range nodeList.Items {
			Expect(n.Spec.ServerConfigHash).To(Equal(rollHash),
				"ValkeyNode %s must be created with the roll hash; an empty hash means the pod starts without the config-hash annotation and rolls as soon as the hash is later populated", n.Name)
		}
	})
})

var _ = Describe("pod scheduling issue handling", func() {
	ctx := context.Background()

	It("sets the cluster degraded when a Valkey pod is unschedulable", func() {
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-unschedulable-status-test",
				Namespace: "default",
			},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:   1,
				Replicas: 1,
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, cluster)
		})

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-unschedulable-status-test-0",
				Namespace: "default",
				Labels: map[string]string{
					LabelCluster: cluster.Name,
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  "server",
					Image: DefaultImage,
				}},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, pod)
		})

		pod.Status.Conditions = []corev1.PodCondition{{
			Type:    corev1.PodScheduled,
			Status:  corev1.ConditionFalse,
			Reason:  corev1.PodReasonUnschedulable,
			Message: "0/1 nodes are available: pod topology spread constraints not satisfied",
		}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		fakeRecorder := events.NewFakeRecorder(100)
		reconciler := &ValkeyClusterReconciler{
			Client:    k8sClient,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  fakeRecorder,
		}

		result, handled, err := reconciler.handlePodSchedulingIssues(ctx, cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(handled).To(BeTrue())
		Expect(result.RequeueAfter).To(Equal(10 * time.Second))

		updated := &valkeyiov1alpha1.ValkeyCluster{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), updated)).To(Succeed())
		Expect(updated.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateDegraded))
		Expect(updated.Status.Reason).To(Equal(valkeyiov1alpha1.ReasonPodUnschedulable))
		Expect(updated.Status.Message).To(ContainSubstring("pod topology spread constraints not satisfied"))

		degraded := testutils.FindCondition(updated.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Status).To(Equal(metav1.ConditionTrue))
		Expect(degraded.Reason).To(Equal(valkeyiov1alpha1.ReasonPodUnschedulable))

		events := collectEvents(fakeRecorder)
		Expect(events).To(ContainElement(ContainSubstring("Warning")))
		Expect(events).To(ContainElement(ContainSubstring(valkeyiov1alpha1.ReasonPodUnschedulable)))
		Expect(events).To(ContainElement(ContainSubstring("pod topology spread constraints not satisfied")))
	})

	It("clears stale unschedulable Ready and Degraded conditions when scheduling resolves", func() {
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-scheduling-resolved-test",
				Namespace: "default",
			},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:   1,
				Replicas: 1,
			},
		}
		setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonPodUnschedulable, "Pod is unschedulable", metav1.ConditionFalse)
		setCondition(cluster, valkeyiov1alpha1.ConditionDegraded, valkeyiov1alpha1.ReasonPodUnschedulable, "Pod is unschedulable", metav1.ConditionTrue)

		reconciler := &ValkeyClusterReconciler{
			Client:    k8sClient,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  events.NewFakeRecorder(100),
		}

		result, handled, err := reconciler.handlePodSchedulingIssues(ctx, cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(handled).To(BeFalse())
		Expect(result).To(Equal(reconcile.Result{}))
		Expect(testutils.FindCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionReady)).To(BeNil())
		Expect(testutils.FindCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)).To(BeNil())
	})

	It("ignores pods that are already scheduled", func() {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "scheduled-pod"},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{{
					Type:   corev1.PodScheduled,
					Status: corev1.ConditionTrue,
				}},
			},
		}

		Expect(podSchedulingIssueForPod(pod)).To(BeNil())
	})
})

var _ = Describe("reconcileUsersAcl", func() {
	Context("When reconciling ACL secrets", func() {
		It("should return an error when a user references a missing password secret", func() {
			ctx := context.Background()
			cluster := &valkeyiov1alpha1.ValkeyCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "acl-missing-secret-test",
					Namespace: "default",
				},
				Spec: valkeyiov1alpha1.ValkeyClusterSpec{
					Shards:   1,
					Replicas: 0,
					Users: []valkeyiov1alpha1.UserAclSpec{
						{
							Name:    "testuser",
							Enabled: true,
							PasswordSecret: valkeyiov1alpha1.PasswordSecretSpec{
								Name: "nonexistent-secret",
								Keys: []string{"password"},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, cluster) }()

			reconciler := &ValkeyClusterReconciler{
				Client:    k8sClient,
				APIReader: k8sClient,
				Scheme:    k8sClient.Scheme(),
				Recorder:  events.NewFakeRecorder(100),
			}

			err := reconciler.reconcileUsersAcl(ctx, cluster)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("testuser"))
		})

		It("should create the internal ACL secret with the ACL secret type", func() {
			ctx := context.Background()
			cluster := &valkeyiov1alpha1.ValkeyCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "acl-type-test",
					Namespace: "default",
				},
				Spec: valkeyiov1alpha1.ValkeyClusterSpec{
					Shards:   1,
					Replicas: 0,
					Users: []valkeyiov1alpha1.UserAclSpec{
						{
							Name:       "testuser",
							Enabled:    true,
							NoPassword: true,
							RawAcl:     "+@all",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, cluster) }()

			reconciler := &ValkeyClusterReconciler{
				Client:    k8sClient,
				APIReader: k8sClient,
				Scheme:    k8sClient.Scheme(),
				Recorder:  events.NewFakeRecorder(100),
			}

			err := reconciler.reconcileUsersAcl(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())

			internalSecret := &corev1.Secret{}
			secretName := types.NamespacedName{
				Name:      getInternalSecretName(cluster.Name),
				Namespace: cluster.Namespace,
			}
			Expect(k8sClient.Get(ctx, secretName, internalSecret)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, internalSecret) }()

			Expect(internalSecret.Type).To(Equal(AclSecretType))
		})

		It("should be able to create the internal ACL secret when unknown system user is present", func() {
			unknownUser := "_unknown"
			ctx := context.Background()
			cluster := &valkeyiov1alpha1.ValkeyCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "acl-unknown-system-user-test",
					Namespace: "default",
				},
				Spec: valkeyiov1alpha1.ValkeyClusterSpec{
					Shards:   1,
					Replicas: 0,
					Exporter: valkeyiov1alpha1.ExporterSpec{
						Enabled: false,
					},
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, cluster) }()

			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      getSystemPasswordSecretName(cluster.Name),
					Namespace: "default",
				},
				Type: AclSecretType,
				StringData: map[string]string{
					unknownUser: "",
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, secret) }()

			reconciler := &ValkeyClusterReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: events.NewFakeRecorder(100),
			}
			err := reconciler.reconcileUsersAcl(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())

			aclSecret := &corev1.Secret{}
			secretName := types.NamespacedName{
				Name:      getInternalSecretName(cluster.Name),
				Namespace: cluster.Namespace,
			}
			Expect(k8sClient.Get(ctx, secretName, aclSecret)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, aclSecret) }()
			acl := string(aclSecret.Data[aclFilename])
			Expect(acl).To(ContainSubstring("user _operator on"))
			Expect(acl).NotTo(ContainSubstring("user " + unknownUser))
		})

		It("should update the system user secret when spec.exporter is enabled after cluster creation", func() {
			ctx := context.Background()
			cluster := &valkeyiov1alpha1.ValkeyCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "acl-system-user-reconciliation-test",
					Namespace: "default",
				},
				Spec: valkeyiov1alpha1.ValkeyClusterSpec{
					Shards:   1,
					Replicas: 0,
					Exporter: valkeyiov1alpha1.ExporterSpec{
						Enabled: false,
					},
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, cluster) }()

			reconciler := &ValkeyClusterReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: events.NewFakeRecorder(100),
			}

			err := reconciler.reconcileUsersAcl(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())

			systemUsersSecret := &corev1.Secret{}
			secretName := types.NamespacedName{
				Name:      getSystemPasswordSecretName(cluster.Name),
				Namespace: cluster.Namespace,
			}
			Expect(k8sClient.Get(ctx, secretName, systemUsersSecret)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, systemUsersSecret) }()
			Expect(systemUsersSecret.Data).NotTo(HaveKey(exporterUser))

			cluster.Spec.Exporter.Enabled = true
			Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
			err = reconciler.reconcileUsersAcl(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, secretName, systemUsersSecret)).To(Succeed())
			Expect(systemUsersSecret.Data).To(HaveKey(exporterUser))
		})
	})
})

var _ = Describe("updateStatus", func() {
	var (
		cluster *valkeyiov1alpha1.ValkeyCluster
		r       *ValkeyClusterReconciler
		ctx     context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		cluster = &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "default",
			},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards: 1,
			},
		}
		// In a real scenario, the reconciler would be created with a real client,
		// but for this focused unit test, we can use a fake client if needed,
		// or pass nil if the tested function doesn't use the client.
		// For updateStatus, we need a client to Get the current object.
		// The envtest client is used here.
		r = &ValkeyClusterReconciler{
			Client:    k8sClient,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  events.NewFakeRecorder(100),
		}

		// Create the cluster object in the fake client
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
	})

	It("should set state to Ready when Ready condition is True", func() {
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:   valkeyiov1alpha1.ConditionReady,
			Status: metav1.ConditionTrue,
			Reason: valkeyiov1alpha1.ReasonClusterHealthy,
		})

		err := r.updateStatus(ctx, cluster, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(cluster.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
		Expect(cluster.Status.Reason).To(Equal(valkeyiov1alpha1.ReasonClusterHealthy))
	})

	It("should set state to Degraded when Degraded condition is True", func() {
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:   valkeyiov1alpha1.ConditionDegraded,
			Status: metav1.ConditionTrue,
			Reason: valkeyiov1alpha1.ReasonNodeAddFailed,
		})
		// A degraded cluster can still be progressing
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:   valkeyiov1alpha1.ConditionProgressing,
			Status: metav1.ConditionTrue,
			Reason: valkeyiov1alpha1.ReasonAddingNodes,
		})

		err := r.updateStatus(ctx, cluster, nil)
		Expect(err).NotTo(HaveOccurred())
		// Degraded takes precedence over Progressing
		Expect(cluster.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateDegraded))
		Expect(cluster.Status.Reason).To(Equal(valkeyiov1alpha1.ReasonNodeAddFailed))
	})

	It("should set state to Reconciling when Progressing is True and shards > 0", func() {
		// First set the cluster to Initializing
		cluster.Status.State = valkeyiov1alpha1.ClusterStateInitializing
		cluster.Status.Shards = 1 // Simulate that the cluster is not new
		Expect(k8sClient.Status().Update(ctx, cluster)).To(Succeed())

		// Now set the Progressing condition
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:   valkeyiov1alpha1.ConditionProgressing,
			Status: metav1.ConditionTrue,
			Reason: valkeyiov1alpha1.ReasonReconciling,
		})

		err := r.updateStatus(ctx, cluster, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(cluster.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReconciling))
		Expect(cluster.Status.Reason).To(Equal(valkeyiov1alpha1.ReasonReconciling))
	})

	It("should set state to Initializing when Progressing is True and shards = 0", func() {
		cluster.Status.Shards = 0 // This is a new cluster
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:   valkeyiov1alpha1.ConditionProgressing,
			Status: metav1.ConditionTrue,
			Reason: valkeyiov1alpha1.ReasonInitializing,
		})

		err := r.updateStatus(ctx, cluster, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(cluster.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReconciling))
		Expect(cluster.Status.Reason).To(Equal(valkeyiov1alpha1.ReasonInitializing))
	})
})

var _ = Describe("EventRecorder", func() {
	var (
		r            *ValkeyClusterReconciler
		ctx          context.Context
		fakeRecorder *events.FakeRecorder
	)

	BeforeEach(func() {
		ctx = context.Background()
		fakeRecorder = events.NewFakeRecorder(100)
		r = &ValkeyClusterReconciler{
			Client:    k8sClient,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  fakeRecorder,
		}
	})

	Context("When creating infrastructure resources", func() {
		It("should emit ServiceCreated event on successful service creation", func() {
			cluster := &valkeyiov1alpha1.ValkeyCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "event-test-cluster",
					Namespace: "default",
				},
				Spec: valkeyiov1alpha1.ValkeyClusterSpec{
					Shards:   3,
					Replicas: 1,
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, cluster) }()

			err := r.upsertService(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())

			events := collectEvents(fakeRecorder)
			Expect(events).To(ContainElement(ContainSubstring("ServiceCreated")))
			Expect(events).To(ContainElement(ContainSubstring("Normal")))
			Expect(events).To(ContainElement(ContainSubstring("Created headless Service")))
		})

		It("should emit ConfigMapCreated event on successful configmap creation", func() {
			cluster := &valkeyiov1alpha1.ValkeyCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "event-test-cluster",
					Namespace: "default",
				},
				Spec: valkeyiov1alpha1.ValkeyClusterSpec{
					Shards:   3,
					Replicas: 1,
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, cluster) }()

			Expect(r.upsertConfigMap(ctx, cluster)).To(Succeed())

			events := collectEvents(fakeRecorder)
			Expect(events).To(ContainElement(ContainSubstring("ConfigMapCreated")))
			Expect(events).To(ContainElement(ContainSubstring("Normal")))
		})

	})

	Context("When reconciling cluster state", func() {
		It("should emit WaitingForShards event when shards are missing", func() {
			cluster := &valkeyiov1alpha1.ValkeyCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "shards-test-cluster",
					Namespace: "default",
				},
				Spec: valkeyiov1alpha1.ValkeyClusterSpec{
					Shards:   3,
					Replicas: 1,
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, cluster) }()

			_, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			events := collectEvents(fakeRecorder)
			Expect(events).To(ContainElement(ContainSubstring("WaitingForShards")))
			Expect(events).To(ContainElement(ContainSubstring("Normal")))
		})
	})

	Context("When handling event types", func() {
		It("should emit normal events for successful operations", func() {
			cluster := &valkeyiov1alpha1.ValkeyCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "event-type-test",
					Namespace: "default",
				},
				Spec: valkeyiov1alpha1.ValkeyClusterSpec{
					Shards:   3,
					Replicas: 1,
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, cluster) }()

			Expect(r.upsertService(ctx, cluster)).To(Succeed())
			events := collectEvents(fakeRecorder)
			Expect(events).To(ContainElement(ContainSubstring("Normal")))
			Expect(events).To(ContainElement(ContainSubstring("ServiceCreated")))
		})
	})

})

// Helper function to collect events from the fake recorder
func collectEvents(recorder *events.FakeRecorder) []string {
	eventsList := []string{}
	for {
		select {
		case event := <-recorder.Events:
			eventsList = append(eventsList, event)
		default:
			return eventsList
		}
	}
}

// Helper function to filter events by reason
func filterEvents(eventsList []string, reason string) []string {
	filtered := []string{}
	for _, event := range eventsList {
		if strings.Contains(event, reason) {
			filtered = append(filtered, event)
		}
	}
	return filtered
}

var _ = Describe("reconcileValkeyNodes", func() {
	const clusterName = "node-reconcile-test"

	var (
		r            *ValkeyClusterReconciler
		fakeRecorder *events.FakeRecorder
		cluster      *valkeyiov1alpha1.ValkeyCluster
		testCtx      context.Context
	)

	var (
		node00   = valkeyNodeName(clusterName, 0, 0) // shard 0 primary
		node01   = valkeyNodeName(clusterName, 0, 1) // shard 0 replica
		node10   = valkeyNodeName(clusterName, 1, 0) // shard 1 primary
		node11   = valkeyNodeName(clusterName, 1, 1) // shard 1 replica
		allNodes = []string{node00, node01, node10, node11}
		// reconcileValkeyNodes update order: descending node index within each shard
		// means replicas are updated before the primary.
		updateOrder = []string{node01, node00, node11, node10}
	)

	BeforeEach(func() {
		testCtx = context.Background()
		fakeRecorder = events.NewFakeRecorder(100)
		r = &ValkeyClusterReconciler{
			Client:    k8sClient,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  fakeRecorder,
		}
		cluster = &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      clusterName,
				Namespace: "default",
			},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:       2,
				Replicas:     1,
				Image:        "valkey/valkey:9.0.0",
				WorkloadType: valkeyiov1alpha1.WorkloadTypeStatefulSet,
			},
		}
		Expect(k8sClient.Create(testCtx, cluster)).To(Succeed())
	})

	AfterEach(func() {
		nodeList := &valkeyiov1alpha1.ValkeyNodeList{}
		Expect(k8sClient.List(testCtx, nodeList,
			client.InNamespace("default"),
			client.MatchingLabels{LabelCluster: clusterName})).To(Succeed())
		for i := range nodeList.Items {
			Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, &nodeList.Items[i]))).To(Succeed())
		}
		Expect(k8sClient.Delete(testCtx, cluster)).To(Succeed())
	})

	// setReady marks a ValkeyNode's status as ready via the status subresource.
	setReady := func(name string) {
		GinkgoHelper()
		node := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: "default"}, node)).To(Succeed())
		node.Status.Ready = true
		node.Status.ObservedGeneration = node.Generation
		Expect(k8sClient.Status().Update(testCtx, node)).To(Succeed())
	}

	setNotReady := func(name string) {
		GinkgoHelper()
		node := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: "default"}, node)).To(Succeed())
		node.Status.Ready = false
		// Deliberately does not update ObservedGeneration: simulates the pod
		// becoming not-ready while ObservedGeneration may already be stale (from
		// a prior spec update). The generation gate will fire before the Ready
		// check in that case; if generations already match, the Ready check fires.
		Expect(k8sClient.Status().Update(testCtx, node)).To(Succeed())
	}

	getResourceVersion := func(name string) string {
		GinkgoHelper()
		node := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: "default"}, node)).To(Succeed())
		return node.ResourceVersion
	}

	getImage := func(name string) string {
		GinkgoHelper()
		node := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: "default"}, node)).To(Succeed())
		return node.Spec.Image
	}

	// reconcileNodes lists the current ValkeyNodes and calls reconcileValkeyNodes.
	reconcileNodes := func() (bool, error) {
		GinkgoHelper()
		nodeList := &valkeyiov1alpha1.ValkeyNodeList{}
		Expect(k8sClient.List(testCtx, nodeList,
			client.InNamespace("default"),
			client.MatchingLabels{LabelCluster: clusterName})).To(Succeed())
		return r.reconcileValkeyNodes(testCtx, cluster, nodeList, "")
	}

	// createAllNodes runs a single reconcile that creates all 4 ValkeyNode CRs.
	// On first reconcile every position is Created so the loop completes without
	// triggering an early-exit requeue.
	createAllNodes := func() {
		GinkgoHelper()
		requeue, err := reconcileNodes()
		Expect(err).NotTo(HaveOccurred())
		Expect(requeue).To(BeFalse(), "initial create pass must not requeue")
	}

	markAllReady := func() {
		GinkgoHelper()
		for _, name := range allNodes {
			setReady(name)
		}
	}

	It("does not update ValkeyNodes when spec is unchanged", func() {
		By("creating all nodes")
		createAllNodes()

		By("marking all nodes ready")
		markAllReady()

		By("recording ResourceVersions before second reconcile")
		rvs := map[string]string{}
		for _, name := range allNodes {
			rvs[name] = getResourceVersion(name)
		}

		By("reconciling with no spec change")
		requeue, err := reconcileNodes()
		Expect(err).NotTo(HaveOccurred())
		Expect(requeue).To(BeFalse())

		By("verifying no ResourceVersions changed")
		for _, name := range allNodes {
			Expect(getResourceVersion(name)).To(Equal(rvs[name]),
				"ResourceVersion of %s must not change on idempotent reconcile", name)
		}
	})

	It("propagates spec changes one node at a time in shard order (replicas before primary)", func() {
		By("creating all nodes and marking them ready")
		createAllNodes()
		markAllReady()

		By("updating cluster image to trigger a rolling update")
		const newImage = "valkey/valkey:9.1.0"
		cluster.Spec.Image = newImage

		for i, name := range updateOrder {
			By("reconcile: only " + name + " should be updated this pass")
			rvsBefore := map[string]string{}
			for _, n := range allNodes {
				rvsBefore[n] = getResourceVersion(n)
			}

			requeue, err := reconcileNodes()
			Expect(err).NotTo(HaveOccurred())
			Expect(requeue).To(BeTrue(), "expected requeue after updating %s", name)

			Expect(getImage(name)).To(Equal(newImage), "image of %s must be propagated", name)
			Expect(getResourceVersion(name)).NotTo(Equal(rvsBefore[name]),
				"ResourceVersion of %s must change after update", name)

			// Nodes later in the update order must not yet be touched.
			for _, other := range updateOrder[i+1:] {
				Expect(getResourceVersion(other)).To(Equal(rvsBefore[other]),
					"%s must not be updated before %s is ready", other, name)
				Expect(getImage(other)).NotTo(Equal(newImage),
					"%s must retain old image before %s is ready", other, name)
			}

			By("marking " + name + " ready to advance the rollout")
			setReady(name)
		}

		By("verifying all nodes received the new image")
		for _, name := range allNodes {
			Expect(getImage(name)).To(Equal(newImage),
				"all nodes must have the new image after full rollout: %s", name)
		}

		By("final reconcile must be idempotent once all nodes are updated and ready")
		rvsFinal := map[string]string{}
		for _, name := range allNodes {
			rvsFinal[name] = getResourceVersion(name)
		}
		requeue, err := reconcileNodes()
		Expect(err).NotTo(HaveOccurred())
		Expect(requeue).To(BeFalse())
		for _, name := range allNodes {
			Expect(getResourceVersion(name)).To(Equal(rvsFinal[name]))
		}
	})

	It("pauses rollout while an updated node is not yet ready", func() {
		By("creating all nodes and marking them ready")
		createAllNodes()
		markAllReady()

		By("updating cluster image")
		cluster.Spec.Image = "valkey/valkey:9.1.0"

		By("first reconcile: " + node01 + " (shard 0 replica) is updated and left not-ready")
		requeue, err := reconcileNodes()
		Expect(err).NotTo(HaveOccurred())
		Expect(requeue).To(BeTrue())

		By("simulating ValkeyNode controller marking " + node01 + " not-ready after spec update")
		setNotReady(node01)

		By("recording ResourceVersions of nodes not yet updated")
		rvOthers := map[string]string{}
		for _, name := range updateOrder[1:] {
			rvOthers[name] = getResourceVersion(name)
		}

		By("reconciling while " + node01 + " is not ready (and ObservedGeneration is stale): rollout must pause")
		requeue, err = reconcileNodes()
		Expect(err).NotTo(HaveOccurred())
		Expect(requeue).To(BeTrue(), "expected requeue while %s is not ready", node01)
		for name, rv := range rvOthers {
			Expect(getResourceVersion(name)).To(Equal(rv),
				"rollout paused: %s must not be updated while %s is not ready", name, node01)
			Expect(getImage(name)).NotTo(Equal("valkey/valkey:9.1.0"),
				"rollout paused: %s must retain the old image while %s is not ready", name, node01)
		}

		By("marking " + node01 + " ready: rollout resumes and " + node00 + " is updated next")
		setReady(node01)
		requeue, err = reconcileNodes()
		Expect(err).NotTo(HaveOccurred())
		Expect(requeue).To(BeTrue())
		Expect(getImage(node00)).To(Equal("valkey/valkey:9.1.0"))
	})
})

var _ = Describe("reconcileValkeyNode", func() {
	const clusterName = "single-node-reconcile-test"

	var (
		r            *ValkeyClusterReconciler
		fakeRecorder *events.FakeRecorder
		cluster      *valkeyiov1alpha1.ValkeyCluster
		testCtx      context.Context
	)

	const (
		shardIndex = 0
		nodeIndex  = 0
	)

	BeforeEach(func() {
		testCtx = context.Background()
		fakeRecorder = events.NewFakeRecorder(100)
		r = &ValkeyClusterReconciler{
			Client:    k8sClient,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  fakeRecorder,
		}
		cluster = &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      clusterName,
				Namespace: "default",
			},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:       1,
				Replicas:     0,
				Image:        "valkey/valkey:9.0.0",
				WorkloadType: valkeyiov1alpha1.WorkloadTypeStatefulSet,
			},
		}
		Expect(k8sClient.Create(testCtx, cluster)).To(Succeed())
	})

	AfterEach(func() {
		nodeName := valkeyNodeName(clusterName, shardIndex, nodeIndex)
		node := &valkeyiov1alpha1.ValkeyNode{}
		if err := k8sClient.Get(testCtx, types.NamespacedName{Name: nodeName, Namespace: "default"}, node); err == nil {
			Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, node))).To(Succeed())
		}
		Expect(k8sClient.Delete(testCtx, cluster)).To(Succeed())
	})

	setNodeReady := func(ready bool) {
		GinkgoHelper()
		nodeName := valkeyNodeName(clusterName, shardIndex, nodeIndex)
		node := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: nodeName, Namespace: "default"}, node)).To(Succeed())
		node.Status.Ready = ready
		node.Status.ObservedGeneration = node.Generation
		Expect(k8sClient.Status().Update(testCtx, node)).To(Succeed())
	}

	It("creates the ValkeyNode and emits ValkeyNodeCreated event", func() {
		result, err := r.reconcileValkeyNode(testCtx, cluster, shardIndex, nodeIndex, nil, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(nodeCreated))

		evts := collectEvents(fakeRecorder)
		Expect(filterEvents(evts, "ValkeyNodeCreated")).To(HaveLen(1))
		Expect(evts).To(ContainElement(MatchRegexp(`Created ValkeyNode for shard \d+ node \d+`)))
	})

	It("updates the ValkeyNode spec, emits ValkeyNodeUpdated event, and signals requeue", func() {
		_, err := r.reconcileValkeyNode(testCtx, cluster, shardIndex, nodeIndex, nil, "")
		Expect(err).NotTo(HaveOccurred())
		collectEvents(fakeRecorder) // drain creation event

		cluster.Spec.Image = "valkey/valkey:9.1.0"
		result, err := r.reconcileValkeyNode(testCtx, cluster, shardIndex, nodeIndex, nil, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(nodeRequeued))

		evts := collectEvents(fakeRecorder)
		Expect(filterEvents(evts, "ValkeyNodeUpdated")).To(HaveLen(1))
	})

	It("signals requeue when node is unchanged but not yet ready", func() {
		_, err := r.reconcileValkeyNode(testCtx, cluster, shardIndex, nodeIndex, nil, "")
		Expect(err).NotTo(HaveOccurred())
		collectEvents(fakeRecorder) // drain creation event

		// Status.Ready defaults to false after creation
		result, err := r.reconcileValkeyNode(testCtx, cluster, shardIndex, nodeIndex, nil, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(nodeRequeued))
	})

	It("does not requeue when node is unchanged and ready", func() {
		_, err := r.reconcileValkeyNode(testCtx, cluster, shardIndex, nodeIndex, nil, "")
		Expect(err).NotTo(HaveOccurred())
		collectEvents(fakeRecorder) // drain creation event

		setNodeReady(true)

		result, err := r.reconcileValkeyNode(testCtx, cluster, shardIndex, nodeIndex, nil, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(nodeUnchanged))
	})

	It("signals requeue when node is unchanged but ObservedGeneration is stale", func() {
		// Create the node
		_, err := r.reconcileValkeyNode(testCtx, cluster, shardIndex, nodeIndex, nil, "")
		Expect(err).NotTo(HaveOccurred())

		// Mark ready but leave ObservedGeneration at 0
		// (simulates ValkeyNode controller hasn't processed yet)
		nodeName := valkeyNodeName(clusterName, shardIndex, nodeIndex)
		node := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: nodeName, Namespace: "default"}, node)).To(Succeed())
		node.Status.Ready = true
		// deliberately NOT setting ObservedGeneration
		Expect(k8sClient.Status().Update(testCtx, node)).To(Succeed())

		// Because ObservedGeneration > 0 guard: newly created node with
		// ObservedGeneration=0 falls through to the Ready check, which
		// passes (Ready=true). No requeue.
		result, err := r.reconcileValkeyNode(testCtx, cluster, shardIndex, nodeIndex, nil, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(nodeUnchanged))

		// Now simulate the ValkeyNode controller having processed once
		// (ObservedGeneration=1), then a spec change bumps Generation to 2.
		// We fake this by setting ObservedGeneration=1 while Generation is
		// already 1, then updating the cluster spec to trigger an update.
		node = &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: nodeName, Namespace: "default"}, node)).To(Succeed())
		node.Status.ObservedGeneration = node.Generation
		Expect(k8sClient.Status().Update(testCtx, node)).To(Succeed())

		// Change cluster spec to trigger an update on next reconcile
		cluster.Spec.Image = "valkey/valkey:9.1.0"
		result, err = r.reconcileValkeyNode(testCtx, cluster, shardIndex, nodeIndex, nil, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(nodeRequeued), "should requeue after updating node")

		// Next reconcile: spec matches (OperationResultNone), but
		// Generation (2) != ObservedGeneration (1) — must requeue.
		result, err = r.reconcileValkeyNode(testCtx, cluster, shardIndex, nodeIndex, nil, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(nodeRequeued), "should requeue while ObservedGeneration is stale")
	})
})

var _ = Describe("buildClusterValkeyNode config passthrough", Label("liveconfig"), func() {
	It("copies cluster Spec.Config into the built ValkeyNode spec", func() {
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "default"},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:   1,
				Replicas: 0,
				Config:   map[string]string{"maxmemory-policy": "allkeys-lru"},
			},
		}
		node := buildClusterValkeyNode(cluster, 0, 0)
		Expect(node.Spec.Config).To(Equal(map[string]string{"maxmemory-policy": "allkeys-lru"}))
	})
})
