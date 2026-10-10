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
	_ "embed"
	"fmt"
	"os/exec"
	"path"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/valkey-io/valkey-operator/test/utils"
)

// An S3-compatible server for the backup and restore specs.
const (
	s3ServerName = "s3-e2e"
	s3Bucket     = "valkey-backups"
	// s3BucketDir is where the server keeps the bucket on disk. The specs
	// read snapshots from there rather than through the S3 API, so a bug
	// shared by rclone's client and server cannot hide itself.
	s3BucketDir = "/data/" + s3Bucket

	s3AccessKeyID     = "e2e-access-key"
	s3SecretAccessKey = "e2e-secret-key"
)

// s3ServerManifest is the server's Deployment and Service, without a
// namespace; see the file for why it is rclone.
//
//go:embed testdata/s3-server.yaml
var s3ServerManifest string

// s3Endpoint is the URL the cluster's backup and restore containers reach
// the server in the given namespace at.
func s3Endpoint(ns string) string {
	return fmt.Sprintf("http://%s.%s.svc:8080", s3ServerName, ns)
}

// installS3Server runs the server in a namespace of its own and waits for it
// to serve. Call it from a BeforeAll: it registers a DeferCleanup that
// deletes the namespace, and waits for that, after the container. Each
// container uses its own namespace, so no container depends on what another
// one installed or removed, whatever order they run in.
func installS3Server(ns string) {
	By("installing the S3 server in namespace " + ns)
	// A namespace left terminating by an earlier run would refuse the
	// resources below.
	cmd := exec.Command("kubectl", "delete", "ns", ns, "--ignore-not-found=true", "--wait=true", "--timeout=2m")
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to remove a leftover S3 server namespace")

	cmd = exec.Command("kubectl", "create", "ns", ns)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to create the S3 server namespace")
	DeferCleanup(func() {
		cmd := exec.Command("kubectl", "delete", "ns", ns, "--ignore-not-found=true", "--wait=true", "--timeout=2m")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to delete the S3 server namespace")
	})

	cmd = exec.Command("kubectl", "create", "secret", "generic", s3ServerName+"-server", "-n", ns,
		"--from-literal=AWS_ACCESS_KEY_ID="+s3AccessKeyID,
		"--from-literal=AWS_SECRET_ACCESS_KEY="+s3SecretAccessKey)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to create the S3 server Secret")

	cmd = exec.Command("kubectl", "apply", "-n", ns, "-f", "-")
	cmd.Stdin = strings.NewReader(s3ServerManifest)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to install the S3 server")

	cmd = exec.Command("kubectl", "rollout", "status", "deployment/"+s3ServerName, "-n", ns, "--timeout=3m")
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "S3 server did not become ready")
}

// createS3CredentialsSecret creates the Secret a ValkeyCluster names in
// credentialsSecret, in the namespace the clusters run in, and removes it
// after the container.
func createS3CredentialsSecret(name string) {
	By("creating the S3 credentials Secret " + name)
	cmd := exec.Command("kubectl", "delete", "secret", name, "--ignore-not-found=true")
	_, _ = utils.Run(cmd)
	cmd = exec.Command("kubectl", "create", "secret", "generic", name,
		"--from-literal=AWS_ACCESS_KEY_ID="+s3AccessKeyID,
		"--from-literal=AWS_SECRET_ACCESS_KEY="+s3SecretAccessKey)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to create the S3 credentials Secret")
	DeferCleanup(func() {
		cmd := exec.Command("kubectl", "delete", "secret", name, "--ignore-not-found=true")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to delete the S3 credentials Secret")
	})
}

// s3ServerShell runs a shell script in the S3 server's container.
func s3ServerShell(ns, script string) (string, error) {
	cmd := exec.Command("kubectl", "exec", "-n", ns, "deployment/"+s3ServerName, "--", "sh", "-c", script)
	return utils.Run(cmd)
}

// listSnapshots returns the names of the complete snapshots under a prefix,
// sorted. A snapshot counts only once its manifest.json is there: the upload
// writes it last, and an empty directory left behind by a deletion is not a
// snapshot.
func listSnapshots(g Gomega, ns, prefix string) []string {
	output, err := s3ServerShell(ns, fmt.Sprintf(
		"[ -d %[1]s ] || exit 0; find %[1]s -mindepth 2 -maxdepth 2 -name manifest.json",
		path.Join(s3BucketDir, prefix)))
	g.Expect(err).NotTo(HaveOccurred())
	var snapshots []string
	for _, line := range utils.GetNonEmptyLines(output) {
		snapshots = append(snapshots, path.Base(path.Dir(strings.TrimSpace(line))))
	}
	slices.Sort(snapshots)
	return snapshots
}

// snapshotFiles returns the file names inside one snapshot, sorted.
func snapshotFiles(g Gomega, ns, prefix, snapshot string) []string {
	output, err := s3ServerShell(ns, "ls -1 "+path.Join(s3BucketDir, prefix, snapshot))
	g.Expect(err).NotTo(HaveOccurred())
	files := utils.GetNonEmptyLines(output)
	for i := range files {
		files[i] = strings.TrimSpace(files[i])
	}
	slices.Sort(files)
	return files
}

// readSnapshotFile returns the first limit bytes of a file inside a
// snapshot, or all of it when limit is 0, together with its size in bytes.
func readSnapshotFile(g Gomega, ns, prefix, snapshot, file string, limit int) (content string, size string) {
	p := path.Join(s3BucketDir, prefix, snapshot, file)
	read := "cat " + p
	if limit > 0 {
		read = fmt.Sprintf("head -c %d %s", limit, p)
	}
	output, err := s3ServerShell(ns, fmt.Sprintf("wc -c < %s && %s", p, read))
	g.Expect(err).NotTo(HaveOccurred())
	size, content, _ = strings.Cut(output, "\n")
	return content, strings.TrimSpace(size)
}

// dumpS3Server writes the server's log and the bucket's contents to the
// GinkgoWriter, for a failed spec.
func dumpS3Server(ns string) {
	cmd := exec.Command("kubectl", "logs", "-n", ns, "deployment/"+s3ServerName, "--tail=100")
	output, _ := utils.Run(cmd)
	_, _ = fmt.Fprintf(GinkgoWriter, "S3 server logs (%s):\n%s\n", ns, output)
	output, _ = s3ServerShell(ns, "find "+s3BucketDir+" -exec ls -ld {} +")
	_, _ = fmt.Fprintf(GinkgoWriter, "S3 bucket contents (%s):\n%s\n", ns, output)
}
