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
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// rotatedAtAnnotation is where system password rotation keeps its clock. The
// copy and the mirror have to carry it, with the .next and .previous keys,
// so a rotation in progress survives the rename.
const rotatedAtAnnotation = "valkey.operator/system-password-rotated-at"

func migrationCluster() *valkeyiov1alpha1.ValkeyCluster {
	return &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns", UID: "cluster-uid"},
		Spec:       valkeyiov1alpha1.ValkeyClusterSpec{Shards: 1},
	}
}

func newMigrationReconciler(t *testing.T, objs ...client.Object) (*ValkeyClusterReconciler, client.Client, *events.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	rec := events.NewFakeRecorder(100)
	return &ValkeyClusterReconciler{Client: c, APIReader: c, Scheme: scheme, Recorder: rec}, c, rec
}

// operatorOwned marks secret the way the operator creates its Secrets: with its
// labels and the cluster as controller.
func operatorOwned(t *testing.T, cluster *valkeyiov1alpha1.ValkeyCluster, secret *corev1.Secret) *corev1.Secret {
	t.Helper()
	if secret.Labels == nil {
		secret.Labels = map[string]string{}
	}
	maps.Copy(secret.Labels, labels(cluster))
	scheme := runtime.NewScheme()
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	require.NoError(t, controllerutil.SetControllerReference(cluster, secret, scheme))
	return secret
}

// legacyPasswords is a system passwords Secret under its legacy name, in the
// middle of a rotation.
func legacyPasswords() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "internal-c-system-passwords",
			Namespace:   "ns",
			Labels:      map[string]string{"team": "data"},
			Annotations: map[string]string{rotatedAtAnnotation: "2026-11-01T00:00:00Z"},
		},
		Type: AclSecretType,
		Data: map[string][]byte{
			operatorUser:                  []byte("operator-pw"),
			replicationUser:               []byte("replication-pw"),
			exporterUser:                  []byte("exporter-pw"),
			operatorUser + ".next":        []byte("operator-next"),
			replicationUser + ".previous": []byte("replication-previous"),
		},
	}
}

func legacyACL() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "internal-c-acl",
			Namespace:   "ns",
			Annotations: map[string]string{hashAnnotationKey: "legacy-hash"},
		},
		Type: AclSecretType,
		Data: map[string][]byte{aclFilename: []byte("user default off\nuser " + aclRevisionUser + " off resetchannels -@all #legacy\n")},
	}
}

func getSecret(t *testing.T, c client.Client, name string) (*corev1.Secret, bool) {
	t.Helper()
	s := &corev1.Secret{}
	err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: name}, s)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	require.NoError(t, err)
	return s, true
}

func drainRecorder(rec *events.FakeRecorder) []string {
	var got []string
	for {
		select {
		case e := <-rec.Events:
			got = append(got, e)
		default:
			return got
		}
	}
}

func hashOf(password string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(password)))
}

// hidingClient reports the named Secrets as not found on Get, the way a cache
// does for a Secret without the managed-by label or one created moments ago.
// Writes go to the real store.
type hidingClient struct {
	client.Client
	hidden []string
}

func (c hidingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, isSecret := obj.(*corev1.Secret); isSecret && slices.Contains(c.hidden, key.Name) {
		return apierrors.NewNotFound(corev1.Resource("secrets"), key.Name)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// nodeStatefulSet is the StatefulSet of ValkeyNode c-0-0 with the given pod spec.
func nodeStatefulSet(spec corev1.PodSpec) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "valkey-c-0-0", Namespace: "ns"},
		Spec:       appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{Spec: spec}},
	}
}

func TestOperatorSecretNames(t *testing.T) {
	assert.Equal(t, "valkey-c-acl", getInternalSecretName("c"))
	assert.Equal(t, "valkey-c-system-passwords", getSystemPasswordSecretName("c"))
	assert.Equal(t, "internal-c-acl", legacyInternalSecretName("c"))
	assert.Equal(t, "internal-c-system-passwords", legacySystemPasswordSecretName("c"))
}

func TestReconcileUsersAclCopiesTheLegacySecrets(t *testing.T) {
	ctx := context.Background()
	cluster := migrationCluster()
	legacy := operatorOwned(t, cluster, legacyPasswords())
	r, c, rec := newMigrationReconciler(t, legacy, operatorOwned(t, cluster, legacyACL()))

	require.NoError(t, r.reconcileUsersAcl(ctx, cluster))

	copied, ok := getSecret(t, c, "valkey-c-system-passwords")
	require.True(t, ok)
	assert.Equal(t, legacy.Data, copied.Data, "every key is copied, the rotation's .next and .previous included, and nothing is generated")
	assert.Equal(t, "2026-11-01T00:00:00Z", copied.Annotations[rotatedAtAnnotation])
	assert.Equal(t, "data", copied.Labels["team"])
	assert.Equal(t, "valkey-operator", copied.Labels["app.kubernetes.io/managed-by"], "the operator's cache only sees managed-by Secrets")
	assert.True(t, metav1.IsControlledBy(copied, cluster))
	assert.Equal(t, AclSecretType, copied.Type)

	acl, ok := getSecret(t, c, "valkey-c-acl")
	require.True(t, ok)
	assert.True(t, metav1.IsControlledBy(acl, cluster))
	assert.Contains(t, string(acl.Data[aclFilename]), "user _operator on #"+hashOf("operator-pw")+" ",
		"the aclfile keeps the password the running nodes hold")

	after, _ := getSecret(t, c, "internal-c-system-passwords")
	assert.Equal(t, legacy.Data, after.Data, "the legacy Secret is not written while the copy is made")
	got := drainRecorder(rec)
	assert.Contains(t, got, "Normal LegacySecretCopied Copied Secret internal-c-system-passwords to valkey-c-system-passwords")
	assert.Contains(t, got, "Normal LegacySecretCopied Copied Secret internal-c-acl to valkey-c-acl")
}

func TestReconcileUsersAclGeneratesPasswordsOnlyWithoutAnySecret(t *testing.T) {
	ctx := context.Background()
	cluster := migrationCluster()
	r, c, _ := newMigrationReconciler(t)

	require.NoError(t, r.reconcileUsersAcl(ctx, cluster))

	created, ok := getSecret(t, c, "valkey-c-system-passwords")
	require.True(t, ok)
	for _, user := range systemUsers {
		assert.Len(t, created.Data[user], passwordLength, user)
	}
	assert.True(t, metav1.IsControlledBy(created, cluster))
	_, legacyExists := getSecret(t, c, "internal-c-system-passwords")
	assert.False(t, legacyExists, "nothing is created under the legacy name")
}

// A Secret restored from a backup, or left by deleting the cluster with
// --cascade=orphan and creating it again, is not controlled by the cluster,
// and may not carry the managed-by label the cache selects on. Its passwords
// are still the ones the nodes hold.
func TestReconcileUsersAclCopiesALegacySecretItCannotSeeInTheCache(t *testing.T) {
	ctx := context.Background()
	cluster := migrationCluster()
	legacy := legacyPasswords()
	legacy.Labels = nil
	r, c, _ := newMigrationReconciler(t, legacy)
	r.Client = labelFilteredClient{Client: c}

	require.NoError(t, r.reconcileUsersAcl(ctx, cluster))

	copied, ok := getSecret(t, c, "valkey-c-system-passwords")
	require.True(t, ok)
	assert.Equal(t, legacy.Data, copied.Data)
	assert.True(t, metav1.IsControlledBy(copied, cluster))

	require.NoError(t, r.retireLegacySecrets(ctx, cluster))
	after, ok := getSecret(t, c, "internal-c-system-passwords")
	require.True(t, ok, "retiring leaves the legacy Secret alone")
	assert.Equal(t, legacy.Data, after.Data)
}

// A cluster with a user whose Secret is missing keeps running on its last
// aclfile. After an upgrade that aclfile is in the legacy Secret, and the nodes
// mount the new name once they roll, so it is copied rather than left behind.
func TestInternalAclSecretExistsCopiesTheLegacyACL(t *testing.T) {
	ctx := context.Background()
	cluster := migrationCluster()
	cluster.Spec.ACL = &valkeyiov1alpha1.ACLSpec{Users: []valkeyiov1alpha1.ACLUserSpec{{
		Name:     "alice",
		Password: &valkeyiov1alpha1.ACLUserPassword{Secret: &valkeyiov1alpha1.PasswordSecretSpec{Name: "missing"}},
	}}}
	legacy := operatorOwned(t, cluster, legacyACL())
	r, c, _ := newMigrationReconciler(t, operatorOwned(t, cluster, legacyPasswords()), legacy)

	err := r.reconcileUsersAcl(ctx, cluster)
	var unresolved *userSecretUnresolvedError
	require.ErrorAs(t, err, &unresolved)
	assert.True(t, r.internalAclSecretExists(ctx, cluster), "the legacy aclfile is the one to fall back to")

	copied, ok := getSecret(t, c, "valkey-c-acl")
	require.True(t, ok)
	assert.Equal(t, legacy.Data, copied.Data)
	assert.Equal(t, legacy.Annotations, copied.Annotations)
	assert.True(t, metav1.IsControlledBy(copied, cluster))
}

func TestOperatorSecretReadsACacheMissThroughTheAPIReader(t *testing.T) {
	ctx := context.Background()
	cluster := migrationCluster()
	current := operatorOwned(t, cluster, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "valkey-c-system-passwords", Namespace: "ns"},
		Data:       map[string][]byte{operatorUser: []byte("current-pw")},
	})
	r, c, rec := newMigrationReconciler(t, current, operatorOwned(t, cluster, legacyPasswords()))
	r.Client = hidingClient{Client: c, hidden: []string{"valkey-c-system-passwords"}}

	got, err := r.operatorSecret(ctx, cluster, "valkey-c-system-passwords", "internal-c-system-passwords")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "current-pw", string(got.Data[operatorUser]), "a Secret the cache has not seen yet is not copied over again")
	assert.Empty(t, drainRecorder(rec))
}

func TestOperatorSecretRefusesASecretTheClusterDoesNotControl(t *testing.T) {
	ctx := context.Background()
	stranger := func() *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "valkey-c-system-passwords", Namespace: "ns"},
			Data:       map[string][]byte{operatorUser: []byte("someone-elses-pw"), replicationUser: []byte("x")},
		}
	}

	t.Run("while the cluster still controls the legacy Secret", func(t *testing.T) {
		cluster := migrationCluster()
		legacy := operatorOwned(t, cluster, legacyPasswords())
		r, c, _ := newMigrationReconciler(t, stranger(), legacy)

		err := r.reconcileUsersAcl(ctx, cluster)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "valkey-c-system-passwords exists but this cluster does not control it")
		assert.Contains(t, err.Error(), "internal-c-system-passwords")
		theirs, _ := getSecret(t, c, "valkey-c-system-passwords")
		assert.Equal(t, stranger().Data, theirs.Data, "the Secret is not written")
		assert.Empty(t, theirs.OwnerReferences, "or adopted")
		_, aclExists := getSecret(t, c, "valkey-c-acl")
		assert.False(t, aclExists, "no aclfile is built from its passwords")
	})

	t.Run("even when the legacy Secret it controls has lost the managed-by label", func(t *testing.T) {
		cluster := migrationCluster()
		legacy := operatorOwned(t, cluster, legacyPasswords())
		delete(legacy.Labels, "app.kubernetes.io/managed-by")
		r, c, _ := newMigrationReconciler(t, stranger(), legacy)
		r.Client = labelFilteredClient{Client: c}

		err := r.reconcileUsersAcl(ctx, cluster)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "valkey-c-system-passwords exists but this cluster does not control it")
	})

	t.Run("but uses it when the legacy Secret is not the cluster's either", func(t *testing.T) {
		cluster := migrationCluster()
		r, c, _ := newMigrationReconciler(t, stranger(), legacyPasswords())

		require.NoError(t, r.reconcileUsersAcl(ctx, cluster))
		acl, ok := getSecret(t, c, "valkey-c-acl")
		require.True(t, ok)
		assert.Contains(t, string(acl.Data[aclFilename]), "user _operator on #"+hashOf("someone-elses-pw")+" ")
	})
}

func TestRetireLegacySecrets(t *testing.T) {
	ctx := context.Background()
	secretEnv := func(name string) []corev1.EnvVar {
		return []corev1.EnvVar{{Name: "PRIMARY_AUTH", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: replicationUser,
		}}}}
	}
	aclVolume := []corev1.Volume{{Name: "users-acl", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "internal-c-acl"}}}}
	projectedACL := []corev1.Volume{{Name: "users-acl", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
		Sources: []corev1.VolumeProjection{{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "internal-c-acl"}}}},
	}}}}
	node := func(workloadType valkeyiov1alpha1.WorkloadType, aclSecret string) *valkeyiov1alpha1.ValkeyNode {
		return &valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{Name: "c-0-0", Namespace: "ns", Labels: map[string]string{LabelCluster: "c"}},
			Spec:       valkeyiov1alpha1.ValkeyNodeSpec{WorkloadType: workloadType, UsersACLSecretName: aclSecret},
		}
	}
	rolled := node(valkeyiov1alpha1.WorkloadTypeStatefulSet, "valkey-c-acl")
	pod := func(clusterName string, phase corev1.PodPhase, spec corev1.PodSpec) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "p-" + clusterName, Namespace: "ns", Labels: map[string]string{LabelCluster: clusterName}},
			Spec:       spec,
			Status:     corev1.PodStatus{Phase: phase},
		}
	}

	for desc, tc := range map[string]struct {
		objs []client.Object
		// kept lists the legacy Secrets that must stay, equal to their replacement.
		kept []string
	}{
		"nothing reads them": {objs: []client.Object{rolled}},
		"a node spec still names the legacy ACL Secret": {
			objs: []client.Object{node(valkeyiov1alpha1.WorkloadTypeStatefulSet, "internal-c-acl")},
			kept: []string{"internal-c-acl"},
		},
		"a sidecar the node spec declares reads the passwords while its workload and pod are gone": {
			objs: []client.Object{func() client.Object {
				n := node(valkeyiov1alpha1.WorkloadTypeStatefulSet, "valkey-c-acl")
				n.Spec.Containers = []corev1.Container{{Name: "sidecar", Env: secretEnv("internal-c-system-passwords")}}
				return n
			}()},
			kept: []string{"internal-c-system-passwords"},
		},
		"a StatefulSet template reads the passwords through env": {
			objs: []client.Object{rolled, nodeStatefulSet(corev1.PodSpec{Containers: []corev1.Container{{Name: "server", Env: secretEnv("internal-c-system-passwords")}}})},
			kept: []string{"internal-c-system-passwords"},
		},
		"an init container in the template reads the passwords": {
			objs: []client.Object{rolled, nodeStatefulSet(corev1.PodSpec{InitContainers: []corev1.Container{{Name: "server-init", Env: secretEnv("internal-c-system-passwords")}}})},
			kept: []string{"internal-c-system-passwords"},
		},
		"a Deployment template mounts the ACL Secret": {
			objs: []client.Object{
				node(valkeyiov1alpha1.WorkloadTypeDeployment, "valkey-c-acl"),
				&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "valkey-c-0-0", Namespace: "ns"}, Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Volumes: aclVolume}}}},
			},
			kept: []string{"internal-c-acl"},
		},
		"a running pod reads the passwords through envFrom": {
			objs: []client.Object{rolled, pod("c", corev1.PodRunning, corev1.PodSpec{Containers: []corev1.Container{{Name: "sidecar", EnvFrom: []corev1.EnvFromSource{
				{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "internal-c-system-passwords"}}},
			}}}})},
			kept: []string{"internal-c-system-passwords"},
		},
		"a pending pod mounts the ACL Secret through a projected volume": {
			objs: []client.Object{rolled, pod("c", corev1.PodPending, corev1.PodSpec{Volumes: projectedACL})},
			kept: []string{"internal-c-acl"},
		},
		"only a finished pod reads them": {
			objs: []client.Object{rolled, pod("c", corev1.PodSucceeded, corev1.PodSpec{Volumes: aclVolume, Containers: []corev1.Container{{Name: "server", Env: secretEnv("internal-c-system-passwords")}}})},
		},
		"only another cluster's pod reads them": {
			objs: []client.Object{rolled, pod("d", corev1.PodRunning, corev1.PodSpec{Volumes: aclVolume, Containers: []corev1.Container{{Name: "server", Env: secretEnv("internal-c-system-passwords")}}})},
		},
	} {
		t.Run(desc, func(t *testing.T) {
			cluster := migrationCluster()
			current := map[string]*corev1.Secret{
				"internal-c-system-passwords": operatorOwned(t, cluster, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: "valkey-c-system-passwords", Namespace: "ns", Annotations: map[string]string{rotatedAtAnnotation: "2026-11-02T00:00:00Z"}},
					Data:       map[string][]byte{operatorUser: []byte("operator-next"), replicationUser: []byte("replication-pw")},
				}),
				"internal-c-acl": operatorOwned(t, cluster, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: "valkey-c-acl", Namespace: "ns", Annotations: map[string]string{hashAnnotationKey: "current-hash"}},
					Data:       map[string][]byte{aclFilename: []byte("current aclfile")},
				}),
			}
			objs := append([]client.Object{
				current["internal-c-system-passwords"], current["internal-c-acl"],
				operatorOwned(t, cluster, legacyPasswords()), operatorOwned(t, cluster, legacyACL()),
			}, tc.objs...)
			r, c, rec := newMigrationReconciler(t, objs...)

			require.NoError(t, r.retireLegacySecrets(ctx, cluster))

			for legacyName, replacement := range current {
				old, exists := getSecret(t, c, legacyName)
				if !slices.Contains(tc.kept, legacyName) {
					assert.False(t, exists, "%s is deleted once nothing reads it", legacyName)
					continue
				}
				require.True(t, exists, "%s is kept while something reads it", legacyName)
				assert.Equal(t, replacement.Data, old.Data, "%s carries every key of %s, and no other", legacyName, replacement.Name)
				assert.Equal(t, replacement.Annotations, old.Annotations, "%s carries the annotations of %s", legacyName, replacement.Name)
				assert.True(t, metav1.IsControlledBy(old, cluster))
			}
			deleted := 0
			for _, e := range drainRecorder(rec) {
				if assert.Contains(t, e, "Normal LegacySecretDeleted") {
					deleted++
				}
			}
			assert.Equal(t, 2-len(tc.kept), deleted)
		})
	}

	t.Run("keeps a legacy Secret whose replacement does not exist", func(t *testing.T) {
		cluster := migrationCluster()
		r, c, _ := newMigrationReconciler(t, rolled, operatorOwned(t, cluster, legacyPasswords()))

		require.NoError(t, r.retireLegacySecrets(ctx, cluster))
		_, exists := getSecret(t, c, "internal-c-system-passwords")
		assert.True(t, exists, "it may be the only copy of the passwords the nodes hold")
	})

	t.Run("leaves a legacy Secret the cluster does not control", func(t *testing.T) {
		cluster := migrationCluster()
		theirs := legacyPasswords()
		current := operatorOwned(t, cluster, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "valkey-c-system-passwords", Namespace: "ns"},
			Data:       map[string][]byte{operatorUser: []byte("changed")},
		})
		inUse := nodeStatefulSet(corev1.PodSpec{Containers: []corev1.Container{{Name: "server", Env: secretEnv("internal-c-system-passwords")}}})
		for _, objs := range [][]client.Object{{rolled}, {rolled, inUse}} {
			r, c, _ := newMigrationReconciler(t, append(objs, theirs.DeepCopy(), current.DeepCopy())...)
			require.NoError(t, r.retireLegacySecrets(ctx, cluster))
			after, exists := getSecret(t, c, "internal-c-system-passwords")
			require.True(t, exists, "not deleted")
			assert.Equal(t, theirs.Data, after.Data, "and not written")
		}
	})
}

// Every step of the migration is a separate reconcile, so an operator restart
// can fall between any two of them. Each pass reads the state afresh: here a
// new reconciler on the same store stands for each restart.
func TestSecretMigrationSurvivesARestartAtEveryStep(t *testing.T) {
	ctx := context.Background()
	cluster := migrationCluster()
	unrolled := nodeStatefulSet(corev1.PodSpec{
		Containers: []corev1.Container{{Name: "server", Env: []corev1.EnvVar{{Name: "PRIMARY_AUTH", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "internal-c-system-passwords"}, Key: replicationUser,
		}}}}}},
		Volumes: []corev1.Volume{{Name: "users-acl", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "internal-c-acl"}}}},
	})
	node := &valkeyiov1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{Name: "c-0-0", Namespace: "ns", Labels: map[string]string{LabelCluster: "c"}},
		Spec:       valkeyiov1alpha1.ValkeyNodeSpec{UsersACLSecretName: "internal-c-acl"},
	}
	_, store, _ := newMigrationReconciler(t, node, unrolled, operatorOwned(t, cluster, legacyPasswords()), operatorOwned(t, cluster, legacyACL()))
	pass := func() []string {
		t.Helper()
		rec := events.NewFakeRecorder(100)
		r := &ValkeyClusterReconciler{Client: store, APIReader: store, Scheme: store.Scheme(), Recorder: rec}
		require.NoError(t, r.reconcileUsersAcl(ctx, cluster))
		require.NoError(t, r.retireLegacySecrets(ctx, cluster))
		return drainRecorder(rec)
	}
	versions := func() []string {
		names := []string{"valkey-c-system-passwords", "valkey-c-acl", "internal-c-system-passwords", "internal-c-acl"}
		out := make([]string, 0, len(names))
		for _, name := range names {
			s, ok := getSecret(t, store, name)
			require.True(t, ok, name)
			out = append(out, s.ResourceVersion)
		}
		return out
	}
	assertMirrored := func() {
		t.Helper()
		for _, s := range renamedSecrets("c") {
			current, _ := getSecret(t, store, s.name)
			old, ok := getSecret(t, store, s.legacyName)
			require.True(t, ok, s.legacyName)
			assert.Equal(t, current.Data, old.Data, s.legacyName)
			assert.Equal(t, current.Annotations, old.Annotations, s.legacyName)
		}
	}

	pass()
	assertMirrored()
	copiedPassword, _ := getSecret(t, store, "valkey-c-system-passwords")
	assert.Equal(t, "operator-pw", string(copiedPassword.Data[operatorUser]))

	settled := versions()
	assert.Empty(t, pass(), "a restart after the copy changes nothing")
	assert.Equal(t, settled, versions(), "and writes nothing")

	// A writer updates the new Secret, and the operator restarts before the
	// legacy one is updated to match.
	current, _ := getSecret(t, store, "valkey-c-system-passwords")
	current.Data[operatorUser+".next"] = []byte("operator-next-2")
	delete(current.Data, replicationUser+".previous")
	current.Annotations[rotatedAtAnnotation] = "2026-11-03T00:00:00Z"
	require.NoError(t, store.Update(ctx, current))
	pass()
	assertMirrored()

	// The node rolls: first its spec moves to the new names, then its template.
	require.NoError(t, store.Get(ctx, client.ObjectKeyFromObject(node), node))
	node.Spec.UsersACLSecretName = "valkey-c-acl"
	require.NoError(t, store.Update(ctx, node))
	assert.NotContains(t, pass(), "Normal LegacySecretDeleted Deleted Secret internal-c-acl, which valkey-c-acl replaces", "the template still mounts it")
	sts := &appsv1.StatefulSet{}
	require.NoError(t, store.Get(ctx, client.ObjectKeyFromObject(unrolled), sts))
	sts.Spec.Template.Spec.Containers[0].Env[0].ValueFrom.SecretKeyRef.Name = "valkey-c-system-passwords"
	sts.Spec.Template.Spec.Volumes[0].Secret.SecretName = "valkey-c-acl"
	require.NoError(t, store.Update(ctx, sts))

	assert.ElementsMatch(t, []string{
		"Normal LegacySecretDeleted Deleted Secret internal-c-acl, which valkey-c-acl replaces",
		"Normal LegacySecretDeleted Deleted Secret internal-c-system-passwords, which valkey-c-system-passwords replaces",
	}, pass())
	for _, s := range renamedSecrets("c") {
		_, exists := getSecret(t, store, s.legacyName)
		assert.False(t, exists, s.legacyName)
		_, exists = getSecret(t, store, s.name)
		assert.True(t, exists, s.name)
	}
	assert.Empty(t, pass(), "nothing is left to do")
}

func TestFetchSystemUserPasswordFallsBackToTheLegacySecret(t *testing.T) {
	ctx := context.Background()
	cluster := migrationCluster()
	legacy := operatorOwned(t, cluster, legacyPasswords())

	_, c, _ := newMigrationReconciler(t, legacy)
	password, err := fetchSystemUserPassword(ctx, operatorUser, c, "c", "ns")
	require.NoError(t, err)
	assert.Equal(t, "operator-pw", password, "an upgraded cluster is dialled before its Secret is copied")

	_, c, _ = newMigrationReconciler(t, legacy.DeepCopy(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "valkey-c-system-passwords", Namespace: "ns"},
		Data:       map[string][]byte{operatorUser: []byte("current-pw")},
	})
	password, err = fetchSystemUserPassword(ctx, operatorUser, c, "c", "ns")
	require.NoError(t, err)
	assert.Equal(t, "current-pw", password, "the new Secret wins once it exists")

	_, c, _ = newMigrationReconciler(t)
	_, err = fetchSystemUserPassword(ctx, operatorUser, c, "c", "ns")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "valkey-c-system-passwords", "the error names the Secret the operator expects")
}
