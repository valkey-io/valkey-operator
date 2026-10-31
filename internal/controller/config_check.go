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
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	configCheckContainerName  = "config-check"
	configCheckHashAnnotation = "valkey.io/config-check-hash"
	configCheckMountPath      = "/config"
	configCheckTmpMountPath   = "/tmp"

	// configCheckExitRejected is the check script's exit when Valkey prints
	// FATAL CONFIG FILE ERROR. configCheckExitNotFound means the script could
	// not execute valkey-server, grep, or sleep. configCheckExitStartError is
	// containerd's exit when /bin/sh itself cannot be started. Neither parsed
	// the file. configCheckPendingDeadline bounds a pod that never exits
	// (image pull, scheduling). A real rejection is terminal and is not deleted
	// on that deadline.
	configCheckExitRejected    = 1
	configCheckExitNotFound    = 127
	configCheckExitStartError  = 128
	configCheckPendingDeadline = 5 * time.Minute

	// configCheckUID is the valkey user in the official image. The image
	// config has no USER, and the entrypoint drops to this uid only when
	// argv0 is valkey-server. This pod's command is /bin/sh.
	configCheckUID int64 = 999
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
// Exit 127: valkey-server, grep, or sleep could not be executed. A missing
// /bin/sh never reaches this script; containerd reports StartError and the
// container exits 128.
//
// CONFIG INFO (#141) can replace this pod once the image can describe its own
// settings. The rule stays the same: the binary decides, and a rejected file
// is not published.
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
// published is not checked again, unless spec.image is not the image that
// parsed it. An image downgrade can reject a directive the file already has.
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
	published, err := r.readPublishedServerFile(ctx, cluster)
	if err != nil {
		return err
	}
	if published.data == rendered && r.configCheckedForImage(ctx, cluster, published.image) {
		r.deleteConfigCheck(ctx, cluster)
		meta.RemoveStatusCondition(&cluster.Status.Conditions, valkeyiov1alpha1.ConditionServerConfig)
		return nil
	}

	hash, err := configCheckInputHash(cluster, rendered)
	if err != nil {
		return err
	}
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
		if err := r.Create(ctx, pod); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("create config check pod: %w", err)
			}
			// The cache misses an object that lacks the managed-by label.
			// Read it from the API before treating AlreadyExists as ours.
			pod = &corev1.Pod{}
			if err := r.readConfigCheckObject(ctx, key, pod); err != nil {
				return err
			}
		} else {
			log.V(1).Info("started valkey config check", "image", image, "pod", pod.Name)
			return &configCheckPendingError{image: image}
		}
	}
	if !configCheckOwnedBy(pod, cluster) {
		return fmt.Errorf("config check pod %s is not owned by this cluster", pod.Name)
	}

	return r.configCheckPodResult(ctx, cluster, pod, hash, image)
}

// configCheckPodResult reads a check pod that is already running. Exit 1 stays
// until the inputs change. Exit 127 and 128 stay too: recreating a pod whose
// image has no shell would loop. Any other unfinished pod is replaced after
// configCheckPendingDeadline so a bad pull secret or a stuck schedule retries.
func (r *ValkeyClusterReconciler) configCheckPodResult(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, pod *corev1.Pod, hash, image string) error {
	if pod.Annotations[configCheckHashAnnotation] != hash {
		r.deleteConfigCheckPod(ctx, cluster)
		return &configCheckPendingError{image: image, detail: "config check inputs changed"}
	}
	if detail := configCheckStartFailure(pod); detail != "" {
		return &configCheckPendingError{image: image, detail: detail}
	}

	term := terminatedConfigCheck(pod)
	if term == nil {
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			r.deleteConfigCheckPod(ctx, cluster)
			return &configCheckPendingError{image: image, detail: "config check pod finished without a container status"}
		}
		if configCheckPodStale(pod, time.Now()) {
			r.deleteConfigCheckPod(ctx, cluster)
			return &configCheckPendingError{image: image, detail: "config check pod did not finish within 5m"}
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
	case configCheckExitNotFound, configCheckExitStartError:
		return &configCheckPendingError{image: image, detail: "valkey-server, sh, grep, or sleep was not found in the image"}
	default:
		r.deleteConfigCheckPod(ctx, cluster)
		return &configCheckPendingError{image: image, detail: fmt.Sprintf("config check exited %d", term.ExitCode)}
	}
}

// publishedServerFile is the valkey.conf currently mounted, and the image
// recorded as having parsed it. image is empty when the file predates that
// annotation.
type publishedServerFile struct {
	data  string
	image string
}

func (r *ValkeyClusterReconciler) publishedServerConfig(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) (string, error) {
	published, err := r.readPublishedServerFile(ctx, cluster)
	if err != nil {
		return "", err
	}
	return published.data, nil
}

func (r *ValkeyClusterReconciler) readPublishedServerFile(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) (publishedServerFile, error) {
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: GetServerConfigMapName(cluster.Name), Namespace: cluster.Namespace}, cm)
	if apierrors.IsNotFound(err) {
		return publishedServerFile{}, nil
	}
	if err != nil {
		return publishedServerFile{}, err
	}
	return publishedServerFile{data: cm.Data[configFileKey], image: cm.Annotations[configCheckedImageKey]}, nil
}

// configCheckedForImage reports whether this spec.image is already known to
// parse the published file. A missing annotation is the upgrade case: nodes
// already on spec.image do not need a check pod. A node still on another
// image does, because a downgrade can reject a directive the file already has.
func (r *ValkeyClusterReconciler) configCheckedForImage(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, recorded string) bool {
	image := effectiveImage(cluster.Spec.Image)
	if recorded == image {
		return true
	}
	if recorded != "" {
		return false
	}
	nodes := &valkeyiov1alpha1.ValkeyNodeList{}
	if err := r.List(ctx, nodes, client.InNamespace(cluster.Namespace), client.MatchingLabels{LabelCluster: cluster.Name}); err != nil {
		return false
	}
	for i := range nodes.Items {
		if nodeImage := nodes.Items[i].Spec.Image; nodeImage != "" && nodeImage != image {
			return false
		}
	}
	return true
}

func (r *ValkeyClusterReconciler) ensureConfigCheckConfigMap(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, rendered, hash string) error {
	name := configCheckName(cluster)
	key := types.NamespacedName{Name: name, Namespace: cluster.Namespace}
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, key, cm)
	if apierrors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   cluster.Namespace,
				Labels:      configCheckLabels(cluster),
				Annotations: map[string]string{configCheckHashAnnotation: hash},
			},
			Data: map[string]string{configFileKey: rendered},
		}
		if err := controllerutil.SetControllerReference(cluster, cm, r.Scheme); err != nil {
			return fmt.Errorf("set config check configmap owner: %w", err)
		}
		if err := r.Create(ctx, cm); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("create config check configmap: %w", err)
			}
			cm = &corev1.ConfigMap{}
			if err := r.readConfigCheckObject(ctx, key, cm); err != nil {
				return err
			}
		} else {
			return nil
		}
	} else if err != nil {
		return err
	}
	if !configCheckOwnedBy(cm, cluster) {
		return fmt.Errorf("config check configmap %s is not owned by this cluster", name)
	}
	if cm.Data[configFileKey] == rendered && cm.Annotations[configCheckHashAnnotation] == hash && maps.Equal(cm.Labels, configCheckLabels(cluster)) {
		return nil
	}
	if cm.Annotations == nil {
		cm.Annotations = map[string]string{}
	}
	cm.Annotations[configCheckHashAnnotation] = hash
	cm.Labels = configCheckLabels(cluster)
	cm.Data = map[string]string{configFileKey: rendered}
	if err := r.Update(ctx, cm); err != nil {
		return fmt.Errorf("update config check configmap: %w", err)
	}
	return nil
}

func newConfigCheckPod(cluster *valkeyiov1alpha1.ValkeyCluster, hash string) *corev1.Pod {
	allowEscalation := false
	readOnlyRoot := true
	automount := false
	name := configCheckName(cluster)
	image := effectiveImage(cluster.Spec.Image)
	podSC := configCheckPodSecurityContext(cluster.Spec.PodSecurityContext)
	nonRoot := podSC.RunAsNonRoot == nil || *podSC.RunAsNonRoot
	var containerUser *int64
	if podSC.RunAsUser != nil {
		uid := *podSC.RunAsUser
		containerUser = &uid
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   cluster.Namespace,
			Labels:      configCheckLabels(cluster),
			Annotations: map[string]string{configCheckHashAnnotation: hash},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: &automount,
			ImagePullSecrets:             cluster.Spec.ImagePullSecrets,
			ServiceAccountName:           cluster.Spec.ServiceAccountName,
			SecurityContext:              podSC,
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
				Resources:       configCheckResources(),
				SecurityContext: &corev1.SecurityContext{
					RunAsUser:                containerUser,
					RunAsNonRoot:             &nonRoot,
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
	r.deleteConfigCheckObject(ctx, cluster, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configCheckName(cluster), Namespace: cluster.Namespace},
	})
}

func (r *ValkeyClusterReconciler) deleteConfigCheckPod(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) {
	r.deleteConfigCheckObject(ctx, cluster, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: configCheckName(cluster), Namespace: cluster.Namespace},
	})
}

// deleteConfigCheckObject removes a leftover check pod or ConfigMap. A steady
// reconcile, where the published file already matches, gets the object first
// and skips the delete when it is already gone. An object this cluster does
// not own is left alone: the name used to match another cluster's server
// ConfigMap.
func (r *ValkeyClusterReconciler) deleteConfigCheckObject(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, obj client.Object) {
	err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj)
	if apierrors.IsNotFound(err) {
		return
	}
	if err != nil {
		logf.FromContext(ctx).Error(err, "failed to get config check object", "name", obj.GetName())
		return
	}
	if !configCheckOwnedBy(obj, cluster) {
		logf.FromContext(ctx).Info("leaving config check object in place; this cluster does not own it", "name", obj.GetName())
		return
	}
	if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		logf.FromContext(ctx).Error(err, "failed to delete config check object", "name", obj.GetName())
	}
}

// readConfigCheckObject reads a check pod or ConfigMap. Get on the manager
// cache is NotFound for an object without the managed-by label, so a create
// that lost the race falls back to the API reader and can then see that the
// object belongs to someone else.
func (r *ValkeyClusterReconciler) readConfigCheckObject(ctx context.Context, key types.NamespacedName, obj client.Object) error {
	err := r.Get(ctx, key, obj)
	if !apierrors.IsNotFound(err) || r.APIReader == nil {
		return err
	}
	return r.APIReader.Get(ctx, key, obj)
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

func configCheckLabels(cluster *valkeyiov1alpha1.ValkeyCluster) map[string]string {
	// labels() carries app.kubernetes.io/managed-by. The manager cache
	// selects on that label; without it Get never sees this object.
	// Drop LabelCluster: a cluster that sets it on metadata.labels would
	// otherwise put the check pod in the headless Service.
	out := labels(cluster)
	delete(out, LabelCluster)
	out[LabelConfigCheck] = cluster.Name
	return out
}

// configCheckOwnedBy reports whether obj is a check pod or candidate
// ConfigMap for cluster. Both the controller owner and the check label
// have to match, so a same-named object from another cluster is not updated
// or deleted.
func configCheckOwnedBy(obj client.Object, cluster *valkeyiov1alpha1.ValkeyCluster) bool {
	if obj.GetNamespace() != cluster.Namespace || obj.GetLabels()[LabelConfigCheck] != cluster.Name {
		return false
	}
	if obj.GetLabels()["app.kubernetes.io/managed-by"] != "valkey-operator" {
		return false
	}
	owner := metav1.GetControllerOf(obj)
	return owner != nil &&
		owner.UID == cluster.UID &&
		owner.Name == cluster.Name &&
		owner.Kind == "ValkeyCluster" &&
		owner.APIVersion == valkeyiov1alpha1.GroupVersion.String()
}

// configCheckPendingRequeue is the backstop while a check pod has not
// exited. The cluster controller watches that pod, so the exit usually
// wakes reconcile. This interval only covers a missed watch.
const configCheckPendingRequeue = 30 * time.Second

// configCheckResources is a small fixed request and the same limit.
// Copying spec.resources would leave an 8 second parse waiting on a
// multi-gigabyte schedule hole. A namespace quota on limits.cpu or
// limits.memory rejects a pod that sets only requests.
func configCheckResources() corev1.ResourceRequirements {
	req := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("10m"),
		corev1.ResourceMemory: resource.MustParse("64Mi"),
	}
	return corev1.ResourceRequirements{Requests: req, Limits: req.DeepCopy()}
}

// configCheckPodSecurityContext starts from the cluster's pod security context.
// runAsNonRoot without a uid is rejected by the kubelet when the image user is
// root, which is the official image. Set uid 999 in that case. An explicit
// RunAsNonRoot false is left alone so the caller can run as the image user.
func configCheckPodSecurityContext(in *corev1.PodSecurityContext) *corev1.PodSecurityContext {
	var sc *corev1.PodSecurityContext
	if in != nil {
		sc = in.DeepCopy()
	} else {
		sc = &corev1.PodSecurityContext{}
	}
	if sc.SeccompProfile == nil {
		sc.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	}
	wantsNonRoot := sc.RunAsNonRoot == nil || *sc.RunAsNonRoot
	if wantsNonRoot && sc.RunAsUser == nil {
		uid := configCheckUID
		sc.RunAsUser = &uid
		if sc.RunAsGroup == nil {
			gid := configCheckUID
			sc.RunAsGroup = &gid
		}
		if sc.RunAsNonRoot == nil {
			nonRoot := true
			sc.RunAsNonRoot = &nonRoot
		}
	}
	return sc
}

// configCheckIdentity is the set of inputs that decide whether an existing
// check pod is still the one we would create. The rendered file alone is not
// enough: a fixed image or pull secret has to replace a pod that is waiting
// or that exited 127/128. The resource request is fixed, so a change to
// spec.resources does not rebuild the pod.
type configCheckIdentity struct {
	Rendered           string                     `json:"rendered"`
	Image              string                     `json:"image"`
	ImagePullSecrets   []string                   `json:"imagePullSecrets,omitempty"`
	ServiceAccountName string                     `json:"serviceAccountName,omitempty"`
	NodeSelector       map[string]string          `json:"nodeSelector,omitempty"`
	Tolerations        []corev1.Toleration        `json:"tolerations,omitempty"`
	PriorityClassName  string                     `json:"priorityClassName,omitempty"`
	SecurityContext    *corev1.PodSecurityContext `json:"securityContext,omitempty"`
}

func configCheckInputHash(cluster *valkeyiov1alpha1.ValkeyCluster, rendered string) (string, error) {
	var secrets []string
	for _, s := range cluster.Spec.ImagePullSecrets {
		secrets = append(secrets, s.Name)
	}
	var nodeSelector map[string]string
	var tolerations []corev1.Toleration
	var priority string
	if scheduling := cluster.Spec.Scheduling; scheduling != nil {
		nodeSelector = scheduling.NodeSelector
		tolerations = scheduling.Tolerations
		priority = scheduling.PriorityClassName
	}
	payload, err := json.Marshal(configCheckIdentity{
		Rendered:           rendered,
		Image:              effectiveImage(cluster.Spec.Image),
		ImagePullSecrets:   secrets,
		ServiceAccountName: cluster.Spec.ServiceAccountName,
		NodeSelector:       nodeSelector,
		Tolerations:        tolerations,
		PriorityClassName:  priority,
		SecurityContext:    configCheckPodSecurityContext(cluster.Spec.PodSecurityContext),
	})
	if err != nil {
		return "", fmt.Errorf("hash config check inputs: %w", err)
	}
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("%x", sum), nil
}

func configCheckStartFailure(pod *corev1.Pod) string {
	for i := range pod.Status.ContainerStatuses {
		status := &pod.Status.ContainerStatuses[i]
		if status.Name != configCheckContainerName || status.State.Waiting == nil {
			continue
		}
		reason := status.State.Waiting.Reason
		if reason != "StartError" && reason != "CreateContainerError" {
			continue
		}
		if status.State.Waiting.Message == "" {
			return reason
		}
		return reason + ": " + status.State.Waiting.Message
	}
	return ""
}

func configCheckPodStale(pod *corev1.Pod, now time.Time) bool {
	if pod.CreationTimestamp.IsZero() {
		return false
	}
	return now.Sub(pod.CreationTimestamp.Time) > configCheckPendingDeadline
}

// publishedServerInputs are the ValkeyNode fields that render valkey.conf, plus
// the grace period derived from cluster-manual-failover-timeout. While a
// candidate is unpublished, reconcile keeps the values already on a node so
// the roll hash does not move onto a file that was not written.
type publishedServerInputs struct {
	config            map[string]string
	image             string
	tls               *valkeyiov1alpha1.NodeTLSSpec
	preferredEndpoint valkeyiov1alpha1.PreferredEndpointType
	clusterDomain     string
	grace             *int64
}

func publishedServerInputsFrom(node *valkeyiov1alpha1.ValkeyNode) *publishedServerInputs {
	var grace *int64
	if node.Spec.TerminationGracePeriodSeconds != nil {
		g := *node.Spec.TerminationGracePeriodSeconds
		grace = &g
	}
	return &publishedServerInputs{
		config:            maps.Clone(node.Spec.Config),
		image:             node.Spec.Image,
		tls:               node.Spec.TLS.DeepCopy(),
		preferredEndpoint: node.Spec.PreferredEndpointType,
		clusterDomain:     node.Spec.ClusterDomain,
		grace:             grace,
	}
}

func (in *publishedServerInputs) apply(node *valkeyiov1alpha1.ValkeyNode) {
	node.Spec.Config = maps.Clone(in.config)
	node.Spec.Image = in.image
	node.Spec.TLS = in.tls.DeepCopy()
	node.Spec.PreferredEndpointType = in.preferredEndpoint
	node.Spec.ClusterDomain = in.clusterDomain
	node.Spec.TerminationGracePeriodSeconds = nil
	if in.grace != nil {
		g := *in.grace
		node.Spec.TerminationGracePeriodSeconds = &g
	}
}

// serverSpecHold is set when the candidate was not published and a file is
// already being served. nil means the cluster spec should be copied onto nodes.
type serverSpecHold struct {
	// fallback is copied onto a node created while the candidate is unpublished.
	// Nil when the cluster has no nodes yet.
	fallback *publishedServerInputs
}

// apply keeps the server spec that is already on current. It returns false
// when the node does not exist and there is no existing spec to copy, so the
// caller does not create a node from the unpublished candidate.
func (h *serverSpecHold) apply(desired, current *valkeyiov1alpha1.ValkeyNode, currentExists bool) bool {
	if currentExists {
		publishedServerInputsFrom(current).apply(desired)
		return true
	}
	if h.fallback == nil {
		return false
	}
	h.fallback.apply(desired)
	return true
}

// configCheckName is the check pod and the candidate ConfigMap. It cannot
// equal GetServerConfigMapName, which is always "valkey-<cluster>", and it
// cannot equal a ValkeyNode pod name ("<cluster>-<shard>-<index>"), which
// ends in "-<digits>-<digits>". "valkey-<cluster>-cfg" collided with the
// server ConfigMap of cluster "<cluster>-cfg".
func configCheckName(cluster *valkeyiov1alpha1.ValkeyCluster) string {
	const prefix = "chk."
	const suffix = ".x"
	base := prefix + cluster.Name + suffix
	if len(base) <= 63 {
		return base
	}
	sum := sha256.Sum256([]byte(cluster.Namespace + "/" + cluster.Name))
	hash := fmt.Sprintf("%x", sum[:4])
	keep := 63 - len(prefix) - 1 - len(hash)
	trimmed := cluster.Name
	if len(trimmed) > keep {
		trimmed = trimmed[:keep]
	}
	trimmed = strings.TrimRight(trimmed, "-.")
	if trimmed == "" {
		return prefix + hash
	}
	return prefix + trimmed + "." + hash
}

func trimConfigCheckMessage(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > 512 {
		return message[:512]
	}
	return message
}
