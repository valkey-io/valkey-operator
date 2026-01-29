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
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	valkeyv1 "valkey.io/valkey-operator/api/v1alpha1"
)

func TestCreateClusterDeployment(t *testing.T) {
	cluster := &valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mycluster",
		},
		Spec: valkeyv1.ValkeyClusterSpec{
			Image: "container:version",
		},
	}
	d := createClusterDeployment(cluster)
	if d.Name != "" {
		t.Errorf("Expected empty name field, got %v", d.Name)
	}
	if d.GenerateName != "mycluster-" {
		t.Errorf("Expected %v, got %v", "mycluster-", d.GenerateName)
	}
	if *d.Spec.Replicas != 1 {
		t.Errorf("Expected %v, got %v", 1, d.Spec.Replicas)
	}
	if len(d.Spec.Template.Spec.Containers) != 1 {
		t.Errorf("Expected %v, got %v", 1, len(d.Spec.Template.Spec.Containers))
	}
	if d.Spec.Template.Spec.Containers[0].Image != "container:version" {
		t.Errorf("Expected %v, got %v", "container:version", d.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestCreateClusterDeployment_SetsPodAntiAffinity(t *testing.T) {
	antiAffinity := &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
				{
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/instance": "mycluster",
						},
					},
					TopologyKey: "kubernetes.io/hostname",
				},
			},
		},
	}
	cluster := &valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "mycluster"},
		Spec: valkeyv1.ValkeyClusterSpec{
			Image:    "container:version",
			Affinity: antiAffinity,
		},
	}

	d := createClusterDeployment(cluster)

	got := d.Spec.Template.Spec.Affinity
	if diff := cmp.Diff(antiAffinity, got, cmpopts.EquateEmpty()); diff != "" {
		t.Fatalf("affinity mismatch (-want +got):\n%s", diff)
	}
}

func TestGenerateContainersDef(t *testing.T) {
	t.Run("should return only valkey-server when exporter is disabled", func(t *testing.T) {
		cluster := &valkeyv1.ValkeyCluster{
			Spec: valkeyv1.ValkeyClusterSpec{
				Exporter: valkeyv1.ExporterSpec{
					Enabled: false,
				},
			},
		}
		containers := generateContainersDef(cluster)
		assert.Len(t, containers, 1, "should have only one container")
		assert.Equal(t, "valkey-server", containers[0].Name, "container should be valkey-server")
	})

	t.Run("should include metrics-exporter when exporter is enabled", func(t *testing.T) {
		cluster := &valkeyv1.ValkeyCluster{
			Spec: valkeyv1.ValkeyClusterSpec{
				Exporter: valkeyv1.ExporterSpec{
					Enabled: true,
				},
			},
		}
		containers := generateContainersDef(cluster)
		assert.Len(t, containers, 2, "should have two containers")
		assert.True(t, containerExists(containers, "valkey-server"), "valkey-server container should exist")
		assert.True(t, containerExists(containers, "metrics-exporter"), "metrics-exporter container should exist")
	})

	t.Run("should use default exporter image when not specified", func(t *testing.T) {
		cluster := &valkeyv1.ValkeyCluster{
			Spec: valkeyv1.ValkeyClusterSpec{
				Exporter: valkeyv1.ExporterSpec{
					Enabled: true,
				},
			},
		}
		containers := generateContainersDef(cluster)
		exporter := findContainer(containers, "metrics-exporter")
		assert.NotNil(t, exporter, "exporter container should not be nil")
		assert.Equal(t, DefaultExporterImage, exporter.Image, "should use default exporter image")
	})

	t.Run("should use custom exporter image when specified", func(t *testing.T) {
		customImage := "my-exporter:1.2.3"
		cluster := &valkeyv1.ValkeyCluster{
			Spec: valkeyv1.ValkeyClusterSpec{
				Exporter: valkeyv1.ExporterSpec{
					Enabled: true,
					Image:   customImage,
				},
			},
		}
		containers := generateContainersDef(cluster)
		exporter := findContainer(containers, "metrics-exporter")
		assert.NotNil(t, exporter, "exporter container should not be nil")
		assert.Equal(t, customImage, exporter.Image, "should use custom exporter image")
	})

	t.Run("should set custom resources for exporter", func(t *testing.T) {
		resources := corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
		}
		cluster := &valkeyv1.ValkeyCluster{
			Spec: valkeyv1.ValkeyClusterSpec{
				Exporter: valkeyv1.ExporterSpec{
					Enabled:   true,
					Resources: resources,
				},
			},
		}
		containers := generateContainersDef(cluster)
		exporter := findContainer(containers, "metrics-exporter")
		assert.NotNil(t, exporter, "exporter container should not be nil")
		assert.Equal(t, resources, exporter.Resources, "should set custom resources on exporter")
	})

	t.Run("should set custom resources requests for valkey-server", func(t *testing.T) {
		resourceReqs := corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceRequestsMemory: resource.MustParse("100Mi"),
				corev1.ResourceRequestsCPU:    resource.MustParse("100m"),
			},
		}
		cluster := &valkeyv1.ValkeyCluster{
			Spec: valkeyv1.ValkeyClusterSpec{
				Resources: resourceReqs,
			},
		}
		containers := generateContainersDef(cluster)
		valkeyContainer := findContainer(containers, "valkey-server")
		assert.Equal(t, resourceReqs, valkeyContainer.Resources, "should set custom resources requests on valkey-server")
	})

	t.Run("should set custom resources limits for valkey-server", func(t *testing.T) {
		resourceReqs := corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceLimitsMemory: resource.MustParse("500Mi"),
				corev1.ResourceLimitsCPU:    resource.MustParse("200m"),
			},
		}
		cluster := &valkeyv1.ValkeyCluster{
			Spec: valkeyv1.ValkeyClusterSpec{
				Resources: resourceReqs,
			},
		}
		containers := generateContainersDef(cluster)
		valkeyContainer := findContainer(containers, "valkey-server")
		assert.Equal(t, resourceReqs, valkeyContainer.Resources, "should set custom resources limits on valkey-server")
	})

	t.Run("should set custom resources for valkey-server", func(t *testing.T) {
		resourceReqs := corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceLimitsMemory: resource.MustParse("500Mi"),
				corev1.ResourceLimitsCPU:    resource.MustParse("200m"),
			},
			Requests: corev1.ResourceList{
				corev1.ResourceRequestsMemory: resource.MustParse("100Mi"),
				corev1.ResourceRequestsCPU:    resource.MustParse("100m"),
			},
		}
		cluster := &valkeyv1.ValkeyCluster{
			Spec: valkeyv1.ValkeyClusterSpec{
				Resources: resourceReqs,
			},
		}
		containers := generateContainersDef(cluster)
		valkeyContainer := findContainer(containers, "valkey-server")
		assert.Equal(t, resourceReqs, valkeyContainer.Resources, "should set custom resources on valkey-server")
	})
}

func containerExists(containers []corev1.Container, name string) bool {
	return findContainer(containers, name) != nil
}

func findContainer(containers []corev1.Container, name string) *corev1.Container {
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}

func TestLivenessCheckScript(t *testing.T) {
	scriptPath := filepath.Join("scripts", "liveness-check.sh")

	tests := []struct {
		name     string
		response string
		wantErr  bool
	}{
		{name: "pong", response: "PONG", wantErr: false},
		{name: "loading", response: "LOADING 123", wantErr: false},
		{name: "masterdown", response: "MASTERDOWN Link with MASTER is down", wantErr: false},
		{name: "error", response: "ERR something bad", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := runProbeScript(t, scriptPath, test.response); (err != nil) != test.wantErr {
				t.Fatalf("unexpected result: err=%v wantErr=%v", err, test.wantErr)
			}
		})
	}
}

func TestReadinessCheckScript(t *testing.T) {
	scriptPath := filepath.Join("scripts", "readiness-check.sh")

	tests := []struct {
		name     string
		response string
		wantErr  bool
	}{
		{name: "pong", response: "PONG", wantErr: false},
		{name: "loading", response: "LOADING 123", wantErr: true},
		{name: "masterdown", response: "MASTERDOWN Link with MASTER is down", wantErr: true},
		{name: "error", response: "ERR something bad", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := runProbeScript(t, scriptPath, test.response); (err != nil) != test.wantErr {
				t.Fatalf("unexpected result: err=%v wantErr=%v", err, test.wantErr)
			}
		})
	}
}

func runProbeScript(t *testing.T, scriptPath, response string) error {
	t.Helper()

	// Stub valkey-cli so the script uses the response we provide.
	binDir := t.TempDir()
	valkeyCli := filepath.Join(binDir, "valkey-cli")
	script := []byte("#!/bin/sh\n" +
		"echo \"${VALKEY_RESPONSE:-PONG}\"\n")
	if err := os.WriteFile(valkeyCli, script, 0o755); err != nil {
		t.Fatalf("write valkey-cli stub: %v", err)
	}

	cmd := exec.Command(scriptPath)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"VALKEY_RESPONSE="+response,
	)
	return cmd.Run()
}
