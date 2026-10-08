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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// reportUsersACLUnresolved emits one warning per distinct failure, not one
// per reconcile: the healthy branch may append the nodes that cannot apply
// the aclfile to the message, and that must not read as a new failure.
func TestReportUsersACLUnresolvedEvents(t *testing.T) {
	recorder := events.NewFakeRecorder(10)
	r := &ValkeyClusterReconciler{Recorder: recorder}
	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	ctx := context.Background()
	drain := func() []string {
		var got []string
		for {
			select {
			case e := <-recorder.Events:
				got = append(got, e)
			default:
				return got
			}
		}
	}
	missing := &userSecretUnresolvedError{User: "bob", Secret: "bob-pw"}

	r.reportUsersACLUnresolved(ctx, cluster, missing)
	require.Len(t, drain(), 1, "a new failure is reported once")
	cond := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)
	require.NotNil(t, cond)
	assert.Equal(t, valkeyiov1alpha1.ReasonUsersACLUnresolved, cond.Reason)
	assert.Equal(t, missing.Error(), cond.Message)

	r.reportUsersACLUnresolved(ctx, cluster, missing)
	assert.Empty(t, drain(), "the same failure is not reported again")

	setCondition(cluster, valkeyiov1alpha1.ConditionDegraded, valkeyiov1alpha1.ReasonUsersACLUnresolved,
		missing.Error()+"; ACL not applied on c-0-0", metav1.ConditionTrue)
	r.reportUsersACLUnresolved(ctx, cluster, missing)
	assert.Empty(t, drain(), "nodes appended by the healthy branch are not a new failure")

	missingKey := &userSecretUnresolvedError{User: "bob", Secret: "bob-pw", Key: "current"}
	r.reportUsersACLUnresolved(ctx, cluster, missingKey)
	require.Len(t, drain(), 1, "a changed failure is reported")

	shorterKey := &userSecretUnresolvedError{User: "bob", Secret: "bob-pw", Key: "curr"}
	r.reportUsersACLUnresolved(ctx, cluster, shorterKey)
	require.Len(t, drain(), 1, "a key that is a prefix of the previous one is still a new failure")

	r.reportUsersACLUnresolved(ctx, cluster, nil)
	got := drain()
	require.Len(t, got, 1, "recovery is reported")
	assert.Contains(t, got[0], "UsersACLResolved")
	assert.Nil(t, meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionDegraded))

	r.reportUsersACLUnresolved(ctx, cluster, nil)
	assert.Empty(t, drain(), "nothing to report while resolved")
}
