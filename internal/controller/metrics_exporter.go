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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

// generateMetricsExporterContainerDef generates the container definition for the metrics exporter sidecar.
func generateMetricsExporterContainerDef(cluster *valkeyiov1alpha1.ValkeyCluster) corev1.Container {
	exporterImage := DefaultExporterImage
	if cluster.Spec.Exporter.Image != "" {
		exporterImage = cluster.Spec.Exporter.Image
	}
	return corev1.Container{
		Name:  "metrics-exporter",
		Image: exporterImage,
		Args:  []string{"--redis.addr=localhost:6379"},
		Ports: []corev1.ContainerPort{
			{
				Name:          "metrics",
				ContainerPort: DefaultExporterPort,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		LivenessProbe: &corev1.Probe{
			InitialDelaySeconds: 10,
			PeriodSeconds:       10,
			TimeoutSeconds:      3,
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/health",
					Port: intstr.FromInt(DefaultExporterPort),
				},
			},
		},
		ReadinessProbe: &corev1.Probe{
			InitialDelaySeconds: 5,
			PeriodSeconds:       1,
			TimeoutSeconds:      3,
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/health",
					Port: intstr.FromInt(DefaultExporterPort),
				},
			},
		},
		Resources: cluster.Spec.Exporter.Resources,
	}
}
