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
	"fmt"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

var _ = Describe("ValkeySentinel controller", func() {
	var (
		ctx        context.Context
		reconciler *ValkeySentinelReconciler
		s          *valkeyiov1alpha1.ValkeySentinel
		req        reconcile.Request
		observed   sentinelObservation
	)

	BeforeEach(func() {
		ctx = context.Background()
		observed = sentinelObservation{}
		reconciler = &ValkeySentinelReconciler{
			Client:    k8sClient,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  events.NewFakeRecorder(100),
			observeFunc: func(context.Context, *valkeyiov1alpha1.ValkeySentinel, string) (sentinelObservation, error) {
				return observed, nil
			},
		}
		s = &valkeyiov1alpha1.ValkeySentinel{
			ObjectMeta: metav1.ObjectMeta{Name: "quorum", Namespace: "default"},
			Spec: valkeyiov1alpha1.ValkeySentinelSpec{
				Config: map[string]string{"sentinel deny-scripts-reconfig": "yes", "port": "1234"},
			},
		}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		req = reconcile.Request{NamespacedName: types.NamespacedName{Name: s.Name, Namespace: s.Namespace}}

		// envtest runs no garbage collector, so delete the children explicitly.
		DeferCleanup(func() {
			name := sentinelResourceName(s)
			pods := &corev1.PodList{}
			_ = k8sClient.List(ctx, pods, client.InNamespace(s.Namespace), client.MatchingLabels(sentinelSelector(s)))
			for i := range pods.Items {
				_ = k8sClient.Delete(ctx, &pods.Items[i], client.GracePeriodSeconds(0))
			}
			for _, obj := range []client.Object{
				&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace}},
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace}},
				&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace}},
				&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace}},
				s,
			} {
				_ = k8sClient.Delete(ctx, obj)
			}
		})
	})

	key := func(name string) types.NamespacedName {
		return types.NamespacedName{Name: name, Namespace: "default"}
	}
	getStatus := func() valkeyiov1alpha1.ValkeySentinelStatus {
		current := &valkeyiov1alpha1.ValkeySentinel{}
		Expect(k8sClient.Get(ctx, req.NamespacedName, current)).To(Succeed())
		return current.Status
	}

	It("creates the Sentinel children owned by the ValkeySentinel", func() {
		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		name := sentinelResourceName(s)

		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, key(name), svc)).To(Succeed())
		Expect(svc.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))
		Expect(svc.Spec.PublishNotReadyAddresses).To(BeTrue())
		Expect(svc.Spec.Selector).To(Equal(map[string]string{LabelSentinel: s.Name}))
		Expect(svc.Spec.Ports[0].Port).To(Equal(int32(26379)))

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, key(name), cm)).To(Succeed())
		Expect(cm.Data[sentinelConfigKey]).To(ContainSubstring("sentinel deny-scripts-reconfig yes\n"))
		Expect(cm.Data[sentinelConfigKey]).To(ContainSubstring("port 26379\n"))
		Expect(cm.Data[sentinelConfigKey]).NotTo(ContainSubstring("port 1234"))

		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key(name), sts)).To(Succeed())
		Expect(metav1.IsControlledBy(sts, s)).To(BeTrue())
		Expect(*sts.Spec.Replicas).To(Equal(int32(3)))
		Expect(sts.Spec.ServiceName).To(Equal(name))
		Expect(sts.Spec.UpdateStrategy.Type).To(Equal(appsv1.OnDeleteStatefulSetStrategyType))
		Expect(sts.Spec.PodManagementPolicy).To(Equal(appsv1.ParallelPodManagement))
		Expect(sts.Spec.Template.Annotations).To(HaveKeyWithValue(configHashKey, sentinelConfigHash(cm)))
		Expect(sts.Spec.Template.Spec.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution).To(HaveLen(1))

		pdb := &policyv1.PodDisruptionBudget{}
		Expect(k8sClient.Get(ctx, key(name), pdb)).To(Succeed())
		Expect(pdb.Spec.Selector.MatchLabels).To(Equal(map[string]string{LabelSentinel: s.Name}))

		status := getStatus()
		Expect(status.State).To(Equal(valkeyiov1alpha1.ValkeySentinelStateReconciling))
		Expect(status.Reason).To(Equal(valkeyiov1alpha1.ReasonWaitingForSentinels))
		warn := meta.FindStatusCondition(status.Conditions, valkeyiov1alpha1.ConditionConfigurationWarning)
		Expect(warn).NotTo(BeNil())
		Expect(warn.Message).To(ContainSubstring(`"port" is managed by the operator`))
	})

	It("does not rewrite the StatefulSet when nothing changed", func() {
		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key(sentinelResourceName(s)), sts)).To(Succeed())
		rv := sts.ResourceVersion

		_, err = reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key(sentinelResourceName(s)), sts)).To(Succeed())
		Expect(sts.ResourceVersion).To(Equal(rv))
	})

	It("warns on an even replica count and deletes the PDB when disabled", func() {
		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, req.NamespacedName, s)).To(Succeed())
		s.Spec.Replicas = 4
		s.Spec.PodDisruptionBudget = &valkeyiov1alpha1.SentinelPodDisruptionBudgetConfig{Mode: valkeyiov1alpha1.SentinelPDBModeDisabled}
		Expect(k8sClient.Update(ctx, s)).To(Succeed())
		_, err = reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		warn := meta.FindStatusCondition(getStatus().Conditions, valkeyiov1alpha1.ConditionConfigurationWarning)
		Expect(warn.Reason).To(Equal(valkeyiov1alpha1.ReasonMultipleConfigurationWarnings))
		Expect(warn.Message).To(ContainSubstring("spec.replicas 4 is even"))
		err = k8sClient.Get(ctx, key(sentinelResourceName(s)), &policyv1.PodDisruptionBudget{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	Describe("rolling pods on an old revision", func() {
		// setup reconciles once, then fakes what the StatefulSet controller would do:
		// three ready pods, the highest two on the old revision.
		setup := func() *appsv1.StatefulSet {
			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, key(sentinelResourceName(s)), sts)).To(Succeed())
			sts.Status = appsv1.StatefulSetStatus{
				ObservedGeneration: sts.Generation,
				Replicas:           3, ReadyReplicas: 3,
				CurrentRevision: "old", UpdateRevision: "new",
			}
			Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())
			for i, rev := range []string{"new", "old", "old"} {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("%s-%d", sentinelResourceName(s), i),
						Namespace: s.Namespace,
						Labels:    map[string]string{LabelSentinel: s.Name, appsv1.ControllerRevisionHashLabelKey: rev},
					},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "sentinel", Image: "valkey"}}},
				}
				Expect(k8sClient.Create(ctx, pod)).To(Succeed())
				pod.Status = corev1.PodStatus{
					PodIP:      fmt.Sprintf("10.0.0.%d", i+1),
					Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
				}
				Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
			}
			return sts
		}
		podExists := func(i int) bool {
			err := k8sClient.Get(ctx, key(fmt.Sprintf("%s-%d", sentinelResourceName(s), i)), &corev1.Pod{})
			return err == nil
		}

		It("deletes only the highest-ordinal outdated pod", func() {
			setup()
			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(podExists(0)).To(BeTrue())
			Expect(podExists(1)).To(BeTrue())
			Expect(podExists(2)).To(BeFalse())
			Expect(getStatus().Reason).To(Equal(valkeyiov1alpha1.ReasonRollingSentinels))
		})

		It("holds the roll while a monitor has no quorum", func() {
			setup()
			observed = sentinelObservation{monitors: []valkeyiov1alpha1.SentinelMonitorStatus{{Name: "cache", OK: false}}}
			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(podExists(2)).To(BeTrue())
			status := getStatus()
			Expect(status.State).To(Equal(valkeyiov1alpha1.ValkeySentinelStateDegraded))
			Expect(status.Monitors).To(HaveLen(1))
			ready := meta.FindStatusCondition(status.Conditions, valkeyiov1alpha1.ConditionReady)
			Expect(ready.Reason).To(Equal(valkeyiov1alpha1.ReasonRollBlocked))
		})

		It("holds the roll while a failover is in progress", func() {
			setup()
			observed = sentinelObservation{
				monitors:           []valkeyiov1alpha1.SentinelMonitorStatus{{Name: "cache", OK: true}},
				failoverInProgress: true,
			}
			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(podExists(2)).To(BeTrue())
			Expect(getStatus().Message).To(ContainSubstring("failover is in progress"))
		})
	})
})

func TestRenderSentinelConfig(t *testing.T) {
	s := &valkeyiov1alpha1.ValkeySentinel{
		ObjectMeta: metav1.ObjectMeta{Name: "q", Namespace: "ns"},
		Spec: valkeyiov1alpha1.ValkeySentinelSpec{
			Config: map[string]string{"loglevel": "verbose", "Sentinel Announce-IP": "1.2.3.4", "DIR": "/tmp"},
			Networking: &valkeyiov1alpha1.SentinelNetworkingSpec{TLS: &valkeyiov1alpha1.SentinelTLSSpec{
				Certificates: valkeyiov1alpha1.TLSCertificates{Server: valkeyiov1alpha1.CertificateSource{SecretName: "tls"}},
			}},
		},
	}
	conf, warnings := renderSentinelConfig(s)
	for _, want := range []string{"loglevel verbose\n", "port 0\n", "tls-port 26379\n", "dir /data\n", "sentinel resolve-hostnames yes\n", "tls-auth-clients optional\n"} {
		if !strings.Contains(conf, want) {
			t.Errorf("config missing %q:\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "/tmp") || strings.Contains(conf, "1.2.3.4") {
		t.Errorf("operator-owned keys must win:\n%s", conf)
	}
	if len(warnings) != 2 {
		t.Errorf("want 2 warnings for the case-insensitive owned keys, got %v", warnings)
	}
}

func TestSentinelAffinity(t *testing.T) {
	s := &valkeyiov1alpha1.ValkeySentinel{ObjectMeta: metav1.ObjectMeta{Name: "q"}}
	if a := sentinelAffinity(s); a.PodAntiAffinity == nil || len(a.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Fatalf("default anti-affinity missing: %+v", a)
	}

	userAnti := &corev1.PodAntiAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "zone"}}}
	s.Spec.Scheduling = &valkeyiov1alpha1.SentinelSchedulingSpec{Affinity: &corev1.Affinity{PodAntiAffinity: userAnti}}
	if a := sentinelAffinity(s); len(a.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 0 || len(a.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Errorf("user anti-affinity must win: %+v", a.PodAntiAffinity)
	}

	s.Spec.Scheduling = &valkeyiov1alpha1.SentinelSchedulingSpec{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{}}}
	if a := sentinelAffinity(s); a.NodeAffinity == nil || a.PodAntiAffinity == nil {
		t.Errorf("node affinity must be kept and anti-affinity added: %+v", a)
	}
	if s.Spec.Scheduling.Affinity.PodAntiAffinity != nil {
		t.Errorf("sentinelAffinity must not mutate the spec")
	}
}

func TestPodOrdinal(t *testing.T) {
	for name, want := range map[string]int{"valkey-sentinel-q-0": 0, "valkey-sentinel-q-12": 12, "nodash": -1, "valkey-sentinel-q-x": -1} {
		if got := podOrdinal(name); got != want {
			t.Errorf("podOrdinal(%q) = %d, want %d", name, got, want)
		}
	}
}
