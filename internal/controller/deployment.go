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
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

func generateContainersDef(cluster *valkeyiov1alpha1.ValkeyCluster) []corev1.Container {
	image := DefaultImage
	if cluster.Spec.Image != "" {
		image = cluster.Spec.Image
	}
	containers := []corev1.Container{
		{
			Name:      "valkey-server",
			Image:     image,
			Resources: cluster.Spec.Resources,
			Command: []string{
				"valkey-server",
				"/config/valkey.conf",
			},
			Ports: []corev1.ContainerPort{
				{
					Name:          "client",
					ContainerPort: DefaultPort,
				},
				{
					Name:          "cluster-bus",
					ContainerPort: DefaultClusterBusPort,
				},
			},
			StartupProbe: &corev1.Probe{
				InitialDelaySeconds: 5,
				PeriodSeconds:       5,
				FailureThreshold:    20,
				TimeoutSeconds:      5,
				SuccessThreshold:    1,
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{
							"/bin/bash",
							"-c",
							"/scripts/liveness-check.sh",
						},
					},
				},
			},
			LivenessProbe: &corev1.Probe{
				InitialDelaySeconds: 5,
				PeriodSeconds:       5,
				FailureThreshold:    5,
				TimeoutSeconds:      5,
				SuccessThreshold:    1,
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{
							"/bin/bash",
							"-c",
							"/scripts/liveness-check.sh",
						},
					},
				},
			},
			ReadinessProbe: &corev1.Probe{
				InitialDelaySeconds: 5,
				PeriodSeconds:       5,
				FailureThreshold:    5,
				TimeoutSeconds:      2,
				SuccessThreshold:    1,
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{
							"/bin/bash",
							"-c",
							"/scripts/readiness-check.sh",
						},
					},
				},
			},
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      "scripts",
					MountPath: "/scripts",
				},
				{
					Name:      "valkey-conf",
					MountPath: "/config",
					ReadOnly:  true,
				},
			},
		},
	}

	// Add exporter sidecar if enabled
	if cluster.Spec.Exporter.Enabled {
		containers = append(containers, generateMetricsExporterContainerDef(cluster))
	}
	return containers
}

// createClusterDeployment builds a single-pod Deployment for a Valkey node.
//
// Each Deployment manages exactly one Pod (Replicas=1) and represents one
// logical node in the Valkey cluster. The shardIndex and role parameters are
// stamped as Kubernetes labels on both the Deployment and the Pod template:
//
//	valkey.io/shard-index = "<shardIndex>"
//	valkey.io/role        = "primary" | "replica"
//
// These labels serve two purposes:
//  1. The reconciler reads them at runtime (via podRoleAndShard) to decide what
//     Valkey command to issue for a pending node—CLUSTER ADDSLOTSRANGE for
//     primaries, CLUSTER REPLICATE for replicas—without relying on potentially
//     stale cluster topology.
//  2. The labels make each Deployment's Selector unique to its (shard, role)
//     pair, ensuring no two Deployments fight over the same Pod.
func createClusterDeployment(cluster *valkeyiov1alpha1.ValkeyCluster, shardIndex int, role string) *appsv1.Deployment {
	containers := generateContainersDef(cluster)

	nodeLabels := labels(cluster)
	nodeLabels[LabelShardIndex] = strconv.Itoa(shardIndex)
	nodeLabels[LabelRole] = role

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: cluster.Name + "-",
			Namespace:    cluster.Namespace,
			Labels:       nodeLabels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: func(i int32) *int32 { return &i }(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: nodeLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: nodeLabels,
				},
				Spec: corev1.PodSpec{
					Containers:   containers,
					Affinity:     cluster.Spec.Affinity,
					NodeSelector: cluster.Spec.NodeSelector,
					Tolerations:  cluster.Spec.Tolerations,
					Volumes: []corev1.Volume{
						{
							Name: "scripts",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: cluster.Name,
									},
									DefaultMode: func(i int32) *int32 { return &i }(0755),
								},
							},
						},
						{
							Name: "valkey-conf",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: cluster.Name,
									},
								},
							},
						},
					},
				},
			},
		},
	}
	return deployment
}
