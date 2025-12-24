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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
	"valkey.io/valkey-operator/test/utils"
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

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
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
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", projectImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace, "--ignore-not-found=true")
		_, _ = utils.Run(cmd)

		By("deleting the metrics ClusterRoleBinding")
		cmd = exec.Command("kubectl", "delete", "clusterrolebinding", metricsRoleBindingName, "--ignore-not-found=true")
		_, _ = utils.Run(cmd)

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

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get the name of the controller-manager pod
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				// Validate the pod's status
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=valkey-operator-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("ensuring the controller pod is ready")
			verifyControllerPodReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", controllerPodName, "-n", namespace,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"), "Controller pod not ready")
			}
			Eventually(verifyControllerPodReady, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Serving metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted, 3*time.Minute, time.Second).Should(Succeed())

			// +kubebuilder:scaffold:e2e-metrics-webhooks-readiness

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": ["curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics"],
							"securityContext": {
								"readOnlyRootFilesystem": true,
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccountName": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			verifyMetricsAvailable := func(g Gomega) {
				metricsOutput, err := getMetricsOutput()
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
				g.Expect(metricsOutput).NotTo(BeEmpty())
				g.Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
			}
			Eventually(verifyMetricsAvailable, 2*time.Minute).Should(Succeed())
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks

	})

	var valkeyClusterName string

	Context("when a ValkeyCluster CR is applied", func() {
		It("creates a Valkey Cluster deployment", func() {
			By("creating the CR")
			cmd := exec.Command("kubectl", "create", "-f", "config/samples/v1alpha1_valkeycluster.yaml")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ValkeyCluster CR")

			By("validating the CR")
			verifyCrExists := func(g Gomega) {
				// Get the name of the ValkeyCluster CR
				cmd := exec.Command("kubectl", "get",
					"ValkeyCluster", "-o", "go-template={{ range .items }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}",
				)
				crOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve ValkeyCluster information")
				crNames := utils.GetNonEmptyLines(crOutput)
				g.Expect(crNames).To(HaveLen(1), "Expected 1 instance of a ValkeyCluster")
				valkeyClusterName = crNames[0]
				g.Expect(valkeyClusterName).To(ContainSubstring("valkeycluster-sample"))
			}
			Eventually(verifyCrExists).Should(Succeed())

			By("validating the Service")
			verifyServiceExists := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "service", valkeyClusterName)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}
			Eventually(verifyServiceExists).Should(Succeed())

			By("validating the ConfigMap")
			verifyConfigMapExists := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "configmap", valkeyClusterName)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}
			Eventually(verifyConfigMapExists).Should(Succeed())

			By("validating Deployments")
			verifyDeploymentsExists := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployments",
					"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", valkeyClusterName),
					"-o", "go-template={{ range .items }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				deployments := utils.GetNonEmptyLines(output)
				g.Expect(deployments).To(HaveLen(6), "Expected 6 Deployments")
			}
			Eventually(verifyDeploymentsExists).Should(Succeed())

			By("validating Pods")
			verifyPodStatuses := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", valkeyClusterName),
					"-o", "go-template={{ range .items }}{{ range .status.conditions }}"+
						"{{ if and (eq .type \"Ready\") (eq .status \"True\")}}"+
						"{{ $.metadata.name}} {{ \"\\n\" }}"+
						"{{ end }}{{ end }}{{ end }}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				podStatuses := utils.GetNonEmptyLines(output)
				g.Expect(podStatuses).To(HaveLen(6), "Expected 6 Pods to be ready")
			}
			Eventually(verifyPodStatuses).Should(Succeed())

			By("validating the ValkeyCluster CR status")
			verifyCrStatus := func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(valkeyClusterName)
				g.Expect(err).NotTo(HaveOccurred())

				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
				g.Expect(cr.Status.Reason).To(Equal(valkeyiov1alpha1.ReasonClusterHealthy))
				g.Expect(cr.Status.Message).To(Equal("Cluster is healthy"))
				g.Expect(cr.Status.Shards).To(Equal(int32(3)))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(3)))

				readyCond := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionReady)
				g.Expect(readyCond).NotTo(BeNil(), "Ready condition not found")
				g.Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(readyCond.Reason).To(Equal(valkeyiov1alpha1.ReasonClusterHealthy))

				progressingCond := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionProgressing)
				g.Expect(progressingCond).NotTo(BeNil(), "Progressing condition not found")
				g.Expect(progressingCond.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(progressingCond.Reason).To(Equal(valkeyiov1alpha1.ReasonReconcileComplete))

				degradedCond := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)
				g.Expect(degradedCond).To(BeNil(), "Degraded condition should not be present")

				clusterFormedCond := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionClusterFormed)
				g.Expect(clusterFormedCond).NotTo(BeNil(), "ClusterFormed condition not found")
				g.Expect(clusterFormedCond.Status).To(Equal(metav1.ConditionTrue))

				slotsAssignedCond := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionSlotsAssigned)
				g.Expect(slotsAssignedCond).NotTo(BeNil(), "SlotsAssigned condition not found")
				g.Expect(slotsAssignedCond.Status).To(Equal(metav1.ConditionTrue))
			}
			Eventually(verifyCrStatus).Should(Succeed())

			By("validating cluster access")
			verifyClusterAccess := func(g Gomega) {
				// Start a Valkey client pod to access the cluster and get its status.
				clusterFqdn := fmt.Sprintf("%s.default.svc.cluster.local", valkeyClusterName)

				cmd := exec.Command("kubectl", "run", "client",
					fmt.Sprintf("--image=%s", valkeyClientImage), "--restart=Never", "--",
					"valkey-cli", "-c", "-h", clusterFqdn, "CLUSTER", "INFO")
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				cmd = exec.Command("kubectl", "wait", "pod/client",
					"--for=jsonpath={.status.phase}=Succeeded", "--timeout=30s")
				_, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				cmd = exec.Command("kubectl", "logs", "client")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				cmd = exec.Command("kubectl", "delete", "pod", "client",
					"--wait=true", "--timeout=30s")
				_, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				// The cluster should be ok.
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
			}
			Eventually(verifyClusterAccess).Should(Succeed())
		})
	})

	Context("when a ValkeyCluster CR is deleted", func() {
		It("deletes the Valkey Cluster deployment", func() {
			By("deleting the CR")
			cmd := exec.Command("kubectl", "delete", "-f", "config/samples/v1alpha1_valkeycluster.yaml")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete ValkeyCluster CR")

			By("validating that the CR does not exist")
			verifyCrRemoved := func(g Gomega) {
				// Get the name of the ValkeyCluster CR
				cmd := exec.Command("kubectl", "get", "ValkeyCluster", valkeyClusterName)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}
			Eventually(verifyCrRemoved).Should(Succeed())

			By("validating that the Service does not exist")
			verifyServiceRemoved := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "service", valkeyClusterName)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}
			Eventually(verifyServiceRemoved).Should(Succeed())

			By("validating that the ConfigMap does not exist")
			verifyConfigMapRemoved := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "configmap", valkeyClusterName)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}
			Eventually(verifyConfigMapRemoved).Should(Succeed())

			By("validating that no Deployment exist")
			verifyDeploymentsRemoved := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployments",
					"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", valkeyClusterName))
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve Deployments")
				g.Expect(output).To(ContainSubstring("No resources found"))
			}
			Eventually(verifyDeploymentsRemoved).Should(Succeed())
		})
	})

	Context("when a ValkeyCluster experiences degraded state", func() {
		var degradedClusterName string

		It("should detect and recover when a deployment is deleted", func() {
			By("creating a ValkeyCluster")
			degradedClusterManifest := `apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: valkeycluster-degraded-status-test
spec:
  shards: 3
  replicas: 1
`

			manifestFile := filepath.Join(os.TempDir(), "valkeycluster-degraded.yaml")
			err := os.WriteFile(manifestFile, []byte(degradedClusterManifest), 0644)
			Expect(err).NotTo(HaveOccurred(), "Failed to write manifest file")
			defer os.Remove(manifestFile)

			By("applying the CR")
			cmd := exec.Command("kubectl", "create", "-f", manifestFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ValkeyCluster CR")
			degradedClusterName = "valkeycluster-degraded-status-test"

			By("waiting for cluster to become ready first")
			verifyClusterReady := func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(degradedClusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(3)))
			}
			Eventually(verifyClusterReady).Should(Succeed())

			By("getting a deployment to delete")
			var deploymentToDelete string
			getDeployment := func(g Gomega) {
				cmd := exec.Command(
					"kubectl", "get", "deployments",
					"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", degradedClusterName),
					"-o", "go-template={{ range .items }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}",
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				deployments := utils.GetNonEmptyLines(output)
				g.Expect(len(deployments)).To(BeNumerically(">", 0))

				// Get the first deployment
				deploymentToDelete = deployments[0]
			}
			Eventually(getDeployment).Should(Succeed())

			By(fmt.Sprintf("deleting deployment %s to simulate shard loss", deploymentToDelete))
			cmd = exec.Command("kubectl", "delete", "deployment", deploymentToDelete, "--wait=false")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete deployment")

			By("waiting for the cluster to detect the deployment loss and start recovery")
			verifyDegradedState := func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(degradedClusterName)
				g.Expect(err).NotTo(HaveOccurred())

				// The Cluster should detect the deployment loss and not be in Ready state
				// Note: The operator immediately recreates missing deployments, so the cluster
				// transitions through Reconciling/AddingNodes states, and may briefly enter
				// Degraded state (with NodeAddFailed reason), if adding the node fails temporarily).
				// However, this is not guaranteed, so we only check for Recovery here.
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReconciling),
					fmt.Sprintf("Expected cluster to be reconciling after deployment deletion, but got:%s (reason: %s)", cr.Status.State, cr.Status.Reason))

				// Ready condition should be False during recovery
				readyCond := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionReady)
				if readyCond != nil {
					g.Expect(readyCond.Status).To(Equal(metav1.ConditionFalse), "Ready condition should be False when deployment is being recreated")
				}
			}
			Eventually(verifyDegradedState).Should(Succeed())

			By("waiting for the operator to recreate the deployment and recover the cluster")
			verifyClusterRecovery := func(g Gomega) {
				// First, verify all deployments are present (should be 6 total for 3 shards with 1 replica each)
				cmd := exec.Command("kubectl", "get", "deployments",
					"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", degradedClusterName),
					"-o", "go-template={{ range .items }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				deployments := utils.GetNonEmptyLines(output)
				g.Expect(deployments).To(HaveLen(6), "Expected 6 Deployments after operator recreates the deleted one")

				// Verify all pods are ready
				cmd = exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", degradedClusterName),
					"-o", "go-template={{ range .items }}{{ range .status.conditions }}"+
						"{{ if and (eq .type \"Ready\") (eq .status \"True\")}}"+
						"{{ $.metadata.name}} {{ \"\\n\" }}"+
						"{{ end }}{{ end }}{{ end }}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				podStatuses := utils.GetNonEmptyLines(output)
				g.Expect(podStatuses).To(HaveLen(6), "Expected 6 Pods to be ready after recovery")

				// Then verify the cluster returns to Ready state
				cr, err := utils.GetValkeyClusterStatus(degradedClusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady),
					fmt.Sprintf("Expected cluster to recover to Ready state, but got: %s (reason: %s)", cr.Status.State, cr.Status.Reason))
				g.Expect(cr.Status.Reason).To(Equal(valkeyiov1alpha1.ReasonClusterHealthy),
					fmt.Sprintf("Expected ClusterHealthy reason after recovery but got: %s", cr.Status.Reason))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(3)), "All shards should be ready after recovery")

				// Verify Ready condition is True
				readyCond := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionReady)
				g.Expect(readyCond).NotTo(BeNil(), "Ready condition should be present")
				g.Expect(readyCond.Status).To(Equal(metav1.ConditionTrue), "Ready condition should be True after recovery")
				g.Expect(readyCond.Reason).To(Equal(valkeyiov1alpha1.ReasonClusterHealthy))

				// Verify Degraded condition is no longer present or is False
				degradedCond := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)
				if degradedCond != nil {
					g.Expect(degradedCond.Status).To(Equal(metav1.ConditionFalse), "Degraded condition should be False after recovery")
				}
			}
			Eventually(verifyClusterRecovery).Should(Succeed())

			By("cleaning up the degraded cluster")
			cmd = exec.Command("kubectl", "delete", "valkeycluster", degradedClusterName, "--wait=false")
			_, _ = utils.Run(cmd)

			By("waiting for cluster to be deleted")
			verifyClusterDeleted := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "valkeycluster", degradedClusterName)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(), "Cluster should be deleted")
			}
			Eventually(verifyClusterDeleted).Should(Succeed())
		})
	})
})

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	// Temporary file to store the token request
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		// Execute kubectl command to create the token
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		// Parse the JSON output to extract the token
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() (string, error) {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	return utils.Run(cmd)
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}
