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
	hashLength        = 64
	hashAnnotationKey = "valkey.io/internal-acl-hash"
	internalRefByKey  = "valkey.io/internal-acl-ref-by"
	instanceLabel     = "app.kubernetes.io/instance"
)

func getInternalSecretName(cn string) string {
	return cn + "-acl"
}

func getDefaultSecretName(cn string) string {
	return cn + "-secret"
}

func (r *ValkeyClusterReconciler) findReferencedSecrets(ctx context.Context, secret client.Object) []reconcile.Request {

	log := logf.FromContext(ctx)
	secretName := secret.GetName()

	log.V(1).Info("findReferencedSecrets...", "secret", secretName)

	// Fetch a list of all Secrets
	secretList := &corev1.SecretList{}
	if err := r.List(ctx, secretList); err != nil {
		log.Error(err, "failed to list referenced secrets")
		return []reconcile.Request{}
	}

	// debug
	for _, s := range secretList.Items {
		log.V(1).Info("list item", "name", s.GetName())
	}

	// Look for Secrets with our reference annotation
	for _, s := range secretList.Items {
		refSecretName := s.GetAnnotations()[internalRefByKey]

		// If the annotation references the secret that was modified, return
		// a reconciliation request for the cluster that owns the internal secret
		if refSecretName == secretName {
			log.V(1).Info("found internal secret reference", "refname", s.GetName(), "secretname", secretName)

			refClusterName := s.GetLabels()[instanceLabel]
			if refClusterName == "" {
				log.Error(nil, "empty instance from internal secret")
				return []reconcile.Request{}
			}

			log.V(1).Info("returning RR", "name", refClusterName)

			return []reconcile.Request{
				{NamespacedName: types.NamespacedName{
					Name:      refClusterName,
					Namespace: s.GetNamespace(),
				}},
			}
		}
	}

	log.V(1).Info("no referencing secrets found")

	// No secrets found that reference our internal secret
	return []reconcile.Request{}
}

func (r *ValkeyClusterReconciler) reconcileUsersAcl(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) error {

	log := logf.FromContext(ctx)

	// Shortcut
	auth := &cluster.Spec.ValkeySpec.Auth

	// Look for a Secret matching the user-provided name, or clusterName-secret
	userSecretName := getDefaultSecretName(cluster.Name)
	if auth.UsersSecretRef != "" {
		userSecretName = auth.UsersSecretRef
	}
	log.V(2).Info("usersAcl secret", "userSecretName", userSecretName)

	// Query API for user-provided secret
	userSecrets := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      userSecretName,
		Namespace: cluster.Namespace,
	}, userSecrets); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "failed to fetch acl secret")
			return err
		}
		log.V(1).Info("Users secret not found", "userSecretName", userSecretName)
	}

	// Query API for the internal secrets object
	internalSecretName := getInternalSecretName(cluster.Name)
	needCreateInternal := false

	internalAclSecrets := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      internalSecretName,
		Namespace: cluster.Namespace,
	}, internalAclSecrets); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "failed to fetch internal acl secret")
			return err
		}

		// Internal secret was not found. Add metadata to the empty object
		needCreateInternal = true
		log.V(2).Info("creating internal secret", "secretName", internalSecretName)

		internalAclSecrets.Data = make(map[string][]byte)
		internalAclSecrets.ObjectMeta = metav1.ObjectMeta{
			Name:      internalSecretName,
			Namespace: cluster.Namespace,
			Labels:    labels(cluster),
		}

		// Annotate our internal secret with a reference to the name of the user secret.
		// This allows our operator to initiate reconciliation if the user secret is updated.
		upsertAnnotation(internalAclSecrets, internalRefByKey, userSecretName)
	}

	// Register ownership of the internal Secret
	if err := controllerutil.SetControllerReference(cluster, internalAclSecrets, r.Scheme); err != nil {
		log.Error(err, "Failed to grab ownership of internal secret")
		r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "InternalSecretsCreationFailed", "Failed to grab ownership of internal secret: %v", err)
		return err
	}

	// Build the users.acl file contents from the users in the Spec, and secrets reference
	aclFileContents := buildAclFileContents(ctx, auth.Users, userSecrets)

	// Calculate hash of the ACL file contents
	internalAclHash := fmt.Sprintf("%x", sha256.Sum256(aclFileContents))

	// Compare hash to the one already attached to the internal secret, if present.
	// If the hashes are different, then we need to update the internal secret with
	// the new file contents and update the hash annotation. If the hashes are the
	// same, don't update as that would cause infinite reconciliation

	if needsUpdate := upsertAnnotation(internalAclSecrets, hashAnnotationKey, internalAclHash); !needsUpdate {
		log.V(1).Info("internal secrets unchanged")
		return nil
	}

	// Add the acl contents to the internal secret, replacing anything preexisting
	internalAclSecrets.Data["users.acl"] = aclFileContents

	// Create the internal secret, if needed
	if needCreateInternal {
		if err := r.Create(ctx, internalAclSecrets); err != nil {
			log.Error(err, "Failed to create internal secret")
			r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "InternalSecretsCreationFailed", "Failed to create internal secret: %v", err)
			return err
		} else {
			r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "InternalSecretsCreated", "Created internal secret with ACLs")
			return nil
		}
	}

	// Otherwise update it
	if err := r.Update(ctx, internalAclSecrets); err != nil {
		log.Error(err, "Failed to update internal secret")
		r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "InternalSecretsUpdateFailed", "Failed to update internal secret: %v", err)
		return err
	}

	r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "InternalSecretsUpdated", "Synchronized internal secret ACL")

	// All is good; The internal secret will be auto-mounted in the deployment
	return nil
}

// This function takes the map of users from the Spec, and the user-created Secret,
// and builds a string of ACL lines
func buildAclFileContents(ctx context.Context, users map[string]valkeyiov1alpha1.UserAcl, usersSecrets *corev1.Secret) []byte {

	log := logf.FromContext(ctx)

	// Holds the ACLs
	var aclFileContents string

	// Extract usernames from the map, and sort to keep consistent processing order
	sortedNames := make([]string, 0, len(users))
	for k := range users {
		sortedNames = append(sortedNames, k)
	}
	sort.Strings(sortedNames)

	// Loop over users and build acl line
	for _, un := range sortedNames {

		// Get this user's ACL
		acl := users[un]

		if acl.Permissions == "" {
			log.Error(nil, "no permissions for user", "username", un)
			continue
		}

		if acl.Password == "" && acl.PasswordKeyRef == "" {
			log.Error(nil, "no password found for user", "username", un)
			continue
		}

		// If password is empty, fetch from secret
		pw := acl.Password
		if pw == "" {

			// Reference to key in secret file for password; defaults to username
			secretKeyRef := un
			if acl.PasswordKeyRef != "" {
				secretKeyRef = acl.PasswordKeyRef
			}

			// Check if password is in Secret
			refPw, found := usersSecrets.Data[secretKeyRef]
			if !found {
				log.Error(nil, "no password for user in secret", "username", un, "secretRef", secretKeyRef)
				continue
			}
			pw = string(refPw)
		}

		// We should have a username, cleartext, or hashed password, and acl

		var hashedPass string
		pw = strings.TrimRight(pw, "\n")
		pwLen := len(pw)

		if pwLen == hashLength {
			// If the password string is 64 characters, assume it is hashed, and prefix it
			hashedPass = "#" + pw

		} else if pwLen < hashLength {
			// If password string is less than 64 characters, assume plaintext password, and hash it

			// Strip off plaintext prefix, if found
			if pw[:1] == ">" {
				pw = pw[1:]
			}

			// Hash password, and append prefix
			hashedPass = fmt.Sprintf("#%x", sha256.Sum256([]byte(pw)))

		} else if pw[:1] == "#" && pwLen == hashLength+1 {
			// If password begins with #, and is is 65 characters, copy as-is
			hashedPass = pw

		} else {
			// Anything else is something we don't recognize

			log.Error(nil, "unknown password format", "username", un)
			continue
		}

		// Build and append full ACL string
		aclFileContents += fmt.Sprintf("user %s on %s %s\n", un, hashedPass, acl.Permissions)
	}

	return []byte(aclFileContents)
}
