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
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	valkeyv1 "valkey.io/valkey-operator/api/v1alpha1"
)

func TestCreateClusterStatefulSet(t *testing.T) {
	cluster := &valkeyv1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mycluster",
		},
		Spec: valkeyv1.ValkeyClusterSpec{
			Image: "container:version",
		},
	}
	sts := createStatefulSet(cluster)
	assert.Empty(t, sts.Name, "Expected empty name field")
	assert.Equal(t, "mycluster-", sts.GenerateName)
	assert.Equal(t, int32(1), *sts.Spec.Replicas)
	assert.Len(t, sts.Spec.Template.Spec.Containers, 1)
	assert.Equal(t, "container:version", sts.Spec.Template.Spec.Containers[0].Image)

	assert.Len(t, sts.Spec.Template.Spec.Volumes, 2)
	assert.Equal(t, "scripts", sts.Spec.Template.Spec.Volumes[0].Name)
	assert.Equal(t, "valkey-conf", sts.Spec.Template.Spec.Volumes[1].Name)
}
