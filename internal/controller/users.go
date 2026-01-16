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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

const (
	hashLength        = 64
	hashAnnotationKey = "valkey.io/internal-acl-hash"
)

func getInternalSecretName(cn string) string {
	return "internal-" + cn + "-secret"
}

func (r *ValkeyClusterReconciler) reconcileUsersAcl(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) error {

	log := logf.FromContext(ctx)

	// Shortcut
	auth := &cluster.Spec.ValkeySpec.Auth

	// Look for a Secret matching the user-provided name, or clusterName-secret
	userSecretName := cluster.Name + "-secret"
	if auth.UsersSecretRef != "" {
		userSecretName = auth.UsersSecretRef
	}
	log.V(2).Info("usersAcl secret", "userSecretName", userSecretName)

	// Query API for user-provided secret
	usersSecrets := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      userSecretName,
		Namespace: cluster.Namespace,
	}, usersSecrets); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "failed to fetch acl secret")
			return err
		}
		log.V(1).Info("did not find users secret", "userSecretName", userSecretName)
	}

	// Query API for the internal secrets object
	internalSecretName := getInternalSecretName(cluster.Name)

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
		log.V(2).Info("creating internal secret", "secretName", internalSecretName)

		internalAclSecrets.Data = make(map[string][]byte)
		internalAclSecrets.ObjectMeta = metav1.ObjectMeta{
			Name:      internalSecretName,
			Namespace: cluster.Namespace,
			Labels:    labels(cluster),
		}
	}

	// Register ownership of the internal Secret
	if err := controllerutil.SetControllerReference(cluster, internalAclSecrets, r.Scheme); err != nil {
		log.Error(err, "Failed to grab ownership of internal secret")
		return err
	}

	// Build the users.acl file contents from the users in the Spec, and secrets reference
	aclFileContents := buildAclFileContents(ctx, auth.Users, usersSecrets)

	// Calculate hash of the ACL file contents
	internalAclHash := fmt.Sprintf("%x", sha256.Sum256(aclFileContents))

	// Compare hash to the one already attached to the internal secret, if present.
	// If the hashes are different, then we need to update the internal secret with
	// the new file contents and update the hash annotation. If the hashes are the
	// same, don't update as that would cause infinite reconciliation

	if needsUpdate := upsertAnnotation(internalAclSecrets, hashAnnotationKey, internalAclHash); !needsUpdate {
		log.V(2).Info("internal secrets unchanged")
		return nil
	}

	// Add the acl contents to the internal secret, replacing anything preexisting
	internalAclSecrets.Data["users.acl"] = aclFileContents

	// Create, or update this internal secret
	if err := r.Create(ctx, internalAclSecrets); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if err := r.Update(ctx, internalAclSecrets); err != nil {
				log.Error(err, "Failed to update internal secret")
				r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "InternalSecretsUpdateFailed", "Failed to update internal secret: %v", err)
				return err
			}
		} else {
			log.Error(err, "Failed to create internal secret")
			r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "InternalSecretsCreationFailed", "Failed to create internal secret: %v", err)
			return err
		}
	} else {
		r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "InternalSecretsCreated", "Created internal secret with ACLs")
	}

	// All is good; The internal secret will be auto-mounted in the deployment
	return nil
}

// This function takes the map of users from the Spec, and the user-created Secret,
// and builds a string of ACL lines
func buildAclFileContents(ctx context.Context, users map[string]valkeyiov1alpha1.UserAcl, usersSecrets *corev1.Secret) []byte {

	log := logf.FromContext(ctx)

	// Holds the ACLs
	var aclFileContents string

	// Loop over users and build acl line
	for un, acl := range users {

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
