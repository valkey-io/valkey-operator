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

package utils

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2" // nolint:revive,staticcheck
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

const (
	certmanagerVersion = "v1.19.2"
	certmanagerURLTmpl = "https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml"

	defaultKindBinary  = "kind"
	defaultKindCluster = "kind"
)

func warnError(err error) {
	_, _ = fmt.Fprintf(GinkgoWriter, "warning: %v\n", err)
}

// Run executes the provided command within this context
func Run(cmd *exec.Cmd) (string, error) {
	dir, _ := GetProjectDir()
	cmd.Dir = dir

	if err := os.Chdir(cmd.Dir); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "chdir dir: %q\n", err)
	}

	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%q failed with error %q: %w", command, string(output), err)
	}

	return string(output), nil
}

// UninstallCertManager uninstalls the cert manager
func UninstallCertManager() {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	cmd := exec.Command("kubectl", "delete", "-f", url)
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}

	// Delete leftover leases in kube-system (not cleaned by default)
	kubeSystemLeases := []string{
		"cert-manager-cainjector-leader-election",
		"cert-manager-controller",
	}
	for _, lease := range kubeSystemLeases {
		cmd = exec.Command("kubectl", "delete", "lease", lease,
			"-n", "kube-system", "--ignore-not-found", "--force", "--grace-period=0")
		if _, err := Run(cmd); err != nil {
			warnError(err)
		}
	}
}

// InstallCertManager installs the cert manager bundle.
func InstallCertManager() error {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	cmd := exec.Command("kubectl", "apply", "-f", url)
	if _, err := Run(cmd); err != nil {
		return err
	}
	// Wait for cert-manager-webhook to be ready, which can take time if cert-manager
	// was re-installed after uninstalling on a cluster.
	cmd = exec.Command("kubectl", "wait", "deployment.apps/cert-manager-webhook",
		"--for", "condition=Available",
		"--namespace", "cert-manager",
		"--timeout", "5m",
	)

	_, err := Run(cmd)
	return err
}

// IsCertManagerCRDsInstalled checks if any Cert Manager CRDs are installed
// by verifying the existence of key CRDs related to Cert Manager.
func IsCertManagerCRDsInstalled() bool {
	// List of common Cert Manager CRDs

	certManagerCRDs := []string{
		"certificates.cert-manager.io",
		"issuers.cert-manager.io",
		"clusterissuers.cert-manager.io",
		"certificaterequests.cert-manager.io",
		"orders.acme.cert-manager.io",
		"challenges.acme.cert-manager.io",
	}

	// Execute the kubectl command to get all CRDs
	cmd := exec.Command("kubectl", "get", "crds")
	output, err := Run(cmd)
	if err != nil {
		return false
	}

	// Check if any of the Cert Manager CRDs are present
	crdList := GetNonEmptyLines(output)
	for _, crd := range certManagerCRDs {
		for _, line := range crdList {
			if strings.Contains(line, crd) {
				return true
			}
		}
	}

	return false
}

// LoadImageToKindClusterWithName loads a local docker image to the kind cluster
func LoadImageToKindClusterWithName(name string) error {
	cluster := defaultKindCluster
	if v, ok := os.LookupEnv("KIND_CLUSTER"); ok {
		cluster = v
	}
	kindOptions := []string{"load", "docker-image", name, "--name", cluster}
	kindBinary := defaultKindBinary
	if v, ok := os.LookupEnv("KIND"); ok {
		kindBinary = v
	}
	cmd := exec.Command(kindBinary, kindOptions...)
	_, err := Run(cmd)
	return err
}

// GetNonEmptyLines converts given command output string into individual objects
// according to line breakers, and ignores the empty elements in it.
func GetNonEmptyLines(output string) []string {
	var res []string
	elements := strings.SplitSeq(output, "\n")
	for element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}

// GetProjectDir will return the directory where the project is
func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, fmt.Errorf("failed to get current working directory: %w", err)
	}
	wd = strings.ReplaceAll(wd, "/test/e2e", "")
	return wd, nil
}

// UncommentCode searches for target in the file and remove the comment prefix
// of the target content. The target content may span multiple lines.
func UncommentCode(filename, target, prefix string) error {
	// false positive
	// nolint:gosec
	content, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("failed to read file %q: %w", filename, err)
	}
	strContent := string(content)

	idx := strings.Index(strContent, target)
	if idx < 0 {
		return fmt.Errorf("unable to find the code %q to be uncomment", target)
	}

	out := new(bytes.Buffer)
	_, err = out.Write(content[:idx])
	if err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	scanner := bufio.NewScanner(bytes.NewBufferString(target))
	if !scanner.Scan() {
		return nil
	}
	for {
		if _, err = out.WriteString(strings.TrimPrefix(scanner.Text(), prefix)); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
		// Avoid writing a newline in case the previous line was the last in target.
		if !scanner.Scan() {
			break
		}
		if _, err = out.WriteString("\n"); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
	}

	if _, err = out.Write(content[idx+len(target):]); err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	// false positive
	// nolint:gosec
	if err = os.WriteFile(filename, out.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write file %q: %w", filename, err)
	}

	return nil
}

// FindCondition searches for a condition with the specified type in a list of conditions.
// Returns the condition if found, nil otherwise.
func FindCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

func GetValkeyClusterStatus(name string) (*valkeyiov1alpha1.ValkeyCluster, error) {
	cmd := exec.Command("kubectl", "get", "valkeycluster", name, "-o", "json")
	output, err := Run(cmd)
	if err != nil {
		return nil, err
	}
	var cr valkeyiov1alpha1.ValkeyCluster
	err = json.Unmarshal([]byte(output), &cr)
	if err != nil {
		return nil, err
	}
	return &cr, nil
}

// GetEvents fetches and categorizes Kubernetes events for a given resource.
func GetEvents(resourceName string) (map[string]bool, map[string]bool, error) {
	cmd := exec.Command("kubectl", "get", "events", "--field-selector",
		fmt.Sprintf("involvedObject.name=%s", resourceName), "-o", "json")
	output, err := Run(cmd)
	if err != nil {
		return nil, nil, err
	}

	var eventList struct {
		Items []struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"items"`
	}
	err = json.Unmarshal([]byte(output), &eventList)
	if err != nil {
		return nil, nil, err
	}

	normalEvents := make(map[string]bool)
	warningEvents := make(map[string]bool)
	for _, event := range eventList.Items {
		switch event.Type {
		case "Normal":
			normalEvents[event.Reason] = true
		case "Warning":
			warningEvents[event.Reason] = true
		}
	}

	return normalEvents, warningEvents, nil
}

// TODO: use this function until we have a better way to find the replica deployment.
// maybe we need to update valkey cluster status to include master and replica deployment identifiers.
// GetReplicaDeployment finds a deployment that is running a replica instance
// by checking the logs of its pods for the ":S" role indicator.
func GetReplicaDeployment(selector string) (string, error) {
	// List pods matching the selector
	cmd := exec.Command("kubectl", "get", "pods", "-l", selector, "-o", "jsonpath={.items[*].metadata.name}")
	output, err := Run(cmd)
	if err != nil {
		return "", fmt.Errorf("failed to list pods: %w", err)
	}
	podNames := strings.Fields(output)

	// Compile the regex once to capture the role (M or S)
	rolePattern := regexp.MustCompile(`^\s*\d+:([MS])`)

	for _, podName := range podNames {
		// Get logs (last 20 lines should be enough to see the role)
		cmd = exec.Command("kubectl", "logs", podName, "--tail=20")
		logs, err := Run(cmd)
		if err != nil {
			// Ignore errors getting logs (pod might be initializing)
			continue
		}

		lines := strings.Split(logs, "\n")
		// Search from the end to find the most recent role
		for i := len(lines) - 1; i >= 0; i-- {
			matches := rolePattern.FindStringSubmatch(lines[i])
			if len(matches) > 1 {
				role := matches[1]
				if role == "S" {
					// Found a replica pod, now find its deployment.
					// 1. Get ReplicaSet owner of the pod
					cmd = exec.Command("kubectl", "get", "pod", podName, "-o", "jsonpath={.metadata.ownerReferences[0].name}")
					rsName, err := Run(cmd)
					if err != nil || rsName == "" {
						break // Try next pod
					}

					// 2. Get Deployment owner of the ReplicaSet
					cmd = exec.Command("kubectl", "get", "rs", rsName, "-o", "jsonpath={.metadata.ownerReferences[0].name}")
					deployName, err := Run(cmd)
					if err != nil || deployName == "" {
						break // Try next pod
					}

					return deployName, nil
				}
				// If role is "M", it's a master, so this pod is not a replica. Stop checking this pod.
				break
			}
		}
	}

	return "", fmt.Errorf("no replica deployment found with selector %q", selector)
}

// CollectDebugInfo collects debugging information including controller logs,
// Kubernetes events, and pod descriptions. This is useful for troubleshooting failed tests.
func CollectDebugInfo(namespace string) {
	var controllerPodName string
	cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
		"-o", "go-template={{ range .items }}"+
			"{{ if not .metadata.deletionTimestamp }}"+
			"{{ .metadata.name }}"+
			"{{ \"\\n\" }}{{ end }}{{ end }}",
		"-n", namespace)
	podOutput, err := Run(cmd)
	if err == nil {
		podNames := GetNonEmptyLines(podOutput)
		if len(podNames) > 0 {
			controllerPodName = podNames[0]
		}
	}

	if controllerPodName != "" {
		By("Fetching controller manager pod logs")
		cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
		controllerLogs, err := Run(cmd)
		if err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n%s", controllerLogs)
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
		}

		By("Fetching controller manager pod description")
		cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
		podDescription, err := Run(cmd)
		if err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "Pod description:\n%s", podDescription)
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "Failed to describe controller pod\n")
		}
	} else {
		_, _ = fmt.Fprintf(GinkgoWriter, "Warning: Could not fetch controller pod name\n")
	}

	By("Fetching Kubernetes events")
	cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
	eventsOutput, err := Run(cmd)
	if err == nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
	} else {
		_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
	}
}
