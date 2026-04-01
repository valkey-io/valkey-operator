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
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
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
	l := baseLabels(node.Name, "valkey-node")
	for _, key := range []string{
		LabelCluster,
		LabelShardIndex,
		LabelNodeIndex,
	} {
		if v, ok := node.Labels[key]; ok {
			l[key] = v
		}
	}
	return l
}

// buildValkeyNodeConfigMap builds a ConfigMap containing the embedded liveness
// and readiness probe scripts, plus an empty valkey.conf.
// The ConfigMap is named after valkeyNodeResourceName(node).
func buildValkeyNodeConfigMap(node *valkeyiov1alpha1.ValkeyNode) (*corev1.ConfigMap, error) {
	liveness, err := scripts.ReadFile("scripts/liveness-check.sh")
	if err != nil {
		return nil, fmt.Errorf("reading embedded liveness-check.sh: %w", err)
	}
	readiness, err := scripts.ReadFile("scripts/readiness-check.sh")
	if err != nil {
		return nil, fmt.Errorf("reading embedded readiness-check.sh: %w", err)
	}

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      valkeyNodeResourceName(node),
			Namespace: node.Namespace,
			Labels:    valkeyNodeLabels(node),
		},
		Data: map[string]string{
			"valkey.conf":        "",
			"liveness-check.sh":  string(liveness),
			"readiness-check.sh": string(readiness),
		},
	}, nil
}

// mergePatchContainers applies a strategic merge patch to base containers using
// patches as the patch source. Containers are matched by name; any patch
// container whose name matches a base container is merged into it, while patch
// containers with new names are appended in patch-list order.
func mergePatchContainers(base, patches []corev1.Container) ([]corev1.Container, error) {
	var output []corev1.Container

	patchByName := make(map[string]corev1.Container, len(patches))
	for _, c := range patches {
		patchByName[c.Name] = c
	}

	for _, c := range base {
		patch, ok := patchByName[c.Name]
		if !ok {
			output = append(output, c)
			continue
		}
		baseBytes, err := json.Marshal(c)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal JSON for container %s: %w", c.Name, err)
		}
		patchBytes, err := json.Marshal(patch)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal JSON for patch container %s: %w", c.Name, err)
		}
		merged, err := strategicpatch.StrategicMergePatch(baseBytes, patchBytes, corev1.Container{})
		if err != nil {
			return nil, fmt.Errorf("failed to generate merge patch for container %s: %w", c.Name, err)
		}
		var result corev1.Container
		if err := json.Unmarshal(merged, &result); err != nil {
			return nil, fmt.Errorf("failed to unmarshal merged container %s: %w", c.Name, err)
		}
		output = append(output, result)
		delete(patchByName, c.Name)
	}

	// Append any patch containers that did not match a base container, in
	// the original patch-list order.
	for _, c := range patches {
		if _, remaining := patchByName[c.Name]; remaining {
			output = append(output, c)
		}
	}

	return output, nil
}

// buildContainersDef builds the base containers definition for the ValkeyNode
// and applies any strategic merge patches from node.Spec.Containers.
func buildContainersDef(node *valkeyiov1alpha1.ValkeyNode) ([]corev1.Container, error) {
	image := DefaultImage
	if node.Spec.Image != "" {
		image = node.Spec.Image
	}

	containers := []corev1.Container{
		{
			Name:      "server",
			Image:     image,
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
		containers = append(containers, generateMetricsExporterContainerDef(node.Spec.Exporter, node.Labels))
	}

	return mergePatchContainers(containers, node.Spec.Containers)
}

// buildValkeyNodePodTemplateSpec constructs a PodTemplateSpec for a single
// Valkey node.
func buildValkeyNodePodTemplateSpec(node *valkeyiov1alpha1.ValkeyNode, labels map[string]string) (corev1.PodTemplateSpec, error) {
	containers, err := buildContainersDef(node)
	if err != nil {
		return corev1.PodTemplateSpec{}, err
	}

	// Use the explicitly provided ConfigMap name, or fall back to the default
	// resource name (which the controller creates automatically).
	configMapName := node.Spec.ScriptsConfigMapName
	if configMapName == "" {
		configMapName = valkeyNodeResourceName(node)
	}

	podSpec := corev1.PodSpec{
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
							Name: configMapName,
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
							Name: configMapName,
						},
					},
				},
			},
		},
	}

	if node.Spec.UsersACLSecretName != "" {
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name: "users-acl",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: node.Spec.UsersACLSecretName,
				},
			},
		})
		// Containers[0] is always the server container (exporter is appended after it).
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name:      "users-acl",
			MountPath: "/config/users",
			ReadOnly:  true,
		})
	}

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: labels,
		},
		Spec: podSpec,
	}, nil
}

// buildValkeyNodeDeployment constructs a single-replica Deployment for a
// ValkeyNode. This is used when node.Spec.WorkloadType is Deployment.
func buildValkeyNodeDeployment(node *valkeyiov1alpha1.ValkeyNode) (*appsv1.Deployment, error) {
	labels := valkeyNodeLabels(node)
	tmpl, err := buildValkeyNodePodTemplateSpec(node, labels)
	if err != nil {
		return nil, err
	}
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
			Template: tmpl,
		},
	}, nil
}

// buildValkeyNodeStatefulSet constructs a single-replica StatefulSet for a
// ValkeyNode. This is used when node.Spec.WorkloadType is StatefulSet (the
// default).
func buildValkeyNodeStatefulSet(node *valkeyiov1alpha1.ValkeyNode) (*appsv1.StatefulSet, error) {
	labels := valkeyNodeLabels(node)
	tmpl, err := buildValkeyNodePodTemplateSpec(node, labels)
	if err != nil {
		return nil, err
	}
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
			Template: tmpl,
		},
	}, nil
}
