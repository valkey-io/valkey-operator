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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// testConfigRevision is a placeholder revision for test nodes that need not
// build a workload. Their spec renders a different one, so the node
// controller writes nothing for it.
const testConfigRevision = "000000000000"

func pinTestCluster(config map[string]string) *valkeyiov1alpha1.ValkeyCluster {
	return &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "pin-test", Namespace: "default"},
		Spec:       valkeyiov1alpha1.ValkeyClusterSpec{Shards: 1, Config: config},
	}
}

func TestRenderPinnedConfig(t *testing.T) {
	node := buildClusterValkeyNode(pinTestCluster(map[string]string{
		"appendfsync":              "always",
		"maxmemory":                "100mb",
		"tls-auto-reload-interval": "60",
	}), 0, 0)
	node.Spec.Image = "valkey/valkey:9.0.0"
	pinned := renderPinnedConfig(node)

	assert.Contains(t, pinned, "appendfsync always\n")
	assert.Contains(t, pinned, "cluster-enabled yes\n")
	assert.NotContains(t, pinned, "maxmemory", "live keys stay out of the pinned file")
	assert.NotContains(t, pinned, "tls-auto-reload-interval", "gated keys are dropped for the image")
	assert.True(t, strings.HasSuffix(pinned, "include /config/live.conf\n"), "the live include comes last")
	assert.Len(t, pinnedConfigRevision(pinned), 12)
}

func TestPinnedConfigRevisionIgnoresLiveKeys(t *testing.T) {
	before := buildClusterValkeyNode(pinTestCluster(map[string]string{"appendfsync": "always", "maxmemory": "100mb"}), 0, 0)
	after := buildClusterValkeyNode(pinTestCluster(map[string]string{"appendfsync": "always", "maxmemory": "500mb"}), 0, 0)
	assert.Equal(t, renderPinnedConfig(before), renderPinnedConfig(after))

	changed := buildClusterValkeyNode(pinTestCluster(map[string]string{"appendfsync": "everysec", "maxmemory": "100mb"}), 0, 0)
	assert.NotEqual(t, pinnedConfigRevision(renderPinnedConfig(before)), pinnedConfigRevision(renderPinnedConfig(changed)))
}

func TestRenderLiveConfig(t *testing.T) {
	cases := []struct {
		name    string
		config  map[string]string
		applied []valkeyiov1alpha1.AppliedLiveConfigEntry
		want    string
	}{
		{
			name:   "nothing applied renders the spec's live keys",
			config: map[string]string{"maxmemory": "100mb", "maxclients": "500", "appendfsync": "always"},
			want:   "maxclients 500\nmaxmemory 100mb\n",
		},
		{
			name:    "applied values win over the spec",
			config:  map[string]string{"maxmemory": "200mb"},
			applied: []valkeyiov1alpha1.AppliedLiveConfigEntry{{Name: "maxmemory", Value: "100mb"}},
			want:    "maxmemory 100mb\n",
		},
		{
			name:   "applied keys removed from the spec are dropped",
			config: map[string]string{"maxmemory": "100mb"},
			applied: []valkeyiov1alpha1.AppliedLiveConfigEntry{
				{Name: "maxclients", Value: "500"},
				{Name: "maxmemory", Value: "100mb"},
			},
			want: "maxmemory 100mb\n",
		},
		{
			name:    "applied keys outside the live set are dropped",
			config:  map[string]string{"appendfsync": "always"},
			applied: []valkeyiov1alpha1.AppliedLiveConfigEntry{{Name: "appendfsync", Value: "always"}},
			want:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := buildClusterValkeyNode(pinTestCluster(tc.config), 0, 0)
			node.Status.AppliedLiveConfig = tc.applied
			assert.Equal(t, tc.want, renderLiveConfig(node))
		})
	}
}

func TestPinnedConfigRef(t *testing.T) {
	liveSet := map[string]struct{}{"maxmemory": {}, "maxmemory-policy": {}, "maxclients": {}}
	keys := func(names ...string) map[string]struct{} {
		out := map[string]struct{}{}
		for _, n := range names {
			out[n] = struct{}{}
		}
		return out
	}
	const base = "# Base Config\ncluster-enabled yes\ndir /data\ninclude /config/live.conf\n"

	cases := []struct {
		name         string
		desired      string
		specLiveKeys map[string]struct{}
		liveData     string
		noLiveRev    bool
		applied      map[string]string
		keepLive     bool
	}{
		{
			name:         "live value change keeps the revision",
			desired:      "# User Config\nappendonly yes\n" + base,
			specLiveKeys: keys("maxmemory"),
			liveData:     "# User Config\nappendonly yes\n# Base Config\ncluster-enabled yes\ndir /data\n",
			applied:      map[string]string{"maxmemory": "200mb"},
			keepLive:     true,
		},
		{
			name:     "restart-only change gives a new revision",
			desired:  "# User Config\nappendonly yes\n" + base,
			liveData: "# User Config\nappendonly no\n" + base + "# old\n",
			keepLive: false,
		},
		{
			name:         "allowlist growth keeps it",
			desired:      "# User Config\nappendonly yes\n" + base,
			specLiveKeys: keys("maxmemory"),
			liveData:     "# User Config\nappendonly yes\nmaxmemory 100mb\n" + base,
			applied:      map[string]string{"maxmemory": "200mb"},
			keepLive:     true,
		},
		{
			name:     "removed live key present in liveData gives a new one",
			desired:  "# User Config\nappendonly yes\n" + base,
			liveData: "# User Config\nappendonly yes\nmaxmemory 100mb\n" + base,
			keepLive: false,
		},
		{
			name:     "added restart-only key gives a new one",
			desired:  "# User Config\nappendonly yes\nappendfsync always\n" + base,
			liveData: "# User Config\nappendonly yes\n" + base,
			keepLive: false,
		},
		{
			name:     "removed restart-only key gives a new one",
			desired:  "# User Config\nappendonly yes\n" + base,
			liveData: "# User Config\nappendonly yes\nappendfsync always\n" + base,
			keepLive: false,
		},
		{
			name:     "comments and include lines are not keys",
			desired:  "# User Config\nappendonly yes\n" + base,
			liveData: "appendonly yes\ncluster-enabled yes\ndir /data\ninclude /init/*.conf\n",
			keepLive: true,
		},
		{
			name:     "save values compared in order",
			desired:  "save 3600 1\nsave 300 100\n" + base,
			liveData: "save 300 100\nsave 3600 1\n" + base + "# old\n",
			keepLive: false,
		},
		{
			name:     "same save values keep it",
			desired:  "save 3600 1\nsave 300 100\n" + base,
			liveData: "# old\nsave 3600 1\nsave 300 100\n" + base,
			keepLive: true,
		},
		{
			name:     "applied value outside the live set overrides liveData",
			desired:  "appendonly yes\nhz 20\n" + base,
			liveData: "appendonly yes\nhz 10\n" + base,
			applied:  map[string]string{"hz": "20"},
			keepLive: true,
		},
		{
			name:      "no live revision gives the desired one",
			desired:   "# User Config\nappendonly yes\n" + base,
			liveData:  "# User Config\nappendonly yes\n" + base + "# old\n",
			noLiveRev: true,
			keepLive:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			liveRev := pinnedConfigRevision(tc.liveData)
			if tc.noLiveRev {
				liveRev = ""
			}
			specLive := tc.specLiveKeys
			if specLive == nil {
				specLive = keys()
			}
			got := pinnedConfigRef(tc.desired, specLive, liveRev, tc.liveData, tc.applied, liveSet)
			want := pinnedConfigRevision(tc.desired)
			if tc.keepLive {
				want = liveRev
			}
			assert.NotEqual(t, pinnedConfigRevision(tc.desired), liveRev, "the case must start from different revisions")
			assert.Equal(t, want, got)
		})
	}
}
