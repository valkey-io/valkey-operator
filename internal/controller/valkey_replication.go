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

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

// failoverTimeoutMillis bounds a planned FAILOVER.
// The primary aborts the handoff if the target has not caught up by then, and keeps its role.
const failoverTimeoutMillis = 10000

// noFailover is the master_failover_state a node reports when no FAILOVER is running.
const noFailover = "no-failover"

// replicationState is one node's view of replication, parsed from INFO replication.
type replicationState struct {
	// role is RolePrimary or RoleReplica.
	role string
	// offset is master_repl_offset on a primary and slave_repl_offset on a replica, or -1 when absent.
	offset int64
	// primaryHost is master_host on a replica.
	primaryHost string
	// linkUp is master_link_status:up on a replica.
	linkUp bool
	// failoverState is master_failover_state, which is no-failover when idle.
	failoverState string
}

// parseReplicationState parses the output of INFO replication.
func parseReplicationState(info string) replicationState {
	m := valkey.InfoStringToMap(info)
	s := replicationState{
		offset:        -1,
		primaryHost:   m["master_host"],
		linkUp:        m["master_link_status"] == "up",
		failoverState: m["master_failover_state"],
	}
	offsetKey := ""
	switch m["role"] {
	case RoleMaster:
		s.role = RolePrimary
		offsetKey = "master_repl_offset"
	case RoleSlave:
		s.role = RoleReplica
		offsetKey = "slave_repl_offset"
	}
	if offset, err := strconv.ParseInt(m[offsetKey], 10, 64); err == nil {
		s.offset = offset
	}
	return s
}

// replicationClient issues the replication commands the Valkey controller needs against one node.
// Tests replace it to run without a Valkey server.
type replicationClient interface {
	// state reads INFO replication from the node.
	state(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) (replicationState, error)
	// replicaOf points the node at host on DefaultPort.
	replicaOf(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode, host string) error
	// promote makes the node a primary with REPLICAOF NO ONE.
	promote(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) error
	// failoverTo asks the primary to hand its role to the replica at targetIP.
	// The primary pauses writes until the target has caught up, so no acknowledged write is lost.
	failoverTo(ctx context.Context, primary *valkeyiov1alpha1.ValkeyNode, targetIP string) error
}

// valkeyReplicationClient implements replicationClient over the shared ClientProvider.
type valkeyReplicationClient struct {
	clients ClientProvider
}

func (c *valkeyReplicationClient) state(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) (replicationState, error) {
	vc, err := c.clients.ForNode(ctx, node)
	if err != nil {
		return replicationState{}, err
	}
	info, err := vc.Do(ctx, vc.B().Info().Section("replication").Build()).ToString()
	if err != nil {
		return replicationState{}, err
	}
	return parseReplicationState(info), nil
}

func (c *valkeyReplicationClient) replicaOf(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode, host string) error {
	vc, err := c.clients.ForNode(ctx, node)
	if err != nil {
		return err
	}
	return vc.Do(ctx, vc.B().Replicaof().Host(host).Port(DefaultPort).Build()).Error()
}

func (c *valkeyReplicationClient) promote(ctx context.Context, node *valkeyiov1alpha1.ValkeyNode) error {
	vc, err := c.clients.ForNode(ctx, node)
	if err != nil {
		return err
	}
	return vc.Do(ctx, vc.B().Replicaof().No().One().Build()).Error()
}

func (c *valkeyReplicationClient) failoverTo(ctx context.Context, primary *valkeyiov1alpha1.ValkeyNode, targetIP string) error {
	vc, err := c.clients.ForNode(ctx, primary)
	if err != nil {
		return err
	}
	return vc.Do(ctx, vc.B().Failover().To().Host(targetIP).Port(DefaultPort).Timeout(failoverTimeoutMillis).Build()).Error()
}

// nodeView pairs a ValkeyNode with its observed replication state.
type nodeView struct {
	node *valkeyiov1alpha1.ValkeyNode
	// observed is true when the node is ready and its INFO replication was read.
	observed bool
	state    replicationState
}

// Reasons planReplication reports when it cannot settle on a primary.
const (
	planPrimaryNotReady   = "PrimaryNotReady"
	planMultiplePrimaries = "MultiplePrimaries"
	planNoPrimary         = "NoPrimary"
	planFailoverRunning   = "FailoverInProgress"
)

// replicationPlan is what the Valkey controller should do to converge replication.
type replicationPlan struct {
	// primary is the node every other node should follow, or nil when none can be chosen yet.
	primary *nodeView
	// promote is a replica to promote because no node is primary, or nil.
	promote *nodeView
	// repoint lists observed nodes that do not follow primary yet.
	repoint []*nodeView
	// reason says why primary is nil.
	reason string
}

// planReplication decides which node is primary and which nodes must be repointed at it.
// knownPrimary is the primary recorded in status by an earlier reconcile.
// hostOf returns the address a replica uses to reach a primary.
//
// It never promotes over a known primary that is merely unreachable.
// Nobody promotes automatically in failover mode None, so a known primary that is not ready blocks every change until it returns.
// A replica restarts as an empty-config primary, so the known primary wins over any other node that reports role master.
func planReplication(views []*nodeView, knownPrimary string, hostOf func(*valkeyiov1alpha1.ValkeyNode) string) replicationPlan {
	for _, view := range views {
		if view.observed && view.state.failoverState != "" && view.state.failoverState != noFailover {
			return replicationPlan{reason: planFailoverRunning}
		}
	}

	var known *nodeView
	var primaries []*nodeView
	allObserved := len(views) > 0
	for _, view := range views {
		if view.node.Name == knownPrimary {
			known = view
		}
		if !view.observed {
			allObserved = false
			continue
		}
		if view.state.role == RolePrimary {
			primaries = append(primaries, view)
		}
	}

	plan := replicationPlan{}
	switch {
	case known != nil && !known.observed:
		return replicationPlan{reason: planPrimaryNotReady}
	case known != nil && known.state.role == RolePrimary:
		plan.primary = known
	case len(primaries) == 1:
		plan.primary = primaries[0]
	case len(primaries) > 1 && knownPrimary == "":
		// Bootstrap: every fresh node starts as a primary, and the lowest index wins.
		plan.primary = primaries[0]
	case len(primaries) > 1:
		return replicationPlan{reason: planMultiplePrimaries}
	case allObserved:
		// Every node is reachable and none is primary, so promoting the best-synced replica loses nothing.
		return replicationPlan{promote: bestSyncedReplica(views, nil), reason: planNoPrimary}
	default:
		return replicationPlan{reason: planNoPrimary}
	}

	host := hostOf(plan.primary.node)
	for _, view := range views {
		if view == plan.primary || !view.observed {
			continue
		}
		if view.state.role == RolePrimary || view.state.primaryHost != host {
			plan.repoint = append(plan.repoint, view)
		}
	}
	return plan
}

// bestSyncedReplica returns the observed replica with the highest replication offset, or nil.
// When primary is set, only replicas with a live link that follow it qualify, so a planned handoff targets a replica that is caught up.
// Ties go to the lowest node index, which is the order views arrive in.
func bestSyncedReplica(views []*nodeView, primary *nodeView) *nodeView {
	var best *nodeView
	for _, view := range views {
		if !view.observed || view.state.role != RoleReplica || view == primary {
			continue
		}
		if primary != nil && !view.state.linkUp {
			continue
		}
		if best == nil || view.state.offset > best.state.offset {
			best = view
		}
	}
	return best
}
