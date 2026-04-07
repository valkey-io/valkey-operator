/*
Copyright 2025 Valkey Contributors.

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
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

const (
	hashAnnotationKey = "valkey.io/internal-acl-hash"
	aclFilename       = "users.acl"
)

var (
	operatorUser    = "_operator"
	exporterUser    = "_exporter"
	systemUsers     = []string{operatorUser, exporterUser}
	systemUsersAcls = map[string]string{
		operatorUser: "+@all",
		exporterUser: "-@all +@connection +memory -readonly +strlen +config|get +xinfo +pfcount -quit +zcard +type +xlen -readwrite -command +client -wait +scard +llen +hlen +get +eval +slowlog +cluster|info +cluster|slots +cluster|nodes -hello -echo +info +latency +scan -reset -auth -asking",
	}
)

func getInternalSecretName(clusterName string) string {
	return "internal-" + clusterName + "-acl"
}

func getDefaultSecretName(clusterName string) string {
	return clusterName + "-users"
}

func getSystemPasswordSecretName(clusterName string) string {
	return "internal-" + clusterName + "-system-passwords"
}

// When a Secret is updated, Watch() calls this function to discover
// which object should be reconciled. Because multiple secrets can be
// used by the same cluster, and a single secret used by multiple clusters,
// we grab a list of all Valkey clusters, and iterate through the ACLs
// looking for the modified secret. We return a list of clusters that
// need to be reconciled.
func (r *ValkeyClusterReconciler) findReferencedClusters(ctx context.Context, secret client.Object) []reconcile.Request {

	log := logf.FromContext(ctx)
	secretName := secret.GetName() // the Secret that was updated

	log.V(1).Info("findReferencedClusters", "modified", secretName)

	// List all ValkeyClusters
	valkeyClusterList := &valkeyiov1alpha1.ValkeyClusterList{}
	if err := r.List(ctx, valkeyClusterList,
		client.InNamespace(secret.GetNamespace()),
	); err != nil {
		log.Error(err, "failed to list valkey clusters")
		return []reconcile.Request{}
	}

	requests := []reconcile.Request{}

	// Take our list of clusters, and iterate through them, matching against
	// spec.Users[].PasswordSecret.Name. Return a list of clusters to be reconciled.
	for _, cluster := range valkeyClusterList.Items {
		for _, user := range cluster.Spec.Users {
			if user.PasswordSecret.Name == secretName {
				log.V(1).Info("adding cluster to reconcile", "name", cluster.Name)
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      cluster.Name,
						Namespace: cluster.Namespace,
					},
				})
			}
		}
	}

	return requests
}

func (r *ValkeyClusterReconciler) createSystemUsersAcl(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) (string, error) {
	log := logf.FromContext(ctx)
	log.Info("getting system users secret: " + cluster.Name)
	var systemsAcls strings.Builder
	systemUserSecret := &corev1.Secret{}
	err := r.Client.Get(ctx, types.NamespacedName{
		Namespace: cluster.Namespace,
		Name:      getSystemPasswordSecretName(cluster.Name),
	}, systemUserSecret)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "failed to fetch system users secret")
			return "", err
		}
		systemUserSecret, err = r.upsertSystemUsersPasswordSecret(ctx, r.Client, cluster)

	}
	for _, user := range systemUsers {
		if user == exporterUser && !cluster.Spec.Exporter.Enabled {
			continue
		}
		passwordHash := fmt.Sprintf("%x", sha256.Sum256(systemUserSecret.Data[user]))
		userAcl := valkeyiov1alpha1.UserAclSpec{
			Name:    user,
			Enabled: true,
			RawAcl:  systemUsersAcls[user],
			PasswordSecret: valkeyiov1alpha1.PasswordSecretSpec{
				Name: systemUserSecret.Name,
				Keys: []string{user},
			},
		}
		fmt.Fprintf(&systemsAcls, "%s\n", buildUserAcl(userAcl, []string{passwordHash}))
	}

	return systemsAcls.String(), nil
}

func (r *ValkeyClusterReconciler) reconcileUsersAcl(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) error {

	log := logf.FromContext(ctx)

	// Sort users for consistency in hash calculations
	sort.Slice(cluster.Spec.Users, func(i, j int) bool {
		return cluster.Spec.Users[i].Name < cluster.Spec.Users[j].Name
	})

	// Process each user, generating a complete ACL string
	var usersAcls strings.Builder
	for _, user := range cluster.Spec.Users {

		// Get passwords from Secret
		passwords, err := fetchUserPasswords(ctx, user, r.Client, cluster.Name, cluster.Namespace)
		if err != nil {
			log.Error(err, "failed to fetch password", "username", user.Name)
			continue
		}

		// Build ACL string for this user with found password(s)
		acl := buildUserAcl(user, passwords)
		fmt.Fprintf(&usersAcls, "%s\n", acl)
	}
	// append system users ACL
	systemUsersAcl, err := r.createSystemUsersAcl(ctx, cluster)
	if err != nil {
		log.Error(err, "failed to generate system users ACL")
		return err
	}
	fmt.Fprintf(&usersAcls, "%s\n", systemUsersAcl)
	usersAclsBytes := []byte(usersAcls.String())

	// update the internal ACL secret with the generated users ACLs
	err = r.upsertInternalAclSecret(ctx, cluster, usersAclsBytes)
	if err != nil {
		log.Error(err, "failed to reconcile users ACL")
		return err
	}
	// All is good; The internal secret will be auto-mounted in the deployment
	return nil
}

// Helper for repeated actions
func appendAcl(acl *strings.Builder, permissions []string, prefix string) {
	for _, permission := range permissions {
		fmt.Fprintf(acl, " %s%s", prefix, permission)
	}
}

// Builds a user ACL string
func buildUserAcl(user valkeyiov1alpha1.UserAclSpec, passwords []string) string {

	// Holds the ACL as we build it
	var acl strings.Builder

	// Initial acl
	fmt.Fprintf(&acl, "user %s ", user.Name)

	// Is the user enabled?
	if user.Enabled {
		acl.WriteString("on")
	} else {
		acl.WriteString("off")
	}

	// If enabled, append password(s), which should already be prefix-hashed
	if user.NoPassword {
		fmt.Fprintf(&acl, " nopass")
	} else {
		appendAcl(&acl, passwords, "#")
	}

	// Add key restrictions
	appendAcl(&acl, user.Keys.ReadWrite, "~")
	appendAcl(&acl, user.Keys.ReadOnly, "%R~")
	appendAcl(&acl, user.Keys.WriteOnly, "%W~")

	// Add channel restrictions
	if len(user.Channels.Patterns) > 0 {
		acl.WriteString(" resetchannels")
		appendAcl(&acl, user.Channels.Patterns, "&")
	}

	// Build command ACLs
	appendAcl(&acl, user.Commands.Allow, "+")
	appendAcl(&acl, user.Commands.Deny, "-")

	// Append remaining/raw permissions
	fmt.Fprintf(&acl, " %s", user.RawAcl)

	return acl.String()
}

// Fetches a Secret, and looks for referenced passwords
func fetchUserPasswords(ctx context.Context, user valkeyiov1alpha1.UserAclSpec, apiClient client.Client, clusterName, clusterNamespace string) ([]string, error) {

	log := logf.FromContext(ctx)

	// If this user doesn't have a password, return empty
	if user.NoPassword {
		return []string{}, nil
	}
	// Look for a Secret matching the user-provided name, or clusterName-users
	userSecretName := getDefaultSecretName(clusterName)
	if user.PasswordSecret.Name != "" {
		userSecretName = user.PasswordSecret.Name
	}

	// Query API for the referenced Secret
	userSecret := &corev1.Secret{}
	if err := apiClient.Get(ctx, types.NamespacedName{
		Name:      userSecretName,
		Namespace: clusterNamespace,
	}, userSecret); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "failed to fetch acl secret")
			return []string{}, err
		}
		log.V(1).Info("Users secret not found", "userSecretName", userSecretName, "user", user.Name)

		// The Secret was not found; And since NoPassword is false, then we cannot add this user
		return []string{}, fmt.Errorf("no password or reference found")
	}

	// Sort the password keys; default to username if no keys present
	passwordKeys := user.PasswordSecret.Keys
	sort.Strings(passwordKeys)
	if len(passwordKeys) == 0 {
		passwordKeys = []string{user.Name}
	}

	// Now that we have a secret, look for any reference keys. If empty, default to username
	passwords := []string{}
	for _, key := range passwordKeys {

		log.V(2).Info("looking for password", "user", user.Name, "secret", userSecretName, "key", key)

		// byte-string password
		password, exists := userSecret.Data[key]
		if !exists {
			log.Error(nil, "missing password key in secret", "user", user.Name, "secret", userSecretName, "key", key)
			return []string{}, fmt.Errorf("missing password key in secret")
		}

		// Test if the string in the Secret is a pre-hashed sha256 password
		if isPreHashedPassword(password) {
			passwords = append(passwords, string(password[1:]))
			continue
		}

		// Otherwise, we assume a plaintext password, and we hash it before appending
		hashedPassword := fmt.Sprintf("%x", sha256.Sum256(password))
		passwords = append(passwords, hashedPassword)
	}

	return passwords, nil
}

func fetchSystemUserPassword(ctx context.Context, username string, apiClient client.Client, clusterName, clusterNamespace string) (string, error) {
	systemSecret := &corev1.Secret{}
	err := apiClient.Get(ctx, types.NamespacedName{
		Namespace: clusterNamespace,
		Name:      getSystemPasswordSecretName(clusterName),
	}, systemSecret)
	if err != nil {
		return "", err
	}
	return string(systemSecret.Data[username]), nil
}

// Check if byte-string begins with # (byte 35) and is 65 total characters long.
// If so, we assume this is a pre-hashed sha256 password.
func isPreHashedPassword(password []byte) bool {
	return password[0] == 35 && len(password) == 65
}

func generatePassword() string {
	randstr := rand.Text()
	// hasher := sha256.New()
	// hasher.Write([]byte(randstr))
	// hashBytes := hasher.Sum(nil)
	return randstr
}

func (r *ValkeyClusterReconciler) upsertSystemUsersPasswordSecret(ctx context.Context, apiClient client.Client, cluster *valkeyiov1alpha1.ValkeyCluster) (*corev1.Secret, error) {
	log := logf.FromContext(ctx)

	systemUsersSecret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      getSystemPasswordSecretName(cluster.Name),
			Namespace: cluster.Namespace,
			Labels:    labels(cluster),
		},
		Data: map[string][]byte{},
	}
	// Register ownership of the new internal Secret
	if err := controllerutil.SetControllerReference(cluster, &systemUsersSecret, r.Scheme); err != nil {
		log.Error(err, "Failed to grab ownership of system users secret")
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "InternalSecretsCreationFailed", "ReconcileUsers", "Failed to grab ownership of system users secret: %v", err)
		return &systemUsersSecret, err
	}
	for _, user := range systemUsers {
		systemUsersSecret.Data[user] = []byte(generatePassword())
	}

	err := apiClient.Create(ctx, &systemUsersSecret)
	return &systemUsersSecret, err
}

func (r *ValkeyClusterReconciler) upsertInternalAclSecret(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, aclBytes []byte) error {
	log := logf.FromContext(ctx)
	aclHash := fmt.Sprintf("%x", sha256.Sum256(aclBytes))
	// An "internal" secrets object is used for synchronization
	internalSecretName := getInternalSecretName(cluster.Name)
	internalAclSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      internalSecretName,
		Namespace: cluster.Namespace,
	}, internalAclSecret); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "failed to fetch internal acl secret")
			return err
		}

		// Internal secret was not found.
		// Init, and add metadata to the new Secret object
		log.V(2).Info("creating internal secret", "secretName", internalSecretName)

		internalAclSecret.ObjectMeta = metav1.ObjectMeta{
			Name:      internalSecretName,
			Namespace: cluster.Namespace,
			Labels:    labels(cluster),
			Annotations: map[string]string{
				hashAnnotationKey: aclHash,
			},
		}
		internalAclSecret.Data = map[string][]byte{
			aclFilename: aclBytes,
		}

		// Register ownership of the new internal Secret
		if err := controllerutil.SetControllerReference(cluster, internalAclSecret, r.Scheme); err != nil {
			log.Error(err, "Failed to grab ownership of internal secret")
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "InternalSecretsCreationFailed", "ReconcileUsers", "Failed to grab ownership of internal secret: %v", err)
			return err
		}

		// Create the internal Secret
		if err := r.Create(ctx, internalAclSecret); err != nil {
			log.Error(err, "Failed to create internal secret")
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "InternalSecretsCreationFailed", "ReconcileUsers", "Failed to create internal secret: %v", err)
			return err
		}

		r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "InternalSecretsCreated", "ReconcileUsers", "Created internal ACLs")
		return nil
	}
	// All good; new internal Secret with contents created

	// Calculate, and compare the hashes to
	// determine if anything needs updating. If the hashes are the
	// same, don't update as that would cause infinite reconciliation

	if !upsertAnnotation(internalAclSecret, hashAnnotationKey, aclHash) {
		log.V(1).Info("internal ACLs unchanged")
		return nil
	}
	// Hashes are different; Update the acl contents of the internal secret
	internalAclSecret.Data[aclFilename] = aclBytes

	// Update secret
	if err := r.Update(ctx, internalAclSecret); err != nil {
		log.Error(err, "Failed to update internal secret")
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "InternalSecretsUpdateFailed", "ReconcileUsers", "Failed to update internal secret: %v", err)
		return err
	}

	r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "InternalSecretsUpdated", "ReconcileUsers", "Synchronized internal ACLs")
	return nil
}
