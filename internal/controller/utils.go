package controller

import (
	"fmt"
	"maps"

	valkeyv1 "valkey.io/valkey-operator/api/v1alpha1"
)

func labels(cluster *valkeyv1.ValkeyCluster, shard int, salt string) map[string]string {
	if cluster.Labels == nil {
		cluster.Labels = make(map[string]string)
	}
	l := maps.Clone(cluster.Labels)
	l["app.kubernetes.io/name"] = "valkey"
	l["app.kubernetes.io/instance"] = cluster.Name
	l["app.kubernetes.io/managed-by"] = "valkey-operator"
	l["app.kubernetes.io/part-of"] = "valkey"
	l["app.kubernetes.io/component"] = "valkey-cluster"
	if shard >= 0 {
		l["valkey.io/shard"] = fmt.Sprintf("%d", shard)
	}
	if salt != "" {
		l["valkey.io/salt"] = salt
	}
	return l
}

func l(cluster *valkeyv1.ValkeyCluster) map[string]string {
	return labels(cluster, -1, "")
}

func annotations(cluster *valkeyv1.ValkeyCluster) map[string]string {
	return maps.Clone(cluster.Annotations)
}
