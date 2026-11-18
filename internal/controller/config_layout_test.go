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
	stderrors "errors"
	"fmt"
	"slices"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// withConfigRevision sets spec.configRevision to the revision the node's spec
// renders, as a parent does, and returns the node.
func withConfigRevision(node *valkeyiov1alpha1.ValkeyNode) *valkeyiov1alpha1.ValkeyNode {
	node.Spec.ConfigRevision = pinnedConfigRevision(renderPinnedConfig(node))
	return node
}

// Config layout scenarios. envtest runs no StatefulSet controller,
// kubelet or garbage collector, so the specs drive both reconcilers by hand,
// mark nodes Ready the way the reconcileValkeyNodes specs do, and stand in
// for CONFIG SET by writing status.appliedLiveConfig.
var _ = Describe("config layout", func() {
	const ns = "default"

	var (
		ctx          context.Context
		cluster      *valkeyiov1alpha1.ValkeyCluster
		parentWrites *writeCountingClient
		nodeWrites   *writeCountingClient
		pr           *ValkeyClusterReconciler
		nr           *ValkeyNodeReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()
		parentWrites = newWriteCountingClient(k8sClient)
		nodeWrites = newWriteCountingClient(k8sClient)
		pr = &ValkeyClusterReconciler{
			Client:        parentWrites,
			APIReader:     k8sClient,
			Scheme:        k8sClient.Scheme(),
			Recorder:      events.NewFakeRecorder(1000),
			ValkeyClients: testValkeyClients(k8sClient),
		}
		nr = &ValkeyNodeReconciler{
			Client:    nodeWrites,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  events.NewFakeRecorder(1000),
		}
	})

	cleanup := func(name string) {
		sel := client.MatchingLabels{LabelCluster: name}
		pods := &corev1.PodList{}
		Expect(k8sClient.List(ctx, pods, client.InNamespace(ns), sel)).To(Succeed())
		for i := range pods.Items {
			p := &pods.Items[i]
			if len(p.Finalizers) > 0 {
				p.Finalizers = nil
				Expect(client.IgnoreNotFound(k8sClient.Update(ctx, p))).To(Succeed())
			}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, p))).To(Succeed())
		}
		nodes := &valkeyiov1alpha1.ValkeyNodeList{}
		Expect(k8sClient.List(ctx, nodes, client.InNamespace(ns), sel)).To(Succeed())
		for i := range nodes.Items {
			n := &nodes.Items[i]
			if len(n.Finalizers) > 0 {
				n.Finalizers = nil
				Expect(client.IgnoreNotFound(k8sClient.Update(ctx, n))).To(Succeed())
			}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, n))).To(Succeed())
		}
		Expect(k8sClient.DeleteAllOf(ctx, &appsv1.StatefulSet{}, client.InNamespace(ns), sel)).To(Succeed())
		Expect(k8sClient.DeleteAllOf(ctx, &corev1.ConfigMap{}, client.InNamespace(ns), sel)).To(Succeed())
		svcs := &corev1.ServiceList{}
		Expect(k8sClient.List(ctx, svcs, client.InNamespace(ns), sel)).To(Succeed())
		for i := range svcs.Items {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &svcs.Items[i]))).To(Succeed())
		}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}))).To(Succeed())
	}

	newCluster := func(name string, shards int32, config map[string]string) {
		GinkgoHelper()
		cluster = &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:       shards,
				Image:        "valkey/valkey:9.0.0",
				WorkloadType: valkeyiov1alpha1.WorkloadTypeStatefulSet,
				Config:       config,
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(cleanup, name)
	}

	updateCluster := func(mutate func(*valkeyiov1alpha1.ValkeyCluster)) {
		GinkgoHelper()
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
		mutate(cluster)
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
	}

	listNodes := func() []valkeyiov1alpha1.ValkeyNode {
		GinkgoHelper()
		nodes := &valkeyiov1alpha1.ValkeyNodeList{}
		Expect(k8sClient.List(ctx, nodes, client.InNamespace(ns), client.MatchingLabels{LabelCluster: cluster.Name})).To(Succeed())
		slices.SortFunc(nodes.Items, func(a, b valkeyiov1alpha1.ValkeyNode) int {
			if a.Name < b.Name {
				return -1
			}
			if a.Name > b.Name {
				return 1
			}
			return 0
		})
		return nodes.Items
	}

	getNode := func(name string) *valkeyiov1alpha1.ValkeyNode {
		GinkgoHelper()
		n := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, n)).To(Succeed())
		return n
	}

	revisions := func() map[string]string {
		GinkgoHelper()
		out := map[string]string{}
		for _, n := range listNodes() {
			out[n.Name] = n.Spec.ConfigRevision
		}
		return out
	}

	nodeCM := func(name string) *corev1.ConfigMap {
		GinkgoHelper()
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeConfigMapName(name), Namespace: ns}, cm)).To(Succeed())
		return cm
	}

	sts := func(name string) *appsv1.StatefulSet {
		GinkgoHelper()
		s := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourcePrefix + name, Namespace: ns}, s)).To(Succeed())
		return s
	}

	// parentPass runs one pass of the cluster controller's node loop.
	parentPass := func() bool {
		GinkgoHelper()
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
		nodes := &valkeyiov1alpha1.ValkeyNodeList{Items: listNodes()}
		requeue, err := pr.reconcileValkeyNodes(ctx, cluster, nodes, nil, "")
		Expect(err).NotTo(HaveOccurred())
		return requeue
	}

	reconcileNode := func(name string) {
		GinkgoHelper()
		_, err := nr.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
	}

	markReady := func(name string) {
		GinkgoHelper()
		n := getNode(name)
		n.Status.Ready = true
		n.Status.ObservedGeneration = n.Generation
		Expect(k8sClient.Status().Update(ctx, n)).To(Succeed())
	}

	// nodesRound reconciles every node, then marks it Ready as a running pod
	// would.
	nodesRound := func() {
		GinkgoHelper()
		for _, n := range listNodes() {
			reconcileNode(n.Name)
			markReady(n.Name)
		}
	}

	settle := func() {
		GinkgoHelper()
		for range 50 {
			requeue := parentPass()
			nodesRound()
			if !requeue {
				return
			}
		}
		Fail("cluster did not settle in 50 passes")
	}

	setApplied := func(name string, applied map[string]string) {
		GinkgoHelper()
		n := getNode(name)
		n.Status.AppliedLiveConfig = configEntries(applied)
		Expect(k8sClient.Status().Update(ctx, n)).To(Succeed())
	}

	liveConf := func(name string) string {
		GinkgoHelper()
		return nodeCM(name).Data[liveConfigFileKey]
	}

	It("RestartOnlyChangeRollsOneNodeAtATime", func() {
		newCluster("cfg-roll", 3, map[string]string{"appendfsync": "everysec"})
		settle()
		before := revisions()
		Expect(before).To(HaveLen(3))
		for name, rev := range before {
			Expect(configRevisionKeys(nodeCM(name))).To(Equal([]string{configRevisionFileKey(rev)}))
			Expect(sts(name).Spec.Template.Annotations).To(HaveKeyWithValue(configRevisionKey, rev))
		}

		updateCluster(func(c *valkeyiov1alpha1.ValkeyCluster) { c.Spec.Config["appendfsync"] = "always" })

		changed := map[string]bool{}
		for pass := range 3 {
			By(fmt.Sprintf("pass %d", pass))
			Expect(parentPass()).To(BeTrue())
			var now []string
			for name, rev := range revisions() {
				if rev != before[name] && !changed[name] {
					now = append(now, name)
				}
			}
			Expect(now).To(HaveLen(1), "exactly one node gets a new config revision per pass")
			changed[now[0]] = true
			for name := range before {
				if !changed[name] {
					Expect(revisions()[name]).To(Equal(before[name]), "%s must wait its turn", name)
				}
			}
			nodesRound()
			rolled := getNode(now[0])
			Expect(nodeCM(rolled.Name).Data).To(HaveKey(configRevisionFileKey(rolled.Spec.ConfigRevision)))
			Expect(sts(rolled.Name).Spec.Template.Annotations).To(HaveKeyWithValue(configRevisionKey, rolled.Spec.ConfigRevision))
		}
		Expect(parentPass()).To(BeFalse())
		for name, rev := range revisions() {
			Expect(rev).NotTo(Equal(before[name]))
			Expect(nodeCM(name).Data[configRevisionFileKey(rev)]).To(ContainSubstring("appendfsync always"))
		}
	})

	It("LiveChangeDoesNotRoll", func() {
		newCluster("cfg-live", 3, map[string]string{"maxmemory": "100mb"})
		settle()
		for _, n := range listNodes() {
			setApplied(n.Name, map[string]string{"maxmemory": "100mb"})
		}
		nodesRound()
		before := revisions()
		templates := map[string]string{}
		for name := range before {
			templates[name] = podTemplateRollHash(sts(name).Spec.Template)
			Expect(liveConf(name)).To(Equal("maxmemory 100mb\n"))
		}

		updateCluster(func(c *valkeyiov1alpha1.ValkeyCluster) { c.Spec.Config["maxmemory"] = "200mb" })
		settle()

		for name, rev := range revisions() {
			Expect(rev).To(Equal(before[name]), "a live key must not change the config revision")
			Expect(getNode(name).Spec.Config).To(HaveKeyWithValue("maxmemory", "200mb"))
			Expect(podTemplateRollHash(sts(name).Spec.Template)).To(Equal(templates[name]), "a live key must not change the pod template")
			Expect(liveConf(name)).To(Equal("maxmemory 100mb\n"), "live.conf follows what the server accepted")
		}

		for name := range before {
			setApplied(name, map[string]string{"maxmemory": "200mb"})
			reconcileNode(name)
			Expect(liveConf(name)).To(Equal("maxmemory 200mb\n"))
		}
	})

	It("rewrites live.conf in the reconcile that records the applied value", func() {
		newCluster("cfg-live-lag", 1, map[string]string{"maxmemory": "100mb"})
		settle()
		name := valkeyNodeName(cluster.Name, 0, 0)
		setApplied(name, map[string]string{"maxmemory": "100mb"})
		reconcileNode(name)
		updateCluster(func(c *valkeyiov1alpha1.ValkeyCluster) { c.Spec.Config["maxmemory"] = "200mb" })
		settle()
		Expect(liveConf(name)).To(Equal("maxmemory 100mb\n"))

		fake := &fakeConfigClient{}
		nr.newConfigClient = func(context.Context, *ValkeyNodeReconciler, *valkeyiov1alpha1.ValkeyNode) (valkeyConfigClient, error) {
			return fake, nil
		}
		nodeWrites.Reset()
		_, _, err := nr.applyLiveSettings(ctx, getNode(name))
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.params).To(Equal(map[string]string{"maxmemory": "200mb"}))
		Expect(getNode(name).Status.AppliedLiveConfig).To(Equal([]valkeyiov1alpha1.ConfigEntry{{Name: "maxmemory", Value: "200mb"}}))
		Expect(liveConf(name)).To(Equal("maxmemory 200mb\n"))
		Expect(nodeWrites.Count("ConfigMap", verbPatch)).To(Equal(1))

		By("writing nothing when live.conf already matches")
		_, _, err = nr.applyLiveSettings(ctx, getNode(name))
		Expect(err).NotTo(HaveOccurred())
		Expect(nodeWrites.Count("ConfigMap", verbPatch)).To(Equal(1))
	})

	It("AllowlistGrowthDoesNotRoll", func() {
		newCluster("cfg-grow", 3, map[string]string{"appendfsync": "everysec"})
		settle()
		before := revisions()
		templates := map[string]string{}
		for name := range before {
			templates[name] = podTemplateRollHash(sts(name).Spec.Template)
		}

		liveConfigAllowlist["appendfsync"] = struct{}{}
		DeferCleanup(func() { delete(liveConfigAllowlist, "appendfsync") })
		for _, n := range listNodes() {
			Expect(pinnedConfigRevision(renderPinnedConfig(&n))).NotTo(Equal(before[n.Name]),
				"the desired revision moves, so the parent has to read the live one")
		}

		settle()
		for name, rev := range revisions() {
			Expect(rev).To(Equal(before[name]), "allowlist growth must not change the config revision")
			Expect(podTemplateRollHash(sts(name).Spec.Template)).To(Equal(templates[name]))
			Expect(configRevisionKeys(nodeCM(name))).To(Equal([]string{configRevisionFileKey(rev)}))
			Expect(liveConf(name)).To(Equal("appendfsync everysec\n"))
		}
	})

	It("ScaleOutAndIn", func() {
		newCluster("cfg-scale", 3, nil)
		settle()
		Expect(listNodes()).To(HaveLen(3))

		updateCluster(func(c *valkeyiov1alpha1.ValkeyCluster) { c.Spec.Shards = 4 })
		settle()
		Expect(listNodes()).To(HaveLen(4))
		added := getNode(valkeyNodeName(cluster.Name, 3, 0))
		cm := nodeCM(added.Name)
		Expect(configRevisionKeys(cm)).To(Equal([]string{configRevisionFileKey(added.Spec.ConfigRevision)}))
		Expect(cm.Labels).To(HaveKeyWithValue(LabelCluster, cluster.Name), "the parent's ConfigMap watch maps by this label")

		updateCluster(func(c *valkeyiov1alpha1.ValkeyCluster) { c.Spec.Shards = 3 })
		// handleScaleIn deletes the node once its slots are gone, which needs
		// a live cluster. Delete it as that step would.
		Expect(k8sClient.Delete(ctx, added)).To(Succeed())
		settle()
		Expect(listNodes()).To(HaveLen(3))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(added), &valkeyiov1alpha1.ValkeyNode{}))).To(BeTrue())

		// envtest runs no garbage collector. The controller reference is what
		// removes the ConfigMap with the node.
		owner := metav1.GetControllerOf(nodeCM(added.Name))
		Expect(owner).NotTo(BeNil())
		Expect(owner.UID).To(Equal(added.UID))
		Expect(owner.Kind).To(Equal("ValkeyNode"))
		Expect(owner.BlockOwnerDeletion).To(HaveValue(BeTrue()))
	})

	It("OldRevisionCleanup", func() {
		newCluster("cfg-cleanup", 1, map[string]string{"appendfsync": "everysec"})
		settle()
		name := valkeyNodeName(cluster.Name, 0, 0)
		oldRev := getNode(name).Spec.ConfigRevision
		oldKey := configRevisionFileKey(oldRev)

		By("a pod mounting the old revision, held by a finalizer so it can sit terminating")
		template := sts(name).Spec.Template
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:       resourcePrefix + name + "-0",
				Namespace:  ns,
				Labels:     template.Labels,
				Finalizers: []string{"test.valkey.io/hold"},
			},
			Spec: template.Spec,
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())

		By("rolling the node to a new revision")
		updateCluster(func(c *valkeyiov1alpha1.ValkeyCluster) { c.Spec.Config["appendfsync"] = "always" })
		Expect(parentPass()).To(BeTrue())
		newRev := getNode(name).Spec.ConfigRevision
		Expect(newRev).NotTo(Equal(oldRev))
		newKey := configRevisionFileKey(newRev)
		nodesRound()
		Expect(sts(name).Spec.Template.Annotations).To(HaveKeyWithValue(configRevisionKey, newRev))
		Expect(configRevisionKeys(nodeCM(name))).To(ConsistOf(oldKey, newKey), "the roll has not finished")

		By("finishing the roll while the pod still mounts the old revision")
		s := sts(name)
		s.Status.ObservedGeneration = s.Generation
		s.Status.Replicas = 1
		s.Status.CurrentRevision = "rev-2"
		s.Status.UpdateRevision = "rev-2"
		Expect(k8sClient.Status().Update(ctx, s)).To(Succeed())
		reconcileNode(name)
		Expect(configRevisionKeys(nodeCM(name))).To(ConsistOf(oldKey, newKey), "a pod still mounts the old revision")

		By("deleting the pod, which stays terminating")
		Expect(k8sClient.Delete(ctx, pod)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)).To(Succeed())
		Expect(pod.DeletionTimestamp).NotTo(BeNil())
		reconcileNode(name)
		Expect(configRevisionKeys(nodeCM(name))).To(ConsistOf(oldKey, newKey), "a terminating pod still mounts the old revision")

		By("letting the pod go")
		pod.Finalizers = nil
		Expect(k8sClient.Update(ctx, pod)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}))
		}).Should(BeTrue())
		reconcileNode(name)
		cm := nodeCM(name)
		Expect(configRevisionKeys(cm)).To(Equal([]string{newKey}))
		Expect(cm.Data).To(HaveKey(liveConfigFileKey))
	})

	It("LiveWriteCount", func() {
		const nodes = 10
		newCluster("cfg-count", nodes, map[string]string{"maxmemory": "100mb"})
		settle()
		Expect(pr.upsertConfigMap(ctx, cluster)).To(Succeed())
		serverCM := func() *corev1.ConfigMap {
			GinkgoHelper()
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: GetServerConfigMapName(cluster.Name), Namespace: ns}, cm)).To(Succeed())
			return cm
		}
		serverRV := serverCM().ResourceVersion
		for _, n := range listNodes() {
			setApplied(n.Name, map[string]string{"maxmemory": "100mb"})
		}
		nodesRound()
		parentWrites.Reset()
		nodeWrites.Reset()

		updateCluster(func(c *valkeyiov1alpha1.ValkeyCluster) { c.Spec.Config["maxmemory"] = "200mb" })
		Expect(pr.upsertConfigMap(ctx, cluster)).To(Succeed())
		settle()
		for _, n := range listNodes() {
			setApplied(n.Name, map[string]string{"maxmemory": "200mb"})
			reconcileNode(n.Name)
			markReady(n.Name)
			Expect(parentPass()).To(BeFalse())
		}
		nodesRound()
		for _, n := range listNodes() {
			Expect(liveConf(n.Name)).To(Equal("maxmemory 200mb\n"))
		}

		configMapWrites := func(c *writeCountingClient) map[string]int {
			out := map[string]int{}
			for k, n := range c.Counts() {
				if k.Kind == "ConfigMap" {
					out[k.Verb] = n
				}
			}
			return out
		}
		Expect(configMapWrites(nodeWrites)).To(Equal(map[string]int{verbPatch: nodes}), "one live.conf patch per node")
		Expect(configMapWrites(parentWrites)).To(BeEmpty(), "the parent writes no ConfigMap for a live change")
		Expect(serverCM().ResourceVersion).To(Equal(serverRV), "valkey-<cluster> is not updated")
	})

	It("LiveWritesNotBlockedByACLError", func() {
		newCluster("cfg-acl", 3, map[string]string{"maxmemory": "100mb"})
		settle()
		for _, n := range listNodes() {
			setApplied(n.Name, map[string]string{"maxmemory": "100mb"})
		}
		updateCluster(func(c *valkeyiov1alpha1.ValkeyCluster) { c.Spec.Config["maxmemory"] = "200mb" })
		settle()

		By("breaking the parent's ACL step")
		injected := stderrors.New("injected ACL Secret write failure")
		failSecretWrites := interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					return injected
				}
				return c.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					return injected
				}
				return c.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					return injected
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}
		withWatch, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		pr.Client = interceptor.NewClient(withWatch, failSecretWrites)
		_, err = pr.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
		Expect(err).To(MatchError(ContainSubstring(injected.Error())))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
		Expect(meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionReady)).To(
			HaveField("Reason", valkeyiov1alpha1.ReasonUsersAclError))

		By("each node applying the new value while the parent is stuck")
		for _, n := range listNodes() {
			setApplied(n.Name, map[string]string{"maxmemory": "200mb"})
			_, err = pr.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
			Expect(err).To(MatchError(ContainSubstring(injected.Error())))
			reconcileNode(n.Name)
			Expect(liveConf(n.Name)).To(Equal("maxmemory 200mb\n"),
				"the node controller writes live.conf without the parent's ACL step")
		}
	})

	It("writes a missing revision only when the spec renders to it, and never rewrites one", func() {
		newCluster("cfg-hash", 1, map[string]string{"appendfsync": "everysec"})
		settle()
		name := valkeyNodeName(cluster.Name, 0, 0)
		rev := getNode(name).Spec.ConfigRevision

		By("leaving an existing revision key and other keys alone")
		cm := nodeCM(name)
		cm.Data[configRevisionFileKey(rev)] = "edited\n"
		cm.Data["unrelated"] = "kept"
		cm.Data[liveConfigFileKey] = "maxclients 1\n"
		Expect(k8sClient.Update(ctx, cm)).To(Succeed())
		nodeWrites.Reset()
		reconcileNode(name)
		cm = nodeCM(name)
		Expect(cm.Data).To(HaveKeyWithValue(configRevisionFileKey(rev), "edited\n"))
		Expect(cm.Data).To(HaveKeyWithValue("unrelated", "kept"))
		Expect(cm.Data).To(HaveKeyWithValue(liveConfigFileKey, ""), "live.conf is rewritten from the node")
		Expect(nodeWrites.Count("ConfigMap", verbPatch)).To(Equal(1))

		By("skipping the write when nothing changed")
		reconcileNode(name)
		Expect(nodeWrites.Count("ConfigMap", verbPatch)).To(Equal(1))

		By("refusing a revision the spec does not render")
		n := getNode(name)
		n.Spec.ConfigRevision = "0123456789ab"
		Expect(k8sClient.Update(ctx, n)).To(Succeed())
		generation := sts(name).Generation
		reconcileNode(name)
		Expect(nodeCM(name).Data).NotTo(HaveKey(configRevisionFileKey("0123456789ab")))
		Expect(sts(name).Generation).To(Equal(generation), "no template may reference a key that is not there")
		ready := meta.FindStatusCondition(getNode(name).Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(reasonConfigRevisionMismatch))
		Expect(nodeWrites.Count("ConfigMap", verbPatch)).To(Equal(1))
	})

	It("requeues the parent while its cache lacks the live revision", func() {
		newCluster("cfg-cache", 1, map[string]string{"appendfsync": "everysec"})
		settle()
		name := valkeyNodeName(cluster.Name, 0, 0)
		rev := getNode(name).Spec.ConfigRevision
		updateCluster(func(c *valkeyiov1alpha1.ValkeyCluster) { c.Spec.Config["appendfsync"] = "always" })

		pr.Client = staleConfigMapClient{Client: k8sClient}
		Expect(parentPass()).To(BeTrue())
		Expect(getNode(name).Spec.ConfigRevision).To(Equal(rev), "an unread live revision must not roll the node")

		pr.Client = k8sClient
		Expect(parentPass()).To(BeTrue())
		Expect(getNode(name).Spec.ConfigRevision).NotTo(Equal(rev))
	})

	It("rolls a node to the desired revision when its live revision's key is gone", func() {
		newCluster("cfg-gone", 3, map[string]string{"appendfsync": "everysec"})
		settle()
		before := revisions()

		By("growing the allowlist, so each node keeps a live revision that differs from the desired one")
		liveConfigAllowlist["appendfsync"] = struct{}{}
		DeferCleanup(func() { delete(liveConfigAllowlist, "appendfsync") })
		settle()
		Expect(revisions()).To(Equal(before))

		By("deleting one node's ConfigMap")
		name := valkeyNodeName(cluster.Name, 1, 0)
		Expect(k8sClient.Delete(ctx, nodeCM(name))).To(Succeed())
		reconcileNode(name)
		ready := meta.FindStatusCondition(getNode(name).Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionReady)
		Expect(ready).To(HaveField("Reason", reasonConfigRevisionMismatch))
		markReady(name)

		By("the parent naming the desired revision for that node only")
		Expect(parentPass()).To(BeTrue())
		desired := pinnedConfigRevision(renderPinnedConfig(getNode(name)))
		Expect(desired).NotTo(Equal(before[name]))
		for n, rev := range revisions() {
			if n == name {
				Expect(rev).To(Equal(desired))
			} else {
				Expect(rev).To(Equal(before[n]), "%s keeps its live revision", n)
			}
		}

		settle()
		Expect(configRevisionKeys(nodeCM(name))).To(Equal([]string{configRevisionFileKey(desired)}))
		Expect(sts(name).Spec.Template.Annotations).To(HaveKeyWithValue(configRevisionKey, desired))
		ready = meta.FindStatusCondition(getNode(name).Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionReady)
		Expect(ready).NotTo(HaveField("Reason", reasonConfigRevisionMismatch))
	})
})
