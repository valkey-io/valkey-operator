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
	"k8s.io/apimachinery/pkg/api/resource"
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
			Name:            "valkey-server",
			Image:           image,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Resources:       cluster.Spec.Resources,
			Command: []string{
				"valkey-server",
				"/config/valkey.conf",
			},
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: func(b bool) *bool { return &b }(false),
				RunAsNonRoot:             func(b bool) *bool { return &b }(true),
				RunAsUser:                func(i int64) *int64 { return &i }(1001),
				Capabilities: &corev1.Capabilities{
					Drop: []corev1.Capability{"ALL"},
				},
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
				{
					Name:      "data",
					MountPath: "/data",
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

func createClusterDeployment(cluster *valkeyiov1alpha1.ValkeyCluster) *appsv1.Deployment {
	containers := generateContainersDef(cluster)

	volumes := []corev1.Volume{
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
	}

	// Add emptyDir volume for data if persistent storage is not enabled
	if !cluster.Spec.Storage.Enabled {
		volumes = append(volumes, corev1.Volume{
			Name: "data",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		})
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: cluster.Name + "-",
			Namespace:    cluster.Namespace,
			Labels:       labels(cluster),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: func(i int32) *int32 { return &i }(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: labels(cluster),
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels(cluster),
				},
				Spec: corev1.PodSpec{
					Containers:   containers,
					Affinity:     cluster.Spec.Affinity,
					NodeSelector: cluster.Spec.NodeSelector,
					Tolerations:  cluster.Spec.Tolerations,
					Volumes:      volumes,
				},
			},
		},
	}
	return deployment
}

func createClusterStatefulSet(cluster *valkeyiov1alpha1.ValkeyCluster) *appsv1.StatefulSet {
	containers := generateContainersDef(cluster)

	// Optional init container to fix volume permissions
	initContainers := []corev1.Container{}
	if cluster.Spec.VolumePermissions && cluster.Spec.Storage.Enabled {
		image := DefaultImage
		if cluster.Spec.Image != "" {
			image = cluster.Spec.Image
		}
		initContainers = append(initContainers, corev1.Container{
			Name:            "volume-permissions",
			Image:           image,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command: []string{
				"/bin/chown",
				"-R",
				"1001:1001",
				"/data",
			},
			SecurityContext: &corev1.SecurityContext{
				RunAsUser: func(i int64) *int64 { return &i }(0),
			},
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      "data",
					MountPath: "/data",
				},
			},
		})
	}

	// Define volumeClaimTemplates for persistent storage
	volumeClaimTemplates := []corev1.PersistentVolumeClaim{}
	if cluster.Spec.Storage.Enabled {
		size := "1Gi"
		if cluster.Spec.Storage.Size != "" {
			size = cluster.Spec.Storage.Size
		}

		accessModes := []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
		if len(cluster.Spec.Storage.AccessModes) > 0 {
			accessModes = cluster.Spec.Storage.AccessModes
		}

		pvc := corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name: "data",
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: accessModes,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse(size),
					},
				},
			},
		}

		if cluster.Spec.Storage.StorageClassName != nil {
			pvc.Spec.StorageClassName = cluster.Spec.Storage.StorageClassName
		}

		volumeClaimTemplates = append(volumeClaimTemplates, pvc)
	}

	volumes := []corev1.Volume{
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
	}

	// Add emptyDir volume for data if persistent storage is not enabled
	if !cluster.Spec.Storage.Enabled {
		volumes = append(volumes, corev1.Volume{
			Name: "data",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		})
	}

	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: cluster.Name + "-",
			Namespace:    cluster.Namespace,
			Labels:       labels(cluster),
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: cluster.Name,
			Replicas:    func(i int32) *int32 { return &i }(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: labels(cluster),
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels(cluster),
				},
				Spec: corev1.PodSpec{
					InitContainers: initContainers,
					Containers:     containers,
					Affinity:       cluster.Spec.Affinity,
					NodeSelector:   cluster.Spec.NodeSelector,
					Tolerations:    cluster.Spec.Tolerations,
					Volumes:        volumes,
					SecurityContext: &corev1.PodSecurityContext{
						FSGroup:    func(i int64) *int64 { return &i }(1001),
						RunAsUser:  func(i int64) *int64 { return &i }(1001),
						RunAsGroup: func(i int64) *int64 { return &i }(1001),
					},
				},
			},
			VolumeClaimTemplates: volumeClaimTemplates,
		},
	}
	return statefulSet
}
