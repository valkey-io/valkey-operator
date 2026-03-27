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
	"cmp"
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

func getConfigMapName(clusterName string) string {
	return clusterName + "-config"
}

// Return a base config of parameters that users shouldn't be able to override
func getBaseConfig() string {
	return `# Base operator config
cluster-enabled yes
protected-mode no
cluster-node-timeout 2000
aclfile /config/users/users.acl
`
}

func getUserConfig(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) string {

	specConfig := cluster.Spec.Config

	// Exit early if nothing
	if len(specConfig) == 0 {
		return ""
	}

	log := logf.FromContext(ctx)

	// Build the config
	var configBuilder strings.Builder
	configBuilder.Grow(len(specConfig) * averageParameterLength)
	writeConfigLine(&configBuilder, "#", "Extra Config")

	// Sort the config keys to keep consistent processing order
	sortedKeys := slices.Sorted(maps.Keys(specConfig))

	for _, param := range sortedKeys {

		if slices.Contains(valkeyiov1alpha1.NonUserOverrideConfigParameters, param) {
			log.Error(nil, "Prohibited valkey server config", "parameter", param)
			continue
		}

		writeConfigLine(&configBuilder, param, specConfig[param])
	}

	return configBuilder.String()
}

// Build config file contents for any modules
func getModulesConfig(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, r *ValkeyClusterReconciler) string {

	modulesConfig := cluster.Spec.Modules

	// Exit early if no modules
	if len(modulesConfig) == 0 {
		return ""
	}

	log := logf.FromContext(ctx)

	// Build each module's config
	var moduleBuilder strings.Builder
	moduleBuilder.Grow(len(modulesConfig) * averageParameterLength)
	writeConfigLine(&moduleBuilder, "#", "Modules")

	// Sort using module names to keep consistent processing order, as some modules might depend on others
	slices.SortFunc(modulesConfig, func(a, b valkeyiov1alpha1.ValkeyModule) int {
		return cmp.Compare(a.Name, b.Name)
	})

	for _, module := range modulesConfig {

		// Build the individual config parameters for this module, which could be a
		// raw value, in a ConfigMap, or in a Secret
		cfg, err := buildModuleConfig(ctx, module, cluster.Namespace, r)
		if err != nil {
			log.Error(err, "error building module config", "module", module.Name)

			// This module had issues with its config, but continue processing other modules
			continue
		}

		// Module entry
		writeConfigLine(&moduleBuilder, "#", module.Name)
		writeConfigLine(&moduleBuilder, "loadmodule", module.Path)

		// Module's config lines
		moduleBuilder.WriteString(cfg)
		moduleBuilder.WriteString("\n")
	}

	return moduleBuilder.String()
}

// Module config parameters and values can be in raw, string form,
// or stored in a ConfigMap, or stored in a Secret.
func buildModuleConfig(ctx context.Context, module valkeyiov1alpha1.ValkeyModule, namespace string, r *ValkeyClusterReconciler) (string, error) {

	var moduleConfigBuilder strings.Builder

	// Individual config parameters for this module
	for _, config := range module.Config {

		if len(config.Value) > 0 {

			// Simple, raw value
			writeConfigLine(&moduleConfigBuilder, config.Name, config.Value)

		} else if config.ValueFrom != nil {

			// Value is stored in ConfigMap, or Secret
			var val string
			var err error

			if keyRef := config.ValueFrom.ConfigMapKeyRef; keyRef != nil {

				val, err = r.getConfigValue(ctx, namespace, keyRef.Name, keyRef.Key, false)

			} else if keyRef := config.ValueFrom.SecretKeyRef; keyRef != nil {

				val, err = r.getConfigValue(ctx, namespace, keyRef.Name, keyRef.Key, true)

			} else {
				return "", fmt.Errorf("no reference to ConfigMap/Secret for module config parameter %q", config.Name)
			}

			if err != nil {
				return "", fmt.Errorf("failed to retrieve module config %q: %w", config.Name, err)
			}

			writeConfigLine(&moduleConfigBuilder, config.Name, val)

		} else {
			return "", fmt.Errorf("missing value for module config parameter %q", config.Name)
		}
	}

	return moduleConfigBuilder.String(), nil
}

func (r *ValkeyClusterReconciler) getConfigValue(ctx context.Context, namespace, name, key string, isSecret bool) (string, error) {

	if name == "" || key == "" {
		return "", fmt.Errorf("name and key must not be empty")
	}

	if isSecret {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      name,
			Namespace: namespace,
		}, secret); err != nil {
			return "", fmt.Errorf("failed to locate module secret %q: %w", name, err)
		}

		if val, exists := secret.Data[key]; exists {
			return string(val), nil
		}

		return "", fmt.Errorf("key %q not found in secret %q", key, name)
	}

	configMap := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      name,
		Namespace: namespace,
	}, configMap); err != nil {
		return "", fmt.Errorf("failed to locate module configmap %q: %w", name, err)
	}

	if val, exists := configMap.Data[key]; exists {
		return val, nil
	}

	return "", fmt.Errorf("key %q not found in configmap %q", key, name)
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

	// Get base config
	var newConfigBuilder strings.Builder
	newConfigBuilder.WriteString(getBaseConfig())

	// User-provided config from spec
	newConfigBuilder.WriteString(getUserConfig(ctx, cluster))

	// Modules config
	newConfigBuilder.WriteString(getModulesConfig(ctx, cluster, r))

	// Final string version of the config
	newServerConfig := newConfigBuilder.String()

	// Calculate hash of constructed configMap contents (ie: updated scripts, changed/added parameters)
	newServerConfigHash := fmt.Sprintf("%x", sha256.Sum256([]byte(newServerConfig)))

	// Look for, and fetch existing configMap for this cluster
	serverConfigMapName := getConfigMapName(cluster.Name)
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
