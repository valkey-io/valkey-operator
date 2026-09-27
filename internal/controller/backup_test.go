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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyv1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

func backupTestCluster() *valkeyv1.ValkeyCluster {
	return &valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "prod"},
		Spec: valkeyv1.ValkeyClusterSpec{
			Shards:   3,
			Replicas: 1,
			Backup: &valkeyv1.BackupSpec{
				Schedule:  "0 2 * * *",
				Retention: 7,
				Storage: valkeyv1.BackupStorage{S3: &valkeyv1.S3Storage{
					Bucket:            "valkey-backups",
					Endpoint:          "http://s3.s3.svc:9000",
					CredentialsSecret: "s3-creds",
				}},
			},
		},
	}
}

func envValue(env []corev1.EnvVar, name string) string {
	for _, e := range env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

func envSecretKey(env []corev1.EnvVar, name string) string {
	for _, e := range env {
		if e.Name == name && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			return e.ValueFrom.SecretKeyRef.Name + "/" + e.ValueFrom.SecretKeyRef.Key
		}
	}
	return ""
}

func TestBuildBackupCronJob(t *testing.T) {
	cluster := backupTestCluster()
	cj := buildBackupCronJob(cluster)

	assert.Equal(t, "valkey-shop-backup", cj.Name)
	assert.Equal(t, "0 2 * * *", cj.Spec.Schedule)
	assert.False(t, *cj.Spec.Suspend)
	assert.Equal(t, "valkey-backup", cj.Labels["app.kubernetes.io/component"])
	assert.Equal(t, "shop", cj.Labels[LabelCluster])

	pod := cj.Spec.JobTemplate.Spec.Template.Spec
	require.Len(t, pod.InitContainers, 1)
	require.Len(t, pod.Containers, 1)
	dump, upload := pod.InitContainers[0], pod.Containers[0]

	// The dump runs on the cluster's own image and finds the shards through
	// the headless Service, reading as the two system users it needs.
	assert.Equal(t, DefaultImage, dump.Image)
	assert.Equal(t, []string{"sh", "/scripts/backup-dump.sh"}, dump.Command)
	assert.Equal(t, "valkey-shop", envValue(dump.Env, "VALKEY_HOST"))
	assert.Equal(t, "Replica", envValue(dump.Env, "BACKUP_SOURCE"), "Replica is the default source")
	assert.Equal(t, "internal-shop-system-passwords/_operator", envSecretKey(dump.Env, "VALKEY_OPERATOR_PASSWORD"))
	assert.Equal(t, "internal-shop-system-passwords/_replication", envSecretKey(dump.Env, "VALKEY_REPLICATION_PASSWORD"))
	assert.Empty(t, envValue(dump.Env, "VALKEY_TLS_ARGS"), "no TLS flags on a plaintext cluster")

	// The upload runs on the backup image with rclone configured from env.
	assert.Equal(t, DefaultBackupImage, upload.Image)
	assert.Equal(t, []string{"sh", "/scripts/backup-upload.sh"}, upload.Command)
	assert.Equal(t, "http://s3.s3.svc:9000", envValue(upload.Env, "RCLONE_CONFIG_S3_ENDPOINT"))
	assert.Equal(t, "s3-creds/AWS_ACCESS_KEY_ID", envSecretKey(upload.Env, "RCLONE_CONFIG_S3_ACCESS_KEY_ID"))
	assert.Equal(t, "s3-creds/AWS_SECRET_ACCESS_KEY", envSecretKey(upload.Env, "RCLONE_CONFIG_S3_SECRET_ACCESS_KEY"))
	assert.Equal(t, "valkey-backups", envValue(upload.Env, "BACKUP_BUCKET"))
	assert.Equal(t, "shop", envValue(upload.Env, "BACKUP_PREFIX"), "the prefix defaults to the cluster name")
	assert.Equal(t, "7", envValue(upload.Env, "BACKUP_RETENTION"))
	assert.Empty(t, envValue(upload.Env, "RCLONE_CONFIG_S3_REGION"))

	// Both share the scratch volume, and the scripts come from the backup ConfigMap.
	names := make([]string, 0, len(pod.Volumes))
	for _, v := range pod.Volumes {
		names = append(names, v.Name)
	}
	assert.ElementsMatch(t, []string{"backup", "scripts"}, names)
	for _, v := range pod.Volumes {
		if v.Name == "scripts" {
			assert.Equal(t, "valkey-shop-backup", v.ConfigMap.Name)
		}
	}
}

func TestBuildBackupCronJobOptions(t *testing.T) {
	cluster := backupTestCluster()
	suspend := true
	cluster.Spec.Backup.Suspend = &suspend
	cluster.Spec.Backup.Source = valkeyv1.BackupSourcePrimary
	cluster.Spec.Backup.Image = "example.com/rclone:custom"
	cluster.Spec.Backup.Storage.S3.Prefix = "clusters/shop"
	cluster.Spec.Backup.Storage.S3.Region = "eu-central-1"
	cluster.Spec.Image = "valkey/valkey:9.1.0"
	cluster.Spec.Networking = &valkeyv1.NetworkingSpec{TLS: &valkeyv1.TLSSpec{
		Certificates: valkeyv1.TLSCertificates{Server: valkeyv1.CertificateSource{SecretName: "shop-tls"}},
	}}

	cj := buildBackupCronJob(cluster)
	assert.True(t, *cj.Spec.Suspend)
	pod := cj.Spec.JobTemplate.Spec.Template.Spec
	dump, upload := pod.InitContainers[0], pod.Containers[0]
	assert.Equal(t, "valkey/valkey:9.1.0", dump.Image)
	assert.Equal(t, "Primary", envValue(dump.Env, "BACKUP_SOURCE"))
	assert.Equal(t, "--tls --cacert /tls/ca.crt", envValue(dump.Env, "VALKEY_TLS_ARGS"))
	assert.Equal(t, "example.com/rclone:custom", upload.Image)
	assert.Equal(t, "clusters/shop", envValue(upload.Env, "BACKUP_PREFIX"))
	assert.Equal(t, "eu-central-1", envValue(upload.Env, "RCLONE_CONFIG_S3_REGION"))

	// TLS mounts the server Secret into the dump container only.
	var tlsVolume *corev1.Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == tlsVolumeName {
			tlsVolume = &pod.Volumes[i]
		}
	}
	require.NotNil(t, tlsVolume, "TLS clusters mount the server certificate Secret")
	assert.Equal(t, "shop-tls", tlsVolume.Secret.SecretName)
	assert.Contains(t, dump.VolumeMounts, corev1.VolumeMount{Name: tlsVolumeName, MountPath: tlsCertMountPath, ReadOnly: true})
	for _, m := range upload.VolumeMounts {
		assert.NotEqual(t, tlsVolumeName, m.Name, "the upload never talks to Valkey")
	}
}

func TestBuildBackupConfigMap(t *testing.T) {
	cm, err := buildBackupConfigMap(backupTestCluster())
	require.NoError(t, err)
	assert.Equal(t, "valkey-shop-backup", cm.Name)
	assert.Contains(t, cm.Data["backup-dump.sh"], "valkey-cli")
	assert.Contains(t, cm.Data["backup-dump.sh"], "--rdb")
	assert.Contains(t, cm.Data["backup-upload.sh"], "rclone")
}
