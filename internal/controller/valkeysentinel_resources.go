/*
Copyright 2026 Valkey Contributors.

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
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

const (
	// LabelSentinel selects the pods of one ValkeySentinel.
	// It is separate from LabelCluster so a ValkeySentinel never selects data pods of a same-named Valkey or ValkeyCluster.
	LabelSentinel = "valkey.io/sentinel"

	sentinelResourcePrefix = "valkey-sentinel-"
	sentinelContainerName  = "sentinel"
	sentinelConfigKey      = "sentinel.conf"
	sentinelStartScriptKey = "start-sentinel.sh"
	sentinelProbeScriptKey = "probe-sentinel.sh"
)

// sentinelStartScript copies the operator-rendered sentinel.conf into /data and starts Sentinel from it.
// Sentinel rewrites its own config file, and the ConfigMap mount is read-only.
// The per-pod announce address is appended here because it differs for every pod.
// The script lives in the ConfigMap, so changing it does not change the pod template.
const sentinelStartScript = `#!/bin/sh
set -e
cp /config/sentinel.conf /data/sentinel.conf
echo "sentinel announce-ip ${POD_NAME}.${SENTINEL_SERVICE}.${POD_NAMESPACE}.svc.${CLUSTER_DOMAIN}" >> /data/sentinel.conf
exec valkey-server /data/sentinel.conf --sentinel
`

// sentinelProbeScript reports healthy once Sentinel answers PING.
const sentinelProbeScript = `#!/bin/sh
valkey-cli -p 26379 ${VALKEY_TLS_ARGS} ping | grep -q PONG
`

// sentinelResourceName names the Service, ConfigMap, StatefulSet and PDB of a ValkeySentinel.
func sentinelResourceName(s *valkeyiov1alpha1.ValkeySentinel) string {
	return sentinelResourcePrefix + s.Name
}

// sentinelSelector selects the pods of one ValkeySentinel.
func sentinelSelector(s *valkeyiov1alpha1.ValkeySentinel) map[string]string {
	return map[string]string{LabelSentinel: s.Name}
}

// sentinelLabels returns the standard labels merged with the user-defined labels, plus the pod selector.
// The k8s recommended labels and the selector win over user-defined labels with the same key.
func sentinelLabels(s *valkeyiov1alpha1.ValkeySentinel) map[string]string {
	l := baseLabels(s.Name, "valkey-sentinel")
	for k, v := range s.Labels {
		if _, exists := l[k]; !exists {
			l[k] = v
		}
	}
	maps.Copy(l, sentinelSelector(s))
	return l
}

// sentinelServiceFQDN is the headless Service FQDN, the default TLS ServerName.
func sentinelServiceFQDN(s *valkeyiov1alpha1.ValkeySentinel) string {
	return fmt.Sprintf("%s.%s.svc.%s", sentinelResourceName(s), s.Namespace, s.GetClusterDomain())
}

// sentinelTLSServerName returns the ServerName the operator verifies Sentinel certificates against.
func sentinelTLSServerName(s *valkeyiov1alpha1.ValkeySentinel) string {
	if t := s.GetTLS(); t != nil && t.ServerName != "" {
		return t.ServerName
	}
	return sentinelServiceFQDN(s)
}

// sentinelBaseConfig returns the directives the operator owns in sentinel.conf.
// The per-pod "sentinel announce-ip" is owned too, but the start script appends it.
func sentinelBaseConfig(s *valkeyiov1alpha1.ValkeySentinel) map[string]string {
	port := strconv.Itoa(valkey.SentinelPort)
	config := map[string]string{
		"dir":            dataMountPath,
		"protected-mode": "no",
		"port":           port,
		// Pod IPs change on every restart, so Sentinel announces and resolves stable pod FQDNs instead.
		"sentinel resolve-hostnames":  "yes",
		"sentinel announce-hostnames": "yes",
		"sentinel announce-port":      port,
	}
	if s.GetTLS() != nil {
		config["port"] = "0"
		config["tls-port"] = port
		config["tls-cert-file"] = tlsCertMountPath + "/" + tlsSecretKeyCert
		config["tls-key-file"] = tlsCertMountPath + "/" + tlsSecretKeyKey
		config["tls-ca-cert-file"] = tlsCertMountPath + "/" + tlsSecretKeyCA
		// Sentinel talks to the servers it monitors over TLS.
		config["tls-replication"] = "yes"
		// SentinelTLSSpec is server-only, so client certificates are accepted but not required.
		config["tls-auth-clients"] = "optional"
	}
	return config
}

// sentinelOwnedKey reports whether a spec.config key collides with a directive the operator owns.
// Directive names are case-insensitive.
func sentinelOwnedKey(base map[string]string, key string) bool {
	k := strings.ToLower(key)
	if k == "sentinel announce-ip" {
		return true
	}
	_, owned := base[k]
	return owned
}

// renderSentinelConfig renders sentinel.conf from spec.config and the operator-owned directives.
// It returns a warning per user key the operator overrode.
func renderSentinelConfig(s *valkeyiov1alpha1.ValkeySentinel) (string, []configWarning) {
	base := sentinelBaseConfig(s)
	merged := map[string]string{}
	var warnings []configWarning
	for _, k := range slices.Sorted(maps.Keys(s.Spec.Config)) {
		if sentinelOwnedKey(base, k) {
			warnings = append(warnings, configWarning{
				reason:  valkeyiov1alpha1.ReasonUnsupportedConfigDirective,
				message: fmt.Sprintf("spec.config key %q is managed by the operator and was ignored", k),
			})
			continue
		}
		merged[k] = s.Spec.Config[k]
	}
	maps.Copy(merged, base)
	return renderConfig(merged), warnings
}

// buildSentinelConfigMap builds the ConfigMap holding sentinel.conf and the start and probe scripts.
func buildSentinelConfigMap(s *valkeyiov1alpha1.ValkeySentinel) (*corev1.ConfigMap, []configWarning) {
	conf, warnings := renderSentinelConfig(s)
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sentinelResourceName(s),
			Namespace: s.Namespace,
			Labels:    sentinelLabels(s),
		},
		Data: map[string]string{
			sentinelConfigKey:      conf,
			sentinelStartScriptKey: sentinelStartScript,
			sentinelProbeScriptKey: sentinelProbeScript,
		},
	}, warnings
}

// sentinelConfigHash hashes sentinel.conf into the pod template, so a config change creates a new StatefulSet revision.
// The scripts are left out, so changing them never rolls pods.
func sentinelConfigHash(cm *corev1.ConfigMap) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(cm.Data[sentinelConfigKey])))
}

// sentinelAffinity returns the user's affinity, adding a preferred hostname anti-affinity when the user set none.
// Preferred rather than required keeps a 3-pod quorum schedulable on a 1 or 2 node cluster.
func sentinelAffinity(s *valkeyiov1alpha1.ValkeySentinel) *corev1.Affinity {
	var affinity *corev1.Affinity
	if s.Spec.Scheduling != nil && s.Spec.Scheduling.Affinity != nil {
		affinity = s.Spec.Scheduling.Affinity.DeepCopy()
	}
	if affinity != nil && affinity.PodAntiAffinity != nil {
		return affinity
	}
	if affinity == nil {
		affinity = &corev1.Affinity{}
	}
	affinity.PodAntiAffinity = &corev1.PodAntiAffinity{
		PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
			Weight: 100,
			PodAffinityTerm: corev1.PodAffinityTerm{
				LabelSelector: &metav1.LabelSelector{MatchLabels: sentinelSelector(s)},
				TopologyKey:   corev1.LabelHostname,
			},
		}},
	}
	return affinity
}

// buildSentinelContainers builds the Sentinel container and applies the user's container patches.
func buildSentinelContainers(s *valkeyiov1alpha1.ValkeySentinel) ([]corev1.Container, error) {
	image := DefaultImage
	if s.Spec.Image != "" {
		image = s.Spec.Image
	}
	probe := func(failureThreshold int32) *corev1.Probe {
		return &corev1.Probe{
			InitialDelaySeconds: 5,
			PeriodSeconds:       5,
			FailureThreshold:    failureThreshold,
			TimeoutSeconds:      2,
			SuccessThreshold:    1,
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: []string{shellPath, "/scripts/" + sentinelProbeScriptKey}},
			},
		}
	}
	fieldEnv := func(name, path string) corev1.EnvVar {
		return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: path}}}
	}
	c := corev1.Container{
		Name:      sentinelContainerName,
		Image:     image,
		Resources: s.Spec.Resources,
		Command:   []string{shellPath, "/scripts/" + sentinelStartScriptKey},
		Env: []corev1.EnvVar{
			fieldEnv("POD_NAME", "metadata.name"),
			fieldEnv("POD_NAMESPACE", "metadata.namespace"),
			{Name: "SENTINEL_SERVICE", Value: sentinelResourceName(s)},
			{Name: "CLUSTER_DOMAIN", Value: s.GetClusterDomain()},
		},
		Ports:          []corev1.ContainerPort{{Name: "sentinel", ContainerPort: valkey.SentinelPort}},
		StartupProbe:   probe(20),
		LivenessProbe:  probe(5),
		ReadinessProbe: probe(3),
		VolumeMounts: []corev1.VolumeMount{
			{Name: "scripts", MountPath: "/scripts"},
			{Name: "sentinel-conf", MountPath: "/config", ReadOnly: true},
			{Name: dataVolumeName, MountPath: dataMountPath},
		},
	}
	if s.GetTLS() != nil {
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: tlsVolumeName, MountPath: tlsCertMountPath, ReadOnly: true})
		c.Env = append(c.Env, corev1.EnvVar{
			Name:  "VALKEY_TLS_ARGS",
			Value: fmt.Sprintf("--tls --cacert %s/%s", tlsCertMountPath, tlsSecretKeyCA),
		})
	}
	merged, err := mergePatchContainers([]corev1.Container{c}, s.Spec.Containers)
	if err != nil {
		return nil, err
	}
	applyContainerAPIDefaults(merged)
	return merged, nil
}

// buildSentinelStatefulSet builds the StatefulSet running the Sentinel pods.
// updateStrategy is OnDelete so the operator decides when each pod is replaced.
// Pods start in parallel because Sentinels do not depend on each other to boot.
func buildSentinelStatefulSet(s *valkeyiov1alpha1.ValkeySentinel, configHash string) (*appsv1.StatefulSet, error) {
	containers, err := buildSentinelContainers(s)
	if err != nil {
		return nil, err
	}
	var scheduling valkeyiov1alpha1.SentinelSchedulingSpec
	if s.Spec.Scheduling != nil {
		scheduling = *s.Spec.Scheduling
	}
	name := sentinelResourceName(s)
	labels := sentinelLabels(s)
	int32Ptr := func(i int32) *int32 { return &i }

	podSpec := corev1.PodSpec{
		Containers:                containers,
		ImagePullSecrets:          s.Spec.ImagePullSecrets,
		NodeSelector:              scheduling.NodeSelector,
		Affinity:                  sentinelAffinity(s),
		Tolerations:               scheduling.Tolerations,
		PriorityClassName:         scheduling.PriorityClassName,
		TopologySpreadConstraints: scheduling.TopologySpreadConstraints,
		SecurityContext:           s.Spec.PodSecurityContext,
		// API-server defaults, set explicitly so the desired spec equals the stored one and the update is a no-op (#315).
		RestartPolicy:                 corev1.RestartPolicyAlways,
		DNSPolicy:                     corev1.DNSClusterFirst,
		SchedulerName:                 corev1.DefaultSchedulerName,
		TerminationGracePeriodSeconds: func(i int64) *int64 { return &i }(corev1.DefaultTerminationGracePeriodSeconds),
		Volumes: []corev1.Volume{
			{
				Name: "scripts",
				VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: name},
					Items: []corev1.KeyToPath{
						{Key: sentinelStartScriptKey, Path: sentinelStartScriptKey},
						{Key: sentinelProbeScriptKey, Path: sentinelProbeScriptKey},
					},
					DefaultMode: int32Ptr(0755),
				}},
			},
			{
				Name: "sentinel-conf",
				VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: name},
					Items:                []corev1.KeyToPath{{Key: sentinelConfigKey, Path: sentinelConfigKey}},
					DefaultMode:          int32Ptr(corev1.ConfigMapVolumeSourceDefaultMode),
				}},
			},
			{
				// Sentinel's rewritten config lives here. Losing it is safe: monitors come back from re-registration and gossip.
				Name:         dataVolumeName,
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			},
		},
	}
	if podSpec.SecurityContext == nil {
		podSpec.SecurityContext = &corev1.PodSecurityContext{}
	}
	if t := s.GetTLS(); t != nil {
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name: tlsVolumeName,
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName:  t.Certificates.Server.SecretName,
				DefaultMode: int32Ptr(corev1.SecretVolumeSourceDefaultMode),
			}},
		})
	}

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    int32Ptr(s.Spec.Replicas),
			ServiceName: name,
			Selector:    &metav1.LabelSelector{MatchLabels: sentinelSelector(s)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{configHashKey: configHash},
				},
				Spec: podSpec,
			},
			PodManagementPolicy:  appsv1.ParallelPodManagement,
			UpdateStrategy:       appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType},
			RevisionHistoryLimit: int32Ptr(10),
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			},
		},
	}, nil
}

// buildSentinelService builds the headless Service over the Sentinel pods.
// publishNotReadyAddresses lets Sentinels resolve and gossip with each other before they pass readiness.
func buildSentinelService(s *valkeyiov1alpha1.ValkeySentinel) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sentinelResourceName(s),
			Namespace: s.Namespace,
			Labels:    sentinelLabels(s),
		},
		Spec: corev1.ServiceSpec{
			Type:                     corev1.ServiceTypeClusterIP,
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Selector:                 sentinelSelector(s),
			Ports: []corev1.ServicePort{{
				Name:       "sentinel",
				Port:       valkey.SentinelPort,
				Protocol:   corev1.ProtocolTCP,
				TargetPort: intstr.FromInt32(valkey.SentinelPort),
			}},
		},
	}
}
