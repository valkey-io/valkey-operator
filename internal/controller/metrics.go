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
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

var (
	clusterInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "valkey_operator_cluster_info",
			Help: "Information about a ValkeyCluster. Value is always 1, state is in the label.",
		},
		[]string{"cluster", "namespace", "state"},
	)

	clusterShards = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "valkey_operator_cluster_shards",
			Help: "Total number of shards in a ValkeyCluster.",
		},
		[]string{"cluster", "namespace"},
	)

	clusterShardsReady = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "valkey_operator_cluster_shards_ready",
			Help: "Number of ready shards in a ValkeyCluster.",
		},
		[]string{"cluster", "namespace"},
	)

	failoversTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "valkey_operator_failovers_total",
			Help: "Total number of failover events.",
		},
		[]string{"cluster", "namespace", "type"},
	)

	slotMigrationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "valkey_operator_slot_migrations_total",
			Help: "Total number of slot migration batches completed.",
		},
		[]string{"cluster", "namespace"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		clusterInfo,
		clusterShards,
		clusterShardsReady,
		failoversTotal,
		slotMigrationsTotal,
	)
}

// clusterStates lists all possible ValkeyCluster states for metric cleanup.
var clusterStates = []string{"Ready", "Reconciling", "Degraded", "Failed"}

// updateClusterMetrics sets the Prometheus gauges for a ValkeyCluster.
func updateClusterMetrics(cluster *valkeyiov1alpha1.ValkeyCluster) {
	name := cluster.Name
	ns := cluster.Namespace

	// Set info gauge: 1 for current state, 0 for all others
	for _, s := range clusterStates {
		val := float64(0)
		if string(cluster.Status.State) == s {
			val = 1
		}
		clusterInfo.WithLabelValues(name, ns, s).Set(val)
	}

	clusterShards.WithLabelValues(name, ns).Set(float64(cluster.Status.Shards))
	clusterShardsReady.WithLabelValues(name, ns).Set(float64(cluster.Status.ReadyShards))
}
