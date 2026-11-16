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
	"flag"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/valkey-io/valkey-operator/internal/controller"
)

func TestImageFlags(t *testing.T) {
	const (
		valkeyFlag   = "--default-valkey-image"
		exporterFlag = "--default-exporter-image"
		valkeyArg    = valkeyFlag + "= example.com/valkey:1 "
		exporterArg  = exporterFlag + "= example.com/exporter:2 "
	)
	for _, tc := range []struct {
		name      string
		args      []string
		wantError string
	}{
		{"both set", []string{valkeyArg, exporterArg}, ""},
		{"missing Valkey", []string{exporterArg}, valkeyFlag + " must not be empty"},
		{"blank Valkey", []string{valkeyFlag + "= ", exporterArg}, valkeyFlag + " must not be empty"},
		{"missing exporter", []string{valkeyArg}, exporterFlag + " must not be empty"},
		{"blank exporter", []string{valkeyArg, exporterFlag + "= "}, exporterFlag + " must not be empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			valkey, exporter := controller.DefaultImage, controller.DefaultExporterImage
			t.Cleanup(func() { controller.DefaultImage, controller.DefaultExporterImage = valkey, exporter })

			fs := flag.NewFlagSet("manager", flag.ContinueOnError)
			imageFlags(fs)
			require.NoError(t, fs.Parse(tc.args))
			err := validateDefaultImages()
			if tc.wantError != "" {
				require.EqualError(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "example.com/valkey:1", controller.DefaultImage)
			require.Equal(t, "example.com/exporter:2", controller.DefaultExporterImage)
		})
	}
}
