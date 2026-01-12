package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"valkey.io/valkey-operator/test/utils"
)

var _ = Describe("Valkey Matrics Exporter", func() {
	Context("Metrics Exporter Enabled", func() {
		It("should deploy ValkeyCluster with metrics exporter sidecar by default", func() {
			valkeyName := "valkeycluster-with-exporter"
			// Create a basic ValkeyCluster YAML (exporter enabled by default)
			valkeyYaml := fmt.Sprintf(`
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 1
  replicas: 0
  exporter:
    enabled: true
    resources:
      requests:
        memory: "32Mi"
        cpu: "10m"
      limits:
        memory: "64Mi"
        cpu: "50m"
`, valkeyName)

			// Create temporary YAML file
			tmpFile, err := os.CreateTemp("", fmt.Sprintf("%s-*.yaml", valkeyName))
			Expect(err).NotTo(HaveOccurred(), "Failed to create temporary YAML file")
			defer os.Remove(tmpFile.Name())

			_, err = tmpFile.Write([]byte(valkeyYaml))
			Expect(err).NotTo(HaveOccurred())
			tmpFile.Close()

			By("Applying the ValkeyCluster CR with default exporter settings")
			cmd := exec.Command("kubectl", "apply", "-f", tmpFile.Name())
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to apply ValkeyCluster: %s", output))

			By("waiting for deployment to be created")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", "-l", "app.kubernetes.io/instance="+valkeyName)
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Deployment not found")
				g.Expect(out).To(ContainSubstring(valkeyName))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("Verifying pod has 2 containers (valkey-server and metrics-exporter)")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", "app.kubernetes.io/instance="+valkeyName, "-o", "jsonpath={.items[0].spec.containers[*].name}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get pod containers")
				containers := strings.Fields(out)
				g.Expect(containers).To(HaveLen(2), "Expected 2 containers in pod")
				g.Expect(containers).To(ContainElement("valkey-server"), "Should have valkey-server container")
				g.Expect(containers).To(ContainElement("metrics-exporter"), "Should have metrics-exporter container")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("Verifying metrics-exporter container uses correct image")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", "app.kubernetes.io/instance="+valkeyName, "-o", "jsonpath={.items[0].spec.containers[?(@.name=='metrics-exporter')].image}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get exporter image")
				g.Expect(out).To(ContainSubstring("redis_exporter"), "Should use redis_exporter image")
			}, 1*time.Minute, 5*time.Second).Should(Succeed())

			By("Verifying metrics-exporter container has correct port")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", "app.kubernetes.io/instance="+valkeyName, "-o", "jsonpath={.items[0].spec.containers[?(@.name=='metrics-exporter')].ports[0].containerPort}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get exporter port")
				g.Expect(out).To(Equal("9121"), "Exporter should expose port 9121")
			}, 1*time.Minute, 5*time.Second).Should(Succeed())

			By("Verifying resource requests are set correctly")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", "app.kubernetes.io/instance="+valkeyName, "-o", "jsonpath={.items[0].spec.containers[?(@.name=='metrics-exporter')].resources.requests.memory}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get memory request")
				g.Expect(out).To(Equal("32Mi"), "Memory request should be 32Mi")
			}, 1*time.Minute, 5*time.Second).Should(Succeed())

			By("Verifying resource limits are set correctly")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", "app.kubernetes.io/instance="+valkeyName, "-o", "jsonpath={.items[0].spec.containers[?(@.name=='metrics-exporter')].resources.limits.memory}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get memory limit")
				g.Expect(out).To(Equal("64Mi"), "Memory limit should be 64Mi")
			}, 1*time.Minute, 5*time.Second).Should(Succeed())

			By("Waiting for pod to be running")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", "app.kubernetes.io/instance="+valkeyName, "-o", "jsonpath={.items[0].status.phase}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get pod status")
				g.Expect(out).To(Equal("Running"), "Pod should be running")
			}, 3*time.Minute, 10*time.Second).Should(Succeed())

			By("Verifying both containers are ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", "app.kubernetes.io/instance="+valkeyName, "-o", "jsonpath={.items[0].status.containerStatuses[*].ready}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get container ready statuses")
				readyStatuses := strings.Fields(out)
				g.Expect(readyStatuses).To(HaveLen(2), "Should have 2 container statuses")
				for _, status := range readyStatuses {
					g.Expect(status).To(Equal("true"), "All containers should be ready")
				}
			}, 3*time.Minute, 10*time.Second).Should(Succeed())

			By("Getting pod name for metrics verification")
			var podName string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", "app.kubernetes.io/instance="+valkeyName, "-o", "jsonpath={.items[0].metadata.name}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get pod name")
				g.Expect(out).NotTo(BeEmpty(), "Pod name should not be empty")
				podName = out
			}, 1*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying common Valkey metrics are exposed")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", podName, "-c", "metrics-exporter", "--", "wget", "-q", "-O-", "http://localhost:9121/metrics")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get metrics")
				// Check that redis_up is 1 (indicating successful connection)
				g.Expect(out).To(MatchRegexp(`redis_up\s+1`), "redis_up should be 1 (healthy)")
				// Verify essential Prometheus metrics are present
				g.Expect(out).To(ContainSubstring("redis_up"), "Should contain redis_up metric")
				g.Expect(out).To(ContainSubstring("# HELP"), "Should contain Prometheus HELP comments")
				g.Expect(out).To(ContainSubstring("# TYPE"), "Should contain Prometheus TYPE comments")
				// Verify presence of common metrics
				expectedMetrics := []string{
					"redis_connected_clients",
					"redis_memory_used_bytes",
					"redis_commands_processed_total",
				}
				for _, metric := range expectedMetrics {
					g.Expect(out).To(ContainSubstring(metric), fmt.Sprintf("Should contain metric: %s", metric))
				}
			}, 2*time.Minute, 10*time.Second).Should(Succeed())

			By("Verifying /health endpoint is accessible")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", podName, "-c", "metrics-exporter", "--", "wget", "-q", "-O-", "http://localhost:9121/health")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Health endpoint should be accessible")
				g.Expect(out).NotTo(BeEmpty(), "Health endpoint should return a response")
			}, 1*time.Minute, 5*time.Second).Should(Succeed())

			By("Cleaning up test resources")
			cmd = exec.Command("kubectl", "delete", "valkeycluster", valkeyName, "--ignore-not-found=true")
			utils.Run(cmd)
		})
	})

	Context("Metrics Exporter Disabled", func() {
		It("should deploy ValkeyCluster without metrics exporter when disabled", func() {
			valkeyName := "valkeycluster-no-exporter"
			// Create ValkeyCluster YAML with exporter explicitly disabled
			valkeyYaml := fmt.Sprintf(`
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 1
  replicas: 0
  exporter:
    enabled: false
`, valkeyName)

			// Create temporary YAML file
			tmpFile, err := os.CreateTemp("", fmt.Sprintf("%s-*.yaml", valkeyName))
			Expect(err).NotTo(HaveOccurred(), "Failed to create temporary YAML file")
			defer os.Remove(tmpFile.Name())

			_, err = tmpFile.Write([]byte(valkeyYaml))
			Expect(err).NotTo(HaveOccurred())
			tmpFile.Close()

			By("Applying the ValkeyCluster CR with exporter disabled")
			cmd := exec.Command("kubectl", "apply", "-f", tmpFile.Name())
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to apply ValkeyCluster: %s", output))

			By("Waiting for deployment to be created")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", "-l", "app.kubernetes.io/instance="+valkeyName)
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Deployment not found")
				g.Expect(out).To(ContainSubstring(valkeyName))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("Verifying pod has only 1 container (valkey-server)")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", "app.kubernetes.io/instance="+valkeyName, "-o", "jsonpath={.items[0].spec.containers[*].name}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get pod containers")
				containers := strings.Fields(out)
				g.Expect(containers).To(HaveLen(1), "Expected only 1 container in pod")
				g.Expect(containers[0]).To(Equal("valkey-server"), "Should only have valkey-server container")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("Verifying metrics-exporter container is NOT present")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", "app.kubernetes.io/instance="+valkeyName, "-o", "jsonpath={.items[0].spec.containers[*].name}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get pod containers")
				g.Expect(out).NotTo(ContainSubstring("metrics-exporter"), "Should NOT have metrics-exporter container")
			}, 1*time.Minute, 5*time.Second).Should(Succeed())

			By("Waiting for pod to be running")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", "app.kubernetes.io/instance="+valkeyName, "-o", "jsonpath={.items[0].status.phase}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get pod status")
				g.Expect(out).To(Equal("Running"), "Pod should be running")
			}, 3*time.Minute, 10*time.Second).Should(Succeed())

			By("Cleaning up test resources")
			cmd = exec.Command("kubectl", "delete", "valkeycluster", valkeyName, "--ignore-not-found=true")
			utils.Run(cmd)
		})
	})
})
