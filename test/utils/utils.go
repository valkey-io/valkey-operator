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
	"strings"

	. "github.com/onsi/ginkgo/v2" // nolint:revive,staticcheck
	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	certmanagerVersion = "v1.20.2"
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
		return fmt.Errorf("unable to find the code %q to be uncommented", target)
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

// GetValkeyNodeStatus fetches the current state of a ValkeyNode by name.
func GetValkeyNodeStatus(name string) (*valkeyiov1alpha1.ValkeyNode, error) {
	cmd := exec.Command("kubectl", "get", "valkeynode", name, "-o", "json")
	output, err := Run(cmd)
	if err != nil {
		return nil, err
	}
	var node valkeyiov1alpha1.ValkeyNode
	if err := json.Unmarshal([]byte(output), &node); err != nil {
		return nil, err
	}
	return &node, nil
}

// GetValkeyClusterNodes lists all ValkeyNodes belonging to a cluster, selected
// by the valkey.io/cluster label.
func GetValkeyClusterNodes(clusterName string) (*valkeyiov1alpha1.ValkeyNodeList, error) {
	cmd := exec.Command("kubectl", "get", "valkeynodes",
		"-l", fmt.Sprintf("valkey.io/cluster=%s", clusterName), "-o", "json")
	output, err := Run(cmd)
	if err != nil {
		return nil, err
	}
	var list valkeyiov1alpha1.ValkeyNodeList
	if err := json.Unmarshal([]byte(output), &list); err != nil {
		return nil, err
	}
	return &list, nil
}

// ValkeyCLIOptions configures a valkey-cli invocation in a pod.
type ValkeyCLIOptions struct {
	// Password authenticates as the default user. Leave empty for a cluster
	// with no declared users: valkey-cli would otherwise inherit a stale
	// VALKEYCLI_AUTH from the pod environment and print an AUTH warning.
	Password string
	// ConnectTimeoutSeconds bounds each connect. A stale MOVED redirect can
	// point at a terminated pod's unroutable IP, where a connect otherwise
	// hangs for the TCP SYN timeout. Zero omits the flag.
	ConnectTimeoutSeconds int
}

// valkeyCLIPrefix builds the shell prologue and valkey-cli invocation shared by
// the helpers below.
func valkeyCLIPrefix(opts ValkeyCLIOptions) (prologue, cli string) {
	prologue = "unset VALKEYCLI_AUTH REDISCLI_AUTH; "
	if opts.Password != "" {
		prologue = fmt.Sprintf("export VALKEYCLI_AUTH=%q; ", opts.Password)
	}
	cli = "valkey-cli -c -h 127.0.0.1"
	if opts.ConnectTimeoutSeconds > 0 {
		cli = fmt.Sprintf("valkey-cli -t %d -c -h 127.0.0.1", opts.ConnectTimeoutSeconds)
	}
	return prologue, cli
}

// WriteValkeyKeys writes count keys named <prefix>:<n> with value val:<n>
// through a single valkey-cli in pod, following cluster redirects. It fails
// unless every write is acknowledged, so a partial write is reported here rather
// than leaving a later read to show a confusing count.
func WriteValkeyKeys(pod, prefix string, count int, opts ValkeyCLIOptions) error {
	prologue, cli := valkeyCLIPrefix(opts)
	script := fmt.Sprintf(
		"%sawk 'BEGIN{for(i=1;i<=%d;i++) print \"SET %s:\"i\" val:\"i}' | %s | grep -c '^OK$'",
		prologue, count, prefix, cli)
	out, err := Run(exec.Command("kubectl", "exec", pod, "-c", "server", "--", "sh", "-c", script))
	if err != nil {
		return fmt.Errorf("writing %d keys to %s: %w (output: %s)", count, pod, err, out)
	}
	if got := strings.TrimSpace(out); got != fmt.Sprintf("%d", count) {
		return fmt.Errorf("wrote %s of %d keys to %s", got, count, pod)
	}
	return nil
}

// CountValkeyKeys reads back the keys WriteValkeyKeys wrote and returns how many
// held their expected value.
func CountValkeyKeys(pod, prefix string, count int, opts ValkeyCLIOptions) (int, error) {
	prologue, cli := valkeyCLIPrefix(opts)
	// Generate commands, execute them and verify replies.
	script := fmt.Sprintf(
		"%sawk 'BEGIN{for(i=1;i<=%d;i++) print \"GET %s:\"i}' | %s | awk -v v=val: '$0 == v NR {ok++} END{print ok+0}'",
		prologue, count, prefix, cli)
	out, err := Run(exec.Command("kubectl", "exec", pod, "-c", "server", "--", "sh", "-c", script))
	if err != nil {
		return 0, fmt.Errorf("reading %d keys from %s: %w (output: %s)", count, pod, err, out)
	}
	var found int
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d", &found); err != nil {
		return 0, fmt.Errorf("parsing key count from %q: %w", out, err)
	}
	return found, nil
}

// ValkeyCLI runs a valkey-cli command in pod's server container. valkey-cli
// exits 0 and prints the reply even when the server returns an error, so an
// error reply is reported here rather than being left to a confusing downstream
// assertion.
func ValkeyCLI(pod string, opts ValkeyCLIOptions, args ...string) (string, error) {
	prologue, cli := valkeyCLIPrefix(opts)
	script := fmt.Sprintf("%s%s %s", prologue, cli, strings.Join(args, " "))
	out, err := Run(exec.Command("kubectl", "exec", pod, "-c", "server", "--", "sh", "-c", script))
	if err != nil {
		return out, err
	}
	for _, line := range GetNonEmptyLines(out) {
		line = strings.TrimSpace(line)
		for _, p := range []string{"ERR ", "WRONGPASS", "NOPERM", "NOAUTH", "CLUSTERDOWN", "MASTERDOWN"} {
			if strings.HasPrefix(line, p) {
				return out, fmt.Errorf("valkey-cli %s replied: %s", strings.Join(args, " "), line)
			}
		}
	}
	return out, nil
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
