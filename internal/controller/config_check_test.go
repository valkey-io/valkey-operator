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
	"errors"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

func TestConfigCheckNameFitsLabel(t *testing.T) {
	short := configCheckName(&valkeyiov1alpha1.ValkeyCluster{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "ns"}})
	if short != "chk.demo.x" {
		t.Fatalf("short name %q", short)
	}
	if short == GetServerConfigMapName("demo-cfg") || strings.HasPrefix(short, resourcePrefix) {
		t.Fatalf("name collides with a server configmap: %s", short)
	}
	long := configCheckName(&valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 200), Namespace: "ns"},
	})
	if len(long) > 63 {
		t.Fatalf("len %d name %q", len(long), long)
	}
	if strings.HasPrefix(long, resourcePrefix) || long == "chk."+strings.Repeat("a", 200)+".x" {
		t.Fatalf("long name was not shortened safely: %s", long)
	}
}

func TestNewConfigCheckPodParsesWithTheClusterImage(t *testing.T) {
	runAsNonRoot := true
	cluster := &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "ns"},
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{
			Image:            "valkey/valkey:9.0.0",
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "pull"}},
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
			},
			Scheduling: &valkeyiov1alpha1.SchedulingSpec{
				NodeSelector:      map[string]string{"pool": "valkey"},
				PriorityClassName: "high",
				Tolerations:       []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "valkey"}},
				Affinity:          &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{}},
			},
		},
	}
	pod := newConfigCheckPod(cluster, "abc")
	if pod.Name != "chk.demo.x" || pod.Namespace != "ns" {
		t.Fatalf("identity %s/%s", pod.Namespace, pod.Name)
	}
	if pod.Annotations[configCheckHashAnnotation] != "abc" {
		t.Fatalf("hash %q", pod.Annotations[configCheckHashAnnotation])
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("restart %s", pod.Spec.RestartPolicy)
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("containers %d", len(pod.Spec.Containers))
	}
	container := pod.Spec.Containers[0]
	if container.Image != "valkey/valkey:9.0.0" {
		t.Fatalf("image %s", container.Image)
	}
	if container.ImagePullPolicy != defaultImagePullPolicy(container.Image) {
		t.Fatalf("pull policy %s", container.ImagePullPolicy)
	}
	wantRes := configCheckResources()
	if container.Resources.Requests.Cpu().Cmp(wantRes.Requests[corev1.ResourceCPU]) != 0 ||
		container.Resources.Requests.Memory().Cmp(wantRes.Requests[corev1.ResourceMemory]) != 0 {
		t.Fatalf("fixed request %#v", container.Resources.Requests)
	}
	if container.Resources.Limits.Cpu().Cmp(wantRes.Limits[corev1.ResourceCPU]) != 0 ||
		container.Resources.Limits.Memory().Cmp(wantRes.Limits[corev1.ResourceMemory]) != 0 {
		t.Fatalf("limits must match the fixed request, got %#v", container.Resources.Limits)
	}
	script := container.Command[2]
	if !strings.Contains(script, "FATAL CONFIG FILE ERROR") || !strings.Contains(script, "--aclfile") || !strings.Contains(script, "--bind 127.0.0.1") {
		t.Fatalf("script does not ask valkey to parse: %s", script)
	}
	// cluster-enabled no would hide "replicaof directive not allowed in cluster mode".
	if strings.Contains(script, "--cluster-enabled") {
		t.Fatal("cluster-enabled must stay as rendered")
	}
	if container.SecurityContext == nil || container.SecurityContext.ReadOnlyRootFilesystem == nil || !*container.SecurityContext.ReadOnlyRootFilesystem {
		t.Fatal("root filesystem should be read-only")
	}
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.RunAsNonRoot == nil || *pod.Spec.SecurityContext.RunAsNonRoot != runAsNonRoot {
		t.Fatal("pod should run as non-root")
	}
	if pod.Spec.SecurityContext.RunAsUser == nil || *pod.Spec.SecurityContext.RunAsUser != configCheckUID {
		t.Fatal("pod should set uid 999; the image user is root and this command is not valkey-server")
	}
	if pod.Labels[LabelConfigCheck] != cluster.Name {
		t.Fatalf("config-check label %v", pod.Labels)
	}
	if pod.Labels["app.kubernetes.io/managed-by"] != "valkey-operator" {
		t.Fatalf("managed-by label %v", pod.Labels)
	}
	if _, ok := pod.Labels[LabelCluster]; ok {
		t.Fatal("config check pod must not carry the cluster label")
	}
	cluster.Labels = map[string]string{LabelCluster: cluster.Name}
	copied := newConfigCheckPod(cluster, "abc")
	if _, ok := copied.Labels[LabelCluster]; ok {
		t.Fatal("a cluster metadata label must not put the check pod in the headless Service")
	}
	if container.SecurityContext.RunAsUser == nil || *container.SecurityContext.RunAsUser != configCheckUID {
		t.Fatal("container should run as uid 999")
	}
	if pod.Spec.NodeSelector["pool"] != "valkey" || pod.Spec.PriorityClassName != "high" || len(pod.Spec.Tolerations) != 1 {
		t.Fatalf("scheduling was not copied: %#v", pod.Spec)
	}
	if pod.Spec.Affinity != nil {
		t.Fatal("affinity should not be copied onto the one-shot pod")
	}
	if pod.Spec.ImagePullSecrets[0].Name != "pull" {
		t.Fatal("image pull secret was not copied")
	}
	var conf, tmp, data bool
	for _, mount := range container.VolumeMounts {
		if mount.Name == "valkey-conf" && mount.MountPath == "/config" && mount.ReadOnly {
			conf = true
		}
		if mount.Name == "tmp" && mount.MountPath == "/tmp" {
			tmp = true
		}
		if mount.Name == dataVolumeName && mount.MountPath == dataMountPath {
			data = true
		}
	}
	if !conf || !tmp || !data {
		t.Fatalf("mounts %#v", container.VolumeMounts)
	}
}

func TestConfigCheckPodSecurityContextKeepsExplicitUser(t *testing.T) {
	uid := int64(1000)
	gid := int64(1000)
	cluster := &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "ns"},
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{
			PodSecurityContext: &corev1.PodSecurityContext{RunAsUser: &uid, RunAsGroup: &gid},
		},
	}
	pod := newConfigCheckPod(cluster, "abc")
	if pod.Spec.SecurityContext.RunAsUser == nil || *pod.Spec.SecurityContext.RunAsUser != uid {
		t.Fatalf("uid %#v", pod.Spec.SecurityContext.RunAsUser)
	}
	if pod.Spec.SecurityContext.RunAsGroup == nil || *pod.Spec.SecurityContext.RunAsGroup != gid {
		t.Fatalf("gid %#v", pod.Spec.SecurityContext.RunAsGroup)
	}
}

func TestConfigCheckPodSecurityContextAllowsExplicitRoot(t *testing.T) {
	nonRoot := false
	sc := configCheckPodSecurityContext(&corev1.PodSecurityContext{RunAsNonRoot: &nonRoot})
	if sc.RunAsUser != nil {
		t.Fatalf("explicit root should not be rewritten to %d", *sc.RunAsUser)
	}
	if sc.RunAsNonRoot == nil || *sc.RunAsNonRoot {
		t.Fatal("explicit RunAsNonRoot false should be kept")
	}
}

func TestConfigCheckInputHashTracksPodInputs(t *testing.T) {
	cluster := &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "ns"},
		Spec:       valkeyiov1alpha1.ValkeyClusterSpec{Image: "valkey/valkey:9.0.0"},
	}
	base, err := configCheckInputHash(cluster, "cluster-enabled yes\n")
	if err != nil {
		t.Fatal(err)
	}
	again, err := configCheckInputHash(cluster, "cluster-enabled yes\n")
	if err != nil || again != base {
		t.Fatalf("hash not stable: %v %s %s", err, base, again)
	}
	cluster.Spec.Image = "valkey/valkey:9.1.0"
	changed, err := configCheckInputHash(cluster, "cluster-enabled yes\n")
	if err != nil || changed == base {
		t.Fatal("image change should replace the check pod")
	}
	cluster.Spec.Image = "valkey/valkey:9.0.0"
	cluster.Spec.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
	changed, err = configCheckInputHash(cluster, "cluster-enabled yes\n")
	if err != nil || changed != base {
		t.Fatal("a fixed check request must ignore spec.resources")
	}
}

func TestConfigCheckPodStale(t *testing.T) {
	now := time.Now()
	fresh := &corev1.Pod{}
	fresh.CreationTimestamp = metav1.NewTime(now.Add(-time.Minute))
	if configCheckPodStale(fresh, now) {
		t.Fatal("a one-minute-old pod is not stale")
	}
	old := &corev1.Pod{}
	old.CreationTimestamp = metav1.NewTime(now.Add(-configCheckPendingDeadline - time.Second))
	if !configCheckPodStale(old, now) {
		t.Fatal("a pod past the deadline should be replaced")
	}
	unset := &corev1.Pod{}
	if configCheckPodStale(unset, now) {
		t.Fatal("a pod with no creation time should be kept")
	}
}

func TestConfigCheckRefusesForeignObject(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := valkeyiov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	demo := &valkeyiov1alpha1.ValkeyCluster{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "ns", UID: "demo-uid"}}
	other := &valkeyiov1alpha1.ValkeyCluster{ObjectMeta: metav1.ObjectMeta{Name: "demo-cfg", Namespace: "ns", UID: "other-uid"}}
	foreign := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configCheckName(demo),
			Namespace: "ns",
			Labels:    labels(other),
		},
		Data: map[string]string{configFileKey: "live", readinessScriptKey: "keep"},
	}
	if err := controllerutil.SetControllerReference(other, foreign, scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(foreign).Build()
	r := &ValkeyClusterReconciler{Client: c, APIReader: c, Scheme: scheme}

	r.deleteConfigCheck(ctx, demo)
	got := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(foreign), got); err != nil {
		t.Fatal(err)
	}
	if got.Data[configFileKey] != "live" || got.Data[readinessScriptKey] != "keep" {
		t.Fatalf("foreign configmap changed: %#v", got.Data)
	}
	err := r.ensureConfigCheckConfigMap(ctx, demo, "candidate\n", "hash")
	if err == nil {
		t.Fatal("expected an ownership error")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(foreign), got); err != nil {
		t.Fatal(err)
	}
	if got.Data[configFileKey] != "live" || got.Data[readinessScriptKey] != "keep" {
		t.Fatalf("foreign configmap overwritten: %#v", got.Data)
	}
}

func TestUnchangedFileWithANewImageStartsACheck(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := valkeyiov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "ns", UID: "demo-uid"},
		Spec:       valkeyiov1alpha1.ValkeyClusterSpec{Image: "valkey/valkey:8.0.0"},
	}
	rendered := buildServerConfig(cluster)
	published := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      GetServerConfigMapName(cluster.Name),
			Namespace: "ns",
			Labels:    labels(cluster),
		},
		Data: map[string]string{configFileKey: rendered},
	}
	node := &valkeyiov1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo-0-0",
			Namespace: "ns",
			Labels:    map[string]string{LabelCluster: cluster.Name},
		},
		Spec: valkeyiov1alpha1.ValkeyNodeSpec{Image: "valkey/valkey:9.0.0"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(published, node).Build()
	r := &ValkeyClusterReconciler{Client: c, APIReader: c, Scheme: scheme}
	err := r.checkRenderedConfig(ctx, cluster, rendered)
	var pending *configCheckPendingError
	if !errors.As(err, &pending) {
		t.Fatalf("image downgrade should be checked, got %v", err)
	}

	stored := &valkeyiov1alpha1.ValkeyNode{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(node), stored); err != nil {
		t.Fatal(err)
	}
	stored.Spec.Image = cluster.Spec.Image
	if err := c.Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	// Nodes already on spec.image, and no image stamped on the file, is the
	// upgrade case. The check pod from the downgrade is removed and not replaced.
	if err := r.checkRenderedConfig(ctx, cluster, rendered); err != nil {
		t.Fatalf("nodes already on spec.image should skip the check: %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: configCheckName(cluster), Namespace: "ns"}, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("check pod created for an image the nodes already run: %v", err)
	}
}

var _ = Describe("unparsable spec.config", func() {
	var accept func(context.Context, *valkeyiov1alpha1.ValkeyCluster, string) error

	BeforeEach(func() {
		accept = serverConfigParsesFn
		serverConfigParsesFn = nil
	})

	AfterEach(func() {
		serverConfigParsesFn = accept
	})

	It("does not replace a published valkey.conf when the image rejects the candidate", func() {
		ctx := context.Background()
		const name = "cfg-keep"
		cluster := newConfigCheckCluster(name)
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { deleteConfigCheckObjects(ctx, name) })

		r := newConfigCheckReconciler()
		serverConfigParsesFn = func(context.Context, *valkeyiov1alpha1.ValkeyCluster, string) error { return nil }
		Expect(r.upsertConfigMap(ctx, cluster)).To(Succeed())

		serverCM := &corev1.ConfigMap{}
		serverKey := types.NamespacedName{Name: GetServerConfigMapName(name), Namespace: "default"}
		Expect(k8sClient.Get(ctx, serverKey, serverCM)).To(Succeed())
		published := serverCM.Data[configFileKey]
		Expect(published).NotTo(ContainSubstring("ACTOKEY"))

		serverConfigParsesFn = func(context.Context, *valkeyiov1alpha1.ValkeyCluster, string) error {
			return &configRejectedError{message: "rejected ACTOKEY ACTOSTRING"}
		}
		cluster.Spec.Config = map[string]string{"ACTOKEY": "ACTOSTRING"}
		err := r.upsertConfigMap(ctx, cluster)
		var rejected *configRejectedError
		Expect(errors.As(err, &rejected)).To(BeTrue())

		Expect(k8sClient.Get(ctx, serverKey, serverCM)).To(Succeed())
		Expect(serverCM.Data[configFileKey]).To(Equal(published))
	})

	It("keeps the server file unpublished when the check pod is still running or exits 1", func() {
		ctx := context.Background()
		const name = "cfg-reject"
		cluster := newConfigCheckCluster(name)
		cluster.Spec.Config = map[string]string{"ACTOKEY": "ACTOSTRING"}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { deleteConfigCheckObjects(ctx, name) })

		r := newConfigCheckReconciler()
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(configCheckPendingRequeue))

		serverKey := types.NamespacedName{Name: GetServerConfigMapName(name), Namespace: "default"}
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, serverKey, &corev1.ConfigMap{}))).To(BeTrue())

		checkPod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: configCheckName(cluster), Namespace: "default"}, checkPod)).To(Succeed())
		Expect(checkPod.Labels).To(HaveKeyWithValue(LabelConfigCheck, name))
		Expect(checkPod.Labels).NotTo(HaveKey(LabelCluster))
		Expect(checkPod.Spec.SecurityContext.RunAsUser).NotTo(BeNil())
		Expect(*checkPod.Spec.SecurityContext.RunAsUser).To(Equal(configCheckUID))

		checkCM := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: configCheckName(cluster), Namespace: "default"}, checkCM)).To(Succeed())
		Expect(checkCM.Data[configFileKey]).To(ContainSubstring("ACTOKEY ACTOSTRING"))
		Expect(checkCM.Labels).To(HaveKeyWithValue(LabelConfigCheck, name))
		Expect(checkCM.Labels).NotTo(HaveKey(LabelCluster))

		markConfigCheckExit(ctx, configCheckName(cluster), 1, "*** FATAL CONFIG FILE ERROR ***\n>>> 'ACTOKEY ACTOSTRING'\nBad directive or wrong number of arguments")

		result, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(30 * time.Second))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, serverKey, &corev1.ConfigMap{}))).To(BeTrue())

		updated := &valkeyiov1alpha1.ValkeyCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, updated)).To(Succeed())
		cond := meta.FindStatusCondition(updated.Status.Conditions, valkeyiov1alpha1.ConditionServerConfig)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(valkeyiov1alpha1.ReasonConfigRejected))
		Expect(cond.Message).To(ContainSubstring("ACTOKEY ACTOSTRING"))
		Expect(updated.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateFailed))

		events := collectEvents(r.Recorder.(*events.FakeRecorder))
		Expect(events).To(ContainElement(ContainSubstring("ConfigRejected")))

		// The failed pod stays, so an unchanged spec is not checked again.
		result, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(30 * time.Second))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: configCheckName(cluster), Namespace: "default"}, &corev1.Pod{})).To(Succeed())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, serverKey, &corev1.ConfigMap{}))).To(BeTrue())
	})

	It("publishes valkey.conf only after the check pod exits 0", func() {
		ctx := context.Background()
		const name = "cfg-accept"
		cluster := newConfigCheckCluster(name)
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { deleteConfigCheckObjects(ctx, name) })

		r := newConfigCheckReconciler()
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
		serverKey := types.NamespacedName{Name: GetServerConfigMapName(name), Namespace: "default"}
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, serverKey, &corev1.ConfigMap{}))).To(BeTrue())

		markConfigCheckExit(ctx, configCheckName(cluster), 0, "")
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())

		serverCM := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, serverKey, serverCM)).To(Succeed())
		Expect(serverCM.Data[configFileKey]).To(ContainSubstring("cluster-enabled yes"))
		Expect(serverCM.Data[configFileKey]).NotTo(ContainSubstring("ACTOKEY"))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: configCheckName(cluster), Namespace: "default"}, &corev1.Pod{}))).To(BeTrue())

		updated := &valkeyiov1alpha1.ValkeyCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, updated)).To(Succeed())
		Expect(meta.FindStatusCondition(updated.Status.Conditions, valkeyiov1alpha1.ConditionServerConfig)).To(BeNil())

		// The published file matches, so a later reconcile does not start another check.
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: configCheckName(cluster), Namespace: "default"}, &corev1.Pod{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: configCheckName(cluster), Namespace: "default"}, &corev1.ConfigMap{}))).To(BeTrue())
	})

	It("keeps a check pod that exits 128 because the image has no shell", func() {
		ctx := context.Background()
		const name = "cfg-noshell"
		cluster := newConfigCheckCluster(name)
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { deleteConfigCheckObjects(ctx, name) })

		r := newConfigCheckReconciler()
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
		markConfigCheckExit(ctx, configCheckName(cluster), configCheckExitStartError, "failed to create containerd task: stat /bin/sh: no such file")

		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(configCheckPendingRequeue))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: configCheckName(cluster), Namespace: "default"}, &corev1.Pod{})).To(Succeed())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: GetServerConfigMapName(name), Namespace: "default"}, &corev1.ConfigMap{}))).To(BeTrue())

		// The same exit on the next pass still does not delete the pod.
		result, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(configCheckPendingRequeue))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: configCheckName(cluster), Namespace: "default"}, &corev1.Pod{})).To(Succeed())
	})

	It("keeps serving the published file and does not copy a rejected candidate onto ValkeyNodes", func() {
		ctx := context.Background()
		const name = "cfg-hold"
		cluster := newConfigCheckCluster(name)
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { deleteConfigCheckObjects(ctx, name) })

		r := newConfigCheckReconciler()
		serverConfigParsesFn = func(context.Context, *valkeyiov1alpha1.ValkeyCluster, string) error { return nil }
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())

		serverKey := types.NamespacedName{Name: GetServerConfigMapName(name), Namespace: "default"}
		serverCM := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, serverKey, serverCM)).To(Succeed())
		published := serverCM.Data[configFileKey]

		nodeKey := types.NamespacedName{Name: valkeyNodeName(name, 0, 0), Namespace: "default"}
		node := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, nodeKey, node)).To(Succeed())
		Expect(node.Spec.Config).NotTo(HaveKey("ACTOKEY"))

		fresh := &valkeyiov1alpha1.ValkeyCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, fresh)).To(Succeed())
		fresh.Spec.Config = map[string]string{"ACTOKEY": "ACTOSTRING"}
		fresh.Spec.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
		Expect(k8sClient.Update(ctx, fresh)).To(Succeed())

		serverConfigParsesFn = func(context.Context, *valkeyiov1alpha1.ValkeyCluster, string) error {
			return &configRejectedError{message: "rejected ACTOKEY ACTOSTRING"}
		}
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).NotTo(BeZero())

		Expect(k8sClient.Get(ctx, serverKey, serverCM)).To(Succeed())
		Expect(serverCM.Data[configFileKey]).To(Equal(published))
		Expect(k8sClient.Get(ctx, nodeKey, node)).To(Succeed())
		Expect(node.Spec.Config).NotTo(HaveKey("ACTOKEY"))
		Expect(node.Spec.Resources.Limits.Memory().Equal(resource.MustParse("1Gi"))).To(BeTrue())

		updated := &valkeyiov1alpha1.ValkeyCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, updated)).To(Succeed())
		cond := meta.FindStatusCondition(updated.Status.Conditions, valkeyiov1alpha1.ConditionServerConfig)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(valkeyiov1alpha1.ReasonConfigRejected))
		Expect(updated.Status.State).NotTo(Equal(valkeyiov1alpha1.ClusterStateFailed))
	})

	It("keeps reconciling a published cluster when the check cannot be created", func() {
		ctx := context.Background()
		const name = "cfg-forbid"
		cluster := newConfigCheckCluster(name)
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { deleteConfigCheckObjects(ctx, name) })

		r := newConfigCheckReconciler()
		serverConfigParsesFn = func(context.Context, *valkeyiov1alpha1.ValkeyCluster, string) error { return nil }
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())

		nodeKey := types.NamespacedName{Name: valkeyNodeName(name, 0, 0), Namespace: "default"}
		node := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, nodeKey, node)).To(Succeed())
		before := node.Spec.Config

		fresh := &valkeyiov1alpha1.ValkeyCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, fresh)).To(Succeed())
		fresh.Spec.Config = map[string]string{"ACTOKEY": "ACTOSTRING"}
		Expect(k8sClient.Update(ctx, fresh)).To(Succeed())

		serverConfigParsesFn = func(context.Context, *valkeyiov1alpha1.ValkeyCluster, string) error {
			return errors.New("pods is forbidden")
		}
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
		// The node is not Ready in envtest, so that wait is shorter than the
		// check backstop. Either way reconcile did not return the create error.
		Expect(result.RequeueAfter).NotTo(BeZero())
		Expect(k8sClient.Get(ctx, nodeKey, node)).To(Succeed())
		Expect(node.Spec.Config).NotTo(HaveKey("ACTOKEY"))
		Expect(node.Spec.Config).To(Equal(before))

		updated := &valkeyiov1alpha1.ValkeyCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, updated)).To(Succeed())
		cond := meta.FindStatusCondition(updated.Status.Conditions, valkeyiov1alpha1.ConditionServerConfig)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(valkeyiov1alpha1.ReasonConfigCheckPending))
		Expect(cond.Message).To(ContainSubstring("forbidden"))
		Expect(updated.Status.State).NotTo(Equal(valkeyiov1alpha1.ClusterStateFailed))
	})

	It("reads the check pod through the managed-by cache and publishes after exit 0", func() {
		ctx, stop := context.WithCancel(context.Background())
		DeferCleanup(stop)
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                 k8sClient.Scheme(),
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			Cache:                  cache.Options{ByObject: ManagedCacheByObject()},
		})
		Expect(err).NotTo(HaveOccurred())
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(ctx)).To(Succeed())
		}()
		Expect(mgr.GetCache().WaitForCacheSync(ctx)).To(BeTrue())

		const name = "cfg-cache"
		cluster := newConfigCheckCluster(name)
		Expect(k8sClient.Create(context.Background(), cluster)).To(Succeed())
		DeferCleanup(func() { deleteConfigCheckObjects(context.Background(), name) })
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, cluster)).To(Succeed())

		r := newConfigCheckReconciler()
		r.Client = mgr.GetClient()
		r.APIReader = mgr.GetClient()
		r.Scheme = mgr.GetScheme()

		err = r.checkRenderedConfig(context.Background(), cluster, buildServerConfig(cluster))
		var pending *configCheckPendingError
		Expect(errors.As(err, &pending)).To(BeTrue())

		checkName := configCheckName(cluster)
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: checkName, Namespace: "default"}, pod)).To(Succeed())
		Expect(pod.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "valkey-operator"))
		Expect(pod.Labels).To(HaveKeyWithValue(LabelConfigCheck, name))

		Eventually(func() error {
			return mgr.GetClient().Get(context.Background(), types.NamespacedName{Name: checkName, Namespace: "default"}, &corev1.Pod{})
		}, 10*time.Second, 100*time.Millisecond).Should(Succeed())

		markConfigCheckExit(context.Background(), checkName, 0, "")
		Eventually(func() int32 {
			got := &corev1.Pod{}
			if err := mgr.GetClient().Get(context.Background(), types.NamespacedName{Name: checkName, Namespace: "default"}, got); err != nil {
				return -1
			}
			term := terminatedConfigCheck(got)
			if term == nil {
				return -1
			}
			return term.ExitCode
		}, 10*time.Second, 100*time.Millisecond).Should(Equal(int32(0)))

		Expect(r.upsertConfigMap(context.Background(), cluster)).To(Succeed())
		server := &corev1.ConfigMap{}
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: GetServerConfigMapName(name), Namespace: "default"}, server)).To(Succeed())
		Expect(server.Data[configFileKey]).To(ContainSubstring("cluster-enabled yes"))
		Expect(server.Annotations).To(HaveKeyWithValue(configCheckedImageKey, "valkey/valkey:9.0.0"))
	})
})

func newConfigCheckCluster(name string) *valkeyiov1alpha1.ValkeyCluster {
	return &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{
			Shards:   1,
			Replicas: 0,
			Image:    "valkey/valkey:9.0.0",
		},
	}
}

func newConfigCheckReconciler() *ValkeyClusterReconciler {
	return &ValkeyClusterReconciler{
		Client:        k8sClient,
		APIReader:     k8sClient,
		Scheme:        k8sClient.Scheme(),
		Recorder:      events.NewFakeRecorder(100),
		ValkeyClients: testValkeyClients(k8sClient),
		clusterStateFunc: func(context.Context, *valkeyiov1alpha1.ValkeyNodeList) *valkey.ClusterState {
			return &valkey.ClusterState{}
		},
	}
}

func markConfigCheckExit(ctx context.Context, name string, code int32, message string) {
	pod := &corev1.Pod{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, pod)).To(Succeed())
	pod.Status.Phase = corev1.PodFailed
	if code == 0 {
		pod.Status.Phase = corev1.PodSucceeded
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: configCheckContainerName,
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Reason: "Error", Message: message},
		},
	}}
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
}

func deleteConfigCheckObjects(ctx context.Context, name string) {
	ns := client.InNamespace("default")
	labels := client.MatchingLabels{LabelCluster: name}
	checkLabels := client.MatchingLabels{LabelConfigCheck: name}
	_ = k8sClient.DeleteAllOf(ctx, &corev1.Pod{}, ns, labels)
	_ = k8sClient.DeleteAllOf(ctx, &corev1.Pod{}, ns, checkLabels)
	_ = k8sClient.DeleteAllOf(ctx, &corev1.ConfigMap{}, ns, labels)
	_ = k8sClient.DeleteAllOf(ctx, &corev1.ConfigMap{}, ns, checkLabels)
	_ = k8sClient.DeleteAllOf(ctx, &corev1.Service{}, ns, labels)
	_ = k8sClient.DeleteAllOf(ctx, &corev1.Secret{}, ns, labels)
	_ = k8sClient.DeleteAllOf(ctx, &policyv1.PodDisruptionBudget{}, ns, labels)
	_ = k8sClient.DeleteAllOf(ctx, &valkeyiov1alpha1.ValkeyNode{}, ns, labels)
	_ = k8sClient.Delete(ctx, &valkeyiov1alpha1.ValkeyCluster{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}})
}
