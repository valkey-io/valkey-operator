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
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// The podMetadata rules must accept exactly what the API server accepts on a
// pod, apart from the keys reserved for the operator: stricter rules would
// block valid metadata, looser ones would let the workload write fail after
// admission. Each case is checked against the apimachinery validators the API
// server itself uses.
var _ = Describe("podMetadata CEL validation", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	keys := []string{
		"team",
		"Team",
		"app.kubernetes.io/version",
		"prometheus.io/scrape",
		"example.com/some.key_name-1",
		"a.b.c.d.e/x.y.z",
		"sub.domain-1.example.com/Name_With.Dots-and_Underscores",
		"x/y",
		"Example.COM/team",
		strings.Repeat("a", 63),
		strings.Repeat("a", 253) + "/x",
		strings.Repeat("a", 64),
		strings.Repeat("a", 254) + "/x",
		"a/b/c",
		"/team",
		"example.com/",
		"-team",
		"team-",
		"data platform",
		"team!",
		"a..b/team",
		".example.com/team",
		"example.com./team",
		"",
	}
	labelValues := []string{
		"",
		"data",
		"v1.2.3",
		"A_b-c.D",
		strings.Repeat("a", 63),
		strings.Repeat("a", 64),
		"data platform",
		"-data",
		"data-",
		"a/b",
		"x:y",
	}

	podMetadataObjects := func(name string, pm *valkeyiov1alpha1.EmbeddedObjectMetadata) []client.Object {
		return []client.Object{
			&valkeyiov1alpha1.ValkeyCluster{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-c", Namespace: "default"},
				Spec:       valkeyiov1alpha1.ValkeyClusterSpec{Shards: 1, PodMetadata: pm},
			},
			&valkeyiov1alpha1.ValkeyNode{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-n", Namespace: "default"},
				Spec:       valkeyiov1alpha1.ValkeyNodeSpec{PodMetadata: pm},
			},
		}
	}
	expectAdmission := func(name string, pm *valkeyiov1alpha1.EmbeddedObjectMetadata, wantAccepted bool, wantMessage string) {
		GinkgoHelper()
		for _, obj := range podMetadataObjects(name, pm) {
			err := k8sClient.Create(ctx, obj)
			if wantAccepted {
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
				continue
			}
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(wantMessage))
		}
	}

	for i, key := range keys {
		labelAccepted := len(validation.IsQualifiedName(key)) == 0
		It(fmt.Sprintf("label key %q: accepted=%t, like the API server", key, labelAccepted), func() {
			expectAdmission(fmt.Sprintf("pm-lk-%d", i),
				&valkeyiov1alpha1.EmbeddedObjectMetadata{Labels: map[string]valkeyiov1alpha1.LabelValue{key: "v"}},
				labelAccepted, "label keys must be")
		})

		annotationAccepted := len(apivalidation.ValidateAnnotations(map[string]string{key: "v"}, field.NewPath("annotations"))) == 0
		It(fmt.Sprintf("annotation key %q: accepted=%t, like the API server", key, annotationAccepted), func() {
			expectAdmission(fmt.Sprintf("pm-ak-%d", i),
				&valkeyiov1alpha1.EmbeddedObjectMetadata{Annotations: map[string]string{key: "v"}},
				annotationAccepted, "annotation keys must be")
		})
	}

	for i, value := range labelValues {
		accepted := len(validation.IsValidLabelValue(value)) == 0
		It(fmt.Sprintf("label value %q: accepted=%t, like the API server", value, accepted), func() {
			expectAdmission(fmt.Sprintf("pm-lv-%d", i),
				&valkeyiov1alpha1.EmbeddedObjectMetadata{Labels: map[string]valkeyiov1alpha1.LabelValue{"team": valkeyiov1alpha1.LabelValue(value)}},
				accepted, "spec.podMetadata.labels.team")
		})
	}

	It("does not restrict annotation values", func() {
		expectAdmission("pm-av",
			&valkeyiov1alpha1.EmbeddedObjectMetadata{Annotations: map[string]string{
				"example.com/config": "{\"a\": [1, 2]}\nmulti-line, with spaces / slashes: ok",
			}},
			true, "")
	})

	reservedLabelKeys := []string{
		LabelCluster,
		"valkey.io/anything",
		"app.kubernetes.io/name",
		"app.kubernetes.io/instance",
		"app.kubernetes.io/component",
		"app.kubernetes.io/part-of",
		"app.kubernetes.io/managed-by",
	}
	for i, key := range reservedLabelKeys {
		It(fmt.Sprintf("rejects the operator's label key %q", key), func() {
			expectAdmission(fmt.Sprintf("pm-rl-%d", i),
				&valkeyiov1alpha1.EmbeddedObjectMetadata{Labels: map[string]valkeyiov1alpha1.LabelValue{key: "v"}},
				false, "are reserved for the operator")
		})
	}

	for i, key := range []string{configHashKey, "valkey.io/anything"} {
		It(fmt.Sprintf("rejects the operator's annotation key %q", key), func() {
			expectAdmission(fmt.Sprintf("pm-ra-%d", i),
				&valkeyiov1alpha1.EmbeddedObjectMetadata{Annotations: map[string]string{key: "v"}},
				false, "are reserved for the operator")
		})
	}

	It("accepts keys that only look like the operator's", func() {
		expectAdmission("pm-nr",
			&valkeyiov1alpha1.EmbeddedObjectMetadata{
				Labels: map[string]valkeyiov1alpha1.LabelValue{
					"app.kubernetes.io/version": "9.0",
					"team.valkey.io/owner":      "data",
					"valkey.io":                 "v",
				},
				Annotations: map[string]string{"app.kubernetes.io/name": "v"},
			},
			true, "")
	})

	// The pod also carries the operator's config-hash annotation, so the limit
	// is checked against the final map, as the API server will see it.
	operatorAnnotations := map[string]string{configHashKey: strings.Repeat("0", sha256.Size*2)}
	operatorSize := len(configHashKey) + sha256.Size*2
	for _, total := range []int{apivalidation.TotalAnnotationSizeLimitB, apivalidation.TotalAnnotationSizeLimitB + 1} {
		annotations := map[string]string{"example.com/a": "x"}
		annotations["example.com/b"] = strings.Repeat("x", total-operatorSize-len("example.com/a")-len("x")-len("example.com/b"))
		final := maps.Clone(annotations)
		maps.Copy(final, operatorAnnotations)
		accepted := len(apivalidation.ValidateAnnotations(final, field.NewPath("annotations"))) == 0
		It(fmt.Sprintf("annotations totalling %d bytes on the pod: accepted=%t, like the API server", total, accepted), func() {
			expectAdmission(fmt.Sprintf("pm-as-%d", total),
				&valkeyiov1alpha1.EmbeddedObjectMetadata{Annotations: annotations},
				accepted, "annotations must total at most 262059 characters")
		})
	}
})
