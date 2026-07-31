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
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

func conditionGaugeValue(t *testing.T, name, ns, condType, status string) float64 {
	t.Helper()
	return testutil.ToFloat64(clusterCondition.WithLabelValues(name, ns, condType, status))
}

// expectConditionSeries asserts the three status series for a condition type.
func expectConditionSeries(t *testing.T, name, ns, condType string, wantTrue, wantFalse, wantUnknown float64) {
	t.Helper()
	if got := conditionGaugeValue(t, name, ns, condType, "true"); got != wantTrue {
		t.Errorf("%s{status=true} = %v, want %v", condType, got, wantTrue)
	}
	if got := conditionGaugeValue(t, name, ns, condType, "false"); got != wantFalse {
		t.Errorf("%s{status=false} = %v, want %v", condType, got, wantFalse)
	}
	if got := conditionGaugeValue(t, name, ns, condType, "unknown"); got != wantUnknown {
		t.Errorf("%s{status=unknown} = %v, want %v", condType, got, wantUnknown)
	}
}

func TestInitClusterMetrics_PreCreatesConditionSeries(t *testing.T) {
	const name, ns = "metrics-cond-init-test", "default"
	before := testutil.CollectAndCount(clusterCondition)
	initClusterMetrics(name, ns)

	want := before + len(valkeyiov1alpha1.ClusterConditionTypes)*3
	if got := testutil.CollectAndCount(clusterCondition); got != want {
		t.Fatalf("expected %d condition series after init, got %d", want, got)
	}

	deleteClusterMetrics(name, ns)
	if got := testutil.CollectAndCount(clusterCondition); got != before {
		t.Fatalf("expected %d condition series after delete, got %d", before, got)
	}
}

func TestUpdateClusterMetrics_ClusterCondition(t *testing.T) {
	const name, ns = "metrics-cond-test", "default"
	const condType = valkeyiov1alpha1.ConditionSchedulingSatisfied
	initClusterMetrics(name, ns)
	defer deleteClusterMetrics(name, ns)

	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	cluster.Name = name
	cluster.Namespace = ns

	// Condition absent -> all three status series read 0.
	updateClusterMetrics(cluster)
	expectConditionSeries(t, name, ns, condType, 0, 0, 0)

	setCondition(cluster, condType, valkeyiov1alpha1.ReasonAllPodsScheduled, "ok", metav1.ConditionTrue)
	updateClusterMetrics(cluster)
	expectConditionSeries(t, name, ns, condType, 1, 0, 0)

	setCondition(cluster, condType, valkeyiov1alpha1.ReasonPodsPendingScheduling, "pending", metav1.ConditionFalse)
	updateClusterMetrics(cluster)
	expectConditionSeries(t, name, ns, condType, 0, 1, 0)

	setCondition(cluster, condType, valkeyiov1alpha1.ReasonPodsPendingScheduling, "unknown", metav1.ConditionUnknown)
	updateClusterMetrics(cluster)
	expectConditionSeries(t, name, ns, condType, 0, 0, 1)

	// Condition removed again -> back to all zeros.
	cluster.Status.Conditions = nil
	updateClusterMetrics(cluster)
	expectConditionSeries(t, name, ns, condType, 0, 0, 0)
}

func TestUpdateClusterMetrics_UnregisteredConditionType(t *testing.T) {
	const name, ns = "metrics-cond-unregistered-test", "default"
	const condType = "NotInClusterConditionTypes"
	initClusterMetrics(name, ns)
	defer deleteClusterMetrics(name, ns)

	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	cluster.Name = name
	cluster.Namespace = ns
	setCondition(cluster, condType, "SomeReason", "some message", metav1.ConditionTrue)

	updateClusterMetrics(cluster)
	expectConditionSeries(t, name, ns, condType, 1, 0, 0)
}
