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
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"
	"strings"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

const (
	// configRevisionKey is the pod template annotation naming the pinned
	// config revision (spec.configRevision) the pod boots on.
	configRevisionKey = "valkey.io/config-revision"

	liveConfigFileKey = "live.conf"

	// pinnedConfigRevisionLength is the number of hex characters of the
	// sha256 of a pinned file that name its revision.
	pinnedConfigRevisionLength = 12
)

// renderPinnedConfig renders the node's pinned valkey.conf: user config minus
// live-settable and version-gated keys, then the base config, then an include
// of live.conf so live values override any copy in the pinned file.
func renderPinnedConfig(node *valkeyiov1alpha1.ValkeyNode) string {
	excluded := maps.Clone(liveConfigAllowlist)
	maps.Copy(excluded, gatedUserKeysToSuppress(node.Spec.Image, node.Spec.Config))
	hostname := node.Spec.PreferredEndpointType == valkeyiov1alpha1.PreferredEndpointTypeHostname
	var b strings.Builder
	b.WriteString(renderServerConfig(node.Spec.Config, getBaseConfig(node.Spec.TLS, hostname, node.Spec.Image), excluded))
	writeConfigLine(&b, "include", configMountPath+"/"+liveConfigFileKey)
	return b.String()
}

// pinnedConfigRevision names a pinned file by its content.
func pinnedConfigRevision(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])[:pinnedConfigRevisionLength]
}

// liveConfigEntries returns the values live.conf holds: the applied live
// values whose keys are in the live set and still in spec.config, or the
// live-set keys of spec.config when nothing has been applied yet. Keys the
// node's image does not support are dropped.
func liveConfigEntries(node *valkeyiov1alpha1.ValkeyNode) map[string]string {
	var out map[string]string
	if len(node.Status.AppliedLiveConfig) > 0 {
		out = map[string]string{}
		for _, e := range node.Status.AppliedLiveConfig {
			if _, live := liveConfigAllowlist[e.Name]; !live {
				continue
			}
			if _, inSpec := node.Spec.Config[e.Name]; !inSpec {
				continue
			}
			out[e.Name] = e.Value
		}
	} else {
		out = liveConfigToApply(node.Spec.Config)
	}
	for key := range gatedUserKeysToSuppress(node.Spec.Image, out) {
		delete(out, key)
	}
	return out
}

// renderLiveConfig renders the node's live.conf, one sorted line per key.
func renderLiveConfig(node *valkeyiov1alpha1.ValkeyNode) string {
	return renderConfig(liveConfigEntries(node))
}

// specLiveKeys returns the live-set keys present in config.
func specLiveKeys(config map[string]string) map[string]struct{} {
	out := map[string]struct{}{}
	for key := range liveConfigAllowlist {
		if _, ok := config[key]; ok {
			out[key] = struct{}{}
		}
	}
	return out
}

// appliedLiveConfigMap returns status.appliedLiveConfig as a map.
func appliedLiveConfigMap(entries []valkeyiov1alpha1.ConfigEntry) map[string]string {
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		out[e.Name] = e.Value
	}
	return out
}

// configEntries returns config as a list sorted by name.
func configEntries(config map[string]string) []valkeyiov1alpha1.ConfigEntry {
	if len(config) == 0 {
		return nil
	}
	out := make([]valkeyiov1alpha1.ConfigEntry, 0, len(config))
	for _, name := range slices.Sorted(maps.Keys(config)) {
		out = append(out, valkeyiov1alpha1.ConfigEntry{Name: name, Value: config[name]})
	}
	return out
}

// parseConfigLines returns the directives of a config file, each with its
// values in file order. A directive can repeat (save). Comments, blank lines
// and include lines are not directives.
func parseConfigLines(content string) map[string][]string {
	out := map[string][]string{}
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value := line, ""
		if i := strings.IndexAny(line, " \t"); i >= 0 {
			name, value = line[:i], line[i+1:]
		}
		name = strings.ToLower(name)
		if name == "include" {
			continue
		}
		out[name] = append(out[name], strings.TrimSpace(value))
	}
	return out
}

// pinnedConfigRef returns the pinned revision the node's template should
// reference. desired is the pinned file rendered from the node spec. liveRev
// and liveData are the revision the live workload references and its content.
// applied is status.appliedLiveConfig. A key's effective value is its applied
// value if present, else its value in liveData.
//
// The live revision is kept unless some key outside liveSet differs between
// desired and effective (added and removed keys count), or a live-set key
// absent from specLiveKeys is still in liveData: a restart would boot that
// stale pinned value. Otherwise the desired revision is returned, which rolls
// the node. An empty liveRev returns the desired revision.
func pinnedConfigRef(desired string, specLiveKeys map[string]struct{}, liveRev, liveData string,
	applied map[string]string, liveSet map[string]struct{}) string {
	desiredRev := pinnedConfigRevision(desired)
	if liveRev == "" || liveRev == desiredRev {
		return desiredRev
	}
	if pinnedConfigDiffers(parseConfigLines(desired), specLiveKeys, parseConfigLines(liveData), applied, liveSet) {
		return desiredRev
	}
	return liveRev
}

func pinnedConfigDiffers(desired map[string][]string, specLiveKeys map[string]struct{}, live map[string][]string,
	applied map[string]string, liveSet map[string]struct{}) bool {
	effective := func(key string) ([]string, bool) {
		if v, ok := applied[key]; ok {
			return []string{v}, true
		}
		v, ok := live[key]
		return v, ok
	}

	keys := map[string]struct{}{}
	for key := range desired {
		keys[key] = struct{}{}
	}
	for key := range live {
		keys[key] = struct{}{}
	}
	for key := range applied {
		keys[key] = struct{}{}
	}

	for key := range keys {
		if _, isLive := liveSet[key]; isLive {
			if _, inSpec := specLiveKeys[key]; !inSpec {
				if _, inLive := live[key]; inLive {
					return true
				}
			}
			continue
		}
		want, wantOK := desired[key]
		have, haveOK := effective(key)
		if wantOK != haveOK || !slices.Equal(want, have) {
			return true
		}
	}
	return false
}
