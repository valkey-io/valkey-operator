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
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestGetConfigVolume(t *testing.T) {

	volumeName := "test1"
	mapName := "alice-conf"
	optional := false

	vol := getConfigVolume(volumeName, mapName, optional)

	if vol.Name != volumeName {
		t.Errorf("Expected volume name to be '%v', got %v", volumeName, vol.Name)
	}
	if reflect.TypeOf(vol.VolumeSource) != reflect.TypeOf(corev1.VolumeSource{}) {
		t.Errorf("Expected VolumeSource to be native k8s type")
	}
	if reflect.TypeOf(vol.VolumeSource.ConfigMap) != reflect.TypeOf(&corev1.ConfigMapVolumeSource{}) {
		t.Errorf("Expected VolumeSource.ConfigMap to be native k8s type")
	}
	if vol.VolumeSource.ConfigMap.Name != mapName {
		t.Errorf("Expected ConfigMap name to be '%v', got '%v'", mapName, vol.VolumeSource.ConfigMap.Name)
	}
	if *vol.VolumeSource.ConfigMap.Optional != optional {
		t.Errorf("Expection ConfigMap.Optional to be %v, got %v", optional, vol.VolumeSource.ConfigMap.Optional)
	}
}

func TestGetSecretConfigVolume(t *testing.T) {

	volumeName := "test2"
	secretName := "bob-conf"
	optional := true

	vol := getSecretConfigVolume(volumeName, secretName, optional)

	if vol.Name != volumeName {
		t.Errorf("Expected volume name to be '%v', got %v", volumeName, vol.Name)
	}
	if reflect.TypeOf(vol.VolumeSource) != reflect.TypeOf(corev1.VolumeSource{}) {
		t.Errorf("Expected VolumeSource to be native k8s type")
	}
	if reflect.TypeOf(vol.VolumeSource.Secret) != reflect.TypeOf(&corev1.SecretVolumeSource{}) {
		t.Errorf("Expected VolumeSource.Secret to be native k8s type")
	}
	if vol.VolumeSource.Secret.SecretName != secretName {
		t.Errorf("Expected Secret name to be '%v', got '%v'", secretName, vol.VolumeSource.Secret.SecretName)
	}
	if *vol.VolumeSource.Secret.Optional != optional {
		t.Errorf("Expection Secret.Optional to be %v, got %v", optional, vol.VolumeSource.Secret.Optional)
	}
}
