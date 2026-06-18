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
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
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
)

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
}
