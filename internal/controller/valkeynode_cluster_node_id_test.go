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
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

func TestNextClusterNodeID(t *testing.T) {
	tests := []struct {
		name                                     string
		recordedID, recordedVolumeUID, volumeUID string
		readID                                   string
		want                                     string
	}{
		{"same volume, read ok", "x", "u1", "u1", "x", "x"},
		{"same volume, no read (unready or failed)", "x", "u1", "u1", "", "x"},
		{"same volume, node answers with another ID", "x", "u1", "u1", "y", "y"},
		{"volume replaced, no read yet", "x", "u1", "u2", "", ""},
		{"volume replaced, read ok", "x", "u1", "u2", "y", "y"},
		{"volume gone", "x", "u1", "", "", ""},
		{"volume missing, read ok", "x", "u1", "", "x", ""},
		{"first reconcile, read ok", "", "", "u1", "x", "x"},
		{"first reconcile, no volume, read ok", "", "", "", "x", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, nextClusterNodeID(tt.recordedID, tt.recordedVolumeUID, tt.volumeUID, tt.readID))
		})
	}
}

func TestDataVolumeUID(t *testing.T) {
	persistent := &valkeyiov1alpha1.ValkeyNode{Spec: valkeyiov1alpha1.ValkeyNodeSpec{
		Persistence: &valkeyiov1alpha1.PersistenceSpec{},
	}}
	ephemeral := &valkeyiov1alpha1.ValkeyNode{}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: types.UID("q1")}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{UID: types.UID("p1")}}
	now := metav1.Now()
	terminating := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{UID: types.UID("p1"), DeletionTimestamp: &now}}

	tests := []struct {
		name string
		node *valkeyiov1alpha1.ValkeyNode
		pvc  *corev1.PersistentVolumeClaim
		pod  *corev1.Pod
		want string
	}{
		{"persistence uses the PVC", persistent, pvc, pod, "p1"},
		{"persistence, PVC nil", persistent, nil, pod, ""},
		{"persistence, PVC terminating", persistent, terminating, pod, "p1"},
		{"no persistence uses the pod", ephemeral, pvc, pod, "q1"},
		{"no persistence, pod nil", ephemeral, nil, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, dataVolumeUID(tt.node, tt.pvc, tt.pod))
		})
	}
}
