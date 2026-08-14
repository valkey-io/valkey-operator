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
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

const (
	// DefaultRolePollInterval is how often the RolePoller samples live cluster
	// state.
	DefaultRolePollInterval = 5 * time.Second
	// rolePollMaxBackoff caps the per-node backoff applied after a node fails to
	// answer, so one dead pod cannot turn the poller into a connection generator
	// against a socket that is not listening.
	rolePollMaxBackoff = time.Minute
)

// RolePoller samples live Valkey topology and triggers a ValkeyNode reconcile
// when a node's live role differs from the role recorded on its CR.
//
// It is a detector, never a writer. The ValkeyNode controller remains the sole
// writer of Status.Role and re-resolves the role from its own connection when
// woken; the poller only decides *when* to wake it. Keeps the single-writer
// invariant intact.
//
// The poller complements the two existing triggers: the Pod watch is still
// faster for pod recreation, while the ValkeyNode requeue can lag behind
// the real state
type RolePoller struct {
	// Client is cache-backed, so the per-tick List calls cost nothing.
	Client client.Client
	// APIReader is uncached, used only to read the TLS secret when scraping.
	APIReader client.Reader
	// Interval is how often live state is sampled. Zero means
	// DefaultRolePollInterval.
	Interval time.Duration
	// Events receives one GenericEvent per node whose live role has drifted.
	Events chan<- event.GenericEvent

	// scrapeFunc, when set, overrides how the poller reads live cluster state.
	// Tests inject a fake (envtest has no running Valkey server); production
	// leaves it nil and dials through scrapeClusterState. This is also the seam
	// where pooled clients replace per-tick connections.
	scrapeFunc func(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, addresses []string) *valkey.ClusterState

	// backoff tracks consecutive scrape failures per node. Entries are pruned
	// each tick for nodes that no longer exist. Only Start's goroutine touches
	// it, so it needs no lock.
	backoff map[types.NamespacedName]nodeBackoff
}

// nodeBackoff is the poller's dial state for a single node.
type nodeBackoff struct {
	failures    int
	nextAttempt time.Time
}

// NeedLeaderElection reports that only the elected leader runs the poller.
// Runnables added with mgr.Add default to the leader-election group, but the
// intent is declared explicitly here: a standby replica must not dial Valkey
// servers.
func (p *RolePoller) NeedLeaderElection() bool {
	return true
}

// Start runs the poll loop until the context is cancelled. It satisfies
// manager.Runnable.
func (p *RolePoller) Start(ctx context.Context) error {
	interval := p.Interval
	if interval <= 0 {
		interval = DefaultRolePollInterval
	}
	log := logf.FromContext(ctx).WithName("rolepoller")
	log.Info("starting role poller", "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("stopping role poller")
			return nil
		case <-ticker.C:
			p.tick(logf.IntoContext(ctx, log), time.Now())
		}
	}
}

// tick samples every cluster once and emits an event per drifted node. A tick
// where every role matches performs no API writes at all.
//
// now is passed in rather than read from the clock so tests can drive the
// per-node backoff deterministically.
func (p *RolePoller) tick(ctx context.Context, now time.Time) {
	log := logf.FromContext(ctx)

	clusters := &valkeyiov1alpha1.ValkeyClusterList{}
	if err := p.Client.List(ctx, clusters); err != nil {
		log.Error(err, "failed to list ValkeyClusters")
		return
	}

	seen := make(map[types.NamespacedName]struct{})
	for i := range clusters.Items {
		p.pollCluster(ctx, &clusters.Items[i], now, seen)
	}

	// Drop backoff state for nodes that have gone away, so the map cannot grow
	// without bound across cluster deletions and scale-in.
	for key := range p.backoff {
		if _, ok := seen[key]; !ok {
			delete(p.backoff, key)
		}
	}
}

// pollCluster scrapes one cluster and emits drift events for its nodes. Every
// node it considers is recorded in seen, which tick uses to prune stale backoff
// entries.
func (p *RolePoller) pollCluster(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, now time.Time, seen map[types.NamespacedName]struct{}) {
	log := logf.FromContext(ctx).WithValues("cluster", cluster.Name, "namespace", cluster.Namespace)

	nodes := &valkeyiov1alpha1.ValkeyNodeList{}
	if err := p.Client.List(ctx, nodes,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels(map[string]string{LabelCluster: cluster.Name}),
	); err != nil {
		log.Error(err, "failed to list ValkeyNodes")
		return
	}

	// Candidates are nodes with a pod IP that are not currently backed off. A
	// node still waiting for an IP has nothing to dial, and a node in backoff is
	// left out of the address list entirely so no connection is attempted.
	var addresses []string
	candidates := make([]*valkeyiov1alpha1.ValkeyNode, 0, len(nodes.Items))
	for i := range nodes.Items {
		node := &nodes.Items[i]
		seen[client.ObjectKeyFromObject(node)] = struct{}{}
		if node.Status.PodIP == "" {
			continue
		}
		if state, ok := p.backoff[client.ObjectKeyFromObject(node)]; ok && now.Before(state.nextAttempt) {
			continue
		}
		candidates = append(candidates, node)
		addresses = append(addresses, node.Status.PodIP)
	}
	if len(addresses) == 0 {
		return
	}

	state := p.scrape(ctx, cluster, addresses)
	if state == nil {
		return
	}
	defer state.CloseClients()

	for _, node := range candidates {
		key := client.ObjectKeyFromObject(node)
		liveRole := liveRoleForAddress(state, node.Status.PodIP)
		if liveRole == "" {
			// Either the node did not answer, or it answered but is not yet part
			// of a shard (bootstrap, or a demoting primary that has given up its
			// slots). Only the first is a dial failure worth backing off.
			if !state.HasAddress(node.Status.PodIP) {
				p.recordFailure(key, now)
			} else {
				delete(p.backoff, key)
			}
			continue
		}
		delete(p.backoff, key)

		if liveRole == node.Status.Role {
			continue
		}
		// A disagreement the node controller cannot settle — it failed to resolve
		// the role, or the pod is not ready — is re-emitted every tick until it
		// clears. That is bounded to one reconcile per interval per node, and the
		// workqueue's rate limiter collapses repeats while one is already queued.
		log.V(1).Info("live role differs from status, triggering reconcile",
			"node", node.Name, "live", liveRole, "status", node.Status.Role)
		p.emit(node)
	}
}

// scrape reads live cluster state, through scrapeFunc when a test has injected
// one. Fetching the operator password is what can fail here; without it every
// connection would be rejected, so the tick is skipped rather than dialled.
func (p *RolePoller) scrape(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, addresses []string) *valkey.ClusterState {
	if p.scrapeFunc != nil {
		return p.scrapeFunc(ctx, cluster, addresses)
	}
	password, err := fetchSystemUserPassword(ctx, operatorUser, p.Client, cluster.Name, cluster.Namespace)
	if err != nil {
		logf.FromContext(ctx).V(1).Info("skipping role poll, operator password unavailable",
			"cluster", cluster.Name, "namespace", cluster.Namespace, "err", err)
		return nil
	}
	return scrapeClusterState(ctx, p.APIReader, cluster, addresses, operatorUser, password)
}

// emit pushes a reconcile trigger for the node, dropping it if the channel is
// full. Dropping is correct: the backstop requeue still catches the change,
// whereas blocking would stall every other cluster behind one slow consumer.
func (p *RolePoller) emit(node *valkeyiov1alpha1.ValkeyNode) {
	select {
	case p.Events <- event.GenericEvent{Object: node.DeepCopy()}:
	default:
	}
}

// recordFailure advances a node's backoff after it failed to answer, doubling
// the delay per consecutive failure up to rolePollMaxBackoff.
func (p *RolePoller) recordFailure(key types.NamespacedName, now time.Time) {
	if p.backoff == nil {
		p.backoff = make(map[types.NamespacedName]nodeBackoff)
	}
	interval := p.Interval
	if interval <= 0 {
		interval = DefaultRolePollInterval
	}
	failures := p.backoff[key].failures + 1
	delay := interval
	for range failures - 1 {
		delay *= 2
		if delay >= rolePollMaxBackoff {
			delay = rolePollMaxBackoff
			break
		}
	}
	p.backoff[key] = nodeBackoff{failures: failures, nextAttempt: now.Add(delay)}
}

// liveRoleForAddress returns the live replication role ("primary" or "replica")
// of the node with the given address per the cluster state, or "" if the address
// is not part of any shard.
//
// This mirrors the slot-ownership rule the ValkeyNode controller applies in
// parseClusterNodesRole: the shard's primary is the node that owns slots, so a
// slot-less master (a node still being added, or one that has just given up its
// slots) is not reported as a primary. Such a node sits in PendingNodes rather
// than a shard, so it resolves to "" here and is simply not compared.
func liveRoleForAddress(state *valkey.ClusterState, address string) string {
	shard := state.FindShardForAddress(address)
	if shard == nil {
		return ""
	}
	if primary := shard.GetPrimaryNode(); primary != nil && primary.Address == address {
		return RolePrimary
	}
	return RoleReplica
}
