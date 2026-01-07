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
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

func attachVolumesToCluster(ctx context.Context, c client.Client, cluster *valkeyiov1alpha1.ValkeyCluster) error {

	// Volume containing health, and liveliness scripts
	scriptsVolume := corev1.Volume{
		Name: "scripts",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: cluster.Name,
				},
				DefaultMode: func(i int32) *int32 { return &i }(0755),
			},
		},
	}

	// Volume containing default Valkey configuration
	defaultConfigVolume := corev1.Volume{
		Name: "valkey-conf",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: cluster.Name,
				},
			},
		},
	}

	// User config volume
	userConfigVolume, err := getUserConfigVolume(ctx, c, cluster)
	if err != nil {
		return err
	}

	// Add volumes to spec
	cluster.Spec.Volumes = []corev1.Volume{
		scriptsVolume,
		defaultConfigVolume,
		userConfigVolume,
	}

	return nil
}

// Discover user-created Valkey configuration. This can be either a Secret, or ConfigMap, created by the user
func getUserConfigVolume(ctx context.Context, c client.Client, cluster *valkeyiov1alpha1.ValkeyCluster) (corev1.Volume, error) {
	log := logf.FromContext(ctx)

	configMapName := cluster.Name + "-conf"
	userConfigFilter := types.NamespacedName{
		Namespace: cluster.Namespace,
		Name:      configMapName,
	}

	// First, look for a Secret named "$clusterName-conf"
	err := c.Get(ctx, userConfigFilter, &corev1.Secret{})
	if err == nil {
		log.V(1).Info("found user-created secret config")
		return getSecretConfigVolume("user-conf", configMapName, false), nil
	}
	if !apierrors.IsNotFound(err) {
		log.Error(err, "failed to search for user config Secret")
		return corev1.Volume{}, err
	}

	// Next, look for a ConfigMap named "$clusterName-conf"
	err = c.Get(ctx, userConfigFilter, &corev1.ConfigMap{})
	if err == nil {
		log.V(1).Info("found user-created configMap")
		return getConfigVolume("user-conf", configMapName, false), nil
	}
	if !apierrors.IsNotFound(err) {
		log.Error(err, "failed to search for user config ConfigMap")
		return corev1.Volume{}, err
	}

	// If neither found, create empty (optional) config-volume
	log.V(1).Info("using empty configMap")
	return getConfigVolume("user-conf", configMapName, true), nil
}

func getConfigVolume(configVolumeName, configMapName string, optional bool) corev1.Volume {
	return corev1.Volume{
		Name: configVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: configMapName,
				},
				Optional: func(b bool) *bool { return &b }(optional),
			},
		},
	}
}

func getSecretConfigVolume(configVolumeName, configMapName string, optional bool) corev1.Volume {
	return corev1.Volume{
		Name: configVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: configMapName,
				Optional:   func(b bool) *bool { return &b }(optional),
			},
		},
	}
}
