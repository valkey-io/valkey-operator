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

package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/valkey-io/valkey-operator/internal/controller"
)

func TestDefaultImagesJSON(t *testing.T) {
	t.Run("the compiled defaults are set", func(t *testing.T) {
		out, err := defaultImagesJSON()
		require.NoError(t, err)
		var images defaultImages
		require.NoError(t, json.Unmarshal(out, &images))
		assert.NotEmpty(t, images.Valkey)
		assert.NotEmpty(t, images.Exporter)
	})

	t.Run("an override is what gets printed, valkey first", func(t *testing.T) {
		valkey, exporter := controller.DefaultImage, controller.DefaultExporterImage
		t.Cleanup(func() { controller.DefaultImage, controller.DefaultExporterImage = valkey, exporter })
		controller.DefaultImage = "example.com/valkey:1"
		controller.DefaultExporterImage = "example.com/exporter:2"
		out, err := defaultImagesJSON()
		require.NoError(t, err)
		assert.Equal(t, `{"valkey":"example.com/valkey:1","exporter":"example.com/exporter:2"}`, string(out))
	})
}
