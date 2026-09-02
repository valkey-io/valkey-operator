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
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

const (
	labelValkeyCluster   = "valkey_cluster"
	labelTargetNamespace = "target_namespace"
)

var factory = promauto.With(metrics.Registry)

var (
	clusterStateInfo = factory.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "valkey_operator_cluster_state_info",
			Help: "Information about a ValkeyCluster. Value is 1 for the current state, 0 for all others.",
		},
		[]string{labelValkeyCluster, labelTargetNamespace, "state"},
	)

	clusterShards = factory.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "valkey_operator_cluster_shards",
			Help: "Total number of shards in a ValkeyCluster.",
		},
		[]string{labelValkeyCluster, labelTargetNamespace},
	)

	clusterShardsReady = factory.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "valkey_operator_cluster_shards_ready",
			Help: "Number of ready shards in a ValkeyCluster.",
		},
		[]string{labelValkeyCluster, labelTargetNamespace},
	)

	failoversTotal = factory.NewCounterVec(
		prometheus.CounterOpts{
			Name: "valkey_operator_failovers_total",
			Help: "Total number of failover events.",
		},
		[]string{labelValkeyCluster, labelTargetNamespace, "type"},
	)

	slotMigrationBatchesTotal = factory.NewCounterVec(
		prometheus.CounterOpts{
			Name: "valkey_operator_slot_migration_batches_total",
			Help: "Total number of slot migration batches completed.",
		},
		[]string{labelValkeyCluster, labelTargetNamespace},
	)

	clusterCondition = factory.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "valkey_operator_cluster_condition",
			Help: "Status of a ValkeyCluster condition. 1 for the condition's current status (true/false/unknown), 0 for the others; all zeros when the condition is not reported.",
		},
		[]string{labelValkeyCluster, labelTargetNamespace, "type", "status"},
	)
)

var conditionStatuses = []metav1.ConditionStatus{
	metav1.ConditionTrue,
	metav1.ConditionFalse,
	metav1.ConditionUnknown,
}

// setConditionSeries sets the three status series for one condition type.
// A nil cond (not reported) leaves all three at 0.
func setConditionSeries(name, namespace, condType string, cond *metav1.Condition) {
	for _, s := range conditionStatuses {
		val := float64(0)
		if cond != nil && cond.Status == s {
			val = 1
		}
		clusterCondition.WithLabelValues(name, namespace, condType, strings.ToLower(string(s))).Set(val)
	}
}

// initClusterMetrics creates empty metrics for a valkey cluster
func initClusterMetrics(name, namespace string) {
	for _, s := range valkeyiov1alpha1.ClusterStates {
		clusterStateInfo.WithLabelValues(name, namespace, string(s))
	}
	for _, s := range FailoverTypes {
		failoversTotal.WithLabelValues(name, namespace, s.String())
	}

	clusterShards.WithLabelValues(name, namespace)
	clusterShardsReady.WithLabelValues(name, namespace)
	slotMigrationBatchesTotal.WithLabelValues(name, namespace)

	for _, condType := range valkeyiov1alpha1.ClusterConditionTypes {
		setConditionSeries(name, namespace, condType, nil)
	}
}

// updateClusterMetrics sets the Prometheus gauges for a ValkeyCluster.
func updateClusterMetrics(cluster *valkeyiov1alpha1.ValkeyCluster) {
	name := cluster.Name
	ns := cluster.Namespace

	// Set info gauge: 1 for current state, 0 for all others
	for _, s := range valkeyiov1alpha1.ClusterStates {
		val := float64(0)
		if cluster.Status.State == s {
			val = 1
		}
		clusterStateInfo.WithLabelValues(name, ns, string(s)).Set(val)
	}

	clusterShards.WithLabelValues(name, ns).Set(float64(cluster.Status.Shards))
	clusterShardsReady.WithLabelValues(name, ns).Set(float64(cluster.Status.ReadyShards))

	// Export every condition, registered or not; a condition absent from
	// status.conditions reads 0 on all three status series.
	reported := make(map[string]*metav1.Condition, len(cluster.Status.Conditions))
	for i := range cluster.Status.Conditions {
		cond := &cluster.Status.Conditions[i]
		reported[cond.Type] = cond
	}
	for _, condType := range valkeyiov1alpha1.ClusterConditionTypes {
		setConditionSeries(name, ns, condType, reported[condType])
		delete(reported, condType)
	}
	for condType, cond := range reported {
		setConditionSeries(name, ns, condType, cond)
	}
}

// deleteClusterMetrics removes all metrics for a deleted ValkeyCluster.
func deleteClusterMetrics(name, namespace string) {
	for _, s := range valkeyiov1alpha1.ClusterStates {
		clusterStateInfo.DeleteLabelValues(name, namespace, string(s))
	}
	clusterShards.DeleteLabelValues(name, namespace)
	clusterShardsReady.DeleteLabelValues(name, namespace)
	failoversTotal.DeletePartialMatch(prometheus.Labels{labelValkeyCluster: name, labelTargetNamespace: namespace})
	slotMigrationBatchesTotal.DeleteLabelValues(name, namespace)
	clusterCondition.DeletePartialMatch(prometheus.Labels{labelValkeyCluster: name, labelTargetNamespace: namespace})
}
