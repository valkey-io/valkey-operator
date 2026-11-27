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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"maps"
	"net"
	"strconv"
	"strings"
	"time"

	vclient "github.com/valkey-io/valkey-go"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

// bootstrapPlan is what one reconcile knows about the source cluster.
type bootstrapPlan struct {
	// shards are the source primaries ordered by first slot; target shard i
	// replicates shards[i].
	shards []valkey.SourceShard
	// seedIP and seedPort are a source node the new nodes MEET.
	seedIP   string
	seedPort int
	// password authenticates replication from the source ("" for none).
	password string
}

// clusterMigrated reports whether every shard the operator manages owns slots,
// i.e. cutover has happened. It reads live state only, so it holds across
// operator restarts and needs no stored phase.
func clusterMigrated(state *valkey.ClusterState, shards int32) bool {
	owning := 0
	for _, shard := range state.Shards {
		if len(shard.Slots) > 0 {
			owning++
		}
	}
	return shards > 0 && owning >= int(shards)
}

// bootstrapMigrated reports whether cutover has happened. It is true as soon as
// every shard owns slots, and stays true afterwards through the status
// condition finishBootstrapConditions leaves. Without the stored marker, raising
// spec.shards after cutover would look like a bootstrap again, and the operator
// would try to plan a source that may be gone.
func bootstrapMigrated(cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState) bool {
	if c := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionBootstrapSynced); c != nil &&
		c.Status == metav1.ConditionTrue && c.Reason == valkeyiov1alpha1.ReasonBootstrapComplete {
		return true
	}
	return clusterMigrated(state, cluster.Spec.Shards)
}

// bootstrapCutoverRequested reports whether the user moved spec.bootstrap.stage
// past Bootstrap. It is the only trigger for cutover.
func bootstrapCutoverRequested(cluster *valkeyiov1alpha1.ValkeyCluster) bool {
	b := cluster.Spec.Bootstrap
	return b != nil && b.Stage != "" && b.Stage != valkeyiov1alpha1.BootstrapStageBootstrap
}

// sourceMembers returns the cluster members that are not this cluster's own
// nodes, read from one reachable node's table. After cutover these are the old
// source nodes, which stay as replicas until the stage is Complete.
//
// A node counts as ours when it was scraped, or when its address is the pod IP
// of one of owned. The second check matters when a scrape of our own pod
// failed: that pod must not be mistaken for a source node and have its failover
// settings or credentials changed.
func sourceMembers(state *valkey.ClusterState, owned []valkeyiov1alpha1.ValkeyNode) []valkey.ClusterNode {
	all := state.AllNodes()
	if len(all) == 0 {
		return nil
	}
	var out []valkey.ClusterNode
	for _, entry := range all[0].Peers() {
		if entry.IsMyself() || state.FindNodeById(entry.Id) != nil || hasNodeWithPodIP(owned, entry.Host) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// finishBootstrapConditions replaces the in-progress bootstrap conditions once
// the new nodes own the slots, and reports whether source nodes are still
// members of the cluster.
func finishBootstrapConditions(cluster *valkeyiov1alpha1.ValkeyCluster, remaining int) {
	meta.RemoveStatusCondition(&cluster.Status.Conditions, valkeyiov1alpha1.ConditionBootstrapFailed)
	if meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionBootstrapSynced) != nil {
		setCondition(cluster, valkeyiov1alpha1.ConditionBootstrapSynced, valkeyiov1alpha1.ReasonBootstrapComplete, "Cutover complete", metav1.ConditionTrue)
	}
	if remaining == 0 {
		setCondition(cluster, valkeyiov1alpha1.ConditionBootstrapSourceDetached, valkeyiov1alpha1.ReasonSourceDetached,
			"No source nodes are left in the cluster", metav1.ConditionTrue)
		return
	}
	msg := fmt.Sprintf("%d source node(s) are still in the cluster as replicas and cannot be elected primary; "+
		"set spec.bootstrap.stage to Complete once you are done with the source", remaining)
	if cluster.Spec.Bootstrap.Stage == valkeyiov1alpha1.BootstrapStageComplete {
		msg = fmt.Sprintf("%d source node(s) are still in the cluster; shut them down and the operator will forget them", remaining)
	}
	setCondition(cluster, valkeyiov1alpha1.ConditionBootstrapSourceDetached, valkeyiov1alpha1.ReasonSourceNodesRemain, msg, metav1.ConditionFalse)
}

// sourceSecret reads one key of a Secret in the cluster's namespace.
func (r *ValkeyClusterReconciler) sourceSecret(ctx context.Context, namespace string, ref *corev1.SecretKeySelector) ([]byte, error) {
	secret := &corev1.Secret{}
	// The manager caches only operator-labelled Secrets, so a user-provided one
	// must be read straight from the API server.
	var reader client.Reader = r.Client
	if r.APIReader != nil {
		reader = r.APIReader
	}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, secret); err != nil {
		return nil, fmt.Errorf("secret %q: %w", ref.Name, err)
	}
	value, ok := secret.Data[ref.Key]
	if !ok {
		return nil, fmt.Errorf("secret %q has no key %q", ref.Name, ref.Key)
	}
	return value, nil
}

// sourceConn builds the connection settings for the source cluster: its
// password and, when configured, TLS with the user's CA.
func (r *ValkeyClusterReconciler) sourceConn(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) (connConfig, error) {
	src := cluster.Spec.Bootstrap.Source
	var cfg connConfig
	if src.PasswordSecretRef != nil {
		pw, err := r.sourceSecret(ctx, cluster.Namespace, src.PasswordSecretRef)
		if err != nil {
			return cfg, fmt.Errorf("bootstrap source password: %w", err)
		}
		cfg.password = string(pw)
	}
	if src.TLS != nil {
		cfg.tls = &tls.Config{MinVersion: tls.VersionTLS12}
		if src.TLS.CASecretRef != nil {
			ca, err := r.sourceSecret(ctx, cluster.Namespace, src.TLS.CASecretRef)
			if err != nil {
				return cfg, fmt.Errorf("bootstrap source CA: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(ca) {
				return cfg, errors.New("bootstrap source CA: no PEM certificates found")
			}
			cfg.tls.RootCAs = pool
		}
	}
	return cfg, nil
}

// planBootstrap reads the source topology through the first reachable endpoint
// and validates it. It only reads from the source.
func (r *ValkeyClusterReconciler) planBootstrap(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) (*bootstrapPlan, error) {
	src := cluster.Spec.Bootstrap.Source
	cfg, err := r.sourceConn(ctx, cluster)
	if err != nil {
		return nil, err
	}
	plan := &bootstrapPlan{password: cfg.password}

	var lastErr error
	for _, endpoint := range src.Endpoints {
		host, portStr, err := net.SplitHostPort(endpoint)
		if err != nil {
			return nil, fmt.Errorf("bootstrap endpoint %q: %w", endpoint, err)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, fmt.Errorf("bootstrap endpoint %q: bad port", endpoint)
		}
		raw, err := sourceClusterNodes(ctx, endpoint, cfg)
		if err != nil {
			lastErr = fmt.Errorf("endpoint %s: %w", endpoint, err)
			continue
		}
		shards, err := valkey.PlanBootstrapSource(valkey.ParseClusterNodes(raw), int(cluster.Spec.Shards))
		if err != nil {
			return nil, err
		}
		ip, err := resolveIP(host)
		if err != nil {
			return nil, fmt.Errorf("bootstrap endpoint %q: %w", endpoint, err)
		}
		plan.shards, plan.seedIP, plan.seedPort = shards, ip, port
		return plan, nil
	}
	return nil, fmt.Errorf("no bootstrap source endpoint reachable: %w", lastErr)
}

// sourceClusterNodes returns CLUSTER NODES from one source node. The client is
// short-lived and closed here; the source is only read.
func sourceClusterNodes(ctx context.Context, address string, cfg connConfig) (string, error) {
	c, err := vclient.NewClient(clientOption(address, cfg))
	if err != nil {
		return "", err
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := c.Do(ctx, c.B().ClusterNodes().Build()).ToString()
	// A RESP3 verbatim string carries a "txt:" marker that would corrupt the
	// first node ID.
	return strings.TrimPrefix(raw, "txt:"), err
}

// resolveIP returns host as an IP; CLUSTER MEET does not take names.
func resolveIP(host string) (string, error) {
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return "", fmt.Errorf("cannot resolve %q: %v", host, err)
	}
	return ips[0].String(), nil
}

// meetIsolatedToSource introduces every isolated new node to the source
// cluster. Unlike meetIsolatedNodes it is one-way (the new node reaches out, the
// source is never asked to MEET) and leaves config epochs alone: replicas take
// no part in epoch contests.
func (r *ValkeyClusterReconciler) meetIsolatedToSource(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState, plan *bootstrapPlan) (int, error) {
	log := logf.FromContext(ctx)
	met := 0
	for _, node := range state.PendingNodes {
		if !node.IsIsolated() {
			continue
		}
		log.V(1).Info("meet source", "node", node.Address, "source", plan.seedIP)
		if err := node.Client.Do(ctx, node.Client.B().ClusterMeet().Ip(plan.seedIP).Port(int64(plan.seedPort)).Build()).Error(); err != nil {
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "ClusterMeetFailed", "ClusterMeet", "CLUSTER MEET %v -> source %v failed: %v", node.Address, plan.seedIP, err)
			return met, err
		}
		met++
	}
	return met, nil
}

// replicateToSource attaches each pending new node to the source primary of its
// shard, after pointing its replication credentials at the source.
func (r *ValkeyClusterReconciler) replicateToSource(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState, nodes *valkeyiov1alpha1.ValkeyNodeList, plan *bootstrapPlan) (int, error) {
	log := logf.FromContext(ctx)
	replicated := 0
	for _, node := range state.PendingNodes {
		if node.IsIsolated() {
			continue // not MEET'd yet
		}
		_, shardIndex := nodeRoleAndShard(node.Address, nodes)
		if shardIndex < 0 || shardIndex >= len(plan.shards) {
			continue
		}
		// A shard that has already been cut over has one of our own nodes as its
		// primary, which takes this cluster's replication credentials, not the
		// source's.
		user, password := "", plan.password
		if state.FindNodeById(plan.shards[shardIndex].PrimaryId) != nil {
			pw, err := fetchSystemUserPassword(ctx, replicationUser, r.Client, cluster.Name, cluster.Namespace)
			if err != nil || pw == "" {
				return replicated, fmt.Errorf("read replication password for %s: %v", node.Address, err)
			}
			user, password = replicationUser, pw
		}
		if err := setReplicationAuth(ctx, node, user, password); err != nil {
			return replicated, fmt.Errorf("set replication auth on %s: %w", node.Address, err)
		}
		err := r.issueReplicate(ctx, cluster, node, plan.shards[shardIndex].PrimaryId, shardIndex)
		if errors.Is(err, errPrimaryNotReady) {
			log.V(1).Info("source primary not known yet; will retry", "node", node.Address)
			continue
		}
		if err != nil {
			return replicated, err
		}
		replicated++
	}
	return replicated, nil
}

// setReplicationAuth sets the credentials the node uses to sync from a primary.
// Both settings are live, so no pod rolls.
func setReplicationAuth(ctx context.Context, node *valkey.NodeState, user, password string) error {
	for _, kv := range [][2]string{{"primaryuser", user}, {"primaryauth", password}} {
		cmd := node.Client.B().ConfigSet().ParameterValue().ParameterValue(kv[0], kv[1]).Build()
		if err := node.Client.Do(ctx, cmd).Error(); err != nil {
			return err
		}
	}
	return nil
}

// stillSourcePrimaries returns the IDs of the source primaries that have not
// been replaced yet. Once a shard is cut over its slot range belongs to one of
// our own nodes, so it shows up in the plan as a primary too, but its replicas
// follow a node of this cluster and use this cluster's credentials.
func stillSourcePrimaries(state *valkey.ClusterState, plan *bootstrapPlan) map[string]struct{} {
	out := make(map[string]struct{}, len(plan.shards))
	for _, sh := range plan.shards {
		if state.FindNodeById(sh.PrimaryId) == nil {
			out[sh.PrimaryId] = struct{}{}
		}
	}
	return out
}

// restoreSourceReplicationAuth re-applies the source credentials on every new
// node that replicates a source primary but whose link is down. A pod that
// restarted with its saved cluster state is not pending, so replicateToSource
// never sees it, and it would otherwise come back with this cluster's own
// replication credentials, which the source rejects. Nodes with the link up are
// left alone.
func (r *ValkeyClusterReconciler) restoreSourceReplicationAuth(ctx context.Context, state *valkey.ClusterState, plan *bootstrapPlan) {
	source := stillSourcePrimaries(state, plan)
	for _, node := range state.AllNodes() {
		if _, ok := source[node.PrimaryIdFromSelf()]; !ok || node.Info["master_link_status"] == "up" {
			continue
		}
		if err := setReplicationAuth(ctx, node, "", plan.password); err != nil {
			logf.FromContext(ctx).Error(err, "restore source replication auth", "node", node.Address)
		}
	}
}

// restoreReplicationAuth gives the operator's own replication credentials back
// to replicas of a shard that has been cut over. Their sibling was promoted, so
// they now sync from a primary of this cluster, which does not know the source
// password. Idempotent: it touches only replicas whose link is down.
func (r *ValkeyClusterReconciler) restoreReplicationAuth(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState) {
	var password string
	for _, shard := range state.Shards {
		if len(shard.Slots) == 0 {
			continue // not cut over yet
		}
		for _, node := range shard.Nodes {
			if node.Id == shard.PrimaryId || node.Info["master_link_status"] == "up" {
				continue
			}
			if password == "" {
				pw, err := fetchSystemUserPassword(ctx, replicationUser, r.Client, cluster.Name, cluster.Namespace)
				if err != nil || pw == "" {
					logf.FromContext(ctx).Error(err, "cannot read replication password")
					return
				}
				password = pw
			}
			if err := setReplicationAuth(ctx, node, replicationUser, password); err != nil {
				logf.FromContext(ctx).Error(err, "restore replication auth", "node", node.Address)
			}
		}
	}
}

// bootstrapCutover promotes the most caught-up new replica of each shard that
// does not own its slots yet, with a graceful CLUSTER FAILOVER: the replica
// waits for its primary's offset, so no write is lost. It is called only when
// spec.bootstrap.stage is past Bootstrap; nothing else in the operator promotes a
// bootstrapping node. Returns the number of failovers started.
func (r *ValkeyClusterReconciler) bootstrapCutover(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState) int {
	log := logf.FromContext(ctx)
	started := 0
	for _, shard := range state.Shards {
		if len(shard.Slots) > 0 {
			continue
		}
		replica := valkey.HighestOffsetReplica(syncedNodes(shard))
		if replica == nil {
			log.V(1).Info("cutover waiting: no synced replica", "shard", shard.Id)
			continue
		}
		// A graceful failover takes a few seconds. Sending another one while it
		// runs restarts it, so give each replica time before asking again.
		if !r.failoverDue(replica.Id, time.Now()) {
			log.V(1).Info("cutover failover already in progress", "node", replica.Address)
			started++
			continue
		}
		if err := replica.Client.Do(ctx, replica.Client.B().ClusterFailover().Build()).Error(); err != nil {
			r.clearFailover(replica.Id)
			log.Error(err, "CLUSTER FAILOVER failed", "node", replica.Address)
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "BootstrapFailoverFailed", "Bootstrap", "Failover on %v failed: %v", replica.Address, err)
			continue
		}
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, valkeyiov1alpha1.ReasonBootstrapCutover, "Bootstrap", "Started graceful failover on %v", replica.Address)
		started++
	}
	return started
}

// bootstrapFailoverCooldown is how long a graceful failover gets before it is
// asked for again. Valkey aborts a manual failover after about five seconds.
const bootstrapFailoverCooldown = 15 * time.Second

// failoverDue reports whether a failover may be sent to the node now, and
// records it when so. The record is in memory: after an operator restart the
// failover is simply asked for once more.
func (r *ValkeyClusterReconciler) failoverDue(nodeID string, now time.Time) bool {
	r.failoverMu.Lock()
	defer r.failoverMu.Unlock()
	if last, ok := r.failoverIssued[nodeID]; ok && now.Sub(last) < bootstrapFailoverCooldown {
		return false
	}
	if r.failoverIssued == nil {
		r.failoverIssued = map[string]time.Time{}
	}
	r.failoverIssued[nodeID] = now
	return true
}

// clearFailover forgets a failover that could not be sent, so it is retried
// straight away.
func (r *ValkeyClusterReconciler) clearFailover(nodeID string) {
	r.failoverMu.Lock()
	defer r.failoverMu.Unlock()
	delete(r.failoverIssued, nodeID)
}

// syncedNodes returns the shard's nodes whose replication link is up.
func syncedNodes(shard *valkey.ShardState) []*valkey.NodeState {
	var out []*valkey.NodeState
	for _, n := range shard.Nodes {
		if n.Info["master_link_status"] == "up" && n.Info["master_sync_in_progress"] != "1" {
			out = append(out, n)
		}
	}
	return out
}

// reconcileBootstrap runs the bootstrap phases that follow MEET and returns the
// result for this reconcile. It never ends a pass without reporting status.
func (r *ValkeyClusterReconciler) reconcileBootstrap(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState, nodes *valkeyiov1alpha1.ValkeyNodeList, plan *bootstrapPlan) ctrl.Result {
	setBootstrapping := func(msg string) {
		setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonBootstrapping, msg, metav1.ConditionFalse)
		setCondition(cluster, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonBootstrapping, msg, metav1.ConditionTrue)
	}

	r.restoreSourceReplicationAuth(ctx, state, plan)
	replicated, err := r.replicateToSource(ctx, cluster, state, nodes, plan)
	if err != nil {
		setCondition(cluster, valkeyiov1alpha1.ConditionDegraded, valkeyiov1alpha1.ReasonNodeAddFailed, err.Error(), metav1.ConditionTrue)
		return ctrl.Result{RequeueAfter: 2 * time.Second}
	}
	if replicated > 0 {
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "ReplicasAttached", "Bootstrap", "Attached %d node(s) to the source cluster", replicated)
		setBootstrapping("Attaching nodes to the source cluster")
		return ctrl.Result{RequeueAfter: 2 * time.Second}
	}
	meta.RemoveStatusCondition(&cluster.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)

	synced, total := bootstrapSyncProgress(state, cluster, plan)
	if synced == total {
		setCondition(cluster, valkeyiov1alpha1.ConditionBootstrapSynced, valkeyiov1alpha1.ReasonBootstrapSynced,
			"All shards are in sync with the source; set spec.bootstrap.stage to Cutover to cut over", metav1.ConditionTrue)
	} else {
		setCondition(cluster, valkeyiov1alpha1.ConditionBootstrapSynced, valkeyiov1alpha1.ReasonBootstrapSyncing,
			fmt.Sprintf("%d of %d shards in sync with the source", synced, total), metav1.ConditionFalse)
	}

	// The only trigger for cutover is the field.
	if bootstrapCutoverRequested(cluster) {
		if started := r.bootstrapCutover(ctx, cluster, state); started > 0 {
			setBootstrapping("Cutting over to the new cluster")
			return ctrl.Result{RequeueAfter: 2 * time.Second}
		}
		setBootstrapping("Cutover requested; waiting for every shard to be in sync")
		return ctrl.Result{RequeueAfter: 2 * time.Second}
	}
	setBootstrapping(fmt.Sprintf("Replicating from the source (%d of %d shards in sync)", synced, total))
	return ctrl.Result{RequeueAfter: 5 * time.Second}
}

const noFailoverParam = "cluster-replica-no-failover"

// nodeConfig returns the user config each ValkeyNode renders. A cluster that is
// being bootstrapped starts every pod with cluster-replica-no-failover on, so a
// restart can never elect a new node primary before cutover. The setting
// is dropped at stage Complete; between Bootstrap and Complete the operator
// switches it off live once the slots have moved, so the key is stable across
// Bootstrap and Cutover and no pod rolls at cutover.
func nodeConfig(cluster *valkeyiov1alpha1.ValkeyCluster) map[string]string {
	b := cluster.Spec.Bootstrap
	if b == nil || b.Stage == valkeyiov1alpha1.BootstrapStageComplete {
		return cluster.Spec.Config
	}
	config := make(map[string]string, len(cluster.Spec.Config)+1)
	maps.Copy(config, cluster.Spec.Config)
	config[noFailoverParam] = "yes"
	return config
}

// destinationBarred reports whether a new node must not be elected primary.
// Before cutover every new node is barred. After it, a shard that owns its
// slots is free to fail over normally, shard by shard, so a half-finished
// cutover never leaves a moved shard without a way to recover.
func destinationBarred(migrated, shardOwnsSlots bool) bool {
	return !migrated && !shardOwnsSlots
}

// ensureDestinationFailoverPolicy keeps this cluster's own nodes from being
// elected primary until their shard has been cut over, then gives them normal
// failover back. The rendered config already starts new pods barred, so a
// restart never opens a window; this applies the change live and corrects it
// after a restart. It reads the node's nofailover flag and only issues
// CONFIG SET when the flag differs.
func (r *ValkeyClusterReconciler) ensureDestinationFailoverPolicy(ctx context.Context, state *valkey.ClusterState, migrated bool) {
	for _, shard := range state.Shards {
		barred := destinationBarred(migrated, len(shard.Slots) > 0)
		for _, node := range shard.Nodes {
			setNoFailover(ctx, node, barred)
		}
	}
	for _, node := range state.PendingNodes {
		setNoFailover(ctx, node, destinationBarred(migrated, false))
	}
}

func setNoFailover(ctx context.Context, node *valkey.NodeState, barred bool) {
	me := node.Myself()
	if me == nil || me.IsAutomaticFailoverDisabled() == barred {
		return
	}
	want := "no"
	if barred {
		want = "yes"
	}
	cmd := node.Client.B().ConfigSet().ParameterValue().ParameterValue(noFailoverParam, want).Build()
	if err := node.Client.Do(ctx, cmd).Error(); err != nil {
		logf.FromContext(ctx).Error(err, "CONFIG SET "+noFailoverParam, "node", node.Address, "value", want)
	}
}

// sourceFollowers returns the source nodes that now follow one of our own
// primaries: the replicas of a shard that has been cut over. The nodes of a
// shard that has not moved yet are still the source's own, and serve traffic
// with their own failover, so they are not included.
func sourceFollowers(state *valkey.ClusterState, members []valkey.ClusterNode) []valkey.ClusterNode {
	cutOver := map[string]struct{}{}
	for _, shard := range state.Shards {
		if len(shard.Slots) > 0 && shard.PrimaryId != "" {
			cutOver[shard.PrimaryId] = struct{}{}
		}
	}
	var out []valkey.ClusterNode
	for _, m := range members {
		if _, ok := cutOver[m.PrimaryId]; !ok || m.IsPrimary() || m.IsFailing() || m.HasNoAddress() {
			continue
		}
		out = append(out, m)
	}
	return out
}

// protectSourceNodes keeps the source nodes that follow one of our primaries
// from being elected primary, and keeps them in sync, until the user sets the
// stage to Complete. It works shard by shard, as each shard is cut over, so a
// half-finished cutover leaves no moved shard open to a source replica. On
// each such replica it sets, with live CONFIG SET:
//
//   - masteruser and masterauth to this cluster's replication credentials. The
//     source replicas now follow a new primary, which does not know the source
//     password; without this their link would stay down and rollback would
//     have nothing to roll back to.
//   - cluster-replica-no-failover, last.
//
// Nothing is persisted (the operator never runs CONFIG REWRITE), so a restarted
// source node loses these and is set up again on the next pass. A replica that
// already has the flag is still checked: when its link to the new primary is
// down, the credentials are written again, which also covers a pass where the
// replication password could not be read.
func (r *ValkeyClusterReconciler) protectSourceNodes(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState, members []valkey.ClusterNode) {
	log := logf.FromContext(ctx)
	followers := sourceFollowers(state, members)
	if len(followers) == 0 {
		return
	}
	cfg, err := r.sourceConn(ctx, cluster)
	if err != nil {
		log.Error(err, "cannot protect source nodes")
		return
	}
	// Failing to read the replication password must not leave the source
	// electable, so it only skips the credentials.
	replPassword, err := fetchSystemUserPassword(ctx, replicationUser, r.Client, cluster.Name, cluster.Namespace)
	if err != nil {
		log.Error(err, "cannot read the replication password; protecting source nodes without it")
		replPassword = ""
	}
	port := sourcePort(cluster)
	for _, m := range followers {
		address := net.JoinHostPort(m.Host, strconv.Itoa(port))
		if err := protectSourceNode(ctx, address, cfg, replPassword, m.IsAutomaticFailoverDisabled()); err != nil {
			log.Error(err, "protect source node", "node", m.Host)
		}
	}
}

// sourcePort is the port source nodes listen on, taken from the first
// endpoint. CLUSTER NODES entries carry no port, so source nodes are assumed to
// share it.
func sourcePort(cluster *valkeyiov1alpha1.ValkeyCluster) int {
	for _, endpoint := range cluster.Spec.Bootstrap.Source.Endpoints {
		if _, p, err := net.SplitHostPort(endpoint); err == nil {
			if port, err := strconv.Atoi(p); err == nil {
				return port
			}
		}
	}
	return DefaultPort
}

// protectSourceNode applies the settings to one source replica. A replica that
// already carries the flag is left alone while its link to the new primary is
// up; when the link is down the credentials are written again.
func protectSourceNode(ctx context.Context, address string, cfg connConfig, replPassword string, flagged bool) error {
	c, err := vclient.NewClient(clientOption(address, cfg))
	if err != nil {
		return err
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if flagged {
		if replPassword == "" {
			return nil
		}
		info, err := c.Do(ctx, c.B().Info().Section("replication").Build()).ToString()
		if err == nil && strings.Contains(info, "master_link_status:up") {
			return nil
		}
	}
	settings := [][2]string{}
	if replPassword != "" {
		settings = append(settings, [2]string{"masteruser", replicationUser}, [2]string{"masterauth", replPassword})
	}
	settings = append(settings, [2]string{noFailoverParam, "yes"})
	for _, kv := range settings {
		if err := c.Do(ctx, c.B().ConfigSet().ParameterValue().ParameterValue(kv[0], kv[1]).Build()).Error(); err != nil {
			return fmt.Errorf("CONFIG SET %s: %w", kv[0], err)
		}
	}
	return nil
}

// bootstrapSyncProgress counts the shards whose every node replicates its source
// primary over an established link.
func bootstrapSyncProgress(state *valkey.ClusterState, cluster *valkeyiov1alpha1.ValkeyCluster, plan *bootstrapPlan) (synced, total int) {
	want := 1 + int(cluster.GetReplicasPerShard())
	for _, src := range plan.shards {
		total++
		count, up := 0, 0
		for _, shard := range state.Shards {
			for _, n := range shard.Nodes {
				if n.PrimaryIdFromSelf() != src.PrimaryId {
					continue
				}
				count++
				if n.Info["master_link_status"] == "up" && n.Info["master_sync_in_progress"] != "1" {
					up++
				}
			}
		}
		if count == want && up == want {
			synced++
		}
	}
	return synced, total
}
