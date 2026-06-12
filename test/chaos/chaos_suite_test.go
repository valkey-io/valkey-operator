//go:build chaos
// +build chaos

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

package chaos

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"valkey.io/valkey-operator/test/utils"
)

var (
	managerImage             = "valkey/valkey-operator:v0.0.1"
	shouldCleanupCertManager = false
)

func TestChaos(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting valkey-operator chaos test suite\n")
	RunSpecs(t, "Chaos Suite")
}

var _ = BeforeSuite(func() {
	if os.Getenv("KUBECTL_KUBERC") != "true" {
		_ = os.Setenv("KUBECTL_KUBERC", "false")
	}

	By("building the manager image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage))
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager image")

	By("loading the manager image on Kind")
	err = utils.LoadImageToKindClusterWithName(managerImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager image into Kind")

	setupCertManager()

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	By("creating manager namespace")
	cmd = exec.Command("kubectl", "create", "ns", namespace)
	_, _ = utils.Run(cmd) // ignore if already exists

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

	By("increasing controller-manager memory limit for large clusters")
	cmd = exec.Command("kubectl", "set", "resources", "deployment/valkey-operator-controller-manager",
		"-n", namespace, "--limits=memory=512Mi", "--requests=memory=128Mi")
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to set controller-manager memory limits")
})

var _ = AfterSuite(func() {
	// Reset any CPU pressure before teardown.
	utils.UnthrottleNodes(utils.GetWorkerNodes())

	teardownCertManager()

	By("undeploying the controller-manager")
	cmd := exec.Command("make", "undeploy")
	_, _ = utils.Run(cmd)

	By("uninstalling CRDs")
	cmd = exec.Command("make", "uninstall")
	_, _ = utils.Run(cmd)

	By("removing manager namespace")
	cmd = exec.Command("kubectl", "delete", "ns", namespace)
	_, _ = utils.Run(cmd)
})

func setupCertManager() {
	if os.Getenv("CERT_MANAGER_INSTALL_SKIP") == "true" {
		return
	}
	if utils.IsCertManagerCRDsInstalled() {
		return
	}
	shouldCleanupCertManager = true
	By("installing CertManager")
	Expect(utils.InstallCertManager()).To(Succeed(), "Failed to install CertManager")
}

func teardownCertManager() {
	if !shouldCleanupCertManager {
		return
	}
	By("uninstalling CertManager")
	utils.UninstallCertManager()
}
