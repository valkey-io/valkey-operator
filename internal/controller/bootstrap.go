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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
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

// finishBootstrapConditions replaces the in-progress bootstrap conditions once
// the new nodes own the slots.
func finishBootstrapConditions(cluster *valkeyiov1alpha1.ValkeyCluster) {
	meta.RemoveStatusCondition(&cluster.Status.Conditions, valkeyiov1alpha1.ConditionBootstrapFailed)
	if meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionBootstrapSynced) != nil {
		setCondition(cluster, valkeyiov1alpha1.ConditionBootstrapSynced, valkeyiov1alpha1.ReasonBootstrapComplete, "Cutover complete", metav1.ConditionTrue)
	}
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

// planBootstrap reads the source topology through the first reachable endpoint
// and validates it. It only reads from the source.
func (r *ValkeyClusterReconciler) planBootstrap(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) (*bootstrapPlan, error) {
	src := cluster.Spec.Bootstrap.Source
	plan := &bootstrapPlan{}
	if src.PasswordSecretRef != nil {
		pw, err := r.sourceSecret(ctx, cluster.Namespace, src.PasswordSecretRef)
		if err != nil {
			return nil, fmt.Errorf("bootstrap source password: %w", err)
		}
		plan.password = string(pw)
	}
	cfg := connConfig{password: plan.password}
	if src.TLS != nil {
		cfg.tls = &tls.Config{MinVersion: tls.VersionTLS12}
		if src.TLS.CASecretRef != nil {
			ca, err := r.sourceSecret(ctx, cluster.Namespace, src.TLS.CASecretRef)
			if err != nil {
				return nil, fmt.Errorf("bootstrap source CA: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(ca) {
				return nil, errors.New("bootstrap source CA: no PEM certificates found")
			}
			cfg.tls.RootCAs = pool
		}
	}

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
		if err := setReplicationAuth(ctx, node, "", plan.password); err != nil {
			return replicated, fmt.Errorf("set source replication auth on %s: %w", node.Address, err)
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
// spec.bootstrap.complete is true; nothing else in the operator promotes a
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
		if err := replica.Client.Do(ctx, replica.Client.B().ClusterFailover().Build()).Error(); err != nil {
			log.Error(err, "CLUSTER FAILOVER failed", "node", replica.Address)
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "BootstrapFailoverFailed", "Bootstrap", "Failover on %v failed: %v", replica.Address, err)
			continue
		}
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, valkeyiov1alpha1.ReasonBootstrapCutover, "Bootstrap", "Started graceful failover on %v", replica.Address)
		started++
	}
	return started
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
			"All shards are in sync with the source; set spec.bootstrap.complete to cut over", metav1.ConditionTrue)
	} else {
		setCondition(cluster, valkeyiov1alpha1.ConditionBootstrapSynced, valkeyiov1alpha1.ReasonBootstrapSyncing,
			fmt.Sprintf("%d of %d shards in sync with the source", synced, total), metav1.ConditionFalse)
	}

	// The only trigger for cutover is the field.
	if cluster.Spec.Bootstrap.Complete {
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
