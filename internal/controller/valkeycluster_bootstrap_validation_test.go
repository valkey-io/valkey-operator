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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

func bootstrapCluster(name string, stage valkeyiov1alpha1.BootstrapStage) *valkeyiov1alpha1.ValkeyCluster {
	return &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{
			Shards:           1,
			ReplicasPerShard: 0,
			Bootstrap: &valkeyiov1alpha1.BootstrapSpec{
				Source: valkeyiov1alpha1.BootstrapSource{Endpoints: []string{"old-0.invalid:6379"}},
				Stage:  stage,
			},
		},
	}
}

var _ = Describe("ValkeyClusterSpec bootstrap CEL validation", func() {
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	create := func(cluster *valkeyiov1alpha1.ValkeyCluster) error {
		err := k8sClient.Create(ctx, cluster)
		if err == nil {
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })
		}
		return err
	}
	update := func(name string, mutate func(*valkeyiov1alpha1.ValkeyCluster)) error {
		GinkgoHelper()
		cluster := &valkeyiov1alpha1.ValkeyCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, cluster)).To(Succeed())
		mutate(cluster)
		return k8sClient.Update(ctx, cluster)
	}
	setStage := func(stage valkeyiov1alpha1.BootstrapStage) func(*valkeyiov1alpha1.ValkeyCluster) {
		return func(c *valkeyiov1alpha1.ValkeyCluster) { c.Spec.Bootstrap.Stage = stage }
	}
	expectInvalid := func(err error, message string) {
		GinkgoHelper()
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got %v", err)
		Expect(err.Error()).To(ContainSubstring(message))
	}

	It("defaults the stage to Bootstrap", func() {
		Expect(create(bootstrapCluster("bootstrap-default-stage", ""))).To(Succeed())
		stored := &valkeyiov1alpha1.ValkeyCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "bootstrap-default-stage", Namespace: "default"}, stored)).To(Succeed())
		Expect(stored.Spec.Bootstrap.Stage).To(Equal(valkeyiov1alpha1.BootstrapStageBootstrap))
	})

	It("rejects a stage that is not Bootstrap, Cutover or Complete", func() {
		expectInvalid(create(bootstrapCluster("bootstrap-bad-stage", "Done")), "Unsupported value")
	})

	It("moves forward through Bootstrap, Cutover and Complete", func() {
		Expect(create(bootstrapCluster("bootstrap-forward", valkeyiov1alpha1.BootstrapStageBootstrap))).To(Succeed())
		Expect(update("bootstrap-forward", setStage(valkeyiov1alpha1.BootstrapStageCutover))).To(Succeed())
		Expect(update("bootstrap-forward", setStage(valkeyiov1alpha1.BootstrapStageComplete))).To(Succeed())
	})

	It("allows going from Bootstrap straight to Complete", func() {
		Expect(create(bootstrapCluster("bootstrap-skip", valkeyiov1alpha1.BootstrapStageBootstrap))).To(Succeed())
		Expect(update("bootstrap-skip", setStage(valkeyiov1alpha1.BootstrapStageComplete))).To(Succeed())
	})

	It("rejects moving back from Cutover to Bootstrap", func() {
		Expect(create(bootstrapCluster("bootstrap-back-1", valkeyiov1alpha1.BootstrapStageCutover))).To(Succeed())
		expectInvalid(update("bootstrap-back-1", setStage(valkeyiov1alpha1.BootstrapStageBootstrap)), "can only move forward")
	})

	It("rejects moving back from Complete", func() {
		Expect(create(bootstrapCluster("bootstrap-back-2", valkeyiov1alpha1.BootstrapStageComplete))).To(Succeed())
		expectInvalid(update("bootstrap-back-2", setStage(valkeyiov1alpha1.BootstrapStageCutover)), "can only move forward")
		expectInvalid(update("bootstrap-back-2", setStage(valkeyiov1alpha1.BootstrapStageBootstrap)), "can only move forward")
	})

	It("keeps bootstrap.source immutable", func() {
		Expect(create(bootstrapCluster("bootstrap-source", valkeyiov1alpha1.BootstrapStageBootstrap))).To(Succeed())
		err := update("bootstrap-source", func(c *valkeyiov1alpha1.ValkeyCluster) {
			c.Spec.Bootstrap.Source.Endpoints = []string{"other-0.invalid:6379"}
		})
		expectInvalid(err, "bootstrap.source is immutable")
	})

	It("cannot add spec.bootstrap to an existing cluster or remove it", func() {
		plain := bootstrapCluster("bootstrap-add", "")
		plain.Spec.Bootstrap = nil
		Expect(create(plain)).To(Succeed())
		expectInvalid(update("bootstrap-add", func(c *valkeyiov1alpha1.ValkeyCluster) {
			c.Spec.Bootstrap = bootstrapCluster("x", "").Spec.Bootstrap
		}), "")

		Expect(create(bootstrapCluster("bootstrap-remove", valkeyiov1alpha1.BootstrapStageBootstrap))).To(Succeed())
		expectInvalid(update("bootstrap-remove", func(c *valkeyiov1alpha1.ValkeyCluster) { c.Spec.Bootstrap = nil }), "")
	})
})
