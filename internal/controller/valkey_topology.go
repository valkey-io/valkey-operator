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
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// replication returns the client for replication commands.
func (r *ValkeyReconciler) replication() replicationClient {
	if r.replicationClient != nil {
		return r.replicationClient
	}
	return &valkeyReplicationClient{clients: r.ValkeyClients}
}

// valkeyPrimaryHost returns the address replicas use to reach a primary: its pod's DNS name under the Valkey's headless Service.
// The node's StatefulSet uses that Service as its governing Service, so the name stays stable across pod restarts.
func valkeyPrimaryHost(v *valkeyiov1alpha1.Valkey) func(*valkeyiov1alpha1.ValkeyNode) string {
	return func(node *valkeyiov1alpha1.ValkeyNode) string {
		if node.Status.PodName == "" {
			return node.Status.PodIP
		}
		return node.Status.PodName + "." + headlessServiceFQDN(v.Name, v.Namespace, v.GetClusterDomain())
	}
}

// reconcileReplicated converges a Valkey with replicas.
// It wires replication first, and changes node specs or the node count only once every node follows one primary.
// A roll or a scale-in therefore never races a topology change.
func (r *ValkeyReconciler) reconcileReplicated(ctx context.Context, rc *resourceClient, v *valkeyiov1alpha1.Valkey, nodes []*valkeyiov1alpha1.ValkeyNode) (ctrl.Result, error) {
	views := r.observeNodes(ctx, nodes)
	hostOf := valkeyPrimaryHost(v)
	plan := planReplication(views, v.Status.Primary, hostOf)

	progress, err := r.applyReplicationPlan(ctx, v, plan, hostOf)
	if err != nil {
		return ctrl.Result{}, err
	}
	if progress == "" {
		progress, err = r.scaleInValkeyNodes(ctx, rc, v, views, plan.primary)
		if err != nil {
			return ctrl.Result{}, err
		}
	}
	if progress == "" {
		progress, err = r.rollValkeyNode(ctx, rc, v, views, plan.primary)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	setReplicationStatus(v, views, plan, hostOf, progress)
	if meta.IsStatusConditionTrue(v.Status.Conditions, valkeyiov1alpha1.ConditionReady) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: valkeyReplicationRequeue}, nil
}

// observeNodes reads INFO replication from every ready node.
// A node that is not ready, or that fails the read, is returned unobserved.
func (r *ValkeyReconciler) observeNodes(ctx context.Context, nodes []*valkeyiov1alpha1.ValkeyNode) []*nodeView {
	views := make([]*nodeView, 0, len(nodes))
	for _, node := range nodes {
		view := &nodeView{node: node}
		if node.Status.Ready && node.Status.PodIP != "" {
			state, err := r.replication().state(ctx, node)
			if err != nil {
				logf.FromContext(ctx).V(1).Info("failed to read replication state", "node", node.Name, "error", err.Error())
			} else {
				view.state = state
				view.observed = true
			}
		}
		views = append(views, view)
	}
	return views
}

// applyReplicationPlan issues the promote and REPLICAOF commands a plan asks for and records the chosen primary.
// It returns a message describing the change in flight, or "" when replication already matches the plan.
func (r *ValkeyReconciler) applyReplicationPlan(ctx context.Context, v *valkeyiov1alpha1.Valkey, plan replicationPlan, hostOf func(*valkeyiov1alpha1.ValkeyNode) string) (string, error) {
	if plan.promote != nil {
		if err := r.replication().promote(ctx, plan.promote.node); err != nil {
			return "", fmt.Errorf("promoting %s: %w", plan.promote.node.Name, err)
		}
		r.Recorder.Eventf(v, plan.promote.node, corev1.EventTypeNormal, "PrimaryPromoted", "PromotePrimary", "Promoted %s because no node was primary", plan.promote.node.Name)
		return fmt.Sprintf("Promoting %s", plan.promote.node.Name), nil
	}
	if plan.primary == nil {
		return fmt.Sprintf("Waiting for a primary: %s", plan.reason), nil
	}

	v.Status.Primary = plan.primary.node.Name
	host := hostOf(plan.primary.node)
	for _, view := range plan.repoint {
		if err := r.replication().replicaOf(ctx, view.node, host); err != nil {
			return "", fmt.Errorf("pointing %s at %s: %w", view.node.Name, host, err)
		}
		r.Recorder.Eventf(v, view.node, corev1.EventTypeNormal, "ReplicaConfigured", "ConfigureReplica", "Pointed %s at primary %s", view.node.Name, plan.primary.node.Name)
	}
	if len(plan.repoint) > 0 {
		return fmt.Sprintf("Pointing %d node(s) at primary %s", len(plan.repoint), plan.primary.node.Name), nil
	}
	return "", nil
}

// scaleInValkeyNodes deletes the nodes above spec.replicas.
// A node above spec.replicas that holds the primary role first hands it to the best-synced remaining replica.
func (r *ValkeyReconciler) scaleInValkeyNodes(ctx context.Context, rc *resourceClient, v *valkeyiov1alpha1.Valkey, views []*nodeView, primary *nodeView) (string, error) {
	kept := make([]*nodeView, 0, len(views))
	var extra []*nodeView
	for _, view := range views {
		if valkeyNodeIndex(view.node) > int(v.Spec.Replicas) {
			extra = append(extra, view)
		} else {
			kept = append(kept, view)
		}
	}
	if len(extra) == 0 {
		return "", nil
	}
	if slices.Contains(extra, primary) {
		return r.handOffPrimary(ctx, v, primary, kept)
	}
	for _, view := range extra {
		if err := rc.Delete(ctx, view.node); client.IgnoreNotFound(err) != nil {
			return "", fmt.Errorf("deleting %s: %w", view.node.Name, err)
		}
		r.Recorder.Eventf(v, view.node, corev1.EventTypeNormal, "ValkeyNodeDeleted", "DeleteValkeyNode", "Deleted ValkeyNode %s on scale-in", view.node.Name)
	}
	return fmt.Sprintf("Removing %d node(s) above spec.replicas", len(extra)), nil
}

// rollValkeyNode applies the desired spec to at most one node per reconcile.
// Replicas go first in index order and the primary goes last.
// It returns a message describing the change in flight, or "" when every node is up to date.
func (r *ValkeyReconciler) rollValkeyNode(ctx context.Context, rc *resourceClient, v *valkeyiov1alpha1.Valkey, views []*nodeView, primary *nodeView) (string, error) {
	var target *nodeView
	for _, view := range views {
		if view != primary && valkeyNodeChanged(v, view.node) {
			target = view
			break
		}
	}
	if target == nil && primary != nil && valkeyNodeChanged(v, primary.node) {
		target = primary
	}
	if target == nil {
		return "", nil
	}

	// A node that is down has nothing to lose, so it may change while others settle.
	// A healthy node waits until every other node is settled, which keeps one pod restarting at a time.
	if target.observed {
		for _, view := range views {
			if view != target && !valkeyNodeSettled(view.node) {
				return fmt.Sprintf("Waiting for ValkeyNode %s to settle before updating %s", view.node.Name, target.node.Name), nil
			}
		}
	}

	if target == primary {
		rolls, err := valkeyNodeWouldRoll(v, target.node)
		if err != nil {
			return "", err
		}
		if rolls {
			return r.handOffPrimary(ctx, v, primary, views)
		}
	}

	if err := updateValkeyNode(ctx, rc, v, target.node); err != nil {
		return "", err
	}
	return fmt.Sprintf("Updating ValkeyNode %s", target.node.Name), nil
}

// handOffPrimary asks the primary to fail over to the best-synced replica among candidates.
// FAILOVER pauses writes on the primary until the target has caught up, so no acknowledged write is lost.
// The next reconcile sees the new primary and repoints the other replicas at it.
func (r *ValkeyReconciler) handOffPrimary(ctx context.Context, v *valkeyiov1alpha1.Valkey, primary *nodeView, candidates []*nodeView) (string, error) {
	target := bestSyncedReplica(candidates, primary)
	if target == nil {
		return fmt.Sprintf("Primary %s waits for a synced replica to take over", primary.node.Name), nil
	}
	if err := r.replication().failoverTo(ctx, primary.node, target.node.Status.PodIP); err != nil {
		return "", fmt.Errorf("failing over %s to %s: %w", primary.node.Name, target.node.Name, err)
	}
	r.Recorder.Eventf(v, primary.node, corev1.EventTypeNormal, "PrimaryHandoff", "FailoverPrimary", "Handing primary from %s to %s", primary.node.Name, target.node.Name)
	return fmt.Sprintf("Handing primary from %s to %s", primary.node.Name, target.node.Name), nil
}

// valkeyNodeSettled reports whether a node is ready and its controller has observed its latest spec.
func valkeyNodeSettled(node *valkeyiov1alpha1.ValkeyNode) bool {
	if node.Status.ObservedGeneration > 0 && node.Status.ObservedGeneration != node.Generation {
		return false
	}
	return node.Status.Ready
}

// setReplicationStatus derives the replica counts and conditions of a replicated Valkey.
// progress is the change in flight, or "" when nothing is changing.
func setReplicationStatus(v *valkeyiov1alpha1.Valkey, views []*nodeView, plan replicationPlan, hostOf func(*valkeyiov1alpha1.ValkeyNode) string, progress string) {
	setCondition := func(condType, reason, message string, status metav1.ConditionStatus) {
		setConditionOn(&v.Status.Conditions, v.Generation, condType, reason, message, status)
	}

	var synced int32
	if plan.primary != nil {
		host := hostOf(plan.primary.node)
		for _, view := range views {
			if view == plan.primary || valkeyNodeIndex(view.node) > int(v.Spec.Replicas) {
				continue
			}
			if view.observed && view.state.role == RoleReplica && view.state.linkUp && view.state.primaryHost == host {
				synced++
			}
		}
	}
	v.Status.Replicas = int32(max(len(views)-1, 0))
	v.Status.ReadyReplicas = synced

	if plan.primary != nil {
		setCondition(valkeyiov1alpha1.ConditionPrimaryElected, valkeyiov1alpha1.ReasonPrimaryElected, fmt.Sprintf("%s is primary", plan.primary.node.Name), metav1.ConditionTrue)
	} else {
		setCondition(valkeyiov1alpha1.ConditionPrimaryElected, primaryReason(plan.reason), "No primary that every node can follow", metav1.ConditionFalse)
	}

	replicationMessage := fmt.Sprintf("%d of %d replicas in sync", synced, v.Spec.Replicas)
	if synced == v.Spec.Replicas {
		setCondition(valkeyiov1alpha1.ConditionReplicationHealthy, valkeyiov1alpha1.ReasonReplicasInSync, replicationMessage, metav1.ConditionTrue)
	} else {
		setCondition(valkeyiov1alpha1.ConditionReplicationHealthy, valkeyiov1alpha1.ReasonReplicasNotSynced, replicationMessage, metav1.ConditionFalse)
	}

	healthy := plan.primary != nil && plan.primary.observed && synced == v.Spec.Replicas && progress == "" && len(views) == int(v.Spec.Replicas)+1
	if healthy {
		setCondition(valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonInstanceHealthy, "Instance is healthy", metav1.ConditionTrue)
		setCondition(valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonReconcileComplete, "No changes needed", metav1.ConditionFalse)
		return
	}
	if progress == "" {
		progress = replicationMessage
	}
	setCondition(valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonReconciling, progress, metav1.ConditionFalse)
	setCondition(valkeyiov1alpha1.ConditionProgressing, valkeyiov1alpha1.ReasonReconciling, progress, metav1.ConditionTrue)
}

// primaryReason maps a planReplication reason to the PrimaryElected condition reason.
func primaryReason(planReason string) string {
	switch planReason {
	case planPrimaryNotReady:
		return valkeyiov1alpha1.ReasonPrimaryNotReady
	case planMultiplePrimaries:
		return valkeyiov1alpha1.ReasonMultiplePrimaries
	case planFailoverRunning:
		return valkeyiov1alpha1.ReasonFailoverInProgress
	default:
		return valkeyiov1alpha1.ReasonNoPrimary
	}
}
