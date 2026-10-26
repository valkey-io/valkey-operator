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
	"bytes"
	"encoding/json"
	"flag"
	"strings"
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

// restoreDefaults puts the compiled defaults back after a test moved them.
func restoreDefaults(t *testing.T) {
	t.Helper()
	valkey, exporter := controller.DefaultImage, controller.DefaultExporterImage
	t.Cleanup(func() { controller.DefaultImage, controller.DefaultExporterImage = valkey, exporter })
}

func TestImageFlags(t *testing.T) {
	t.Run("an override is trimmed and the other default stays", func(t *testing.T) {
		restoreDefaults(t)
		exporter := controller.DefaultExporterImage
		fs := flag.NewFlagSet("manager", flag.ContinueOnError)
		printDefaultImages, printVersion := imageFlags(fs)
		require.NoError(t, fs.Parse([]string{"--default-valkey-image= example.com/valkey:1 ", "--print-default-images"}))
		require.NoError(t, validateDefaultImages())
		assert.Equal(t, "example.com/valkey:1", controller.DefaultImage)
		assert.Equal(t, exporter, controller.DefaultExporterImage)
		assert.True(t, *printDefaultImages)
		assert.False(t, *printVersion)
	})

	t.Run("a blank value is rejected with its flag named", func(t *testing.T) {
		restoreDefaults(t)
		fs := flag.NewFlagSet("manager", flag.ContinueOnError)
		imageFlags(fs)
		require.NoError(t, fs.Parse([]string{"--default-exporter-image=  "}))
		assert.EqualError(t, validateDefaultImages(), "--default-exporter-image must not be empty")
	})
}

func TestPrintRequested(t *testing.T) {
	t.Run("nothing requested starts the manager", func(t *testing.T) {
		var out bytes.Buffer
		done, err := printRequested(false, false, &out)
		require.NoError(t, err)
		assert.False(t, done)
		assert.Empty(t, out.String())
	})

	t.Run("both flags print the version, then the images, and exit", func(t *testing.T) {
		var out bytes.Buffer
		done, err := printRequested(true, true, &out)
		require.NoError(t, err)
		assert.True(t, done)
		lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
		assert.Contains(t, lines[0], "valkey-operator, version")
		images, err := defaultImagesJSON()
		require.NoError(t, err)
		assert.Equal(t, string(images), lines[len(lines)-1])
	})
}
