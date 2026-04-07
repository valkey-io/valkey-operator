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
	"crypto/sha256"
	"embed"
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

const (
	scriptsHashKey = "valkey.io/script-hash"
	configHashKey  = "valkey.io/config-hash"
	configFileKey  = "valkey.conf"

	readinessScriptKey = "readiness-check.sh"
	livenessScriptKey  = "liveness-check.sh"

	// This hash should be updated whenever the contents of either script changes, which would
	// coincide with operator version bump.
	// $ cat internal/controller/scripts/{liveness-check.sh,readiness-check.sh} | sha256sum
	scriptsHash = "8531132f52ac311772dfcb45c107c34ab05e719a0df644cc332512277b564346"

	// Average-ish length of Valkey parameter + value
	averageParameterLength = 20
)

//go:embed scripts/*
var scripts embed.FS

func getServerConfigMapName(clusterName string) string {
	return "valkey-" + clusterName + "-config"
}

// Return a base config of parameters that users shouldn't be able to override
func getBaseConfig() map[string]string {
	return map[string]string{
		"cluster-enabled":      "yes",
		"protected-mode":       "no",
		"cluster-node-timeout": "2000",
		"aclfile":              "/config/users/users.acl",
	}
}

func buildServerConfig(cluster *valkeyiov1alpha1.ValkeyCluster) string {

	baseConfig := getBaseConfig()
	userConfig := cluster.Spec.Config

	// Build the config
	var configBuilder strings.Builder
	configBuilder.Grow((len(baseConfig) + len(userConfig)) * averageParameterLength)

	// User config goes first, and base config goes last. This prevents users from overriding
	// key parameters as Valkey uses the last value in the file.

	if len(userConfig) > 0 {
		writeConfigLine(&configBuilder, "#", "User Config")

		// Sort the config keys to keep consistent processing order
		sortedKeys := slices.Sorted(maps.Keys(userConfig))

		for _, param := range sortedKeys {
			writeConfigLine(&configBuilder, param, userConfig[param])
		}
	}

	// Add base config
	writeConfigLine(&configBuilder, "#", "Base Config")
	for param, val := range baseConfig {
		writeConfigLine(&configBuilder, param, val)
	}

	return configBuilder.String()
}

// Create or update a default valkey.conf
// If additional config is provided, append to the default map
func (r *ValkeyClusterReconciler) upsertConfigMap(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) error {

	log := logf.FromContext(ctx)

	// Embed readiness check script
	readiness, err := scripts.ReadFile("scripts/readiness-check.sh")
	if err != nil {
		return fmt.Errorf("reading embedded readiness-check.sh: %w", err)
	}

	// Embed liveness check script
	liveness, err := scripts.ReadFile("scripts/liveness-check.sh")
	if err != nil {
		return fmt.Errorf("reading embedded liveness-check.sh: %w", err)
	}

	// Get the new server config
	newServerConfig := buildServerConfig(cluster)

	// Calculate hash of constructed configMap contents (ie: updated scripts, changed/added parameters)
	newServerConfigHash := fmt.Sprintf("%x", sha256.Sum256([]byte(newServerConfig)))

	// Look for, and fetch existing configMap for this cluster
	serverConfigMapName := getServerConfigMapName(cluster.Name)
	serverConfigMap := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      serverConfigMapName,
		Namespace: cluster.Namespace,
	}, serverConfigMap); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "failed to fetch server configmap")
			return err
		}

		// ConfigMap not found; This happens on cluster init
		log.V(2).Info("creating server configMap", "name", serverConfigMapName)

		// Create configMap object with contents
		serverConfigMap.ObjectMeta = metav1.ObjectMeta{
			Name:      serverConfigMapName,
			Namespace: cluster.Namespace,
			Labels:    labels(cluster),
			Annotations: map[string]string{
				configHashKey:  newServerConfigHash,
				scriptsHashKey: scriptsHash,
			},
		}
		serverConfigMap.Data = map[string]string{
			readinessScriptKey: string(readiness),
			livenessScriptKey:  string(liveness),
			configFileKey:      newServerConfig,
		}

		// Register ownership of the configMap
		if err := controllerutil.SetControllerReference(cluster, serverConfigMap, r.Scheme); err != nil {
			log.Error(err, "Failed to grab ownership of server configMap")
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "ConfigMapCreationFailed", "UpsertConfigMap", "Failed to grab ownership of server configMap: %v", err)
			return err
		}

		// Create the configMap
		if err := r.Create(ctx, serverConfigMap); err != nil {
			log.Error(err, "Failed to create server configMap")
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "ConfigMapCreationFailed", "UpsertConfigMap", "Failed to create server configMap: %v", err)
			return err
		}

		r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "ConfigMapCreated", "UpsertConfigMap", "Created server configMap")

		// All good; new configMap with contents created
		return nil
	}

	// ConfigMap exists

	// Compare scripts hash in existing configMap to const value in operator; update scripts contents if different
	updatedScripts := upsertAnnotation(serverConfigMap, scriptsHashKey, scriptsHash)
	if updatedScripts {
		log.V(1).Info("updated readiness, and liveness scripts")
		serverConfigMap.Data[readinessScriptKey] = string(readiness)
		serverConfigMap.Data[livenessScriptKey] = string(liveness)
	}

	// If the generated config contents hash (from above) matches the hash of the current
	// config contents, and we did not update the scripts contents, exit early
	if !updatedScripts && !upsertAnnotation(serverConfigMap, configHashKey, newServerConfigHash) {
		log.V(1).Info("server config unchanged")
		return nil
	}

	// Update the configMap with the generated config contents
	serverConfigMap.Data[configFileKey] = newServerConfig

	// Update
	if err := r.Update(ctx, serverConfigMap); err != nil {
		log.Error(err, "Failed to update server configMap")
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "ConfigMapUpdateFailed", "UpsertConfigMap", "Failed to update server configMap: %v", err)
		return err
	}

	r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "ConfigMapUpdated", "UpsertConfigMap", "Synchronized server configMap")

	// All is good. configMap was updated with new contents.
	return nil
}

// Helper function to write a config line in the form of "parameter value\n" to a strings.Builder
func writeConfigLine(builder *strings.Builder, name, value string) {
	builder.WriteString(name)
	builder.WriteString(" ")
	builder.WriteString(value)
	builder.WriteString("\n")
}
