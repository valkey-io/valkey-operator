//go:build e2e
// +build e2e

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

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/valkey-io/valkey-operator/test/utils"
)

// namespace where the project is deployed in
const namespace = "valkey-operator-system"

// serviceAccountName created for the project
const serviceAccountName = "valkey-operator-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "valkey-operator-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "valkey-operator-metrics-binding"

// valkeyClientImage is the image used to verify cluster access.
const valkeyClientImage = "valkey/valkey:9.0.0"

var (
	// managerImage is the manager image to be built and loaded for testing.
	managerImage = envOrDefault("E2E_MANAGER_IMAGE", "valkey/valkey-operator:v0.0.1")
	// shouldCleanupCertManager tracks whether CertManager was installed by this suite.
	shouldCleanupCertManager = false
)

// envOrDefault returns the value of environment variable key, or def if unset.
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// TestE2E runs the e2e test suite to validate the solution in an isolated environment.
// The default setup requires Kind and CertManager.
//
// To enable kubectl kuberc (use custom kubectl configurations), set: KUBECTL_KUBERC=true
// By default, kuberc is disabled to ensure consistent test behavior across different environments.
// To skip CertManager installation, set: CERT_MANAGER_INSTALL_SKIP=true
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting valkey-operator e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	By("purging old events")
	cmd := exec.Command("kubectl", "delete", "events", "--all")
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to purge old events")

	By("building the manager image")
	if os.Getenv("E2E_SKIP_IMAGE_BUILD") != "true" {
		cmd = exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage))
		_, err = utils.Run(cmd)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager image")
	} else {
		_, _ = fmt.Fprintf(GinkgoWriter, "E2E_SKIP_IMAGE_BUILD=true, using pre-built image %s\n", managerImage)
	}

	By("loading the manager image on Kind")
	err = utils.LoadImageToKindClusterWithName(managerImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager image into Kind")

	configureKubectlKubeRC()
	setupCertManager()

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	By("creating manager namespace")
	cmd = exec.Command("kubectl", "create", "ns", namespace)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

	By("labeling the namespace to enforce the restricted security policy")
	cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
		"pod-security.kubernetes.io/enforce=restricted")
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

	By("installing CRDs")
	cmd = exec.Command("make", "install")
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

	By("deploying the controller-manager")
	cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
})

var _ = AfterSuite(func() {
	teardownCertManager()

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	By("cleaning up the curl pod for metrics")
	cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace, "--ignore-not-found=true")
	_, _ = utils.Run(cmd)

	By("deleting the metrics ClusterRoleBinding")
	cmd = exec.Command("kubectl", "delete", "clusterrolebinding", metricsRoleBindingName, "--ignore-not-found=true")
	_, _ = utils.Run(cmd)

	// Specs delete their ValkeyCluster with --wait=false, so a ValkeyNode can
	// still hold the persistent-volume-cleanup finalizer here. Only the
	// operator clears it, and `make undeploy` removes the operator and then
	// blocks on the valkeynodes CRD until the go test timeout. Let the
	// operator finish the deletes first.
	By("deleting any remaining ValkeyClusters")
	cmd = exec.Command("kubectl", "delete", "valkeyclusters.valkey.io", "--all", "-A",
		"--ignore-not-found=true", "--wait=false")
	_, _ = utils.Run(cmd)

	// `kubectl wait --all` errors when nothing matches (all nodes already gone,
	// or the CRD was never installed), so only wait when ValkeyNodes exist.
	By("waiting for every ValkeyNode to be deleted")
	existing, listErr := listValkeyNodes()
	if listErr != nil {
		// A failed list does not show that no nodes remain, so do not risk
		// undeploying the operator under a node that still has a finalizer.
		Fail(fmt.Sprintf("could not list ValkeyNodes; skipped undeploy so the operator keeps running: %v", listErr))
	}
	if existing == "" {
		_, _ = fmt.Fprintln(GinkgoWriter, "No ValkeyNodes to wait for")
	} else if _, err := utils.Run(exec.Command("kubectl", "wait", "valkeynodes.valkey.io", "--all", "-A",
		"--for=delete", "--timeout=5m")); err != nil {
		// The last node can vanish between the list and the wait, which makes
		// kubectl wait report "no matching resources". Re-list before failing.
		remaining, relistErr := listValkeyNodes()
		if relistErr == nil && remaining == "" {
			_, _ = fmt.Fprintf(GinkgoWriter, "ValkeyNodes are gone (wait reported: %v)\n", err)
		} else {
			// The operator is the only thing that can finish these deletes, so
			// undeploying it now would just move the hang into `make undeploy`.
			// Report what is stuck, leave the operator running, and fail the
			// suite: a ValkeyNode that cannot finish deleting is an operator bug.
			_, _ = fmt.Fprintf(GinkgoWriter, "ValkeyNodes still present; skipping undeploy to avoid hanging: %v\n", err)
			stuck := exec.Command("kubectl", "get", "valkeynodes.valkey.io", "-A", "-o", "wide")
			out, _ := utils.Run(stuck)
			_, _ = fmt.Fprintf(GinkgoWriter, "%s\n", out)
			Fail("ValkeyNodes were not deleted within 5m; skipped undeploy so the operator can still clear their finalizers")
		}
	}

	By("undeploying the controller-manager")
	cmd = exec.Command("make", "undeploy")
	_, _ = utils.Run(cmd)

	By("uninstalling CRDs")
	cmd = exec.Command("make", "uninstall")
	_, _ = utils.Run(cmd)

	By("removing manager namespace")
	cmd = exec.Command("kubectl", "delete", "ns", namespace)
	_, _ = utils.Run(cmd)
})

// Disable kubectl kuberc by default for test isolation.
// This prevents local kubectl configurations from affecting test behavior.
// To enable kuberc, set: KUBECTL_KUBERC=true
func configureKubectlKubeRC() {
	if os.Getenv("KUBECTL_KUBERC") != "true" {
		By("disabling kubectl kuberc for test isolation")
		err := os.Setenv("KUBECTL_KUBERC", "false")
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to disable kubectl kuberc")
		_, _ = fmt.Fprintf(GinkgoWriter,
			"kubectl kuberc disabled for consistent test behavior (override with KUBECTL_KUBERC=true)\n")
	} else {
		_, _ = fmt.Fprintf(GinkgoWriter, "kubectl kuberc enabled (KUBECTL_KUBERC=true)\n")
	}
}

// setupCertManager installs CertManager if needed for webhook tests.
// Skips installation if CERT_MANAGER_INSTALL_SKIP=true or if already present.
func setupCertManager() {
	if os.Getenv("CERT_MANAGER_INSTALL_SKIP") == "true" {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping CertManager installation (CERT_MANAGER_INSTALL_SKIP=true)\n")
		return
	}

	By("checking if CertManager is already installed")
	if utils.IsCertManagerCRDsInstalled() {
		_, _ = fmt.Fprintf(GinkgoWriter, "CertManager is already installed. Skipping installation.\n")
		return
	}

	// Mark for cleanup before installation to handle interruptions and partial installs.
	shouldCleanupCertManager = true

	By("installing CertManager")
	Expect(utils.InstallCertManager()).To(Succeed(), "Failed to install CertManager")
}

// teardownCertManager uninstalls CertManager if it was installed by setupCertManager.
// This ensures we only remove what we installed.
func teardownCertManager() {
	if !shouldCleanupCertManager {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping CertManager cleanup (not installed by this suite)\n")
		return
	}

	By("uninstalling CertManager")
	utils.UninstallCertManager()
}

// listValkeyNodes returns the names of all ValkeyNodes, or "" when there are
// none. A missing valkeynodes CRD counts as none. Any other list error is
// retried a few times, then returned.
func listValkeyNodes() (string, error) {
	const attempts = 5
	var err error
	for i := 0; i < attempts; i++ {
		var out string
		out, err = utils.Run(exec.Command("kubectl", "get", "valkeynodes.valkey.io", "-A", "-o", "name"))
		if err == nil {
			// utils.Run merges stderr into the output, so keep only the
			// resource names (valkeynode.valkey.io/<name>) and ignore notices
			// such as "No resources found" or deprecation warnings.
			var names []string
			for _, line := range strings.Split(out, "\n") {
				if line = strings.TrimSpace(line); strings.HasPrefix(line, "valkeynode") {
					names = append(names, line)
				}
			}
			return strings.Join(names, "\n"), nil
		}
		msg := err.Error()
		if strings.Contains(msg, "doesn't have a resource type") ||
			strings.Contains(msg, "the server could not find the requested resource") {
			return "", nil
		}
		time.Sleep(2 * time.Second)
	}
	return "", err
}
