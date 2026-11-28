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
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	vclient "github.com/valkey-io/valkey-go"
	"github.com/valkey-io/valkey-go/mock"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

// benchValkeyNode answers the commands a reconcile sends to one node of a
// generated cluster: the scrape from canned replies, CLUSTER GETSLOTMIGRATIONS
// with no jobs, and every other command with OK.
type benchValkeyNode struct {
	vclient.Client
	id, shardID, info, clusterInfo, nodes string
	migrations                            *atomic.Int64
}

// B returns a builder; the zero value builds commands without a connection.
func (n *benchValkeyNode) B() vclient.Builder { return vclient.Builder{} }

// DoMulti returns fresh copies of the scrape replies, as a real connection
// does: the scrape keeps substrings of them.
func (n *benchValkeyNode) DoMulti(_ context.Context, cmds ...vclient.Completed) []vclient.ValkeyResult {
	if c := cmds[0].Commands(); len(c) > 1 && c[1] == "GETSLOTMIGRATIONS" {
		now := time.Now()
		return []vclient.ValkeyResult{
			mock.Result(mock.ValkeyArray()),
			mock.Result(mock.ValkeyArray(
				mock.ValkeyBlobString(strconv.FormatInt(now.Unix(), 10)),
				mock.ValkeyBlobString(strconv.Itoa(now.Nanosecond()/1000)))),
		}
	}
	return []vclient.ValkeyResult{
		mock.Result(mock.ValkeyBlobString(strings.Clone(n.id))),
		mock.Result(mock.ValkeyBlobString(strings.Clone(n.shardID))),
		mock.Result(mock.ValkeyBlobString(strings.Clone(n.info))),
		mock.Result(mock.ValkeyBlobString(strings.Clone(n.clusterInfo))),
		mock.Result(mock.ValkeyBlobString(strings.Clone(n.nodes))),
	}
}

func (n *benchValkeyNode) Do(_ context.Context, cmd vclient.Completed) vclient.ValkeyResult {
	if c := cmd.Commands(); len(c) > 1 && c[1] == "MIGRATESLOTS" {
		n.migrations.Add(1)
	}
	return mock.Result(mock.ValkeyString("OK"))
}

// reconcileBench is one generated cluster behind a reconciler: the ValkeyCluster
// and its ValkeyNodes in a fake API server, and a Valkey node per pod address.
type reconcileBench struct {
	r          *ValkeyClusterReconciler
	req        reconcile.Request
	migrations *atomic.Int64
}

// newReconcileBench builds a cluster of shards primaries with one replica each.
// The last empty shards own no slots, as after a scale-out has joined the new
// shards but not yet moved slots to them.
func newReconcileBench(b *testing.B, shards, empty int) *reconcileBench {
	b.Helper()
	ctx := context.Background()
	owners := shards - empty

	type member struct {
		id, shardID, address, primaryID, slots string
		shard, index                           int
	}
	members := make([]member, 0, 2*shards)
	for s := range shards {
		shardID := fmt.Sprintf("%040x", 1<<32|s)
		primaryID := fmt.Sprintf("%040x", 2<<32|s)
		slots := ""
		if s < owners {
			slots = fmt.Sprintf("%d-%d", s*valkey.TotalSlots/owners, (s+1)*valkey.TotalSlots/owners-1)
		}
		members = append(members,
			member{primaryID, shardID, fmt.Sprintf("10.244.%d.%d", s/250, s%250+1), "-", slots, s, 0},
			member{fmt.Sprintf("%040x", 3<<32|s), shardID, fmt.Sprintf("10.245.%d.%d", s/250, s%250+1), primaryID, "", s, 1})
	}
	line := func(m member, myself bool) string {
		flags := "master"
		if m.primaryID != "-" {
			flags = "slave"
		}
		if myself {
			flags = "myself," + flags
		}
		l := fmt.Sprintf("%s %s:6379@16379 %s %s 0 1763650000000 1 connected", m.id, m.address, flags, m.primaryID)
		if m.slots != "" {
			l += " " + m.slots
		}
		return l + "\n"
	}
	clusterInfo := fmt.Sprintf("cluster_enabled:1\r\ncluster_state:ok\r\ncluster_slots_assigned:16384\r\n"+
		"cluster_known_nodes:%d\r\ncluster_size:%d\r\ncluster_current_epoch:%d\r\ncluster_my_epoch:1\r\n",
		len(members), owners, shards)

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		b.Fatal(err)
	}
	if err := valkeyiov1alpha1.AddToScheme(scheme); err != nil {
		b.Fatal(err)
	}
	cluster := &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "bench", Namespace: "default", UID: "bench-uid"},
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{
			Shards:           int32(shards),
			ReplicasPerShard: 1,
			Image:            "valkey/valkey:9.0.0",
			WorkloadType:     valkeyiov1alpha1.WorkloadTypeStatefulSet,
		},
	}

	migrations := &atomic.Int64{}
	r := &ValkeyClusterReconciler{Scheme: scheme}
	objects := make([]client.Object, 0, 1+len(members))
	objects = append(objects, cluster)
	clients := make(map[string]vclient.Client, len(members))
	for i, m := range members {
		var table strings.Builder
		for j, other := range members {
			table.WriteString(line(other, i == j))
		}
		info := "# Replication\r\nrole:master\r\n"
		if m.primaryID != "-" {
			info = "# Replication\r\nrole:slave\r\nmaster_link_status:up\r\nslave_repl_offset:31080\r\n"
		}
		clients[m.address+":6379"] = &benchValkeyNode{
			id: m.id, shardID: m.shardID, info: info, clusterInfo: clusterInfo, nodes: table.String(),
			migrations: migrations,
		}

		// The spec reconcileValkeyNode builds for a node with no workload yet,
		// so the pass finds every ValkeyNode up to date.
		node := buildClusterValkeyNode(cluster, m.shard, m.index)
		withTrustBundle(node, "")
		if err := r.setDesiredConfigRevision(ctx, node, nil, ""); err != nil {
			b.Fatal(err)
		}
		if err := setDesiredWorkloadRevision(node); err != nil {
			b.Fatal(err)
		}
		if err := controllerutil.SetControllerReference(cluster, node, scheme); err != nil {
			b.Fatal(err)
		}
		node.Status.Ready = true
		node.Status.PodIP = m.address
		objects = append(objects, node)
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&valkeyiov1alpha1.ValkeyCluster{}, &valkeyiov1alpha1.ValkeyNode{}).Build()
	pool := valkey.NewPool(valkey.DefaultIdleTTL, func(vclient.ClientOption) (vclient.Client, error) {
		return nil, errors.New("the benchmark dials through clusterStateFunc only")
	})
	b.Cleanup(pool.Close)
	dial := func(_ context.Context, address string) (vclient.Client, error) {
		if c, ok := clients[address]; ok {
			return c, nil
		}
		return nil, fmt.Errorf("no generated node at %s", address)
	}
	r.Client = cl
	r.APIReader = cl
	// A nil Events channel drops events; a buffered one would fill and block.
	r.Recorder = &events.FakeRecorder{}
	r.ValkeyClients = NewClientProvider(cl, cl, pool)
	// A fresh scrape each pass, as in production, where each reconcile
	// builds its own ClusterState.
	r.clusterStateFunc = func(ctx context.Context, nodes *valkeyiov1alpha1.ValkeyNodeList) *valkey.ClusterState {
		return valkey.GetClusterState(ctx, nodeAddresses(nodes), DefaultPort, dial)
	}
	return &reconcileBench{
		r:          r,
		req:        reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cluster)},
		migrations: migrations,
	}
}

func (f *reconcileBench) cluster(b *testing.B) *valkeyiov1alpha1.ValkeyCluster {
	b.Helper()
	c := &valkeyiov1alpha1.ValkeyCluster{}
	if err := f.r.Get(context.Background(), f.req.NamespacedName, c); err != nil {
		b.Fatal(err)
	}
	return c
}

// BenchmarkReconcile times one reconcile pass at each cluster size, for a
// stage of an operation. The first pass creates the Service, PDB, Secrets and
// ConfigMap and writes the status; later passes on the same snapshot write
// nothing, so every timed pass does the same work. Profile one stage and size
// with, for example:
//
//	go test ./internal/controller/ -run '^$' -bench 'Reconcile/stage=steady/nodes=500$' -cpuprofile cpu.out
//
// Set RECONCILE_BENCH_LOG to a file path to keep the operator's logs, at the
// debug level the deployed operator uses. Logging adds to every pass, so time
// with it unset:
//
//	RECONCILE_BENCH_LOG=/tmp/bench.log go test ./internal/controller/ -run '^$' -bench 'Reconcile/stage=rebalance/nodes=100$' -benchtime=1x
func BenchmarkReconcile(b *testing.B) {
	if path := os.Getenv("RECONCILE_BENCH_LOG"); path != "" {
		f, err := os.Create(path)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = f.Close() })
		logf.SetLogger(zap.New(zap.WriteTo(f), zap.UseDevMode(true)))
	} else {
		logf.SetLogger(zap.New(zap.WriteTo(io.Discard)))
	}
	stages := []struct {
		name string
		// empty returns how many of the shards own no slots.
		empty   func(shards int) int
		requeue time.Duration
		check   func(b *testing.B, f *reconcileBench)
	}{
		// Every shard joined and holding slots: the pass a Ready cluster
		// repeats on every requeue and every role poller trigger.
		{"steady", func(int) int { return 0 }, 30 * time.Second, func(b *testing.B, f *reconcileBench) {
			if !meta.IsStatusConditionTrue(f.cluster(b).Status.Conditions, valkeyiov1alpha1.ConditionReady) {
				b.Fatalf("the steady cluster is not Ready: %+v", f.cluster(b).Status)
			}
		}},
		// A scale-out by a tenth of the shards, joined but empty: each pass
		// plans and starts one slot migration. A tenth, not one shard: from
		// 129 shards, one new shard leaves every other within one slot of its
		// target, and PlanRebalanceMove plans no move.
		{"rebalance", func(shards int) int { return max(1, shards/10) }, 2 * time.Second, func(b *testing.B, f *reconcileBench) {
			if f.migrations.Load() == 0 {
				b.Fatalf("the rebalance pass sent no MIGRATESLOTS: %+v", f.cluster(b).Status)
			}
		}},
	}
	for _, stage := range stages {
		for _, shards := range []int{50, 125, 250, 500} {
			b.Run(fmt.Sprintf("stage=%s/nodes=%d", stage.name, 2*shards), func(b *testing.B) {
				f := newReconcileBench(b, shards, stage.empty(shards))
				ctx := context.Background()
				result, err := f.r.Reconcile(ctx, f.req)
				if err != nil {
					b.Fatal(err)
				}
				if result.RequeueAfter != stage.requeue {
					b.Fatalf("first pass requeues after %s, want %s: %+v", result.RequeueAfter, stage.requeue, f.cluster(b).Status)
				}
				stage.check(b, f)
				version := f.cluster(b).ResourceVersion

				for b.Loop() {
					if _, err := f.r.Reconcile(ctx, f.req); err != nil {
						b.Fatal(err)
					}
				}
				if v := f.cluster(b).ResourceVersion; v != version {
					b.Fatalf("a timed pass wrote the cluster (resourceVersion %s to %s), so passes differ", version, v)
				}
			})
		}
	}
}
