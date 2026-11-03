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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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

// Hibernation stops every pod of a ValkeyCluster and keeps everything else.
// The descent runs in this order, and each step is read back from the cluster
// and its ValkeyNodes on every pass, so an operator restart resumes it:
//
//  1. Wait until the cluster is healthy with nothing in flight.
//  2. Pause writes on every primary (CLIENT PAUSE WRITE) and wait until every
//     replica has reached its primary's replication offset.
//  3. Have every node save (BGSAVE, or BGREWRITEAOF with the AOF on) and wait
//     until each one reports its whole dataset on disk.
//  4. Record each primary's data point on its ValkeyNode, then mark the
//     replica ValkeyNodes; their workloads scale to zero.
//  5. Once the replica pods are gone, check that each primary still holds the
//     recorded data point, then mark the primary ValkeyNodes.
//  6. Once every pod is gone, the cluster is Hibernated.
//
// Writes stay paused on the primaries from step 2 until they stop, so every
// node of a shard goes down holding the same data, and the cluster can wake
// with all of its pods at once. Without persistence steps 2, 3 and the checks
// of 5 are skipped: the data is lost either way.

const (
	// hibernationPauseTimeout bounds one CLIENT PAUSE WRITE. Every pass of the
	// descent issues it again, so it only runs out when the operator stops
	// reconciling the cluster for this long. A write that gets in then moves
	// the primary off its recorded data point, and the descent starts over
	// rather than stop the primary.
	hibernationPauseTimeout = 2 * time.Minute
	// hibernationStepRequeue is how soon a descent that is under way is
	// looked at again.
	hibernationStepRequeue = 2 * time.Second
	// hibernatedRequeue is how often a hibernated cluster or node is looked at.
	hibernatedRequeue = 30 * time.Second
	// hibernationSaveAttempts is how many saves one node gets before the
	// descent gives up with SaveFailed.
	hibernationSaveAttempts = 3
	// hibernationRetryAfter is how long after SaveFailed a descent starts again.
	hibernationRetryAfter = time.Minute
	// wakeReplicaGrace is how long the replicas of a waking cluster wait for
	// their primary before they start anyway.
	wakeReplicaGrace = 5 * time.Minute
)

// hibernateValue is the only value of AnnotationHibernate that hibernates.
const hibernateValue = "true"

// hibernationRequested reports whether the cluster carries the hibernate
// annotation with the exact value "true".
func hibernationRequested(cluster *valkeyiov1alpha1.ValkeyCluster) bool {
	return cluster.GetAnnotations()[valkeyiov1alpha1.AnnotationHibernate] == hibernateValue
}

// nodeHibernated reports whether a ValkeyNode's workload is held at zero
// replicas. Only the ValkeyCluster controller hibernates a node, so the
// annotation counts only on a node a ValkeyCluster controls.
func nodeHibernated(node *valkeyiov1alpha1.ValkeyNode) bool {
	return isClusterOwned(node) && node.GetAnnotations()[valkeyiov1alpha1.AnnotationHibernate] == hibernateValue
}

// hibernationTracker holds what a descent cannot read back from the cluster:
// the saves this operator process asked for, and when a descent last failed.
// Losing it in a restart costs one more save per node, or an earlier retry.
type hibernationTracker struct {
	mu       sync.Mutex
	clusters map[types.UID]*hibernationProgress
}

type hibernationProgress struct {
	// saves is keyed by pod IP, so a restarted pod starts over.
	saves    map[string]*saveRequest
	failedAt time.Time
}

type saveRequest struct {
	count int
	// accepted is set once a save request returned no error; a node counts
	// as saved only after one did.
	accepted bool
	lastErr  string
}

func (t *hibernationTracker) progress(uid types.UID) *hibernationProgress {
	if t.clusters == nil {
		t.clusters = map[types.UID]*hibernationProgress{}
	}
	p, ok := t.clusters[uid]
	if !ok {
		p = &hibernationProgress{saves: map[string]*saveRequest{}}
		t.clusters[uid] = p
	}
	return p
}

func (t *hibernationTracker) save(uid types.UID, address string) saveRequest {
	t.mu.Lock()
	defer t.mu.Unlock()
	if req := t.progress(uid).saves[address]; req != nil {
		return *req
	}
	return saveRequest{}
}

func (t *hibernationTracker) recordSave(uid types.UID, address string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.progress(uid)
	req := p.saves[address]
	if req == nil {
		req = &saveRequest{}
		p.saves[address] = req
	}
	req.count++
	if err != nil {
		req.lastErr = err.Error()
	} else {
		req.accepted = true
	}
}

func (t *hibernationTracker) markFailed(uid types.UID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.progress(uid)
	p.saves = map[string]*saveRequest{}
	p.failedAt = time.Now()
}

func (t *hibernationTracker) failedAt(uid types.UID) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	if p, ok := t.clusters[uid]; ok {
		return p.failedAt
	}
	return time.Time{}
}

func (t *hibernationTracker) clearSaves(uid types.UID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if p, ok := t.clusters[uid]; ok {
		p.saves = map[string]*saveRequest{}
	}
}

func (t *hibernationTracker) forget(uid types.UID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.clusters, uid)
}

// hibernationMember is a ValkeyNode together with the scrape of its pod.
type hibernationMember struct {
	node  *valkeyiov1alpha1.ValkeyNode
	state *valkey.NodeState
}

type hibernationShard struct {
	primary  hibernationMember
	replicas []hibernationMember
}

func (s hibernationShard) members() []hibernationMember {
	return append([]hibernationMember{s.primary}, s.replicas...)
}

// hibernationTopology maps the live shards onto the ValkeyNodes. It returns
// why not, and no shards, unless every ValkeyNode answers as a member of a
// shard with a primary and every member has a ValkeyNode.
func hibernationTopology(owned []*valkeyiov1alpha1.ValkeyNode, state *valkey.ClusterState) ([]hibernationShard, string) {
	byIP := map[string]*valkeyiov1alpha1.ValkeyNode{}
	for _, node := range owned {
		if node.Status.PodIP == "" {
			return nil, fmt.Sprintf("ValkeyNode %s has no pod IP", node.Name)
		}
		if !state.HasAddress(node.Status.PodIP) {
			return nil, fmt.Sprintf("ValkeyNode %s does not answer", node.Name)
		}
		byIP[node.Status.PodIP] = node
	}
	if len(state.PendingNodes) > 0 {
		return nil, fmt.Sprintf("%d node(s) have not joined a shard", len(state.PendingNodes))
	}
	shards := make([]hibernationShard, 0, len(state.Shards))
	members := 0
	for _, s := range state.Shards {
		if s.GetPrimaryNode() == nil {
			return nil, fmt.Sprintf("shard %s has no primary", s.Id)
		}
		var shard hibernationShard
		for _, ns := range s.Nodes {
			node, ok := byIP[ns.Address]
			if !ok {
				return nil, fmt.Sprintf("cluster member %s has no ValkeyNode", ns.Address)
			}
			member := hibernationMember{node: node, state: ns}
			if ns.Id == s.PrimaryId {
				shard.primary = member
			} else {
				shard.replicas = append(shard.replicas, member)
			}
			members++
		}
		shards = append(shards, shard)
	}
	if members != len(owned) {
		return nil, fmt.Sprintf("%d of %d ValkeyNodes are members of a shard", members, len(owned))
	}
	return shards, ""
}

// slotMigrationInFlight returns why not, unless no slot of the cluster is
// moving: no MIGRATING or IMPORTING marker in any member's own CLUSTER NODES
// line, and no atomic slot migration on any primary. A migration the operator
// did not start leaves the cluster Ready, so the gate alone does not see it.
func slotMigrationInFlight(ctx context.Context, shards []hibernationShard) string {
	for _, s := range shards {
		for _, m := range s.members() {
			if myself := m.state.Myself(); myself != nil && (len(myself.Migrating) > 0 || len(myself.Importing) > 0) {
				return fmt.Sprintf("slots of %s are being migrated", m.node.Name)
			}
		}
		if s.primary.state == nil || s.primary.state.Client == nil {
			continue
		}
		inProgress, err := valkey.SlotMigrationInProgress(ctx, s.primary.state)
		switch {
		case err != nil && !unsupportedCommand(err):
			return fmt.Sprintf("could not check slot migrations on %s: %v", s.primary.node.Name, err)
		case inProgress:
			return fmt.Sprintf("an atomic slot migration is running on %s", s.primary.node.Name)
		}
	}
	return ""
}

// unsupportedCommand reports whether err is a server's answer to a command it
// does not know, as from a server older than the command: it has none of what
// the command would report.
func unsupportedCommand(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unknown command") || strings.Contains(msg, "unknown subcommand")
}

// readHibernationInfo reads the INFO sections the descent decides on.
func readHibernationInfo(ctx context.Context, n *valkey.NodeState) (map[string]string, error) {
	if n == nil || n.Client == nil {
		return nil, errors.New("no client")
	}
	c := n.Client
	reply, err := c.Do(ctx, c.B().Info().Section("replication", "persistence", "clients").Build()).ToString()
	if err != nil {
		return nil, err
	}
	return valkey.ParseInfo(reply), nil
}

func pauseWrites(ctx context.Context, n *valkey.NodeState) error {
	if n == nil || n.Client == nil {
		return errors.New("no client")
	}
	c := n.Client
	return c.Do(ctx, c.B().ClientPause().Timeout(hibernationPauseTimeout.Milliseconds()).Write().Build()).Error()
}

func resumeWrites(ctx context.Context, n *valkey.NodeState) error {
	if n == nil || n.Client == nil {
		return errors.New("no client")
	}
	c := n.Client
	return c.Do(ctx, c.B().ClientUnpause().Build()).Error()
}

// requestSave starts a save. BGSAVE SCHEDULE queues behind a save already
// running instead of failing.
func requestSave(ctx context.Context, n *valkey.NodeState, aof bool) error {
	if n == nil || n.Client == nil {
		return errors.New("no client")
	}
	c := n.Client
	if aof {
		return c.Do(ctx, c.B().Bgrewriteaof().Build()).Error()
	}
	return c.Do(ctx, c.B().Bgsave().Schedule().Build()).Error()
}

// writesPaused reports whether INFO clients shows a write pause. A server
// that does not report pauses is taken at the word of its CLIENT PAUSE reply.
func writesPaused(info map[string]string) bool {
	actions, ok := info["paused_actions"]
	return !ok || actions == "all" || strings.Contains(actions, "write")
}

// dataPoint identifies the dataset a primary holds, as its replication ID,
// changes since the last save and number of saves. The replication offset is
// left out: a primary sends its replicas a PING every repl-ping-replica-period
// even while writes are paused, which moves the offset and not the data.
// With the AOF on, the RDB counters are left out too: the descent leaves
// changes unsaved to the RDB, so a save point can fire without any write and
// move both. The size of the AOF moves only with a write, or a rewrite, and
// the descent waits for its own rewrite to finish before it records the point.
func dataPoint(info map[string]string) string {
	if info["aof_enabled"] == "1" {
		return info["master_replid"] + ":aof:" + info["aof_current_size"]
	}
	return info["master_replid"] + ":" + info["rdb_changes_since_last_save"] + ":" + info["rdb_saves"]
}

// replicaSync compares a replica with its paused primary. pending is empty once
// the replica holds everything the primary does, and otherwise says why not.
// broken means the replica is not replicating from it at all.
func replicaSync(primary, replica map[string]string) (pending string, broken bool) {
	if replica["master_link_status"] != "up" {
		return "its replication link is down", true
	}
	if replica["master_replid"] != primary["master_replid"] {
		return "it follows another replication ID than its primary", true
	}
	want, err := strconv.ParseInt(primary["master_repl_offset"], 10, 64)
	if err != nil {
		return "its primary's replication offset is unknown", false
	}
	got, err := strconv.ParseInt(replica["slave_repl_offset"], 10, 64)
	if err != nil {
		return "its replication offset is unknown", false
	}
	if got < want {
		return fmt.Sprintf("it is at offset %d of %d", got, want), false
	}
	return "", false
}

type saveProgress int

const (
	// saveIdle: no save is running and the dataset is not known to be on disk.
	saveIdle saveProgress = iota
	saveRunning
	// saveDone: the last save succeeded and the dataset has not changed since.
	saveDone
)

// persistenceProgress reads INFO persistence. With the AOF on, a restarted
// node loads the AOF and ignores the RDB, so the AOF is what has to be
// complete: no rewrite pending, no failed write, nothing left to flush.
func persistenceProgress(info map[string]string) (saveProgress, string) {
	nonZero := func(v string) bool { return v != "" && v != "0" }
	if info["aof_enabled"] == "1" {
		switch {
		case info["aof_rewrite_in_progress"] == "1", info["aof_rewrite_scheduled"] == "1":
			return saveRunning, "an AOF rewrite is running"
		case info["aof_last_bgrewrite_status"] != "ok":
			return saveIdle, "the last AOF rewrite failed"
		case info["aof_last_write_status"] != "ok":
			return saveIdle, "the last AOF write failed"
		case nonZero(info["aof_buffer_length"]), nonZero(info["aof_pending_bio_fsync"]):
			return saveRunning, "the AOF is being flushed"
		}
		return saveDone, ""
	}
	switch {
	case info["rdb_bgsave_in_progress"] == "1":
		return saveRunning, "an RDB save is running"
	case info["rdb_last_bgsave_status"] != "ok":
		return saveIdle, "the last RDB save failed"
	case info["rdb_changes_since_last_save"] != "0":
		return saveIdle, fmt.Sprintf("%s change(s) since the last RDB save", info["rdb_changes_since_last_save"])
	}
	return saveDone, ""
}

// descentBlock is why a descent step cannot finish in this pass.
type descentBlock struct {
	message string
	// abort: the cluster is not as the descent needs it. Before any pod is
	// stopped, writes resume and the regular reconcile repairs the cluster;
	// after, the stopped pods are started again as well.
	abort bool
	// failed: a node could not save its data.
	failed bool
}

// saveMembers has every member save at least once in this descent. It returns
// how many have their data on disk, or why one cannot save.
func (r *ValkeyClusterReconciler) saveMembers(ctx context.Context, uid types.UID, members []hibernationMember, infos map[string]map[string]string) (int, string) {
	log := logf.FromContext(ctx)
	saved := 0
	for _, m := range members {
		info := infos[m.node.Name]
		progress, detail := persistenceProgress(info)
		req := r.hibernation.save(uid, m.state.Address)
		switch {
		case progress == saveRunning:
			continue
		case progress == saveDone && req.accepted:
			saved++
			continue
		case req.count >= hibernationSaveAttempts:
			msg := fmt.Sprintf("%s could not save its data after %d attempts: %s", m.node.Name, req.count, detail)
			if req.lastErr != "" {
				msg += " (" + req.lastErr + ")"
			}
			return saved, msg
		}
		aof := info["aof_enabled"] == "1"
		err := requestSave(ctx, m.state, aof)
		if err != nil {
			log.Error(err, "hibernation save request failed", "node", m.node.Name, "aof", aof)
		} else {
			log.V(1).Info("requested hibernation save", "node", m.node.Name, "aof", aof)
		}
		r.hibernation.recordSave(uid, m.state.Address, err)
	}
	return saved, ""
}

// prepareShards pauses writes on every primary, waits until every replica has
// reached its primary, and has every node save. It returns each primary's
// data point, keyed by ValkeyNode name, once all data is on disk.
func (r *ValkeyClusterReconciler) prepareShards(ctx context.Context, uid types.UID, shards []hibernationShard) (map[string]string, *descentBlock) {
	for _, s := range shards {
		// Back off rather than wait: the primaries paused before this one
		// would otherwise stay paused for as long as it keeps failing.
		if err := pauseWrites(ctx, s.primary.state); err != nil {
			return nil, &descentBlock{abort: true, message: fmt.Sprintf("pausing writes on %s failed: %v", s.primary.node.Name, err)}
		}
	}
	infos := map[string]map[string]string{}
	var members []hibernationMember
	for _, s := range shards {
		for _, m := range s.members() {
			info, err := readHibernationInfo(ctx, m.state)
			if err != nil {
				return nil, &descentBlock{abort: true, message: fmt.Sprintf("reading INFO from %s failed: %v", m.node.Name, err)}
			}
			infos[m.node.Name] = info
			members = append(members, m)
		}
	}
	for _, s := range shards {
		primary := infos[s.primary.node.Name]
		if !writesPaused(primary) {
			return nil, &descentBlock{message: fmt.Sprintf("Waiting for writes to pause on %s", s.primary.node.Name)}
		}
		for _, replica := range s.replicas {
			pending, broken := replicaSync(primary, infos[replica.node.Name])
			if broken {
				return nil, &descentBlock{abort: true, message: fmt.Sprintf("replica %s cannot catch up with %s: %s", replica.node.Name, s.primary.node.Name, pending)}
			}
			if pending != "" {
				return nil, &descentBlock{message: fmt.Sprintf("Writes are paused; waiting for replica %s to catch up with its primary, %s", replica.node.Name, pending)}
			}
		}
	}
	saved, failure := r.saveMembers(ctx, uid, members, infos)
	if failure != "" {
		return nil, &descentBlock{failed: true, message: failure}
	}
	if saved < len(members) {
		return nil, &descentBlock{message: fmt.Sprintf("Writes are paused; %d of %d nodes have saved their data", saved, len(members))}
	}
	points := map[string]string{}
	for _, s := range shards {
		points[s.primary.node.Name] = dataPoint(infos[s.primary.node.Name])
	}
	return points, nil
}

// checkPrimaries verifies, once their replicas are gone, that the primaries
// are paused and still hold the data point their replicas saved. The data
// point only stays put while nothing changes the dataset, so the save each
// primary made with its replicas is still current and is not repeated.
func (r *ValkeyClusterReconciler) checkPrimaries(ctx context.Context, primaries []*valkeyiov1alpha1.ValkeyNode, state *valkey.ClusterState) *descentBlock {
	// Every primary is checked before a wait is reported: the final step acts
	// only on an abort, so a primary that moved must not hide behind another
	// that has to be waited for, nor behind its own lapsed pause.
	var wait *descentBlock
	for _, node := range primaries {
		ns := nodeStateFor(state, node)
		if ns == nil {
			if wait == nil {
				wait = &descentBlock{message: fmt.Sprintf("Replicas are stopped; waiting for primary %s to answer", node.Name)}
			}
			continue
		}
		if !ns.IsPrimary() {
			return &descentBlock{abort: true, message: fmt.Sprintf("%s is no longer a primary", node.Name)}
		}
		info, err := readHibernationInfo(ctx, ns)
		if err != nil {
			if wait == nil {
				wait = &descentBlock{message: fmt.Sprintf("Reading INFO from %s: %v", node.Name, err)}
			}
			continue
		}
		recorded := node.Annotations[valkeyiov1alpha1.AnnotationHibernateDataPoint]
		if live := dataPoint(info); recorded != live {
			return &descentBlock{abort: true, message: fmt.Sprintf("primary %s moved from data point %q to %q after its replicas stopped", node.Name, recorded, live)}
		}
		if !writesPaused(info) && wait == nil {
			wait = &descentBlock{message: fmt.Sprintf("Waiting for writes to pause on %s", node.Name)}
		}
	}
	return wait
}

// nodeStateFor returns the scrape of the node's pod, or nil.
func nodeStateFor(state *valkey.ClusterState, node *valkeyiov1alpha1.ValkeyNode) *valkey.NodeState {
	if state == nil || node.Status.PodIP == "" {
		return nil
	}
	for _, ns := range state.AllNodes() {
		if ns.Address == node.Status.PodIP {
			return ns
		}
	}
	return nil
}

// ownedValkeyNodes returns the ValkeyNodes the cluster controls.
func ownedValkeyNodes(cluster *valkeyiov1alpha1.ValkeyCluster, nodes *valkeyiov1alpha1.ValkeyNodeList) []*valkeyiov1alpha1.ValkeyNode {
	owned := make([]*valkeyiov1alpha1.ValkeyNode, 0, len(nodes.Items))
	for i := range nodes.Items {
		if metav1.IsControlledBy(&nodes.Items[i], cluster) {
			owned = append(owned, &nodes.Items[i])
		}
	}
	return owned
}

// descentStarted reports whether the descent is past its start: from then on
// the regular reconcile does not run until the cluster wakes.
func descentStarted(cond *metav1.Condition) bool {
	if cond == nil {
		return false
	}
	switch cond.Reason {
	case valkeyiov1alpha1.ReasonHibernating, valkeyiov1alpha1.ReasonHibernated:
		return true
	}
	return false
}

// hibernationMayStart reports whether the regular reconcile last found the
// cluster healthy at its current generation: no scale, slot migration or roll
// in flight, and every replica in sync.
func hibernationMayStart(cluster *valkeyiov1alpha1.ValkeyCluster) bool {
	ready := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionReady)
	return ready != nil && ready.Status == metav1.ConditionTrue && ready.ObservedGeneration == cluster.Generation
}

// reconcileHibernation drives a cluster that carries the hibernate annotation.
// It returns handled=false while the descent has not started, so that the
// regular reconcile finishes in-flight work first.
func (r *ValkeyClusterReconciler) reconcileHibernation(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, nodes *valkeyiov1alpha1.ValkeyNodeList, state *valkey.ClusterState) (ctrl.Result, bool, error) {
	owned := ownedValkeyNodes(cluster, nodes)
	cond := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionHibernated)

	var stopping, running []*valkeyiov1alpha1.ValkeyNode
	for _, node := range owned {
		if node.Annotations[valkeyiov1alpha1.AnnotationHibernate] == hibernateValue {
			stopping = append(stopping, node)
		} else {
			running = append(running, node)
		}
	}
	if len(stopping) > 0 && cond != nil && cond.Reason == valkeyiov1alpha1.ReasonWaking {
		// A wake still holds replicas back, after hibernation was asked for
		// again or a descent was restarted: finish the wake first, so that no
		// replica starts ahead of its primary.
		held, err := r.wakeInStages(ctx, cluster, owned, state)
		if err != nil {
			return ctrl.Result{}, true, err
		}
		if held != "" {
			msg := held + "; hibernation starts again once the cluster is awake"
			setCondition(cluster, valkeyiov1alpha1.ConditionHibernated, valkeyiov1alpha1.ReasonWaking, msg, metav1.ConditionFalse)
			setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonWaking, msg, metav1.ConditionFalse)
			setCondition(cluster, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonWaking, msg, metav1.ConditionTrue)
			err = r.updateStatus(ctx, cluster, state)
			return ctrl.Result{RequeueAfter: hibernationStepRequeue}, true, err
		}
		stopping, running = nil, owned
	}
	if len(stopping) > 0 && !descentStarted(cond) {
		// Not marked by this descent, so not after a save: start from the top.
		if err := r.clearHibernationAnnotations(ctx, owned); err != nil {
			return ctrl.Result{}, true, err
		}
		stopping, running = nil, owned
	}
	switch {
	case len(owned) > 0 && len(running) == 0:
		return r.finishHibernation(ctx, cluster, owned, state)
	case len(stopping) > 0:
		return r.stopPrimaries(ctx, cluster, stopping, running, state)
	}

	if !descentStarted(cond) {
		if cond != nil && cond.Reason == valkeyiov1alpha1.ReasonHibernationSaveFailed &&
			time.Since(r.hibernation.failedAt(cluster.UID)) < hibernationRetryAfter {
			return ctrl.Result{}, false, nil
		}
		if len(owned) == 0 || !hibernationMayStart(cluster) {
			setCondition(cluster, valkeyiov1alpha1.ConditionHibernated, valkeyiov1alpha1.ReasonHibernationWaitingForHealthy,
				"Hibernation waits for the cluster to be healthy, with no scale, slot migration or roll in flight", metav1.ConditionFalse)
			return ctrl.Result{}, false, nil
		}
		logf.FromContext(ctx).Info("Hibernation started")
		if cluster.Spec.Persistence == nil {
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "HibernationDataLoss", "Hibernate",
				"The cluster has no persistence: its data is lost when its pods stop, and it wakes as a new, empty cluster")
		}
	}
	return r.stopReplicas(ctx, cluster, owned, state)
}

// stopReplicas runs the descent up to marking the replicas: with persistence,
// writes are paused, every replica catches up and every node saves.
func (r *ValkeyClusterReconciler) stopReplicas(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, owned []*valkeyiov1alpha1.ValkeyNode, state *valkey.ClusterState) (ctrl.Result, bool, error) {
	shards, problem := hibernationTopology(owned, state)
	if problem == "" {
		problem = slotMigrationInFlight(ctx, shards)
	}
	if problem != "" {
		return r.abortDescent(ctx, cluster, state, problem)
	}
	var primaries, replicas []*valkeyiov1alpha1.ValkeyNode
	for _, s := range shards {
		primaries = append(primaries, s.primary.node)
		for _, m := range s.replicas {
			replicas = append(replicas, m.node)
		}
	}

	if cluster.Spec.Persistence != nil {
		points, block := r.prepareShards(ctx, cluster.UID, shards)
		if block != nil {
			switch {
			case block.abort:
				return r.abortDescent(ctx, cluster, state, block.message)
			case block.failed:
				return r.failSave(ctx, cluster, state, block.message)
			}
			return r.descentStep(ctx, cluster, state, block.message)
		}
		for _, node := range primaries {
			if err := r.patchNodeAnnotations(ctx, node, map[string]string{
				valkeyiov1alpha1.AnnotationHibernateDataPoint: points[node.Name],
			}, nil); err != nil {
				return ctrl.Result{}, true, err
			}
		}
		// A data point left by an earlier descent that backed off would make a
		// node that has since become a replica wake as a primary.
		for _, node := range replicas {
			if err := r.patchNodeAnnotations(ctx, node, nil, []string{valkeyiov1alpha1.AnnotationHibernateDataPoint}); err != nil {
				return ctrl.Result{}, true, err
			}
		}
	}

	if len(replicas) == 0 {
		return r.beginStopping(ctx, cluster, primaries, state, fmt.Sprintf("Stopping %d primary pod(s)", len(primaries)))
	}
	msg := fmt.Sprintf("Stopping %d replica pod(s)", len(replicas))
	if cluster.Spec.Persistence != nil {
		msg += "; writes stay paused on the primaries"
	}
	return r.beginStopping(ctx, cluster, replicas, state, msg)
}

// stopPrimaries waits for the replica pods to stop, checks that no write
// reached the primaries since their replicas saved, and marks the primaries.
func (r *ValkeyClusterReconciler) stopPrimaries(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, stopping, running []*valkeyiov1alpha1.ValkeyNode, state *valkey.ClusterState) (ctrl.Result, bool, error) {
	persistent := cluster.Spec.Persistence != nil
	if persistent {
		// Keep the pause from running out while the replicas stop.
		r.renewWritePause(ctx, running, state)
	}
	// A pass that marked only some replicas, cut short by an operator restart
	// or an API error, left the others running. They saved with the rest, and
	// only primaries carry a data point, so a running replica without one is
	// marked rather than checked as a primary.
	var unmarked []*valkeyiov1alpha1.ValkeyNode
	for _, node := range running {
		_, recorded := node.Annotations[valkeyiov1alpha1.AnnotationHibernateDataPoint]
		if ns := nodeStateFor(state, node); ns != nil && !ns.IsPrimary() && !recorded {
			unmarked = append(unmarked, node)
		}
	}
	if len(unmarked) > 0 {
		return r.beginStopping(ctx, cluster, unmarked, state, fmt.Sprintf("Stopping %d replica pod(s)", len(unmarked)))
	}
	left, err := r.nodesWithPods(ctx, stopping)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	if len(left) > 0 {
		return r.descentStep(ctx, cluster, state,
			fmt.Sprintf("Waiting for %d replica pod(s) to stop: %s", len(left), strings.Join(left, ", ")))
	}
	if persistent {
		if block := r.checkPrimaries(ctx, running, state); block != nil {
			if block.abort {
				return r.restartDescent(ctx, cluster, state, block.message)
			}
			return r.descentStep(ctx, cluster, state, block.message)
		}
	}
	return r.beginStopping(ctx, cluster, running, state, fmt.Sprintf("Stopping %d primary pod(s)", len(running)))
}

// renewWritePause issues CLIENT PAUSE WRITE again on the nodes that answer.
func (r *ValkeyClusterReconciler) renewWritePause(ctx context.Context, nodes []*valkeyiov1alpha1.ValkeyNode, state *valkey.ClusterState) {
	for _, node := range nodes {
		if ns := nodeStateFor(state, node); ns != nil {
			if err := pauseWrites(ctx, ns); err != nil {
				logf.FromContext(ctx).V(1).Info("could not renew the write pause", "node", node.Name, "error", err.Error())
			}
		}
	}
}

// finishHibernation waits for the last pods to stop and reports the cluster
// Hibernated.
func (r *ValkeyClusterReconciler) finishHibernation(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, owned []*valkeyiov1alpha1.ValkeyNode, state *valkey.ClusterState) (ctrl.Result, bool, error) {
	left, err := r.nodesWithPods(ctx, owned)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	if len(left) > 0 {
		if cluster.Spec.Persistence != nil {
			// A primary still shutting down saves once more on its way out:
			// keep it paused, and make sure nothing reached it meanwhile, for
			// example while the operator was down for longer than a pause.
			r.renewWritePause(ctx, owned, state)
			var serving []*valkeyiov1alpha1.ValkeyNode
			for _, node := range owned {
				if _, ok := node.Annotations[valkeyiov1alpha1.AnnotationHibernateDataPoint]; ok && nodeStateFor(state, node) != nil {
					serving = append(serving, node)
				}
			}
			if block := r.checkPrimaries(ctx, serving, state); block != nil && block.abort {
				return r.restartDescent(ctx, cluster, state, block.message)
			}
		}
		return r.descentStep(ctx, cluster, state,
			fmt.Sprintf("Waiting for %d pod(s) to stop: %s", len(left), strings.Join(left, ", ")))
	}
	if !meta.IsStatusConditionTrue(cluster.Status.Conditions, valkeyiov1alpha1.ConditionHibernated) {
		logf.FromContext(ctx).Info("Cluster hibernated")
	}
	r.hibernation.forget(cluster.UID)
	kept := "the cluster's objects and volumes are kept"
	if cluster.Spec.Persistence == nil {
		kept = "the cluster's objects are kept; it has no persistence and wakes empty"
	}
	setCondition(cluster, valkeyiov1alpha1.ConditionHibernated, valkeyiov1alpha1.ReasonHibernated,
		"Every pod is stopped; "+kept+". Remove the "+valkeyiov1alpha1.AnnotationHibernate+" annotation to wake it", metav1.ConditionTrue)
	setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonHibernated, "The cluster is hibernated", metav1.ConditionFalse)
	setCondition(cluster, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonHibernated, "The cluster is hibernated", metav1.ConditionFalse)
	err = r.updateStatus(ctx, cluster, state)
	return ctrl.Result{RequeueAfter: hibernatedRequeue}, true, err
}

// beginStopping reports the step, then marks the nodes so their workloads
// scale to zero. The status goes first: a node is never marked by a descent
// the status does not show as started.
func (r *ValkeyClusterReconciler) beginStopping(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, nodes []*valkeyiov1alpha1.ValkeyNode, state *valkey.ClusterState, msg string) (ctrl.Result, bool, error) {
	// A pass can run long, a scrape waiting on pods that are gone, and stopping
	// pods is the step that is not undone at once. Read the annotation again
	// from the API server so that a wake requested meanwhile stops nothing.
	if r.APIReader != nil {
		fresh := &valkeyiov1alpha1.ValkeyCluster{}
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(cluster), fresh); err != nil {
			return ctrl.Result{}, true, err
		}
		if !hibernationRequested(fresh) {
			logf.FromContext(ctx).Info("Hibernation was cancelled during this pass; stopping nothing", "step", msg)
			return ctrl.Result{RequeueAfter: time.Millisecond}, true, nil
		}
	}
	result, handled, err := r.descentStep(ctx, cluster, state, msg)
	if err != nil {
		return result, handled, err
	}
	for _, node := range nodes {
		if err := r.patchNodeAnnotations(ctx, node, map[string]string{valkeyiov1alpha1.AnnotationHibernate: hibernateValue}, nil); err != nil {
			return ctrl.Result{}, true, err
		}
	}
	logf.FromContext(ctx).Info("Stopping pods for hibernation", "step", msg, "nodes", len(nodes))
	return result, handled, nil
}

// descentStep reports a descent that is under way and looks again shortly.
func (r *ValkeyClusterReconciler) descentStep(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState, msg string) (ctrl.Result, bool, error) {
	setCondition(cluster, valkeyiov1alpha1.ConditionHibernated, valkeyiov1alpha1.ReasonHibernating, msg, metav1.ConditionFalse)
	setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonHibernating, msg, metav1.ConditionFalse)
	setCondition(cluster, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonHibernating, msg, metav1.ConditionTrue)
	err := r.updateStatus(ctx, cluster, state)
	return ctrl.Result{RequeueAfter: hibernationStepRequeue}, true, err
}

// abortDescent backs out of a descent that has not stopped a pod: writes
// resume, and the regular reconcile runs to repair the cluster before the
// descent starts again.
func (r *ValkeyClusterReconciler) abortDescent(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState, why string) (ctrl.Result, bool, error) {
	// The pause may be from this very pass, before the condition showed it.
	if err := r.resumeAllWrites(ctx, state); err != nil {
		return retryResume(ctx, err)
	}
	if descentStarted(meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionHibernated)) {
		logf.FromContext(ctx).Info("Hibernation backed off before stopping any pod", "why", why)
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "HibernationRestarted", "Hibernate",
			"Writes are resumed and hibernation waits: %s", why)
	}
	r.hibernation.clearSaves(cluster.UID)
	setCondition(cluster, valkeyiov1alpha1.ConditionHibernated, valkeyiov1alpha1.ReasonHibernationWaitingForHealthy,
		"Hibernation waits for the cluster to be whole: "+why, metav1.ConditionFalse)
	return ctrl.Result{}, false, nil
}

// failSave gives up a descent whose saves failed before any pod stopped:
// writes resume, every pod keeps running, and the descent is retried later.
func (r *ValkeyClusterReconciler) failSave(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState, why string) (ctrl.Result, bool, error) {
	if err := r.resumeAllWrites(ctx, state); err != nil {
		return retryResume(ctx, err)
	}
	r.hibernation.markFailed(cluster.UID)
	logf.FromContext(ctx).Info("Hibernation save failed", "why", why)
	r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, valkeyiov1alpha1.ReasonHibernationSaveFailed, "Hibernate",
		"%s; writes are resumed and no pod was stopped", why)
	setCondition(cluster, valkeyiov1alpha1.ConditionHibernated, valkeyiov1alpha1.ReasonHibernationSaveFailed,
		fmt.Sprintf("%s; writes are resumed, no pod was stopped, and hibernation is retried in %s", why, hibernationRetryAfter), metav1.ConditionFalse)
	return ctrl.Result{}, false, nil
}

// restartDescent backs out of a descent whose replicas have stopped, when the
// primaries cannot follow them down holding the same data: every pod starts
// again and the descent starts over once the cluster is healthy.
//
// Some primaries may have stopped already and come back at new addresses, so
// the pods start the way a wake starts them, primaries first: the marks and
// the data points stay, and the Waking condition hands them to wakeInStages,
// which reconcileHibernation and reconcileWake both run before anything else.
func (r *ValkeyClusterReconciler) restartDescent(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState, why string) (ctrl.Result, bool, error) {
	if err := r.resumeAllWrites(ctx, state); err != nil {
		return retryResume(ctx, err)
	}
	msg := why + "; the pods are started again, primaries first, and hibernation starts over once the cluster is healthy"
	r.hibernation.clearSaves(cluster.UID)
	logf.FromContext(ctx).Info("Hibernation restarted", "why", why)
	r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "HibernationRestarted", "Hibernate", "%s", msg)
	// Removed first so that the transition time marks the start of the wake.
	meta.RemoveStatusCondition(&cluster.Status.Conditions, valkeyiov1alpha1.ConditionHibernated)
	setCondition(cluster, valkeyiov1alpha1.ConditionHibernated, valkeyiov1alpha1.ReasonWaking, msg, metav1.ConditionFalse)
	setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonWaking, msg, metav1.ConditionFalse)
	setCondition(cluster, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonWaking, msg, metav1.ConditionTrue)
	err := r.updateStatus(ctx, cluster, state)
	return ctrl.Result{RequeueAfter: hibernationStepRequeue}, true, err
}

// reconcileWake undoes hibernation once the annotation is gone or no longer
// "true": writes resume and the ValkeyNodes are unmarked, primaries first (see
// wakeInStages). It returns handled=true while replicas wait for their
// primaries; then the regular reconcile brings the cluster back, and the
// Hibernated condition is cleared once the cluster is healthy again.
func (r *ValkeyClusterReconciler) reconcileWake(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, nodes *valkeyiov1alpha1.ValkeyNodeList, state *valkey.ClusterState) (ctrl.Result, bool, error) {
	owned := ownedValkeyNodes(cluster, nodes)
	cond := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionHibernated)
	marked := false
	for _, node := range owned {
		_, hibernate := node.Annotations[valkeyiov1alpha1.AnnotationHibernate]
		_, point := node.Annotations[valkeyiov1alpha1.AnnotationHibernateDataPoint]
		marked = marked || hibernate || point
	}
	if cond == nil && !marked {
		return ctrl.Result{}, false, nil
	}
	if descentStarted(cond) && cond.Reason != valkeyiov1alpha1.ReasonHibernated {
		if err := r.resumeAllWrites(ctx, state); err != nil {
			return retryResume(ctx, err)
		}
	}
	r.hibernation.forget(cluster.UID)
	if cond != nil {
		switch cond.Reason {
		case valkeyiov1alpha1.ReasonWaking:
		case valkeyiov1alpha1.ReasonHibernationWaitingForHealthy, valkeyiov1alpha1.ReasonHibernationSaveFailed:
			meta.RemoveStatusCondition(&cluster.Status.Conditions, valkeyiov1alpha1.ConditionHibernated)
			logf.FromContext(ctx).Info("Hibernation cancelled before any pod stopped")
		default:
			logf.FromContext(ctx).Info("Waking cluster", "from", cond.Reason)
			// Removed first so that the transition time marks the start of the wake.
			meta.RemoveStatusCondition(&cluster.Status.Conditions, valkeyiov1alpha1.ConditionHibernated)
			setCondition(cluster, valkeyiov1alpha1.ConditionHibernated, valkeyiov1alpha1.ReasonWaking,
				"Starting the pods of the cluster, primaries first", metav1.ConditionFalse)
		}
	}
	held, err := r.wakeInStages(ctx, cluster, owned, state)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	if held == "" {
		return ctrl.Result{}, false, nil
	}
	setCondition(cluster, valkeyiov1alpha1.ConditionHibernated, valkeyiov1alpha1.ReasonWaking, held, metav1.ConditionFalse)
	setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonWaking, held, metav1.ConditionFalse)
	setCondition(cluster, valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonWaking, held, metav1.ConditionTrue)
	err = r.updateStatus(ctx, cluster, state)
	return ctrl.Result{RequeueAfter: hibernationStepRequeue}, true, err
}

// wakeInStages unmarks the ValkeyNodes so their pods start: every primary at
// once, and the replicas of a shard once its primary serves again. Every pod
// comes back with a new IP while each node's nodes.conf still lists the old
// ones, so for a while the members cannot reach each other and the primaries
// look failed to their peers. A replica started then can win an election
// against a primary that is up and already acknowledging writes, and the
// primary, demoted, drops those writes when it syncs from the replica. A
// replica that starts only once its primary is back, reachable and not failed
// in anyone's view has no election to win.
//
// The primaries are the nodes that carry a data point, recorded on the way
// down; without persistence there is none, and every node starts at once. A
// primary that is still not running after wakeReplicaGrace no longer holds
// its replicas: a primary that is not up acknowledges no write, and its
// replica holds the same data, so the replica may take over.
//
// It returns why replicas are still held, or "" once every node is unmarked.
func (r *ValkeyClusterReconciler) wakeInStages(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, owned []*valkeyiov1alpha1.ValkeyNode, state *valkey.ClusterState) (string, error) {
	cond := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionHibernated)
	graceOver := cond != nil && cond.Reason == valkeyiov1alpha1.ReasonWaking &&
		time.Since(cond.LastTransitionTime.Time) > wakeReplicaGrace
	primaries := map[string]*valkeyiov1alpha1.ValkeyNode{}
	for _, node := range owned {
		if _, ok := node.Annotations[valkeyiov1alpha1.AnnotationHibernateDataPoint]; ok {
			primaries[node.Labels[LabelShardIndex]] = node
		}
	}
	unmark := func(node *valkeyiov1alpha1.ValkeyNode) error {
		return r.patchNodeAnnotations(ctx, node, nil, []string{valkeyiov1alpha1.AnnotationHibernate})
	}

	var held []string
	for _, node := range owned {
		if node.Annotations[valkeyiov1alpha1.AnnotationHibernate] == "" {
			continue
		}
		primary := primaries[node.Labels[LabelShardIndex]]
		if primary == nil || primary == node || primaryServes(primary, state) {
			if err := unmark(node); err != nil {
				return "", err
			}
			continue
		}
		// A primary that is up but still suspected may already be serving, so
		// only one that is not running at all stops holding its replicas.
		if !graceOver || primary.Status.Ready || nodeStateFor(state, primary) != nil {
			held = append(held, node.Name)
			continue
		}
		logf.FromContext(ctx).Info("Starting a replica whose primary is not running", "replica", node.Name, "primary", primary.Name)
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "ReplicaStartedWithoutPrimary", "Wake",
			"Starting %s after %s because its primary %s is still not running; the replica may take over", node.Name, wakeReplicaGrace, primary.Name)
		if err := unmark(node); err != nil {
			return "", err
		}
	}
	if len(held) > 0 {
		// The regular reconcile, which re-introduces moved members, waits for
		// the replicas; the primaries need it now.
		if healed := r.healStaleAddressPeers(ctx, cluster, state); healed > 0 {
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "StaleAddressesHealed", "ClusterMeet", "Re-introduced %d peer link(s) whose addresses changed", healed)
		}
		slices.Sort(held)
		return fmt.Sprintf("Starting the primaries; %d replica(s) start once their primary serves again: %s", len(held), strings.Join(held, ", ")), nil
	}
	return "", r.clearHibernationAnnotations(ctx, owned)
}

// primaryServes reports whether a woken primary is back as the cluster sees
// it: ready, still a primary, with a cluster state of ok, not even suspected
// (fail?) in any member's view, and not listed anywhere at an address it no
// longer has. Valkey only marks a node failed on a member that suspects it,
// so a primary nobody suspects cannot be failed over from under its replicas;
// right after the stale addresses are healed, the members still trade the
// suspicions they built while they could not reach each other.
func primaryServes(node *valkeyiov1alpha1.ValkeyNode, state *valkey.ClusterState) bool {
	ns := nodeStateFor(state, node)
	return node.Status.Ready && ns != nil && ns.IsPrimary() &&
		ns.ClusterInfo["cluster_state"] == "ok" &&
		!state.IsNodeFailed(ns.Id) &&
		len(state.FindStaleAddressPeers()) == 0
}

// finishWake clears the Hibernated condition of a cluster that woke, once the
// regular reconcile finds it healthy.
func (r *ValkeyClusterReconciler) finishWake(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) {
	cond := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionHibernated)
	if cond == nil || cond.Reason != valkeyiov1alpha1.ReasonWaking {
		return
	}
	logf.FromContext(ctx).Info("Cluster woke from hibernation")
	meta.RemoveStatusCondition(&cluster.Status.Conditions, valkeyiov1alpha1.ConditionHibernated)
}

// resumeAllWrites lifts the write pause on every node that answers.
// It returns an error when a node that answered the scrape could not be
// unpaused; the caller then retries on its next pass instead of moving on with
// writes still paused there.
func (r *ValkeyClusterReconciler) resumeAllWrites(ctx context.Context, state *valkey.ClusterState) error {
	if state == nil {
		return nil
	}
	var errs []error
	for _, ns := range state.AllNodes() {
		if err := resumeWrites(ctx, ns); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ns.Address, err))
		}
	}
	return errors.Join(errs...)
}

// retryResume is the result of a pass that could not resume writes: nothing
// else changes, and the next pass tries again.
func retryResume(ctx context.Context, err error) (ctrl.Result, bool, error) {
	logf.FromContext(ctx).Info("Could not resume writes; retrying", "error", err.Error())
	return ctrl.Result{RequeueAfter: hibernationStepRequeue}, true, nil
}

// nodesWithPods returns the names of the nodes that still have a pod,
// terminating ones included.
func (r *ValkeyClusterReconciler) nodesWithPods(ctx context.Context, nodes []*valkeyiov1alpha1.ValkeyNode) ([]string, error) {
	var names []string
	for _, node := range nodes {
		pods := &corev1.PodList{}
		if err := r.List(ctx, pods, client.InNamespace(node.Namespace), client.MatchingLabels(valkeyNodeLabels(node))); err != nil {
			return nil, err
		}
		if len(pods.Items) > 0 {
			names = append(names, node.Name)
		}
	}
	// Sorted, so that the list order alone never changes the status message.
	slices.Sort(names)
	return names, nil
}

func (r *ValkeyClusterReconciler) clearHibernationAnnotations(ctx context.Context, nodes []*valkeyiov1alpha1.ValkeyNode) error {
	for _, node := range nodes {
		if err := r.patchNodeAnnotations(ctx, node, nil, []string{
			valkeyiov1alpha1.AnnotationHibernate,
			valkeyiov1alpha1.AnnotationHibernateDataPoint,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (r *ValkeyClusterReconciler) patchNodeAnnotations(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode, set map[string]string, remove []string) error {
	patch := client.MergeFrom(node.DeepCopy())
	changed := false
	for key, value := range set {
		if upsertAnnotation(node, key, value) {
			changed = true
		}
	}
	for _, key := range remove {
		if _, ok := node.Annotations[key]; ok {
			delete(node.Annotations, key)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return client.IgnoreNotFound(r.Patch(ctx, node, patch))
}

// reconcileHibernatedNode holds the workload of a hibernated node at zero
// replicas and reports it. Nothing is sent to Valkey: the pod is stopping or
// gone.
func (r *ValkeyNodeReconciler) reconcileHibernatedNode(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) (ctrl.Result, error) {
	if err := r.scaleWorkload(ctx, node, 0); err != nil {
		return ctrl.Result{}, err
	}
	if _, err := r.updateStatus(ctx, node); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: hibernatedRequeue}, nil
}

// scaleWorkload sets the replica count of the node's StatefulSet or
// Deployment. A workload that does not exist yet is left to ensureWorkload.
func (r *ValkeyNodeReconciler) scaleWorkload(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode, replicas int32) error {
	key := client.ObjectKey{Name: valkeyNodeResourceName(node), Namespace: node.Namespace}
	current := func(spec *int32) int32 {
		if spec == nil {
			return 1
		}
		return *spec
	}
	switch node.Spec.WorkloadType {
	case valkeyiov1alpha1.WorkloadTypeStatefulSet:
		sts := &appsv1.StatefulSet{}
		if err := r.Get(ctx, key, sts); err != nil {
			return client.IgnoreNotFound(err)
		}
		if current(sts.Spec.Replicas) == replicas {
			return nil
		}
		patch := client.MergeFrom(sts.DeepCopy())
		sts.Spec.Replicas = &replicas
		if err := r.Patch(ctx, sts, patch); err != nil {
			return err
		}
	case valkeyiov1alpha1.WorkloadTypeDeployment:
		dep := &appsv1.Deployment{}
		if err := r.Get(ctx, key, dep); err != nil {
			return client.IgnoreNotFound(err)
		}
		if current(dep.Spec.Replicas) == replicas {
			return nil
		}
		patch := client.MergeFrom(dep.DeepCopy())
		dep.Spec.Replicas = &replicas
		if err := r.Patch(ctx, dep, patch); err != nil {
			return err
		}
	default:
		return nil
	}
	logf.FromContext(ctx).Info("scaled workload", "node", node.Name, "replicas", replicas)
	return nil
}
