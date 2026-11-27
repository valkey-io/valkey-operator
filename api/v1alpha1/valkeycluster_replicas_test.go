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

package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGetReplicasPerShard(t *testing.T) {
	for name, tc := range map[string]struct {
		spec ValkeyClusterSpec
		want int32
	}{
		"both unset":            {spec: ValkeyClusterSpec{}, want: 0},
		"replicasPerShard only": {spec: ValkeyClusterSpec{ReplicasPerShard: 2}, want: 2},
		"replicas only":         {spec: ValkeyClusterSpec{Replicas: 2}, want: 2},
	} {
		t.Run(name, func(t *testing.T) {
			c := &ValkeyCluster{Spec: tc.spec}
			assert.Equal(t, tc.want, c.GetReplicasPerShard())
		})
	}
}
