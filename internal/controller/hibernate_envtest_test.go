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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
	testutils "github.com/valkey-io/valkey-operator/test/utils"
)

var _ = Describe("ValkeyCluster hibernation", func() {
	ctx := context.Background()

	// newHibernationCluster creates a one-shard cluster with persistence and one
	// replica per fake in frs, lets the reconciler create its ValkeyNodes, marks
	// them ready at 10.0.0.1 (primary) and 10.0.0.2, 10.0.0.3, ... (replicas),
	// and reports it healthy.
	newHibernationCluster := func(name string, fp *fakeValkey, frs ...*fakeValkey) (*ValkeyClusterReconciler, *valkey.ClusterState, func() *valkeyiov1alpha1.ValkeyCluster) {
		GinkgoHelper()
		key := types.NamespacedName{Name: name, Namespace: "default"}
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{Shards: 1, ReplicasPerShard: int32(len(frs)),
				Persistence: &valkeyiov1alpha1.PersistenceSpec{Size: resource.MustParse("1Gi")}},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() {
			nodeList := &valkeyiov1alpha1.ValkeyNodeList{}
			_ = k8sClient.List(ctx, nodeList, client.InNamespace("default"), client.MatchingLabels{LabelCluster: name})
			for i := range nodeList.Items {
				_ = k8sClient.Delete(ctx, &nodeList.Items[i])
			}
			_ = k8sClient.Delete(ctx, cluster)
		})

		state := &valkey.ClusterState{}
		r := &ValkeyClusterReconciler{
			Client:        k8sClient,
			APIReader:     k8sClient,
			Scheme:        k8sClient.Scheme(),
			Recorder:      events.NewFakeRecorder(1000),
			ValkeyClients: testValkeyClients(k8sClient),
			clusterStateFunc: func(context.Context, *valkeyiov1alpha1.ValkeyNodeList) *valkey.ClusterState {
				return state
			},
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		ips := map[string]string{valkeyNodeName(name, 0, 0): "10.0.0.1"}
		members := make([]*valkey.NodeState, 0, 1+len(frs))
		members = append(members, fp.nodeState(GinkgoT(), "10.0.0.1", "p"))
		for i, fr := range frs {
			ip := fmt.Sprintf("10.0.0.%d", i+2)
			ips[valkeyNodeName(name, 0, i+1)] = ip
			members = append(members, fr.nodeState(GinkgoT(), ip, fmt.Sprintf("r%d", i+1)))
		}
		for nodeName, ip := range ips {
			node := &valkeyiov1alpha1.ValkeyNode{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: "default"}, node)).To(Succeed())
			node.Status.Ready = true
			node.Status.PodIP = ip
			node.Status.ObservedGeneration = node.Generation
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
		}
		*state = valkey.ClusterState{Shards: []*valkey.ShardState{{
			Id: "s0", PrimaryId: "p", Slots: []valkey.SlotsRange{{Start: 0, End: 16383}},
			Nodes: members,
		}}}

		get := func() *valkeyiov1alpha1.ValkeyCluster {
			GinkgoHelper()
			c := &valkeyiov1alpha1.ValkeyCluster{}
			Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
			return c
		}
		healthy := get()
		setCondition(healthy, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonClusterHealthy, "Cluster is healthy", metav1.ConditionTrue)
		Expect(k8sClient.Status().Update(ctx, healthy)).To(Succeed())
		return r, state, get
	}

	reconcileCluster := func(r *ValkeyClusterReconciler, name string) {
		GinkgoHelper()
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
	}
	setClusterAnnotation := func(get func() *valkeyiov1alpha1.ValkeyCluster, value string) {
		GinkgoHelper()
		c := get()
		if value == "" {
			delete(c.Annotations, valkeyiov1alpha1.AnnotationHibernate)
		} else {
			if c.Annotations == nil {
				c.Annotations = map[string]string{}
			}
			c.Annotations[valkeyiov1alpha1.AnnotationHibernate] = value
		}
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
	}
	hibernated := func(get func() *valkeyiov1alpha1.ValkeyCluster) *metav1.Condition {
		GinkgoHelper()
		return testutils.FindCondition(get().Status.Conditions, valkeyiov1alpha1.ConditionHibernated)
	}
	nodeAnnotations := func(name string) map[string]string {
		GinkgoHelper()
		node := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, node)).To(Succeed())
		return node.Annotations
	}
	// reconcileUntil reconciles, up to n times, until the Hibernated reason is
	// want and its message contains step.
	reconcileUntil := func(r *ValkeyClusterReconciler, name string, get func() *valkeyiov1alpha1.ValkeyCluster, want, step string, n int) {
		GinkgoHelper()
		for range n {
			reconcileCluster(r, name)
			if c := hibernated(get); c != nil && c.Reason == want && strings.Contains(c.Message, step) {
				return
			}
		}
		Fail("Hibernated never reached " + want + " (" + step + "), last: " + hibernated(get).Reason + ": " + hibernated(get).Message)
	}
	const stoppingReplicas = "replica pod(s)"

	It("waits for a healthy cluster before it pauses anything", func() {
		const name = "hib-gate"
		fp, fr := newFakePrimary(), newFakeReplica(100)
		r, _, get := newHibernationCluster(name, fp, fr)

		unhealthy := get()
		setCondition(unhealthy, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonReconciling, "busy", metav1.ConditionFalse)
		Expect(k8sClient.Status().Update(ctx, unhealthy)).To(Succeed())
		setClusterAnnotation(get, "true")

		// The regular reconcile runs and may fail on the fake servers' unknown
		// commands; only the hibernation outcome matters here.
		_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		cond := hibernated(get)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(valkeyiov1alpha1.ReasonHibernationWaitingForHealthy))
		Expect(fp.sent("CLIENT PAUSE")).To(BeZero())
		Expect(nodeAnnotations(valkeyNodeName(name, 0, 1))).NotTo(HaveKey(valkeyiov1alpha1.AnnotationHibernate))
	})

	It("pauses, waits for the replica, saves, stops replicas then primaries, and wakes", func() {
		const name = "hib-flow"
		fp, fr := newFakePrimary(), newFakeReplica(90)
		r, state, get := newHibernationCluster(name, fp, fr)
		primaryName, replicaName := valkeyNodeName(name, 0, 0), valkeyNodeName(name, 0, 1)

		By("annotating with a value other than \"true\", which does nothing")
		setClusterAnnotation(get, "yes")
		_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(hibernated(get)).To(BeNil())
		Expect(fp.sent("CLIENT PAUSE")).To(BeZero())
		// The regular reconcile ran against the fake scrape and took Ready away.
		healthy := get()
		setCondition(healthy, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonClusterHealthy, "Cluster is healthy", metav1.ConditionTrue)
		Expect(k8sClient.Status().Update(ctx, healthy)).To(Succeed())

		By("leaving a stale data point on the replica, as a descent that backed off before a failover would")
		stale := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: replicaName, Namespace: "default"}, stale)).To(Succeed())
		stale.Annotations = map[string]string{valkeyiov1alpha1.AnnotationHibernateDataPoint: "old:0:1"}
		Expect(k8sClient.Update(ctx, stale)).To(Succeed())

		By("pausing writes and waiting for the lagging replica")
		setClusterAnnotation(get, "true")
		reconcileCluster(r, name)
		cond := hibernated(get)
		Expect(cond.Reason).To(Equal(valkeyiov1alpha1.ReasonHibernating))
		Expect(cond.Message).To(ContainSubstring("offset 90 of 100"))
		Expect(fp.paused).To(BeTrue())
		Expect(get().Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReconciling))

		By("saving every node once the replica caught up, then stopping the replica")
		fr.offset = 100
		reconcileUntil(r, name, get, valkeyiov1alpha1.ReasonHibernating, stoppingReplicas, 5)
		Expect(fp.saves).To(Equal(1))
		Expect(fr.saves).To(Equal(1))
		Expect(nodeAnnotations(replicaName)).To(HaveKeyWithValue(valkeyiov1alpha1.AnnotationHibernate, "true"))
		Expect(nodeAnnotations(primaryName)).NotTo(HaveKey(valkeyiov1alpha1.AnnotationHibernate))
		Expect(nodeAnnotations(primaryName)).To(HaveKeyWithValue(valkeyiov1alpha1.AnnotationHibernateDataPoint, "abc:0:1"))
		Expect(nodeAnnotations(replicaName)).NotTo(HaveKey(valkeyiov1alpha1.AnnotationHibernateDataPoint), "only the current primaries carry a data point")
		Expect(fp.paused).To(BeTrue(), "writes stay paused while the replica stops")

		By("stopping the primary once the replica pod is gone (envtest has no pods) and it is still at the saved point")
		reconcileUntil(r, name, get, valkeyiov1alpha1.ReasonHibernated, "", 5)
		Expect(nodeAnnotations(primaryName)).To(HaveKeyWithValue(valkeyiov1alpha1.AnnotationHibernate, "true"))
		c := get()
		Expect(c.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateHibernated))
		Expect(testutils.FindCondition(c.Status.Conditions, valkeyiov1alpha1.ConditionHibernated).Status).To(Equal(metav1.ConditionTrue))
		Expect(testutils.FindCondition(c.Status.Conditions, valkeyiov1alpha1.ConditionReady).Reason).To(Equal(valkeyiov1alpha1.ReasonHibernated))

		By("waking: the primary starts first, the replica waits for it")
		*state = valkey.ClusterState{}
		setClusterAnnotation(get, "")
		reconcileCluster(r, name)
		cond = hibernated(get)
		Expect(cond.Reason).To(Equal(valkeyiov1alpha1.ReasonWaking))
		Expect(cond.Message).To(ContainSubstring(replicaName))
		Expect(nodeAnnotations(primaryName)).NotTo(HaveKey(valkeyiov1alpha1.AnnotationHibernate))
		Expect(nodeAnnotations(replicaName)).To(HaveKeyWithValue(valkeyiov1alpha1.AnnotationHibernate, "true"))
		Expect(get().Status.State).NotTo(Equal(valkeyiov1alpha1.ClusterStateHibernated))

		By("starting the replica once its primary serves again")
		back := fp.nodeState(GinkgoT(), "10.0.0.1", "p")
		back.ClusterInfo = map[string]string{"cluster_state": "ok"}
		*state = valkey.ClusterState{Shards: []*valkey.ShardState{{Id: "s0", PrimaryId: "p",
			Slots: []valkey.SlotsRange{{Start: 0, End: 16383}}, Nodes: []*valkey.NodeState{back}}}}
		// The regular reconcile runs after the wake and may fail on the fake
		// server's unknown commands; only the wake matters here.
		_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		for _, n := range []string{primaryName, replicaName} {
			Expect(nodeAnnotations(n)).NotTo(HaveKey(valkeyiov1alpha1.AnnotationHibernate))
			Expect(nodeAnnotations(n)).NotTo(HaveKey(valkeyiov1alpha1.AnnotationHibernateDataPoint))
		}
	})

	It("finishes a staged wake before hibernating again", func() {
		const name = "hib-rewake"
		fp, fr := newFakePrimary(), newFakeReplica(100)
		r, state, get := newHibernationCluster(name, fp, fr)
		primaryName, replicaName := valkeyNodeName(name, 0, 0), valkeyNodeName(name, 0, 1)

		setClusterAnnotation(get, "true")
		reconcileUntil(r, name, get, valkeyiov1alpha1.ReasonHibernated, "", 10)
		*state = valkey.ClusterState{}
		setClusterAnnotation(get, "")
		reconcileCluster(r, name)
		Expect(nodeAnnotations(replicaName)).To(HaveKey(valkeyiov1alpha1.AnnotationHibernate))

		By("asking for hibernation again while the replica still waits for its primary")
		setClusterAnnotation(get, "true")
		reconcileCluster(r, name)
		Expect(nodeAnnotations(replicaName)).To(HaveKeyWithValue(valkeyiov1alpha1.AnnotationHibernate, "true"), "the replica does not start ahead of its primary")
		Expect(nodeAnnotations(primaryName)).To(HaveKey(valkeyiov1alpha1.AnnotationHibernateDataPoint))
		cond := hibernated(get)
		Expect(cond.Reason).To(Equal(valkeyiov1alpha1.ReasonWaking))
		Expect(cond.Message).To(ContainSubstring("hibernation starts again once the cluster is awake"))

		By("starting the replica once its primary serves, before a new descent")
		back := fp.nodeState(GinkgoT(), "10.0.0.1", "p")
		back.ClusterInfo = map[string]string{"cluster_state": "ok"}
		*state = valkey.ClusterState{Shards: []*valkey.ShardState{{Id: "s0", PrimaryId: "p",
			Slots: []valkey.SlotsRange{{Start: 0, End: 16383}}, Nodes: []*valkey.NodeState{back}}}}
		_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		for _, n := range []string{primaryName, replicaName} {
			Expect(nodeAnnotations(n)).NotTo(HaveKey(valkeyiov1alpha1.AnnotationHibernate))
			Expect(nodeAnnotations(n)).NotTo(HaveKey(valkeyiov1alpha1.AnnotationHibernateDataPoint))
		}
		Expect(hibernated(get).Reason).To(Equal(valkeyiov1alpha1.ReasonHibernationWaitingForHealthy), "the new descent waits for a healthy cluster")
	})

	It("marks a replica that a cut-short pass left running instead of restarting", func() {
		const name = "hib-partial"
		fp, fr1, fr2 := newFakePrimary(), newFakeReplica(100), newFakeReplica(100)
		r, _, get := newHibernationCluster(name, fp, fr1, fr2)
		secondReplica := valkeyNodeName(name, 0, 2)

		setClusterAnnotation(get, "true")
		reconcileUntil(r, name, get, valkeyiov1alpha1.ReasonHibernating, stoppingReplicas, 6)

		By("losing one replica's mark, as if the operator stopped before patching it")
		replica := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: secondReplica, Namespace: "default"}, replica)).To(Succeed())
		delete(replica.Annotations, valkeyiov1alpha1.AnnotationHibernate)
		Expect(k8sClient.Update(ctx, replica)).To(Succeed())

		reconcileCluster(r, name)
		Expect(nodeAnnotations(secondReplica)).To(HaveKeyWithValue(valkeyiov1alpha1.AnnotationHibernate, "true"))
		Expect(nodeAnnotations(valkeyNodeName(name, 0, 1))).To(HaveKey(valkeyiov1alpha1.AnnotationHibernate))
		Expect(nodeAnnotations(valkeyNodeName(name, 0, 0))).NotTo(HaveKey(valkeyiov1alpha1.AnnotationHibernate))
		cond := hibernated(get)
		Expect(cond.Reason).To(Equal(valkeyiov1alpha1.ReasonHibernating), "the descent goes on, it does not restart")
		Expect(cond.Message).To(ContainSubstring(stoppingReplicas))
		Expect(fp.paused).To(BeTrue())
	})

	It("brings the stopped ValkeyNodes up to date with a change made while hibernated before the primaries start", func() {
		const name = "hib-wake-spec"
		fp, fr := newFakePrimary(), newFakeReplica(100)
		r, state, get := newHibernationCluster(name, fp, fr)
		primaryName, replicaName := valkeyNodeName(name, 0, 0), valkeyNodeName(name, 0, 1)
		nodeSpec := func(nodeName string) valkeyiov1alpha1.ValkeyNodeSpec {
			GinkgoHelper()
			node := &valkeyiov1alpha1.ValkeyNode{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: "default"}, node)).To(Succeed())
			return node.Spec
		}

		setClusterAnnotation(get, "true")
		reconcileUntil(r, name, get, valkeyiov1alpha1.ReasonHibernated, "", 10)

		By("switching the cluster to another TLS certificate Secret while it sleeps")
		c := get()
		c.Spec.Networking = &valkeyiov1alpha1.NetworkingSpec{TLS: &valkeyiov1alpha1.TLSSpec{
			Certificates: valkeyiov1alpha1.TLSCertificates{
				Server: valkeyiov1alpha1.CertificateSource{SecretName: "rotated-tls"},
			},
		}}
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
		reconcileCluster(r, name)
		Expect(nodeSpec(primaryName).TLS).To(BeNil(), "nothing changes on the nodes while the cluster sleeps")

		By("waking: both nodes carry the new Secret by the time the primary is unmarked")
		*state = valkey.ClusterState{}
		setClusterAnnotation(get, "")
		reconcileCluster(r, name)
		Expect(nodeAnnotations(primaryName)).NotTo(HaveKey(valkeyiov1alpha1.AnnotationHibernate))
		Expect(nodeAnnotations(replicaName)).To(HaveKeyWithValue(valkeyiov1alpha1.AnnotationHibernate, "true"))
		for _, n := range []string{primaryName, replicaName} {
			spec := nodeSpec(n)
			Expect(spec.TLS).NotTo(BeNil(), n)
			Expect(spec.TLS.Certificates.Server.SecretName).To(Equal("rotated-tls"), n)
			Expect(spec.WorkloadRevision).NotTo(BeEmpty(), n)
		}
	})

	It("after the grace period, starts a replica only if its primary is not running", func() {
		const name = "hib-grace"
		fp, fr := newFakePrimary(), newFakeReplica(100)
		r, state, get := newHibernationCluster(name, fp, fr)
		primaryName, replicaName := valkeyNodeName(name, 0, 0), valkeyNodeName(name, 0, 1)

		setClusterAnnotation(get, "true")
		reconcileUntil(r, name, get, valkeyiov1alpha1.ReasonHibernated, "", 10)
		*state = valkey.ClusterState{}
		setClusterAnnotation(get, "")
		reconcileCluster(r, name)
		Expect(nodeAnnotations(replicaName)).To(HaveKey(valkeyiov1alpha1.AnnotationHibernate))

		By("backdating the start of the wake past the grace period")
		c := get()
		for i := range c.Status.Conditions {
			if c.Status.Conditions[i].Type == valkeyiov1alpha1.ConditionHibernated {
				c.Status.Conditions[i].LastTransitionTime = metav1.NewTime(time.Now().Add(-wakeReplicaGrace - time.Minute))
			}
		}
		Expect(k8sClient.Status().Update(ctx, c)).To(Succeed())

		By("holding the replica while its primary is up but not yet serving")
		*state = valkey.ClusterState{Shards: []*valkey.ShardState{{Id: "s0", PrimaryId: "p",
			Slots: []valkey.SlotsRange{{Start: 0, End: 16383}}, Nodes: []*valkey.NodeState{fp.nodeState(GinkgoT(), "10.0.0.1", "p")}}}}
		reconcileCluster(r, name)
		Expect(nodeAnnotations(replicaName)).To(HaveKey(valkeyiov1alpha1.AnnotationHibernate), "a suspected primary may already acknowledge writes")

		By("starting it once the primary is not running at all")
		primary := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: primaryName, Namespace: "default"}, primary)).To(Succeed())
		primary.Status.Ready = false
		primary.Status.PodIP = ""
		Expect(k8sClient.Status().Update(ctx, primary)).To(Succeed())
		*state = valkey.ClusterState{}
		_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(nodeAnnotations(replicaName)).NotTo(HaveKey(valkeyiov1alpha1.AnnotationHibernate))
	})

	It("starts over if a stopping primary took a write, for example after the operator was down", func() {
		const name = "hib-final"
		fp, fr := newFakePrimary(), newFakeReplica(100)
		r, state, get := newHibernationCluster(name, fp, fr)
		primaryName, replicaName := valkeyNodeName(name, 0, 0), valkeyNodeName(name, 0, 1)

		By("giving the primary a pod that has not stopped yet")
		primary := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: primaryName, Namespace: "default"}, primary)).To(Succeed())
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-primary-pod", Namespace: "default", Labels: valkeyNodeLabels(primary)},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "server", Image: DefaultImage}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0)) })

		setClusterAnnotation(get, "true")
		reconcileUntil(r, name, get, valkeyiov1alpha1.ReasonHibernating, "1 pod(s) to stop", 10)
		Expect(nodeAnnotations(primaryName)).To(HaveKey(valkeyiov1alpha1.AnnotationHibernate))

		By("a write reaching the primary while it is still running")
		fp.dirty = 7
		reconcileCluster(r, name)
		cond := hibernated(get)
		Expect(cond.Reason).To(Equal(valkeyiov1alpha1.ReasonWaking))
		Expect(cond.Message).To(ContainSubstring("moved from data point"))
		Expect(nodeAnnotations(primaryName)).To(HaveKey(valkeyiov1alpha1.AnnotationHibernateDataPoint), "the primaries stay known for the staged wake")

		By("starting the primary first and holding its stopped replica")
		reconcileCluster(r, name)
		Expect(nodeAnnotations(primaryName)).NotTo(HaveKey(valkeyiov1alpha1.AnnotationHibernate))
		Expect(nodeAnnotations(replicaName)).To(HaveKeyWithValue(valkeyiov1alpha1.AnnotationHibernate, "true"),
			"a replica started now could win an election against a primary that already serves")

		By("starting the replica once its primary serves, then starting over")
		state.Shards[0].Nodes[0].ClusterInfo = map[string]string{"cluster_state": "ok"}
		_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		for _, n := range []string{primaryName, replicaName} {
			Expect(nodeAnnotations(n)).NotTo(HaveKey(valkeyiov1alpha1.AnnotationHibernate))
			Expect(nodeAnnotations(n)).NotTo(HaveKey(valkeyiov1alpha1.AnnotationHibernateDataPoint))
		}
		Expect(hibernated(get).Reason).To(Equal(valkeyiov1alpha1.ReasonHibernationWaitingForHealthy))
	})

	It("retries a failed unpause before waking", func() {
		const name = "hib-unpause"
		fp, fr := newFakePrimary(), newFakeReplica(100)
		r, _, get := newHibernationCluster(name, fp, fr)
		replicaName := valkeyNodeName(name, 0, 1)

		setClusterAnnotation(get, "true")
		reconcileUntil(r, name, get, valkeyiov1alpha1.ReasonHibernating, stoppingReplicas, 6)
		Expect(fp.paused).To(BeTrue())

		By("cancelling while the primary does not take the unpause")
		fp.unpauseFails = 1
		setClusterAnnotation(get, "")
		reconcileCluster(r, name)
		Expect(fp.paused).To(BeTrue())
		Expect(hibernated(get).Reason).To(Equal(valkeyiov1alpha1.ReasonHibernating), "nothing moves on with writes still paused")
		Expect(nodeAnnotations(replicaName)).To(HaveKey(valkeyiov1alpha1.AnnotationHibernate))

		By("unpausing on the next pass, then waking")
		reconcileCluster(r, name)
		Expect(fp.paused).To(BeFalse())
		Expect(hibernated(get).Reason).To(Equal(valkeyiov1alpha1.ReasonWaking))
	})

	It("does not count replication PINGs as a change", func() {
		const name = "hib-ping"
		fp, fr := newFakePrimary(), newFakeReplica(100)
		r, _, get := newHibernationCluster(name, fp, fr)
		replicaName := valkeyNodeName(name, 0, 1)

		setClusterAnnotation(get, "true")
		reconcileUntil(r, name, get, valkeyiov1alpha1.ReasonHibernating, stoppingReplicas, 6)
		Expect(nodeAnnotations(replicaName)).To(HaveKey(valkeyiov1alpha1.AnnotationHibernate))

		By("a replication PING alone, which does not count as a change")
		fp.offset += 14
		reconcileCluster(r, name)
		Expect(hibernated(get).Reason).To(BeElementOf(valkeyiov1alpha1.ReasonHibernating, valkeyiov1alpha1.ReasonHibernated))
		Expect(nodeAnnotations(valkeyNodeName(name, 0, 0))).To(HaveKey(valkeyiov1alpha1.AnnotationHibernate))
	})

	It("starts the replicas again when the primary took a write after they stopped", func() {
		const name = "hib-written"
		fp, fr := newFakePrimary(), newFakeReplica(100)
		r, state, get := newHibernationCluster(name, fp, fr)
		primaryName, replicaName := valkeyNodeName(name, 0, 0), valkeyNodeName(name, 0, 1)

		setClusterAnnotation(get, "true")
		reconcileUntil(r, name, get, valkeyiov1alpha1.ReasonHibernating, stoppingReplicas, 6)
		Expect(nodeAnnotations(replicaName)).To(HaveKey(valkeyiov1alpha1.AnnotationHibernate))

		By("a write reaching the primary after its replica saved")
		fp.offset = 150
		fp.dirty = 2
		reconcileCluster(r, name)
		cond := hibernated(get)
		Expect(cond.Reason).To(Equal(valkeyiov1alpha1.ReasonWaking), "the pods start again the way a wake starts them")
		Expect(cond.Message).To(ContainSubstring(`"abc:0:1" to "abc:2:1"`))
		Expect(nodeAnnotations(primaryName)).NotTo(HaveKey(valkeyiov1alpha1.AnnotationHibernate), "the primary is not stopped ahead of its replica")
		Expect(nodeAnnotations(primaryName)).To(HaveKey(valkeyiov1alpha1.AnnotationHibernateDataPoint), "the primary stays known as one")
		Expect(fp.paused).To(BeFalse(), "writes resume")

		By("starting the replica once its primary serves, then starting over")
		state.Shards[0].Nodes[0].ClusterInfo = map[string]string{"cluster_state": "ok"}
		_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(nodeAnnotations(replicaName)).NotTo(HaveKey(valkeyiov1alpha1.AnnotationHibernate))
		Expect(nodeAnnotations(primaryName)).NotTo(HaveKey(valkeyiov1alpha1.AnnotationHibernateDataPoint))
		Expect(hibernated(get).Reason).To(Equal(valkeyiov1alpha1.ReasonHibernationWaitingForHealthy))
	})

	It("stops nothing when the cluster was woken while the pass ran", func() {
		const name = "hib-stale"
		fp, fr := newFakePrimary(), newFakeReplica(100)
		r, _, get := newHibernationCluster(name, fp, fr)

		// The pass read the cluster while it still asked for hibernation; the
		// API server no longer has the annotation.
		stale := get()
		stale.Annotations = map[string]string{valkeyiov1alpha1.AnnotationHibernate: "true"}
		replica := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: valkeyNodeName(name, 0, 1), Namespace: "default"}, replica)).To(Succeed())

		_, handled, err := r.beginStopping(ctx, stale, []*valkeyiov1alpha1.ValkeyNode{replica}, &valkey.ClusterState{},
			"Stopping 1 replica pod(s)")
		Expect(err).NotTo(HaveOccurred())
		Expect(handled).To(BeTrue())
		Expect(nodeAnnotations(replica.Name)).NotTo(HaveKey(valkeyiov1alpha1.AnnotationHibernate))
		Expect(hibernated(get)).To(BeNil(), "no descent step was reported")
	})

	It("resumes writes and stops nothing when a save keeps failing", func() {
		const name = "hib-savefail"
		fp, fr := newFakePrimary(), newFakeReplica(100)
		fr.saveFails = true
		r, _, get := newHibernationCluster(name, fp, fr)

		setClusterAnnotation(get, "true")
		for range 10 {
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
			if c := hibernated(get); c != nil && c.Reason == valkeyiov1alpha1.ReasonHibernationSaveFailed {
				break
			}
		}
		cond := hibernated(get)
		Expect(cond.Reason).To(Equal(valkeyiov1alpha1.ReasonHibernationSaveFailed))
		Expect(cond.Message).To(ContainSubstring("could not save its data"))
		Expect(fp.paused).To(BeFalse())
		Expect(nodeAnnotations(valkeyNodeName(name, 0, 1))).NotTo(HaveKey(valkeyiov1alpha1.AnnotationHibernate))

		By("cancelling hibernation clears the condition")
		setClusterAnnotation(get, "")
		_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(hibernated(get)).To(BeNil())
	})
})

var _ = Describe("ValkeyNode hibernation", func() {
	ctx := context.Background()

	It("scales a cluster-owned node to zero and back, and ignores the annotation on a standalone node", func() {
		const name = "hib-node"
		key := types.NamespacedName{Name: name, Namespace: "default"}
		node := &valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default",
				Annotations: map[string]string{valkeyiov1alpha1.AnnotationHibernate: "true"}},
			Spec: valkeyiov1alpha1.ValkeyNodeSpec{WorkloadType: valkeyiov1alpha1.WorkloadTypeStatefulSet, Image: DefaultImage},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })

		r := &ValkeyNodeReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: events.NewFakeRecorder(100)}
		reconcileNode := func() {
			GinkgoHelper()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
		}
		stsReplicas := func() int32 {
			GinkgoHelper()
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: valkeyNodeResourceName(node), Namespace: "default"}, sts)).To(Succeed())
			return *sts.Spec.Replicas
		}

		By("running a standalone node normally despite the annotation")
		reconcileNode()
		Expect(stsReplicas()).To(Equal(int32(1)))

		By("scaling to zero once a ValkeyCluster controls the node")
		controller := true
		Expect(k8sClient.Get(ctx, key, node)).To(Succeed())
		rev, err := computeWorkloadRevision(node)
		Expect(err).NotTo(HaveOccurred())
		node.Spec.WorkloadRevision = rev
		node.OwnerReferences = []metav1.OwnerReference{{APIVersion: "valkey.io/v1alpha1", Kind: "ValkeyCluster",
			Name: "parent", UID: "parent-uid", Controller: &controller}}
		Expect(k8sClient.Update(ctx, node)).To(Succeed())
		reconcileNode()
		Expect(stsReplicas()).To(Equal(int32(0)))
		Expect(k8sClient.Get(ctx, key, node)).To(Succeed())
		ready := testutils.FindCondition(node.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal(valkeyiov1alpha1.ValkeyNodeReasonHibernated))

		By("scaling back up on wake, even with a template roll still waiting for its revision")
		Expect(k8sClient.Get(ctx, key, node)).To(Succeed())
		delete(node.Annotations, valkeyiov1alpha1.AnnotationHibernate)
		node.Spec.Image = "valkey/valkey:9.0.1" // not authorized by WorkloadRevision
		Expect(k8sClient.Update(ctx, node)).To(Succeed())
		reconcileNode()
		Expect(stsReplicas()).To(Equal(int32(1)))
	})
})
