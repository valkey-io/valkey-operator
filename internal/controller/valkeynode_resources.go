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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

// valkeyNodeResourceName returns the name used for resources
// owned by the given ValkeyNode.
func valkeyNodeResourceName(node *valkeyiov1alpha1.ValkeyNode) string {
	return "valkey-" + node.Name
}

// valkeyNodeLabels returns the standard Kubernetes recommended labels for
// child resources of the given ValkeyNode.
func valkeyNodeLabels(node *valkeyiov1alpha1.ValkeyNode) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "valkey",
		"app.kubernetes.io/instance":   node.Name,
		"app.kubernetes.io/managed-by": "valkey-operator",
	}
}

// buildContainersDef builds the containers definition for the ValkeyNode.
func buildContainersDef(node *valkeyiov1alpha1.ValkeyNode) []corev1.Container {
	containers := []corev1.Container{
		{
			Name:      "server",
			Image:     node.Spec.Image,
			Resources: node.Spec.Resources,
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

	// Add exporter sidecar if enabled.
	if node.Spec.Exporter.Enabled {
		containers = append(containers, generateMetricsExporterContainerDef(node.Spec.Exporter))
	}

	return containers
}

// buildValkeyNodePodTemplateSpec constructs a PodTemplateSpec for a single
// Valkey node.
func buildValkeyNodePodTemplateSpec(node *valkeyiov1alpha1.ValkeyNode, labels map[string]string) corev1.PodTemplateSpec {
	containers := buildContainersDef(node)

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: labels,
		},
		Spec: corev1.PodSpec{
			Containers:   containers,
			NodeSelector: node.Spec.NodeSelector,
			Affinity:     node.Spec.Affinity,
			Tolerations:  node.Spec.Tolerations,
			Volumes: []corev1.Volume{
				{
					Name: "scripts",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: node.Spec.ScriptsConfigMapName,
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
								Name: node.Spec.ScriptsConfigMapName,
							},
						},
					},
				},
			},
		},
	}
}

// buildValkeyNodeDeployment constructs a single-replica Deployment for a
// ValkeyNode. This is used when node.Spec.WorkloadType is Deployment.
func buildValkeyNodeDeployment(node *valkeyiov1alpha1.ValkeyNode) *appsv1.Deployment {
	labels := valkeyNodeLabels(node)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      valkeyNodeResourceName(node),
			Namespace: node.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: func(i int32) *int32 { return &i }(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: buildValkeyNodePodTemplateSpec(node, labels),
		},
	}
}

// buildValkeyNodeStatefulSet constructs a single-replica StatefulSet for a
// ValkeyNode. This is used when node.Spec.WorkloadType is StatefulSet (the
// default).
func buildValkeyNodeStatefulSet(node *valkeyiov1alpha1.ValkeyNode) *appsv1.StatefulSet {
	labels := valkeyNodeLabels(node)
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      valkeyNodeResourceName(node),
			Namespace: node.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    func(i int32) *int32 { return &i }(1),
			ServiceName: valkeyNodeResourceName(node),
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: buildValkeyNodePodTemplateSpec(node, labels),
		},
	}
}
