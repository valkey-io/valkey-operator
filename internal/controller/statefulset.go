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
	"errors"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

func createClusterStatefulSet(cluster *valkeyiov1alpha1.ValkeyCluster, replicas int32, name string) *appsv1.StatefulSet {
	containers := generateContainersDef(cluster)

	labelsWithIdentity := labels(cluster)
	if shardIndex, replicaIndex, err := parseShardReplicaFromStatefulSetName(name); err == nil {
		labelsWithIdentity = copyMap(labelsWithIdentity)
		labelsWithIdentity[ShardIndexLabelKey] = strconv.Itoa(shardIndex)
		labelsWithIdentity[ReplicaIndexLabelKey] = strconv.Itoa(replicaIndex)
	}

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cluster.Namespace,
			Labels:    labels(cluster),
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: cluster.Name,
			Replicas:    func(i int32) *int32 { return &i }(replicas),
			Selector: &metav1.LabelSelector{
				MatchLabels: labels(cluster),
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labelsWithIdentity,
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
						{
							Name: "data",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					},
				},
			},
		},
	}
}

func parseShardReplicaFromStatefulSetName(name string) (int, int, error) {
	lastDash := strings.LastIndex(name, "-")
	if lastDash == -1 || lastDash == len(name)-1 {
		return 0, 0, errors.New("statefulset name missing replica suffix")
	}
	replica, err := strconv.Atoi(name[lastDash+1:])
	if err != nil {
		return 0, 0, err
	}
	rest := name[:lastDash]
	secondDash := strings.LastIndex(rest, "-")
	if secondDash == -1 || secondDash == len(rest)-1 {
		return 0, 0, errors.New("statefulset name missing shard suffix")
	}
	shard, err := strconv.Atoi(rest[secondDash+1:])
	if err != nil {
		return 0, 0, err
	}
	return shard, replica, nil
}

func copyMap(src map[string]string) map[string]string {
	dst := map[string]string{}
	for key, value := range src {
		dst[key] = value
	}
	return dst
}
