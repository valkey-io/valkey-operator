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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

const (
	configHashKey = "valkey.io/config-hash"
	configFileKey = "valkey.conf"
)

//go:embed scripts/*
var scripts embed.FS

func getConfigMapName(clusterName string) string {
	return clusterName + "-config"
}

// Create or update a default valkey.conf
// If additional config is provided, append to the default map
func (r *ValkeyClusterReconciler) upsertConfigMap(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) error {

	log := logf.FromContext(ctx)

	// Hash writer for embedded scripts, and configuration parameters
	hashConfigContents := sha256.New()

	// Embed readiness check script
	readiness, err := scripts.ReadFile("scripts/readiness-check.sh")
	if err != nil {
		return err
	}
	hashConfigContents.Write(readiness)

	// Embed liveness check script
	liveness, err := scripts.ReadFile("scripts/liveness-check.sh")
	if err != nil {
		return err
	}
	hashConfigContents.Write(liveness)

	// Base config
	serverConfig := `# Base operator config
cluster-enabled yes
protected-mode no
cluster-node-timeout 2000
`

	// Local copy
	specConfig := cluster.Spec.ValkeySpec.Configuration

	// If there are any user-defined config parameters
	if len(specConfig) > 0 {

		// Sort the config keys to keep consistent processing order
		sortedKeys := slices.Sorted(maps.Keys(specConfig))

		// Build the config
		serverConfig += "\n# Extra config\n"
		for _, k := range sortedKeys {

			if slices.Contains(valkeyiov1alpha1.NonUserOverrideConfigParameters, k) {
				log.Error(nil, "Prohibited valkey server config", "parameter", k)
				r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "ConfigMapUpdateFailed", "UpsertConfigMap", "Prohibited config: %v", k)
				continue
			}

			serverConfig += k + " " + specConfig[k] + "\n"
		}
	}

	// Look for, and fetch existing configMap for this cluster
	serverConfigMapName := getConfigMapName(cluster.Name)
	needCreateConfigMap := false

	serverConfigMap := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      serverConfigMapName,
		Namespace: cluster.Namespace,
	}, serverConfigMap); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "failed to fetch server configmap")
			return err
		}

		// ConfigMap not found. Create configMap object with contents
		needCreateConfigMap = true
		log.V(2).Info("creating server configMap", "name", serverConfigMapName)

		serverConfigMap.ObjectMeta = metav1.ObjectMeta{
			Name:      serverConfigMapName,
			Namespace: cluster.Namespace,
			Labels:    labels(cluster),
		}
		serverConfigMap.Data = map[string]string{
			"readiness-check.sh": string(readiness),
			"liveness-check.sh":  string(liveness),
			configFileKey:        serverConfig,
		}
	}

	// Register ownership of the configMap
	if err := controllerutil.SetControllerReference(cluster, serverConfigMap, r.Scheme); err != nil {
		log.Error(err, "Failed to grab ownership of server configMap")
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "ConfigMapCreationFailed", "UpsertConfigMap", "Failed to grab ownership of server configMap: %v", err)
		return err
	}

	// Calculate hash of existing config parameters
	origServerConfigHash := fmt.Sprintf("%x", sha256.Sum256([]byte(serverConfigMap.Data[configFileKey])))

	// Calculate hash of new config parameters
	hashConfigContents.Write([]byte(serverConfig))
	newServerConfigHash := fmt.Sprintf("%x", hashConfigContents.Sum(nil))

	// Was the configMap changed? This is an invalid action, as users should modify
	// the ValkeyCluster CR to change parameters, not the configMap itself.
	origConfigModified := !hasAnnotation(serverConfigMap, configHashKey, origServerConfigHash)

	// Compare hash to the one already attached to the configMap (if previously exists)
	needsUpdate := upsertAnnotation(serverConfigMap, configHashKey, newServerConfigHash)

	// Original config is not modified, and new config doesn't change anything
	if !origConfigModified && !needsUpdate {
		log.V(1).Info("server config unchanged")
		return nil
	}

	// In all other cases (ie: user modified configMap, or modified CR),
	// update the config with the newly changed config. Also sync the
	// check scripts in case they are updated in a later operator version.
	serverConfigMap.Data["readiness-check.sh"] = string(readiness)
	serverConfigMap.Data["liveness-check.sh"] = string(liveness)
	serverConfigMap.Data[configFileKey] = serverConfig

	// Need to create new configMap
	if needCreateConfigMap {
		if err := r.Create(ctx, serverConfigMap); err != nil {
			log.Error(err, "Failed to create server configMap")
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "ConfigMapCreationFailed", "UpsertConfigMap", "Failed to create server configMap: %v", err)
			return err
		} else {
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "ConfigMapCreated", "UpsertConfigMap", "Created server configMap")
			return nil
		}
	}

	// Otherwise, update it
	if err := r.Update(ctx, serverConfigMap); err != nil {
		log.Error(err, "Failed to update server configMap")
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "ConfigMapUpdateFailed", "UpsertConfigMap", "Failed to update server configMap: %v", err)
		return err
	}

	r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "ConfigMapUpdated", "UpsertConfigMap", "Synchronized server configMap")

	// All is good. Server configMap will be auto-mounted in the deployment
	return nil
}
