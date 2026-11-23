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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	vclient "github.com/valkey-io/valkey-go"
	vmock "github.com/valkey-io/valkey-go/mock"
	"go.uber.org/mock/gomock"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

// singleNodeClients hands out one client for every node.
type singleNodeClients struct{ c vclient.Client }

func (s singleNodeClients) ForCluster(context.Context, *valkeyiov1alpha1.ValkeyCluster) (valkey.DialFunc, error) {
	return nil, errors.New("not used")
}

func (s singleNodeClients) ForNode(context.Context, *valkeyiov1alpha1.ValkeyNode) (vclient.Client, error) {
	return s.c, nil
}

// Valkey 9 replies, trimmed. Valkey orders the sections itself, whatever order
// the command names them in.
const (
	infoPersistenceReplication = "# Persistence\r\nloading:%s\r\nasync_loading:0\r\nrdb_bgsave_in_progress:0\r\n\r\n" +
		"# Replication\r\nrole:slave\r\nmaster_host:10.244.2.9\r\nmaster_port:6379\r\nmaster_link_status:%s\r\n" +
		"master_sync_in_progress:%s\r\nslave_repl_offset:3108\r\n"
	infoClusterReplication = "# Replication\r\nrole:%s\r\nconnected_slaves:0\r\n\r\n# Cluster\r\ncluster_enabled:%s\r\n"
)

func TestNodeInfoReadsPersistenceAndReplication(t *testing.T) {
	tests := []struct {
		name                   string
		loading, link, syncing string
		want                   bool
	}{
		{"in sync", "0", "up", "0", false},
		{"loading an RDB", "1", "down", "0", true},
		{"receiving a full sync", "0", "down", "1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			c := vmock.NewClient(ctrl)
			c.EXPECT().Do(gomock.Any(), vmock.Match("INFO", "persistence", "replication")).
				Return(vmock.Result(vmock.ValkeyBlobString(
					fmt.Sprintf(infoPersistenceReplication, tt.loading, tt.link, tt.syncing))))

			r := &ValkeyNodeReconciler{ValkeyClients: singleNodeClients{c}}
			info, err := r.nodeInfo(context.Background(), &valkeyiov1alpha1.ValkeyNode{})
			require.NoError(t, err)
			assert.Equal(t, tt.want, syncInProgress(info))
		})
	}
}

func TestResolveRoleReadsClusterAndReplication(t *testing.T) {
	t.Run("cluster mode takes the role from slot ownership", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		c := vmock.NewClient(ctrl)
		// A just-restarted replica reports role:master until it reconnects.
		c.EXPECT().Do(gomock.Any(), vmock.Match("INFO", "cluster", "replication")).
			Return(vmock.Result(vmock.ValkeyBlobString(fmt.Sprintf(infoClusterReplication, "master", "1"))))
		c.EXPECT().Do(gomock.Any(), vmock.Match("CLUSTER", "NODES")).
			Return(vmock.Result(vmock.ValkeyBlobString(
				"r1 10.0.0.2:6379@16379 myself,master - 0 0 1 connected\n" +
					"p1 10.0.0.1:6379@16379 master - 0 0 1 connected 0-16383\n")))

		r := &ValkeyNodeReconciler{ValkeyClients: singleNodeClients{c}}
		role, id := r.resolveRole(context.Background(), &valkeyiov1alpha1.ValkeyNode{})
		assert.Equal(t, RoleReplica, role)
		assert.Equal(t, "r1", id)
	})

	t.Run("standalone takes the role from INFO replication", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		c := vmock.NewClient(ctrl)
		c.EXPECT().Do(gomock.Any(), vmock.Match("INFO", "cluster", "replication")).
			Return(vmock.Result(vmock.ValkeyBlobString(fmt.Sprintf(infoClusterReplication, "master", "0"))))

		r := &ValkeyNodeReconciler{ValkeyClients: singleNodeClients{c}}
		role, id := r.resolveRole(context.Background(), &valkeyiov1alpha1.ValkeyNode{})
		assert.Equal(t, RolePrimary, role)
		assert.Empty(t, id)
	})
}
