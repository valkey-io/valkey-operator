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
	"strconv"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

var _ = Describe("Valkey controller", func() {
	var (
		ctx        context.Context
		reconciler *ValkeyReconciler
		v          *valkeyiov1alpha1.Valkey
		req        reconcile.Request
	)

	BeforeEach(func() {
		ctx = context.Background()
		reconciler = &ValkeyReconciler{
			Client:    k8sClient,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  events.NewFakeRecorder(100),
		}
		v = &valkeyiov1alpha1.Valkey{
			ObjectMeta: metav1.ObjectMeta{Name: "standalone", Namespace: "default"},
		}
		Expect(k8sClient.Create(ctx, v)).To(Succeed())
		req = reconcile.Request{NamespacedName: types.NamespacedName{Name: v.Name, Namespace: v.Namespace}}

		// envtest runs no garbage collector, so delete the children explicitly.
		DeferCleanup(func() {
			for _, obj := range []client.Object{
				&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: headlessServiceName(v.Name), Namespace: v.Namespace}},
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: GetServerConfigMapName(v.Name), Namespace: v.Namespace}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: getInternalSecretName(v.Name), Namespace: v.Namespace}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: getSystemPasswordSecretName(v.Name), Namespace: v.Namespace}},
				&valkeyiov1alpha1.ValkeyNode{ObjectMeta: metav1.ObjectMeta{Name: valkeyNodeName(v.Name, 0, 0), Namespace: v.Namespace}},
				v,
			} {
				_ = k8sClient.Delete(ctx, obj)
			}
		})
	})

	getStatus := func() valkeyiov1alpha1.ValkeyStatus {
		current := &valkeyiov1alpha1.Valkey{}
		Expect(k8sClient.Get(ctx, req.NamespacedName, current)).To(Succeed())
		return current.Status
	}

	setNodeStatus := func(mutate func(*valkeyiov1alpha1.ValkeyNodeStatus)) {
		node := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: valkeyNodeName(v.Name, 0, 0), Namespace: v.Namespace}, node)).To(Succeed())
		mutate(&node.Status)
		Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
	}

	It("creates the standalone children owned by the Valkey", func() {
		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		ns := v.Namespace
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: headlessServiceName(v.Name), Namespace: ns}, &corev1.Service{})).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: getInternalSecretName(v.Name), Namespace: ns}, &corev1.Secret{})).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: getSystemPasswordSecretName(v.Name), Namespace: ns}, &corev1.Secret{})).To(Succeed())

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: GetServerConfigMapName(v.Name), Namespace: ns}, cm)).To(Succeed())
		Expect(cm.Data[configFileKey]).NotTo(ContainSubstring("cluster-enabled"))

		node := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: valkeyNodeName(v.Name, 0, 0), Namespace: ns}, node)).To(Succeed())
		Expect(metav1.IsControlledBy(node, v)).To(BeTrue())

		// replicas is 0, so no budget is created even though the mode defaults to Managed.
		err = k8sClient.Get(ctx, types.NamespacedName{Name: resourcePrefix + v.Name, Namespace: ns}, &policyv1.PodDisruptionBudget{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())

		status := getStatus()
		Expect(status.State).To(Equal(valkeyiov1alpha1.ValkeyStateReconciling))
		Expect(status.Primary).To(Equal(node.Name))
		Expect(status.Replicas).To(BeZero())
		Expect(status.ReadyReplicas).To(BeZero())
	})

	It("reports Ready once the node is ready", func() {
		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		setNodeStatus(func(s *valkeyiov1alpha1.ValkeyNodeStatus) { s.Ready = true })

		_, err = reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		status := getStatus()
		Expect(status.State).To(Equal(valkeyiov1alpha1.ValkeyStateReady))
		Expect(status.Reason).To(Equal(valkeyiov1alpha1.ReasonInstanceHealthy))
	})

	It("reports Degraded when the node cannot apply the ACL, and clears it after", func() {
		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		setNodeStatus(func(s *valkeyiov1alpha1.ValkeyNodeStatus) {
			s.Ready = true
			meta.SetStatusCondition(&s.Conditions, metav1.Condition{
				Type:   valkeyiov1alpha1.ValkeyNodeConditionACLApplied,
				Status: metav1.ConditionFalse,
				Reason: valkeyiov1alpha1.ValkeyNodeReasonApplyFailed,
			})
		})

		_, err = reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(getStatus().State).To(Equal(valkeyiov1alpha1.ValkeyStateDegraded))

		setNodeStatus(func(s *valkeyiov1alpha1.ValkeyNodeStatus) {
			meta.RemoveStatusCondition(&s.Conditions, valkeyiov1alpha1.ValkeyNodeConditionACLApplied)
		})
		_, err = reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(getStatus().State).To(Equal(valkeyiov1alpha1.ValkeyStateReady))
	})
})

func TestBuildValkeyNode(t *testing.T) {
	disabled := false
	tests := []struct {
		name         string
		spec         valkeyiov1alpha1.ValkeySpec
		wantExporter bool
		wantTLS      bool
	}{
		{name: "defaults", spec: valkeyiov1alpha1.ValkeySpec{}, wantExporter: true},
		{name: "exporter disabled", spec: valkeyiov1alpha1.ValkeySpec{Exporter: valkeyiov1alpha1.ExporterSpec{Enabled: &disabled}}},
		{
			name: "tls",
			spec: valkeyiov1alpha1.ValkeySpec{Networking: &valkeyiov1alpha1.ValkeyNetworkingSpec{
				TLS: &valkeyiov1alpha1.ValkeyTLSSpec{Certificates: valkeyiov1alpha1.TLSCertificates{
					Server: valkeyiov1alpha1.CertificateSource{SecretName: "server-tls"},
				}},
			}},
			wantExporter: true,
			wantTLS:      true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := &valkeyiov1alpha1.Valkey{
				ObjectMeta: metav1.ObjectMeta{Name: "vk", Namespace: "ns", Labels: map[string]string{LabelCluster: "user-value"}},
				Spec:       tt.spec,
			}
			node := buildValkeyNode(v, 0)

			if node.Name != valkeyNodeName("vk", 0, 0) {
				t.Errorf("name = %q", node.Name)
			}
			if node.Labels[LabelCluster] != "vk" {
				t.Errorf("operator label must win over user label, got %q", node.Labels[LabelCluster])
			}
			if node.Spec.WorkloadRevision != "" || node.Spec.PreferredEndpointType != "" {
				t.Errorf("standalone node must leave WorkloadRevision and PreferredEndpointType empty")
			}
			if node.Spec.ClusterDomain != valkeyiov1alpha1.DefaultClusterDomain {
				t.Errorf("ClusterDomain = %q", node.Spec.ClusterDomain)
			}
			gotExporter := node.Spec.Exporter.Enabled != nil && *node.Spec.Exporter.Enabled
			if gotExporter != tt.wantExporter {
				t.Errorf("exporter enabled = %v, want %v", gotExporter, tt.wantExporter)
			}
			if !tt.wantExporter && node.Spec.Exporter.Enabled != nil {
				t.Errorf("disabled exporter must stay nil, not explicit false")
			}
			if (node.Spec.TLS != nil) != tt.wantTLS {
				t.Fatalf("TLS set = %v, want %v", node.Spec.TLS != nil, tt.wantTLS)
			}
			if tt.wantTLS {
				if node.Spec.TLS.ServerName != "valkey-vk.ns.svc.cluster.local" {
					t.Errorf("ServerName = %q", node.Spec.TLS.ServerName)
				}
				if node.Spec.TLS.ClientAuth != nil {
					t.Errorf("ClientAuth must be nil for server-only TLS")
				}
			}
		})
	}
}

func TestBuildValkeyServerConfig(t *testing.T) {
	v := &valkeyiov1alpha1.Valkey{Spec: valkeyiov1alpha1.ValkeySpec{Config: map[string]string{"maxmemory": "1gb"}}}
	conf := buildValkeyServerConfig(v)
	for line := range strings.SplitSeq(conf, "\n") {
		if strings.HasPrefix(line, "cluster-enabled") || strings.HasPrefix(line, "shutdown-on-sigterm") {
			t.Errorf("standalone config must not contain %q", line)
		}
	}
	if !strings.Contains(conf, "aclfile") || !strings.Contains(conf, "maxmemory 1gb") {
		t.Errorf("config is missing the ACL file or user config:\n%s", conf)
	}

	// The node's pod roll hash must change when a non-live key changes.
	before := nodeServerConfigRollHash(buildValkeyNode(v, 0))
	v.Spec.Config["appendonly"] = "yes"
	if nodeServerConfigRollHash(buildValkeyNode(v, 0)) == before {
		t.Errorf("roll hash did not change after a non-live config edit")
	}
}

func TestParseReplicationState(t *testing.T) {
	primary := parseReplicationState("# Replication\r\nrole:master\r\nconnected_slaves:1\r\nmaster_failover_state:no-failover\r\nmaster_repl_offset:420\r\n")
	if primary.role != RolePrimary || primary.offset != 420 || primary.failoverState != noFailover {
		t.Errorf("primary parsed as %+v", primary)
	}
	replica := parseReplicationState("txt:# Replication\r\nrole:slave\r\nmaster_host:valkey-a-0-0-0.valkey-a.ns.svc.cluster.local\r\nmaster_port:6379\r\nmaster_link_status:up\r\nslave_repl_offset:418\r\n")
	if replica.role != RoleReplica || replica.offset != 418 || !replica.linkUp || replica.primaryHost != "valkey-a-0-0-0.valkey-a.ns.svc.cluster.local" {
		t.Errorf("replica parsed as %+v", replica)
	}
	if empty := parseReplicationState(""); empty.role != "" || empty.offset != -1 {
		t.Errorf("empty info parsed as %+v", empty)
	}
}

// testView builds a nodeView for planReplication tests. An empty role leaves the node unobserved.
func testView(index int, role string, offset int64, primaryHost string) *nodeView {
	node := &valkeyiov1alpha1.ValkeyNode{ObjectMeta: metav1.ObjectMeta{
		Name:   valkeyNodeName("a", 0, index),
		Labels: map[string]string{LabelNodeIndex: strconv.Itoa(index)},
	}}
	if role == "" {
		return &nodeView{node: node}
	}
	return &nodeView{node: node, observed: true, state: replicationState{role: role, offset: offset, primaryHost: primaryHost, linkUp: primaryHost != "", failoverState: noFailover}}
}

func TestPlanReplication(t *testing.T) {
	hostOf := func(node *valkeyiov1alpha1.ValkeyNode) string { return node.Name }
	n0, n1, n2 := valkeyNodeName("a", 0, 0), valkeyNodeName("a", 0, 1), valkeyNodeName("a", 0, 2)
	names := func(views []*nodeView) []string {
		out := make([]string, 0, len(views))
		for _, view := range views {
			out = append(out, view.node.Name)
		}
		return out
	}
	tests := []struct {
		name        string
		views       []*nodeView
		known       string
		wantPrimary string
		wantPromote string
		wantRepoint []string
		wantReason  string
	}{
		{
			name:        "bootstrap picks the lowest index",
			views:       []*nodeView{testView(0, RolePrimary, 0, ""), testView(1, RolePrimary, 0, ""), testView(2, RolePrimary, 0, "")},
			wantPrimary: n0, wantRepoint: []string{n1, n2},
		},
		{
			name:        "converged needs nothing",
			views:       []*nodeView{testView(0, RolePrimary, 10, ""), testView(1, RoleReplica, 10, n0), testView(2, RoleReplica, 10, n0)},
			known:       n0,
			wantPrimary: n0, wantRepoint: []string{},
		},
		{
			name:        "a restarted replica that came back as primary is repointed",
			views:       []*nodeView{testView(0, RolePrimary, 10, ""), testView(1, RolePrimary, 0, ""), testView(2, RoleReplica, 10, n0)},
			known:       n0,
			wantPrimary: n0, wantRepoint: []string{n1},
		},
		{
			name:       "an unreachable known primary blocks every change",
			views:      []*nodeView{testView(0, "", 0, ""), testView(1, RolePrimary, 0, ""), testView(2, RoleReplica, 10, n0)},
			known:      n0,
			wantReason: planPrimaryNotReady, wantRepoint: []string{},
		},
		{
			name:        "a completed failover moves the primary and repoints the rest",
			views:       []*nodeView{testView(0, RoleReplica, 10, n1), testView(1, RolePrimary, 10, ""), testView(2, RoleReplica, 10, n0)},
			known:       n0,
			wantPrimary: n1, wantRepoint: []string{n2},
		},
		{
			name:       "two primaries without a known one in either role is ambiguous",
			views:      []*nodeView{testView(0, RoleReplica, 10, n2), testView(1, RolePrimary, 10, ""), testView(2, RolePrimary, 10, "")},
			known:      n0,
			wantReason: planMultiplePrimaries, wantRepoint: []string{},
		},
		{
			name:        "no primary and every node reachable promotes the best-synced replica",
			views:       []*nodeView{testView(0, RoleReplica, 5, n0), testView(1, RoleReplica, 9, n0), testView(2, RoleReplica, 7, n0)},
			known:       n0,
			wantPromote: n1, wantReason: planNoPrimary, wantRepoint: []string{},
		},
		{
			name:       "no primary with an unreachable node does not promote",
			views:      []*nodeView{testView(0, "", 0, ""), testView(1, RoleReplica, 9, n0), testView(2, RoleReplica, 7, n0)},
			wantReason: planNoPrimary, wantRepoint: []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := planReplication(tt.views, tt.known, hostOf)
			gotPrimary, gotPromote := "", ""
			if plan.primary != nil {
				gotPrimary = plan.primary.node.Name
			}
			if plan.promote != nil {
				gotPromote = plan.promote.node.Name
			}
			if gotPrimary != tt.wantPrimary || gotPromote != tt.wantPromote || plan.reason != tt.wantReason {
				t.Errorf("primary=%q promote=%q reason=%q, want %q %q %q", gotPrimary, gotPromote, plan.reason, tt.wantPrimary, tt.wantPromote, tt.wantReason)
			}
			if got := names(plan.repoint); strings.Join(got, ",") != strings.Join(tt.wantRepoint, ",") {
				t.Errorf("repoint = %v, want %v", got, tt.wantRepoint)
			}
		})
	}

	failing := []*nodeView{testView(0, RolePrimary, 10, ""), testView(1, RoleReplica, 10, n0)}
	failing[0].state.failoverState = "waiting-for-sync"
	if plan := planReplication(failing, n0, hostOf); plan.primary != nil || plan.reason != planFailoverRunning {
		t.Errorf("a running FAILOVER must pause the plan, got %+v", plan)
	}
}

func TestBestSyncedReplicaSkipsBrokenLinks(t *testing.T) {
	primary := testView(0, RolePrimary, 10, "")
	behind := testView(1, RoleReplica, 8, "p")
	broken := testView(2, RoleReplica, 9, "p")
	broken.state.linkUp = false
	if got := bestSyncedReplica([]*nodeView{primary, behind, broken}, primary); got != behind {
		t.Errorf("handoff target = %v, want the replica with a live link", got)
	}
}

// fakeReplication stands in for a Valkey server in envtest.
// replicaOf makes the node a replica with a live link, so the next reconcile sees replication converge.
type fakeReplication struct {
	states     map[string]replicationState
	replicaOfs map[string]string
	promoted   []string
	failovers  []string
}

func (f *fakeReplication) state(_ context.Context, node *valkeyiov1alpha1.ValkeyNode) (replicationState, error) {
	if s, ok := f.states[node.Name]; ok {
		return s, nil
	}
	return replicationState{role: RolePrimary, offset: 0, failoverState: noFailover}, nil
}

func (f *fakeReplication) replicaOf(_ context.Context, node *valkeyiov1alpha1.ValkeyNode, host string) error {
	f.replicaOfs[node.Name] = host
	f.states[node.Name] = replicationState{role: RoleReplica, primaryHost: host, linkUp: true, failoverState: noFailover}
	return nil
}

func (f *fakeReplication) promote(_ context.Context, node *valkeyiov1alpha1.ValkeyNode) error {
	f.promoted = append(f.promoted, node.Name)
	return nil
}

func (f *fakeReplication) failoverTo(_ context.Context, primary *valkeyiov1alpha1.ValkeyNode, targetIP string) error {
	f.failovers = append(f.failovers, primary.Name+"->"+targetIP)
	return nil
}

var _ = Describe("Valkey replication", func() {
	var (
		ctx        context.Context
		fake       *fakeReplication
		reconciler *ValkeyReconciler
		v          *valkeyiov1alpha1.Valkey
		req        reconcile.Request
	)

	nodeKey := func(index int) types.NamespacedName {
		return types.NamespacedName{Name: valkeyNodeName(v.Name, 0, index), Namespace: v.Namespace}
	}

	// markReady stands in for the ValkeyNode controller: the node reports ready with a pod.
	markReady := func(index int) {
		node := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, nodeKey(index), node)).To(Succeed())
		node.Status.Ready = true
		node.Status.PodName = valkeyNodeResourceName(node) + "-0"
		node.Status.PodIP = "10.0.0." + strconv.Itoa(index+1)
		node.Status.ObservedGeneration = node.Generation
		Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
	}

	reconcileOnce := func() {
		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
	}

	getValkey := func() *valkeyiov1alpha1.Valkey {
		current := &valkeyiov1alpha1.Valkey{}
		Expect(k8sClient.Get(ctx, req.NamespacedName, current)).To(Succeed())
		return current
	}

	BeforeEach(func() {
		ctx = context.Background()
		fake = &fakeReplication{states: map[string]replicationState{}, replicaOfs: map[string]string{}}
		reconciler = &ValkeyReconciler{
			Client:            k8sClient,
			APIReader:         k8sClient,
			Scheme:            k8sClient.Scheme(),
			Recorder:          events.NewFakeRecorder(100),
			replicationClient: fake,
		}
		v = &valkeyiov1alpha1.Valkey{
			ObjectMeta: metav1.ObjectMeta{Name: "replicated", Namespace: "default"},
			Spec:       valkeyiov1alpha1.ValkeySpec{Replicas: 2},
		}
		Expect(k8sClient.Create(ctx, v)).To(Succeed())
		req = reconcile.Request{NamespacedName: types.NamespacedName{Name: v.Name, Namespace: v.Namespace}}

		// envtest runs no garbage collector, so delete the children explicitly.
		DeferCleanup(func() {
			objs := []client.Object{
				&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: headlessServiceName(v.Name), Namespace: v.Namespace}},
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: GetServerConfigMapName(v.Name), Namespace: v.Namespace}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: getInternalSecretName(v.Name), Namespace: v.Namespace}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: getSystemPasswordSecretName(v.Name), Namespace: v.Namespace}},
				&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: resourcePrefix + v.Name, Namespace: v.Namespace}},
				v,
			}
			for _, obj := range objs {
				_ = k8sClient.Delete(ctx, obj)
			}
			for index := range 3 {
				_ = k8sClient.Delete(ctx, &valkeyiov1alpha1.ValkeyNode{ObjectMeta: metav1.ObjectMeta{Name: valkeyNodeName(v.Name, 0, index), Namespace: v.Namespace}})
			}
		})
	})

	// bootstrap creates the nodes, marks them ready, and lets replication converge.
	bootstrap := func() {
		reconcileOnce()
		for index := range 3 {
			markReady(index)
		}
		reconcileOnce()
		reconcileOnce()
	}

	It("creates one node per position and points the replicas at node 0", func() {
		bootstrap()

		primaryHost := valkeyNodeResourceName(&valkeyiov1alpha1.ValkeyNode{ObjectMeta: metav1.ObjectMeta{Name: nodeKey(0).Name}}) + "-0." + headlessServiceFQDN(v.Name, v.Namespace, valkeyiov1alpha1.DefaultClusterDomain)
		Expect(fake.replicaOfs).To(Equal(map[string]string{nodeKey(1).Name: primaryHost, nodeKey(2).Name: primaryHost}))

		status := getValkey().Status
		Expect(status.State).To(Equal(valkeyiov1alpha1.ValkeyStateReady))
		Expect(status.Primary).To(Equal(nodeKey(0).Name))
		Expect(status.Replicas).To(BeEquivalentTo(2))
		Expect(status.ReadyReplicas).To(BeEquivalentTo(2))
		Expect(meta.IsStatusConditionTrue(status.Conditions, valkeyiov1alpha1.ConditionPrimaryElected)).To(BeTrue())
		Expect(meta.IsStatusConditionTrue(status.Conditions, valkeyiov1alpha1.ConditionReplicationHealthy)).To(BeTrue())

		// With replicas the budget is created.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourcePrefix + v.Name, Namespace: v.Namespace}, &policyv1.PodDisruptionBudget{})).To(Succeed())
	})

	It("rolls replicas one at a time and hands the primary off before replacing it", func() {
		bootstrap()
		current := getValkey()
		current.Spec.Image = "valkey/valkey:9.0.1"
		Expect(k8sClient.Update(ctx, current)).To(Succeed())

		imageOf := func(index int) string {
			node := &valkeyiov1alpha1.ValkeyNode{}
			Expect(k8sClient.Get(ctx, nodeKey(index), node)).To(Succeed())
			return node.Spec.Image
		}

		reconcileOnce()
		Expect([]string{imageOf(0), imageOf(1), imageOf(2)}).To(Equal([]string{"", "valkey/valkey:9.0.1", ""}))

		markReady(1)
		reconcileOnce()
		Expect([]string{imageOf(0), imageOf(2)}).To(Equal([]string{"", "valkey/valkey:9.0.1"}))

		markReady(2)
		reconcileOnce()
		Expect(imageOf(0)).To(BeEmpty(), "the primary is not replaced before the handoff")
		Expect(fake.failovers).To(ConsistOf(nodeKey(0).Name + "->10.0.0.2"))
	})

	It("removes nodes above spec.replicas on scale-in", func() {
		bootstrap()
		current := getValkey()
		current.Spec.Replicas = 1
		Expect(k8sClient.Update(ctx, current)).To(Succeed())

		reconcileOnce()
		err := k8sClient.Get(ctx, nodeKey(2), &valkeyiov1alpha1.ValkeyNode{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})
})
