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
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	configCheckContainerName  = "config-check"
	configCheckHashAnnotation = "valkey.io/config-check-hash"
	configCheckMountPath      = "/config"
	configCheckTmpMountPath   = "/tmp"

	// configCheckExitRejected is the check script's exit when Valkey prints
	// FATAL CONFIG FILE ERROR. configCheckExitNotFound means the image has no
	// valkey-server or shell, so the file was not actually parsed.
	configCheckExitRejected = 1
	configCheckExitNotFound = 127
)

// configCheckScript asks spec.image to parse the rendered file. Valkey exits
// during loadServerConfig on a bad directive or a bad value, before it opens
// aclfile or TLS material. dir is applied as the file is read, so this pod
// mounts a writable /data and the rendered "dir /data" can chdir.
//
// Arguments after the file are parsed last. They keep this pod off the network
// and out of the operator ACL file. cluster-enabled is left as rendered:
// appending cluster-enabled no would hide "replicaof directive not allowed in
// cluster mode", which Valkey checks against the final value. bind 127.0.0.1
// holds both the client port and the cluster bus on localhost. A directive
// Valkey rejects still fails on its own line, before these arguments.
//
// Exit 0: no FATAL CONFIG FILE ERROR (the process ran, or it died later for a
// reason other than config parse). Exit 1: the image rejected the file.
// Exit 127: valkey-server could not be executed.
const configCheckScript = `
if ! command -v valkey-server >/dev/null 2>&1 || ! command -v grep >/dev/null 2>&1 || ! command -v sleep >/dev/null 2>&1; then
  echo "config check needs valkey-server, grep, and sleep" >&2
  exit 127
fi
log=/tmp/valkey-config-check.log
(
  valkey-server /config/valkey.conf \
    --daemonize no \
    --protected-mode no \
    --bind 127.0.0.1 \
    --save "" \
    --aclfile ""
) >"$log" 2>&1 &
pid=$!
i=0
while [ "$i" -lt 8 ]; do
  if grep -q "FATAL CONFIG FILE ERROR" "$log" 2>/dev/null; then
    kill "$pid" 2>/dev/null
    wait "$pid" 2>/dev/null
    cat "$log" >&2
    exit 1
  fi
  if ! kill -0 "$pid" 2>/dev/null; then
    wait "$pid"
    code=$?
    if grep -q "FATAL CONFIG FILE ERROR" "$log" 2>/dev/null; then
      cat "$log" >&2
      exit 1
    fi
    if [ "$code" -eq 127 ]; then
      cat "$log" >&2
      exit 127
    fi
    exit 0
  fi
  i=$((i + 1))
  sleep 1
done
kill "$pid" 2>/dev/null
wait "$pid" 2>/dev/null
exit 0
`

// serverConfigParsesFn, when set, replaces the in-cluster parse check.
// Tests set it: envtest has no kubelet to run the check pod. Production leaves it nil.
var serverConfigParsesFn func(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, rendered string) error

// configRejectedError means spec.image rejected the rendered valkey.conf.
// The published file, if any, was left in place.
type configRejectedError struct {
	message string
}

func (e *configRejectedError) Error() string { return e.message }

// configCheckPendingError means the check pod has not produced a verdict yet.
type configCheckPendingError struct {
	image  string
	detail string
}

func (e *configCheckPendingError) Error() string {
	msg := fmt.Sprintf("checking rendered valkey.conf with %s before writing it", e.image)
	if e.detail != "" {
		msg = msg + ": " + e.detail
	}
	return msg
}

// checkRenderedConfig reports whether rendered is safe to publish as valkey.conf.
// A file the image rejects is not written. A file equal to the one already
// published is not checked again.
func (r *ValkeyClusterReconciler) checkRenderedConfig(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, rendered string) error {
	if serverConfigParsesFn != nil {
		err := serverConfigParsesFn(ctx, cluster, rendered)
		if err == nil {
			meta.RemoveStatusCondition(&cluster.Status.Conditions, valkeyiov1alpha1.ConditionServerConfig)
		}
		return err
	}

	log := logf.FromContext(ctx)
	image := effectiveImage(cluster.Spec.Image)
	published, err := r.publishedServerConfig(ctx, cluster)
	if err != nil {
		return err
	}
	if published == rendered {
		r.deleteConfigCheck(ctx, cluster)
		meta.RemoveStatusCondition(&cluster.Status.Conditions, valkeyiov1alpha1.ConditionServerConfig)
		return nil
	}

	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(rendered)))
	if err := r.ensureConfigCheckConfigMap(ctx, cluster, rendered, hash); err != nil {
		return err
	}

	pod := &corev1.Pod{}
	key := types.NamespacedName{Name: configCheckName(cluster), Namespace: cluster.Namespace}
	if err := r.Get(ctx, key, pod); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		pod = newConfigCheckPod(cluster, hash)
		if err := controllerutil.SetControllerReference(cluster, pod, r.Scheme); err != nil {
			return fmt.Errorf("set config check pod owner: %w", err)
		}
		if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create config check pod: %w", err)
		}
		log.V(1).Info("started valkey config check", "image", image, "pod", pod.Name)
		return &configCheckPendingError{image: image}
	}

	if pod.Annotations[configCheckHashAnnotation] != hash {
		r.deleteConfigCheckPod(ctx, cluster)
		return &configCheckPendingError{image: image, detail: "rendered config changed"}
	}

	term := terminatedConfigCheck(pod)
	if term == nil {
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			r.deleteConfigCheckPod(ctx, cluster)
			return &configCheckPendingError{image: image, detail: "config check pod finished without a container status"}
		}
		return &configCheckPendingError{image: image, detail: configCheckWaiting(pod)}
	}

	switch term.ExitCode {
	case 0:
		r.deleteConfigCheck(ctx, cluster)
		meta.RemoveStatusCondition(&cluster.Status.Conditions, valkeyiov1alpha1.ConditionServerConfig)
		return nil
	case configCheckExitRejected:
		detail := strings.TrimSpace(term.Message)
		if detail == "" {
			detail = "valkey-server rejected the rendered file"
		}
		return &configRejectedError{message: trimConfigCheckMessage(fmt.Sprintf(
			"spec.image %s rejected the rendered valkey.conf; the published file was left unchanged: %s", image, detail))}
	case configCheckExitNotFound:
		return &configCheckPendingError{image: image, detail: "valkey-server, sh, grep, or sleep was not found in the image"}
	default:
		r.deleteConfigCheckPod(ctx, cluster)
		return &configCheckPendingError{image: image, detail: fmt.Sprintf("config check exited %d", term.ExitCode)}
	}
}

func (r *ValkeyClusterReconciler) publishedServerConfig(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) (string, error) {
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: GetServerConfigMapName(cluster.Name), Namespace: cluster.Namespace}, cm)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return cm.Data[configFileKey], nil
}

func (r *ValkeyClusterReconciler) ensureConfigCheckConfigMap(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, rendered, hash string) error {
	name := configCheckName(cluster)
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: cluster.Namespace}, cm)
	if apierrors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   cluster.Namespace,
				Labels:      map[string]string{LabelCluster: cluster.Name},
				Annotations: map[string]string{configCheckHashAnnotation: hash},
			},
			Data: map[string]string{configFileKey: rendered},
		}
		if err := controllerutil.SetControllerReference(cluster, cm, r.Scheme); err != nil {
			return fmt.Errorf("set config check configmap owner: %w", err)
		}
		if err := r.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create config check configmap: %w", err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if cm.Data[configFileKey] == rendered && cm.Annotations[configCheckHashAnnotation] == hash {
		return nil
	}
	if cm.Annotations == nil {
		cm.Annotations = map[string]string{}
	}
	cm.Annotations[configCheckHashAnnotation] = hash
	cm.Data = map[string]string{configFileKey: rendered}
	if err := r.Update(ctx, cm); err != nil {
		return fmt.Errorf("update config check configmap: %w", err)
	}
	return nil
}

func newConfigCheckPod(cluster *valkeyiov1alpha1.ValkeyCluster, hash string) *corev1.Pod {
	runAsNonRoot := true
	allowEscalation := false
	readOnlyRoot := true
	automount := false
	name := configCheckName(cluster)
	image := effectiveImage(cluster.Spec.Image)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   cluster.Namespace,
			Labels:      map[string]string{LabelCluster: cluster.Name},
			Annotations: map[string]string{configCheckHashAnnotation: hash},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: &automount,
			ImagePullSecrets:             cluster.Spec.ImagePullSecrets,
			ServiceAccountName:           cluster.Spec.ServiceAccountName,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &runAsNonRoot,
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "valkey-conf",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: name},
						},
					},
				},
				{
					Name:         "tmp",
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
				},
				// dir is applied while the file is read. The rendered file
				// always ends with dir /data, and that chdir fails when the
				// directory is missing, which would reject a valid config.
				{
					Name:         dataVolumeName,
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
				},
			},
			Containers: []corev1.Container{{
				Name:            configCheckContainerName,
				Image:           image,
				ImagePullPolicy: defaultImagePullPolicy(image),
				Command:         []string{shellPath, "-c", configCheckScript},
				Resources:       cluster.Spec.Resources,
				SecurityContext: &corev1.SecurityContext{
					RunAsNonRoot:             &runAsNonRoot,
					AllowPrivilegeEscalation: &allowEscalation,
					ReadOnlyRootFilesystem:   &readOnlyRoot,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
				VolumeMounts: []corev1.VolumeMount{
					{Name: "valkey-conf", MountPath: configCheckMountPath, ReadOnly: true},
					{Name: "tmp", MountPath: configCheckTmpMountPath},
					{Name: dataVolumeName, MountPath: dataMountPath},
				},
			}},
		},
	}
	// Tolerations and nodeSelector let the check run in the same pool as the
	// server pods. Affinity is not copied: a required anti-affinity against
	// the Valkey pods can leave this one-shot pod unschedulable.
	if scheduling := cluster.Spec.Scheduling; scheduling != nil {
		pod.Spec.Tolerations = scheduling.Tolerations
		pod.Spec.NodeSelector = scheduling.NodeSelector
		pod.Spec.PriorityClassName = scheduling.PriorityClassName
	}
	return pod
}

func terminatedConfigCheck(pod *corev1.Pod) *corev1.ContainerStateTerminated {
	for i := range pod.Status.ContainerStatuses {
		status := &pod.Status.ContainerStatuses[i]
		if status.Name == configCheckContainerName && status.State.Terminated != nil {
			return status.State.Terminated
		}
	}
	return nil
}

func configCheckWaiting(pod *corev1.Pod) string {
	for i := range pod.Status.ContainerStatuses {
		status := &pod.Status.ContainerStatuses[i]
		if status.Name != configCheckContainerName || status.State.Waiting == nil {
			continue
		}
		if status.State.Waiting.Reason == "" {
			return ""
		}
		if status.State.Waiting.Message == "" {
			return status.State.Waiting.Reason
		}
		return status.State.Waiting.Reason + ": " + status.State.Waiting.Message
	}
	return ""
}

func (r *ValkeyClusterReconciler) deleteConfigCheck(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) {
	r.deleteConfigCheckPod(ctx, cluster)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configCheckName(cluster), Namespace: cluster.Namespace},
	}
	if err := r.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
		logf.FromContext(ctx).Error(err, "failed to delete config check configmap", "name", cm.Name)
	}
}

func (r *ValkeyClusterReconciler) deleteConfigCheckPod(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: configCheckName(cluster), Namespace: cluster.Namespace},
	}
	if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		logf.FromContext(ctx).Error(err, "failed to delete config check pod", "name", pod.Name)
	}
}

// noteServerConfig records a check that has not published valkey.conf yet.
// A cluster that is already serving keeps its Ready condition; only the first
// publish holds Ready off, because there is no file to keep serving.
func (r *ValkeyClusterReconciler) noteServerConfig(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, reason, message string) {
	published, err := r.publishedServerConfig(ctx, cluster)
	if err != nil {
		logf.FromContext(ctx).Error(err, "failed to read published valkey.conf")
	} else if published == "" {
		setCondition(cluster, valkeyiov1alpha1.ConditionReady, reason, message, metav1.ConditionFalse)
		progressing := metav1.ConditionFalse
		if reason == valkeyiov1alpha1.ReasonConfigCheckPending {
			progressing = metav1.ConditionTrue
		}
		setCondition(cluster, valkeyiov1alpha1.ConditionProgressing, reason, message, progressing)
	}

	current := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionServerConfig)
	if (current == nil || current.Reason != reason || current.Message != message) &&
		reason == valkeyiov1alpha1.ReasonConfigRejected && r.Recorder != nil {
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, reason, "ConfigCheck", "%s", message)
	}
	setCondition(cluster, valkeyiov1alpha1.ConditionServerConfig, reason, message, metav1.ConditionFalse)
	if err := r.updateStatus(ctx, cluster, nil); err != nil {
		logf.FromContext(ctx).Error(err, statusUpdateFailedMsg)
	}
}

func configCheckName(cluster *valkeyiov1alpha1.ValkeyCluster) string {
	const marker = "-cfg"
	base := resourcePrefix + cluster.Name + marker
	if len(base) <= 63 {
		return base
	}
	sum := sha256.Sum256([]byte(cluster.Namespace + "/" + cluster.Name))
	hash := fmt.Sprintf("%x", sum[:4])
	keep := 63 - len(resourcePrefix) - 1 - len(hash)
	trimmed := cluster.Name
	if len(trimmed) > keep {
		trimmed = trimmed[:keep]
	}
	trimmed = strings.TrimRight(trimmed, "-")
	return resourcePrefix + trimmed + "-" + hash
}

func trimConfigCheckMessage(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > 512 {
		return message[:512]
	}
	return message
}
