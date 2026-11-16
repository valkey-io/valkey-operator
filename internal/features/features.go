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

// Package features defines the operator's feature gates.
package features

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/component-base/featuregate"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

const (
	// Valkey enables the Valkey controller.
	Valkey featuregate.Feature = "Valkey"

	// ValkeySentinel enables the ValkeySentinel controller.
	ValkeySentinel featuregate.Feature = "ValkeySentinel"
)

var specs = map[featuregate.Feature]featuregate.FeatureSpec{
	Valkey:         {Default: false, PreRelease: featuregate.Alpha},
	ValkeySentinel: {Default: false, PreRelease: featuregate.Alpha},
}

// NewGate returns a gate that knows the operator's features.
func NewGate() featuregate.MutableFeatureGate {
	g := featuregate.NewFeatureGate()
	utilruntime.Must(g.Add(specs))
	return g
}

// State returns every known gate and its value, sorted by name.
// For example "Valkey=false,ValkeySentinel=true".
func State(g featuregate.FeatureGate) string {
	names := slices.Sorted(maps.Keys(specs))
	parts := make([]string, 0, len(names))
	for _, f := range names {
		parts = append(parts, fmt.Sprintf("%s=%t", f, g.Enabled(f)))
	}
	return strings.Join(parts, ",")
}

// RequireCRDs returns an error when an enabled gate has no CRD installed.
// Without it the controller would log "no matches for kind" until the CRD appears.
func RequireCRDs(g featuregate.FeatureGate, mapper meta.RESTMapper) error {
	for _, f := range []featuregate.Feature{Valkey, ValkeySentinel} {
		if err := requireCRD(g, mapper, f); err != nil {
			return err
		}
	}
	return nil
}

func requireCRD(g featuregate.FeatureGate, mapper meta.RESTMapper, f featuregate.Feature) error {
	if !g.Enabled(f) {
		return nil
	}
	gk := schema.GroupKind{Group: valkeyiov1alpha1.GroupVersion.Group, Kind: string(f)}
	if _, err := mapper.RESTMapping(gk); err != nil {
		if meta.IsNoMatchError(err) {
			return fmt.Errorf("feature gate %s is enabled but its CRD is not installed: install the alpha CRDs first (make install-alpha)", f)
		}
		return fmt.Errorf("checking the %s CRD: %w", f, err)
	}
	return nil
}
