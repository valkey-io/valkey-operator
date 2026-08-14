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
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crevent "sigs.k8s.io/controller-runtime/pkg/event"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

const (
	pollPrimaryIP = "10.0.0.1"
	pollReplicaIP = "10.0.0.2"
)

// oneShardState builds a snapshot of a single shard owned by pollPrimaryIP,
// with pollReplicaIP replicating it.
func oneShardState() *valkey.ClusterState {
	return &valkey.ClusterState{
		Shards: []*valkey.ShardState{{
			Id:        "shard-1",
			PrimaryId: "id-primary",
			Nodes: []*valkey.NodeState{
				{Id: "id-primary", Address: pollPrimaryIP},
				{Id: "id-replica", Address: pollReplicaIP},
			},
		}},
	}
}

var _ = Describe("liveRoleForAddress", func() {
	It("returns primary for the shard primary address", func() {
		Expect(liveRoleForAddress(oneShardState(), pollPrimaryIP)).To(Equal(RolePrimary))
	})
	It("returns replica for a non-primary shard member", func() {
		Expect(liveRoleForAddress(oneShardState(), pollReplicaIP)).To(Equal(RoleReplica))
	})
	It("returns empty when the address is not in any shard", func() {
		Expect(liveRoleForAddress(oneShardState(), "10.9.9.9")).To(BeEmpty())
	})
})

var _ = Describe("RolePoller", func() {
	const clusterName = "poller-cluster"

	var (
		ctx      context.Context
		cluster  *valkeyiov1alpha1.ValkeyCluster
		primary  *valkeyiov1alpha1.ValkeyNode
		replica  *valkeyiov1alpha1.ValkeyNode
		emitted  chan crevent.GenericEvent
		poller   *RolePoller
		scrapes  int
		nowStamp time.Time
	)

	// newNode creates a ValkeyNode carrying the cluster label, with the given
	// pod IP and recorded role in its status.
	newNode := func(name, podIP, role string) *valkeyiov1alpha1.ValkeyNode {
		node := &valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Labels:    map[string]string{LabelCluster: clusterName},
			},
			Spec: valkeyiov1alpha1.ValkeyNodeSpec{WorkloadType: valkeyiov1alpha1.WorkloadTypeStatefulSet},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(node), node)).To(Succeed())
		node.Status.PodIP = podIP
		node.Status.Role = role
		Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
		return node
	}

	// names collects the node names the poller pushed onto the channel.
	names := func() []string {
		var got []string
		for {
			select {
			case ev := <-emitted:
				got = append(got, ev.Object.GetName())
			default:
				return got
			}
		}
	}

	BeforeEach(func() {
		ctx = context.Background()
		nowStamp = time.Unix(1_700_000_000, 0)
		scrapes = 0

		cluster = &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: "default"},
			Spec:       valkeyiov1alpha1.ValkeyClusterSpec{Shards: 1, Replicas: 1},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		primary = newNode("poller-node-0", pollPrimaryIP, RolePrimary)
		replica = newNode("poller-node-1", pollReplicaIP, RoleReplica)

		emitted = make(chan crevent.GenericEvent, 16)
		poller = &RolePoller{
			Client:   k8sClient,
			Interval: 5 * time.Second,
			Events:   emitted,
			scrapeFunc: func(_ context.Context, c *valkeyiov1alpha1.ValkeyCluster, _ []string) *valkey.ClusterState {
				if c.Name != clusterName {
					return nil
				}
				scrapes++
				return oneShardState()
			},
		}
	})

	AfterEach(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, cluster))).To(Succeed())
		for _, node := range []*valkeyiov1alpha1.ValkeyNode{primary, replica} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, node))).To(Succeed())
		}
	})

	It("emits nothing when every live role matches the recorded role", func() {
		poller.tick(ctx, nowStamp)
		Expect(names()).To(BeEmpty())
	})

	It("emits one event for the node whose live role has drifted", func() {
		By("recording the opposite roles on both CRs, as a failover would leave them")
		for node, role := range map[*valkeyiov1alpha1.ValkeyNode]string{primary: RoleReplica, replica: RoleReplica} {
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(node), node)).To(Succeed())
			node.Status.Role = role
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
		}

		poller.tick(ctx, nowStamp)
		Expect(names()).To(ConsistOf(primary.Name), "only the node that changed role should be woken")
	})

	It("emits for a node whose role has not been resolved yet", func() {
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(replica), replica)).To(Succeed())
		replica.Status.Role = ""
		Expect(k8sClient.Status().Update(ctx, replica)).To(Succeed())

		poller.tick(ctx, nowStamp)
		Expect(names()).To(ConsistOf(replica.Name))
	})

	It("skips a node that has no pod IP to dial", func() {
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(replica), replica)).To(Succeed())
		replica.Status.PodIP = ""
		replica.Status.Role = ""
		Expect(k8sClient.Status().Update(ctx, replica)).To(Succeed())

		poller.tick(ctx, nowStamp)
		Expect(names()).To(BeEmpty(), "a node with no pod IP has nothing to compare against")
	})

	It("does not scrape a cluster whose nodes all lack a pod IP", func() {
		for _, node := range []*valkeyiov1alpha1.ValkeyNode{primary, replica} {
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(node), node)).To(Succeed())
			node.Status.PodIP = ""
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
		}

		poller.tick(ctx, nowStamp)
		Expect(scrapes).To(BeZero())
	})

	Describe("per-node backoff", func() {
		BeforeEach(func() {
			// The replica's address never answers: it is absent from the scrape.
			poller.scrapeFunc = func(_ context.Context, c *valkeyiov1alpha1.ValkeyCluster, addresses []string) *valkey.ClusterState {
				if c.Name != clusterName {
					return nil
				}
				scrapes++
				state := oneShardState()
				state.Shards[0].Nodes = state.Shards[0].Nodes[:1]
				return state
			}
		})

		It("backs off an unreachable node and stops dialling it until the delay expires", func() {
			poller.tick(ctx, nowStamp)
			Expect(poller.backoff).To(HaveKey(client.ObjectKeyFromObject(replica)))
			Expect(poller.backoff[client.ObjectKeyFromObject(replica)].failures).To(Equal(1))

			By("ticking again inside the backoff window")
			var dialled []string
			poller.scrapeFunc = func(_ context.Context, c *valkeyiov1alpha1.ValkeyCluster, addresses []string) *valkey.ClusterState {
				if c.Name != clusterName {
					return nil
				}
				dialled = addresses
				return oneShardState()
			}
			poller.tick(ctx, nowStamp.Add(time.Second))
			Expect(dialled).To(ConsistOf(pollPrimaryIP), "a backed-off node must not be dialled")
		})

		It("doubles the delay on each consecutive failure, up to the ceiling", func() {
			key := client.ObjectKeyFromObject(replica)
			at := nowStamp
			for _, want := range []time.Duration{
				5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second,
				rolePollMaxBackoff, rolePollMaxBackoff,
			} {
				poller.tick(ctx, at)
				Expect(poller.backoff[key].nextAttempt.Sub(at)).To(Equal(want))
				at = poller.backoff[key].nextAttempt
			}
		})

		It("clears the backoff once the node answers again", func() {
			poller.tick(ctx, nowStamp)
			key := client.ObjectKeyFromObject(replica)
			Expect(poller.backoff).To(HaveKey(key))

			poller.scrapeFunc = func(_ context.Context, c *valkeyiov1alpha1.ValkeyCluster, _ []string) *valkey.ClusterState {
				if c.Name != clusterName {
					return nil
				}
				return oneShardState()
			}
			poller.tick(ctx, poller.backoff[key].nextAttempt)
			Expect(poller.backoff).NotTo(HaveKey(key))
		})

		It("forgets backoff state for a node that no longer exists", func() {
			poller.tick(ctx, nowStamp)
			key := client.ObjectKeyFromObject(replica)
			Expect(poller.backoff).To(HaveKey(key))

			Expect(k8sClient.Delete(ctx, replica)).To(Succeed())
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, key, &valkeyiov1alpha1.ValkeyNode{}))
			}, 5*time.Second, 100*time.Millisecond).Should(BeTrue())

			poller.tick(ctx, nowStamp.Add(time.Hour))
			Expect(poller.backoff).NotTo(HaveKey(key), "backoff state must not outlive the node")
		})
	})

	It("runs only on the elected leader", func() {
		Expect(poller.NeedLeaderElection()).To(BeTrue(),
			"a standby replica must not dial Valkey servers")
	})
})

var _ = Describe("RolePoller channel wiring", Label("wiring"), func() {
	const namespace = "role-events"

	var (
		ctx        context.Context
		stop       context.CancelFunc
		node       *valkeyiov1alpha1.ValkeyNode
		roleEvents chan crevent.GenericEvent
		liveRole   atomic.Value
	)

	BeforeEach(func() {
		ctx = context.Background()
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		}))).To(Succeed())

		liveRole.Store(RoleReplica)
		roleEvents = make(chan crevent.GenericEvent, 8)

		// The manager's cache is scoped to this namespace so the running
		// controller cannot reconcile objects other specs own.
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:  k8sClient.Scheme(),
			Metrics: metricsserver.Options{BindAddress: "0"},
			Cache:   cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}},
		})
		Expect(err).NotTo(HaveOccurred())

		Expect((&ValkeyNodeReconciler{
			Client:     mgr.GetClient(),
			Scheme:     mgr.GetScheme(),
			Recorder:   events.NewFakeRecorder(100),
			RoleEvents: roleEvents,
			resolveRoleFunc: func(_ context.Context, _ *valkeyiov1alpha1.ValkeyNode) string {
				return liveRole.Load().(string)
			},
		}).SetupWithManager(mgr)).To(Succeed())

		var mgrCtx context.Context
		mgrCtx, stop = context.WithCancel(ctx)
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())

		node = &valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "wiring-node",
				Namespace: namespace,
				Labels:    map[string]string{LabelCluster: "wiring-cluster"},
			},
			Spec: valkeyiov1alpha1.ValkeyNodeSpec{WorkloadType: valkeyiov1alpha1.WorkloadTypeStatefulSet},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      node.Name + "-pod",
				Namespace: namespace,
				Labels:    valkeyNodeLabels(node),
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "valkey/valkey:9.0.0"}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)).To(Succeed())
		pod.Status.PodIP = "10.0.0.1"
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		// envtest runs no StatefulSet controller, so the rollout has to be
		// reported by hand. Without it the node stays Ready=false and requeues
		// every 10s, which would let the backstop — not the channel — explain any
		// role change this spec observes.
		sts := &appsv1.StatefulSet{}
		stsKey := types.NamespacedName{Name: valkeyNodeResourceName(node), Namespace: namespace}
		Eventually(func() error {
			return k8sClient.Get(ctx, stsKey, sts)
		}, 10*time.Second, 200*time.Millisecond).Should(Succeed())
		sts.Status.ObservedGeneration = sts.Generation
		sts.Status.Replicas = 1
		sts.Status.ReadyReplicas = 1
		sts.Status.CurrentRevision = "rev-1"
		sts.Status.UpdateRevision = "rev-1"
		Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())
	})

	AfterEach(func() {
		stop()
		fresh := &valkeyiov1alpha1.ValkeyNode{}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(node), fresh); err == nil {
			fresh.Finalizers = nil
			Expect(k8sClient.Update(ctx, fresh)).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, fresh))).To(Succeed())
		}
		_ = k8sClient.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-pod", Namespace: namespace}})
	})

	It("reconciles a ValkeyNode pushed onto the channel", func() {
		By("letting the pod watch settle the role, and the write queue drain")
		var lastVersion string
		Eventually(func() bool {
			updated := &valkeyiov1alpha1.ValkeyNode{}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(node), updated); err != nil {
				return false
			}
			settled := updated.Status.Ready && updated.Status.Role == RoleReplica &&
				updated.ResourceVersion == lastVersion
			lastVersion = updated.ResourceVersion
			return settled
		}, 20*time.Second, 500*time.Millisecond).Should(BeTrue(),
			"two consecutive reads with no write means no reconcile is still in flight")

		By("failing over behind the operator's back — nothing in Kubernetes changes")
		liveRole.Store(RolePrimary)
		Consistently(func() string {
			updated := &valkeyiov1alpha1.ValkeyNode{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(node), updated)).To(Succeed())
			return updated.Status.Role
		}, 2*time.Second, 200*time.Millisecond).Should(Equal(RoleReplica),
			"no watch can fire for a change Kubernetes never saw")

		By("pushing the node onto the role-events channel, as the poller does")
		roleEvents <- crevent.GenericEvent{Object: node.DeepCopy()}

		// The backstop requeue is 30s, so anything inside this window can only
		// have come from the channel.
		Eventually(func() string {
			updated := &valkeyiov1alpha1.ValkeyNode{}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(node), updated); err != nil {
				return ""
			}
			return updated.Status.Role
		}, 10*time.Second, 200*time.Millisecond).Should(Equal(RolePrimary))
	})
})
