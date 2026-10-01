/*
Copyright 2024.

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
	"fmt"
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

const (
	// DefaultBackupImage uploads snapshots and downloads them for a restore.
	// Any image with rclone and a POSIX shell works.
	DefaultBackupImage = "rclone/rclone:1.75"

	backupComponent        = "valkey-backup"
	backupDirMountPath     = "/backup"
	backupScriptsMountPath = "/scripts"

	// Keys of the credentials Secret named in S3Storage.
	s3AccessKeyIDKey     = "AWS_ACCESS_KEY_ID"
	s3SecretAccessKeyKey = "AWS_SECRET_ACCESS_KEY"
)

// restoreScripts returns the two scripts the restore init containers run,
// keyed by file name, for the ConfigMap the node pods mount at /scripts.
func restoreScripts() (map[string]string, error) {
	data := make(map[string]string, 2)
	for _, name := range []string{"restore-fetch.sh", "restore-install.sh"} {
		script, err := scripts.ReadFile("scripts/" + name)
		if err != nil {
			return nil, fmt.Errorf("reading embedded %s: %w", name, err)
		}
		data[name] = string(script)
	}
	return data, nil
}

func backupResourceName(cluster *valkeyiov1alpha1.ValkeyCluster) string {
	return resourcePrefix + cluster.Name + "-backup"
}

func backupLabels(cluster *valkeyiov1alpha1.ValkeyCluster) map[string]string {
	l := baseLabels(cluster.Name, backupComponent)
	l[LabelCluster] = cluster.Name
	return l
}

// valkeyCLITLSArgs returns the flags valkey-cli needs to reach a node of a
// TLS cluster, with the client certificate when the server demands one.
func valkeyCLITLSArgs(tls *valkeyiov1alpha1.NodeTLSSpec) string {
	if tls == nil {
		return ""
	}
	args := fmt.Sprintf("--tls --cacert %s", tlsCertMountPath+"/"+tlsSecretKeyCA)
	if tls.RequiresClientCertificate() {
		args = fmt.Sprintf("%s --cert %s --key %s", args,
			tlsCertMountPath+"/"+tlsSecretKeyCert, tlsCertMountPath+"/"+tlsSecretKeyKey)
	}
	return args
}

// s3Env is the rclone configuration for a bucket, read from the environment
// so the container needs no config file.
func s3Env(s3 *valkeyiov1alpha1.S3Storage) []corev1.EnvVar {
	env := []corev1.EnvVar{
		// No config file: the remote comes entirely from the environment, and
		// an empty file keeps rclone from saying so on every call.
		{Name: "RCLONE_CONFIG", Value: "/dev/null"},
		{Name: "RCLONE_CONFIG_S3_TYPE", Value: "s3"},
		{Name: "RCLONE_CONFIG_S3_PROVIDER", Value: "Other"},
		{Name: "RCLONE_CONFIG_S3_ENDPOINT", Value: s3.Endpoint},
		{Name: "RCLONE_CONFIG_S3_ACCESS_KEY_ID", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: s3.CredentialsSecret},
			Key:                  s3AccessKeyIDKey,
		}}},
		{Name: "RCLONE_CONFIG_S3_SECRET_ACCESS_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: s3.CredentialsSecret},
			Key:                  s3SecretAccessKeyKey,
		}}},
	}
	if s3.Region != "" {
		env = append(env, corev1.EnvVar{Name: "RCLONE_CONFIG_S3_REGION", Value: s3.Region})
	}
	return env
}

func systemUserPasswordEnv(name, clusterName, user string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: getSystemPasswordSecretName(clusterName)},
		Key:                  user,
	}}}
}

// backupImage is the image that talks to the bucket.
func backupImage(image string) string {
	if image == "" {
		return DefaultBackupImage
	}
	return image
}

func backupPrefix(cluster *valkeyiov1alpha1.ValkeyCluster) string {
	if p := cluster.Spec.Backup.Storage.S3.Prefix; p != "" {
		return p
	}
	return cluster.Name
}

// buildBackupConfigMap holds the two scripts the backup Job runs.
func buildBackupConfigMap(cluster *valkeyiov1alpha1.ValkeyCluster) (*corev1.ConfigMap, error) {
	dump, err := scripts.ReadFile("scripts/backup-dump.sh")
	if err != nil {
		return nil, fmt.Errorf("reading embedded backup-dump.sh: %w", err)
	}
	upload, err := scripts.ReadFile("scripts/backup-upload.sh")
	if err != nil {
		return nil, fmt.Errorf("reading embedded backup-upload.sh: %w", err)
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backupResourceName(cluster),
			Namespace: cluster.Namespace,
			Labels:    backupLabels(cluster),
		},
		Data: map[string]string{
			"backup-dump.sh":   string(dump),
			"backup-upload.sh": string(upload),
		},
	}, nil
}

// buildBackupCronJob renders the per-cluster backup schedule. Each run is one
// pod: an init container on the cluster's own image reads one RDB per shard
// into an emptyDir, then a container on the backup image uploads the set and
// prunes old snapshots. The pod needs no Kubernetes API access; it finds the
// shards through the headless Service and CLUSTER NODES.
func buildBackupCronJob(cluster *valkeyiov1alpha1.ValkeyCluster) *batchv1.CronJob {
	spec := cluster.Spec.Backup
	s3 := spec.Storage.S3
	labels := backupLabels(cluster)
	tls := nodeTLSFromCluster(cluster)
	source := spec.Source
	if source == "" {
		source = valkeyiov1alpha1.BackupSourceReplica
	}

	dumpEnv := []corev1.EnvVar{
		{Name: "VALKEY_HOST", Value: headlessServiceName(cluster.Name)},
		{Name: "VALKEY_PORT", Value: strconv.Itoa(DefaultPort)},
		{Name: "CLUSTER_NAME", Value: cluster.Name},
		{Name: "NAMESPACE", Value: cluster.Namespace},
		{Name: "BACKUP_DIR", Value: backupDirMountPath},
		{Name: "BACKUP_SOURCE", Value: string(source)},
		{Name: "VALKEY_OPERATOR_USER", Value: operatorUser},
		systemUserPasswordEnv("VALKEY_OPERATOR_PASSWORD", cluster.Name, operatorUser),
		{Name: "VALKEY_REPLICATION_USER", Value: replicationUser},
		systemUserPasswordEnv("VALKEY_REPLICATION_PASSWORD", cluster.Name, replicationUser),
	}
	dumpMounts := []corev1.VolumeMount{
		{Name: backupVolumeName, MountPath: backupDirMountPath},
		{Name: scriptsVolumeName, MountPath: backupScriptsMountPath, ReadOnly: true},
	}
	volumes := []corev1.Volume{
		{Name: backupVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: scriptsVolumeName, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: backupResourceName(cluster)},
			DefaultMode:          func(i int32) *int32 { return &i }(corev1.ConfigMapVolumeSourceDefaultMode),
		}}},
	}
	if tls != nil {
		dumpEnv = append(dumpEnv, corev1.EnvVar{Name: envValkeyTLSArgs, Value: valkeyCLITLSArgs(tls)})
		dumpMounts = append(dumpMounts, corev1.VolumeMount{Name: tlsVolumeName, MountPath: tlsCertMountPath, ReadOnly: true})
		volumes = append(volumes, corev1.Volume{Name: tlsVolumeName, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  tls.Certificates.Server.SecretName,
			DefaultMode: func(i int32) *int32 { return &i }(corev1.SecretVolumeSourceDefaultMode),
		}}})
	}

	uploadEnv := append(s3Env(s3),
		corev1.EnvVar{Name: "BACKUP_DIR", Value: backupDirMountPath},
		corev1.EnvVar{Name: "BACKUP_BUCKET", Value: s3.Bucket},
		corev1.EnvVar{Name: "BACKUP_PREFIX", Value: backupPrefix(cluster)},
		corev1.EnvVar{Name: "BACKUP_RETENTION", Value: strconv.Itoa(int(spec.Retention))},
	)

	suspend := false
	if spec.Suspend != nil {
		suspend = *spec.Suspend
	}
	podSecurityContext := cluster.Spec.PodSecurityContext
	if podSecurityContext == nil {
		podSecurityContext = &corev1.PodSecurityContext{}
	}

	// The defaults below are set explicitly so the desired spec equals what
	// the API server stores and CreateOrUpdate stays quiet (#315).
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backupResourceName(cluster),
			Namespace: cluster.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.CronJobSpec{
			Schedule:                   spec.Schedule,
			Suspend:                    &suspend,
			ConcurrencyPolicy:          batchv1.ForbidConcurrent,
			SuccessfulJobsHistoryLimit: func(i int32) *int32 { return &i }(3),
			FailedJobsHistoryLimit:     func(i int32) *int32 { return &i }(3),
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: batchv1.JobSpec{
					BackoffLimit: func(i int32) *int32 { return &i }(1),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: labels},
						Spec: corev1.PodSpec{
							RestartPolicy:                 corev1.RestartPolicyNever,
							DNSPolicy:                     corev1.DNSClusterFirst,
							SchedulerName:                 corev1.DefaultSchedulerName,
							TerminationGracePeriodSeconds: func(i int64) *int64 { return &i }(corev1.DefaultTerminationGracePeriodSeconds),
							SecurityContext:               podSecurityContext,
							ImagePullSecrets:              cluster.Spec.ImagePullSecrets,
							InitContainers: []corev1.Container{{
								Name:                     "dump",
								Image:                    effectiveImage(cluster.Spec.Image),
								ImagePullPolicy:          corev1.PullIfNotPresent,
								Command:                  []string{"sh", backupScriptsMountPath + "/backup-dump.sh"},
								Env:                      dumpEnv,
								VolumeMounts:             dumpMounts,
								TerminationMessagePath:   corev1.TerminationMessagePathDefault,
								TerminationMessagePolicy: corev1.TerminationMessageReadFile,
							}},
							Containers: []corev1.Container{{
								Name:            "upload",
								Image:           backupImage(spec.Image),
								ImagePullPolicy: corev1.PullIfNotPresent,
								Command:         []string{"sh", backupScriptsMountPath + "/backup-upload.sh"},
								Env:             uploadEnv,
								VolumeMounts: []corev1.VolumeMount{
									{Name: backupVolumeName, MountPath: backupDirMountPath},
									{Name: scriptsVolumeName, MountPath: backupScriptsMountPath, ReadOnly: true},
								},
								TerminationMessagePath:   corev1.TerminationMessagePathDefault,
								TerminationMessagePolicy: corev1.TerminationMessageReadFile,
							}},
							Volumes: volumes,
						},
					},
				},
			},
		},
	}
}

// reconcileBackup keeps the backup CronJob and its script ConfigMap in step
// with spec.backup, and removes both when the field is cleared.
func (r *ValkeyClusterReconciler) reconcileBackup(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) error {
	key := client.ObjectKey{Name: backupResourceName(cluster), Namespace: cluster.Namespace}
	if cluster.Spec.Backup == nil {
		cj := &batchv1.CronJob{}
		switch err := r.Get(ctx, key, cj); {
		case apierrors.IsNotFound(err):
		case err != nil:
			return fmt.Errorf("getting backup CronJob for deletion: %w", err)
		default:
			if err := r.Delete(ctx, cj); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting backup CronJob: %w", err)
			}
			r.Recorder.Eventf(cluster, cj, corev1.EventTypeNormal, "BackupScheduleDeleted", "DeleteBackup", "Deleted backup CronJob")
		}
		cm := &corev1.ConfigMap{}
		switch err := r.Get(ctx, key, cm); {
		case apierrors.IsNotFound(err):
		case err != nil:
			return fmt.Errorf("getting backup ConfigMap for deletion: %w", err)
		default:
			if err := r.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting backup ConfigMap: %w", err)
			}
		}
		return nil
	}

	desiredCM, err := buildBackupConfigMap(cluster)
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = desiredCM.Labels
		cm.Data = desiredCM.Data
		return controllerutil.SetControllerReference(cluster, cm, r.Scheme)
	}); err != nil {
		return fmt.Errorf("upserting backup ConfigMap: %w", err)
	}

	desired := buildBackupCronJob(cluster)
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, cj, func() error {
		cj.Labels = desired.Labels
		cj.Spec = desired.Spec
		return controllerutil.SetControllerReference(cluster, cj, r.Scheme)
	})
	if err != nil {
		r.Recorder.Eventf(cluster, cj, corev1.EventTypeWarning, "BackupScheduleFailed", "UpsertBackup", "Failed to upsert backup CronJob: %v", err)
		return fmt.Errorf("upserting backup CronJob: %w", err)
	}
	if result == controllerutil.OperationResultCreated {
		r.Recorder.Eventf(cluster, cj, corev1.EventTypeNormal, "BackupScheduleCreated", "CreateBackup", "Created backup CronJob with schedule %q", cluster.Spec.Backup.Schedule)
	}
	return nil
}
