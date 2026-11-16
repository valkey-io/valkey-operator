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

package features

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/component-base/featuregate"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

func TestGatesDefaultOff(t *testing.T) {
	g := NewGate()
	for _, f := range []string{"Valkey", "ValkeySentinel"} {
		if g.Enabled(featuregate.Feature(f)) {
			t.Errorf("%s is on by default", f)
		}
	}
}

func TestGateSet(t *testing.T) {
	g := NewGate()
	if err := g.Set("Valkey=true"); err != nil {
		t.Fatal(err)
	}
	if !g.Enabled(Valkey) || g.Enabled(ValkeySentinel) {
		t.Errorf("Valkey=%v ValkeySentinel=%v, want true and false", g.Enabled(Valkey), g.Enabled(ValkeySentinel))
	}
	if err := g.Set("Nope=true"); err == nil {
		t.Error("an unknown gate was accepted")
	}
}

func TestRequireCRD(t *testing.T) {
	installed := meta.NewDefaultRESTMapper([]schema.GroupVersion{valkeyiov1alpha1.GroupVersion})
	installed.Add(valkeyiov1alpha1.GroupVersion.WithKind("Valkey"), meta.RESTScopeNamespace)
	missing := meta.NewDefaultRESTMapper(nil)

	tests := []struct {
		name    string
		set     string
		mapper  meta.RESTMapper
		wantErr string
	}{
		{"gate off, no CRD", "", missing, ""},
		{"gate on, CRD installed", "Valkey=true", installed, ""},
		{"gate on, no CRD", "Valkey=true", missing, "its CRD is not installed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGate()
			if tt.set != "" {
				if err := g.Set(tt.set); err != nil {
					t.Fatal(err)
				}
			}
			err := requireCRD(g, tt.mapper, Valkey)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
