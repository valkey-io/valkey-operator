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
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

// valkeyNodeResourceName returns the name used for resources
// owned by the given ValkeyNode.
func valkeyNodeResourceName(node *valkeyiov1alpha1.ValkeyNode) string {
	return resourcePrefix + node.Name
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
// The ConfigMap is named via config.go:getConfigMapName(node).
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
			Name:      GetServerConfigMapName(node.Name),
			Namespace: node.Namespace,
			Labels:    valkeyNodeLabels(node),
		},
		Data: map[string]string{
			"valkey.conf":        generateValkeyNodeConfig(node),
			"liveness-check.sh":  string(liveness),
			"readiness-check.sh": string(readiness),
		},
	}, nil
}

func valkeyNodePVCName(node *valkeyiov1alpha1.ValkeyNode) string {
	return valkeyNodeResourceName(node) + "-data"
}

func buildValkeyNodePVC(node *valkeyiov1alpha1.ValkeyNode) *corev1.PersistentVolumeClaim {
	if node.Spec.Persistence == nil {
		return nil
	}

	labels := valkeyNodeLabels(node)
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      valkeyNodePVCName(node),
			Namespace: node.Namespace,
			Labels:    labels,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: node.Spec.Persistence.Size,
				},
			},
			StorageClassName: node.Spec.Persistence.StorageClassName,
		},
	}
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

// valkeyAnnounceArgsAndEnv returns the --cluster-announce-* flags and matching
// env vars for the server container: pod IP by default, pod FQDN in Hostname mode.
func valkeyAnnounceArgsAndEnv(node *valkeyiov1alpha1.ValkeyNode) ([]string, []corev1.EnvVar) {
	podNameEnv := corev1.EnvVar{
		Name: "POD_NAME",
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				FieldPath: "metadata.name",
			},
		},
	}
	podIPEnv := corev1.EnvVar{
		Name: "POD_IP",
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				FieldPath: "status.podIP",
			},
		},
	}

	if node.Spec.Networking != nil && node.Spec.Networking.PreferredEndpointType == valkeyiov1alpha1.PreferredEndpointTypeHostname {
		// Requires pod.spec.subdomain = headless service name; set in buildValkeyNodePodTemplateSpec.
		clusterDomain := node.Spec.Networking.ClusterDomain
		if clusterDomain == "" {
			clusterDomain = "cluster.local"
		}
		fqdn := fmt.Sprintf("$(POD_NAME).%s.%s.svc.%s",
			headlessServiceName(node.Labels[LabelCluster]),
			node.Namespace,
			clusterDomain,
		)
		return []string{"--cluster-announce-hostname", fqdn}, []corev1.EnvVar{podNameEnv}
	}

	return []string{"--cluster-announce-ip", "$(POD_IP)"}, []corev1.EnvVar{podIPEnv}
}

// buildContainersDef builds the base containers definition for the ValkeyNode
// and applies any strategic merge patches from node.Spec.Containers.
func buildContainersDef(node *valkeyiov1alpha1.ValkeyNode) ([]corev1.Container, error) {
	image := DefaultImage
	if node.Spec.Image != "" {
		image = node.Spec.Image
	}

	announceArgs, announceEnv := valkeyAnnounceArgsAndEnv(node)

	containers := []corev1.Container{
		{
			Name:      "server",
			Image:     image,
			Resources: node.Spec.Resources,
			Command: append([]string{
				"valkey-server",
				"/config/valkey.conf",
			}, append(announceArgs, []string{
				"--primaryuser",
				replicationUser,
				"--primaryauth",
				"$(PRIMARY_AUTH)",
			}...)...),
			Env: append(announceEnv, corev1.EnvVar{
				Name: "PRIMARY_AUTH",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: getSystemPasswordSecretName(node.Labels[LabelCluster]),
						},
						Key: replicationUser,
					},
				},
			}),
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
							shellPath,
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
							shellPath,
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
							shellPath,
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

	// /data is always mounted (PVC when persistence is set, emptyDir otherwise)
	// so the server can write nodes.conf under readOnlyRootFilesystem.
	containers[0].VolumeMounts = append(containers[0].VolumeMounts, corev1.VolumeMount{
		Name:      dataVolumeName,
		MountPath: dataMountPath,
	})

	if node.Spec.TLS != nil {
		containers[0].VolumeMounts = append(containers[0].VolumeMounts, corev1.VolumeMount{
			Name:      tlsVolumeName,
			MountPath: tlsCertMountPath,
			ReadOnly:  true,
		})
		containers[0].Env = append(containers[0].Env,
			corev1.EnvVar{Name: "VALKEY_TLS_ENABLED", Value: "true"},
			corev1.EnvVar{Name: "VALKEY_TLS_CA_FILE", Value: tlsCertMountPath + "/" + tlsSecretKeyCA},
			corev1.EnvVar{Name: "VALKEY_TLS_CERT_FILE", Value: tlsCertMountPath + "/" + tlsSecretKeyCert},
			corev1.EnvVar{Name: "VALKEY_TLS_KEY_FILE", Value: tlsCertMountPath + "/" + tlsSecretKeyKey},
			corev1.EnvVar{Name: "VALKEY_TLS_ARGS", Value: fmt.Sprintf("--tls --cacert %s --cert %s --key %s",
				tlsCertMountPath+"/"+tlsSecretKeyCA, tlsCertMountPath+"/"+tlsSecretKeyCert, tlsCertMountPath+"/"+tlsSecretKeyKey)},
		)
	}

	// Use operator-managed custom user for probes
	clusterName := node.Labels[LabelCluster]
	probeUserSecret := operatorUserPasswordSecret(clusterName)
	if probeUserSecret != nil && probeUserSecret.Name != "" {
		containers[0].Env = append(containers[0].Env,
			corev1.EnvVar{Name: "VALKEY_USER", Value: operatorUser},
			corev1.EnvVar{Name: "VALKEYCLI_AUTH", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: probeUserSecret}},
		)
	}

	// Add exporter sidecar if enabled.
	if node.Spec.Exporter.Enabled {
		containers = append(containers, generateMetricsExporterContainerDef(node.Spec.Exporter, node.Labels[LabelCluster], node.Spec.TLS))
	}

	return mergePatchContainers(containers, node.Spec.Containers)
}

func buildShardTopologySpreadConstraints(node *valkeyiov1alpha1.ValkeyNode, labels map[string]string) []corev1.TopologySpreadConstraint {
	if len(node.Spec.TopologySpreadConstraints) == 0 {
		return nil
	}

	constraints := make([]corev1.TopologySpreadConstraint, len(node.Spec.TopologySpreadConstraints))
	clusterName := labels[LabelCluster]
	shardIndex := labels[LabelShardIndex]

	for i := range node.Spec.TopologySpreadConstraints {
		constraint := *node.Spec.TopologySpreadConstraints[i].DeepCopy()
		if constraint.LabelSelector == nil {
			constraint.LabelSelector = &metav1.LabelSelector{}
		}
		if constraint.LabelSelector.MatchLabels == nil {
			constraint.LabelSelector.MatchLabels = map[string]string{}
		}
		if clusterName != "" && !topologySpreadConstraintUsesKey(constraint, LabelCluster) {
			constraint.LabelSelector.MatchLabels[LabelCluster] = clusterName
		}
		if shardIndex != "" && !topologySpreadConstraintUsesKey(constraint, LabelShardIndex) {
			constraint.MatchLabelKeys = append(constraint.MatchLabelKeys, LabelShardIndex)
		}
		constraints[i] = constraint
	}

	return constraints
}

func topologySpreadConstraintUsesKey(constraint corev1.TopologySpreadConstraint, key string) bool {
	return labelSelectorUsesKey(constraint.LabelSelector, key) ||
		slices.Contains(constraint.MatchLabelKeys, key)
}

func labelSelectorUsesKey(selector *metav1.LabelSelector, key string) bool {
	if selector == nil {
		return false
	}
	if _, exists := selector.MatchLabels[key]; exists {
		return true
	}
	for _, expr := range selector.MatchExpressions {
		if expr.Key == key {
			return true
		}
	}
	return false
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
	configMapName := node.Spec.ServerConfigMapName
	if configMapName == "" {
		configMapName = GetServerConfigMapName(node.Name)
	}

	podSpec := corev1.PodSpec{
		Containers:                    containers,
		ImagePullSecrets:              node.Spec.ImagePullSecrets,
		NodeSelector:                  node.Spec.NodeSelector,
		Affinity:                      node.Spec.Affinity,
		Tolerations:                   node.Spec.Tolerations,
		PriorityClassName:             node.Spec.PriorityClassName,
		TopologySpreadConstraints:     buildShardTopologySpreadConstraints(node, labels),
		SecurityContext:               node.Spec.PodSecurityContext,
		TerminationGracePeriodSeconds: node.Spec.TerminationGracePeriodSeconds,
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

	if node.Spec.TLS != nil {
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name: tlsVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: node.Spec.TLS.Certificate.SecretName,
				},
			},
		})
	}

	// Only set in Hostname mode so IP-mode clusters aren't rolled on operator upgrade.
	if node.Spec.Networking != nil && node.Spec.Networking.PreferredEndpointType == valkeyiov1alpha1.PreferredEndpointTypeHostname {
		podSpec.Subdomain = headlessServiceName(node.Labels[LabelCluster])
	}

	// Back /data with a PVC when persistence is set; otherwise an emptyDir so
	// the cluster works on readOnlyRootFilesystem.
	dataVolume := corev1.Volume{Name: dataVolumeName}
	if node.Spec.Persistence != nil {
		dataVolume.VolumeSource = corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: valkeyNodePVCName(node),
			},
		}
	} else {
		dataVolume.VolumeSource = corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		}
	}
	podSpec.Volumes = append(podSpec.Volumes, dataVolume)

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
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RecreateDeploymentStrategyType,
			},
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
