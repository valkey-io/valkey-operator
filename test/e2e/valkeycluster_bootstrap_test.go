//go:build e2e
// +build e2e

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

package e2e

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/test/utils"
)

// Covers https://github.com/valkey-io/valkey-operator/issues/696. A plain
// StatefulSet Valkey cluster (no operator) is the source. A ValkeyCluster with
// spec.bootstrap joins it as replicas. Only spec.bootstrap.stage moves the
// migration: Cutover promotes the new nodes, and Complete forgets the source.
var _ = Describe("ValkeyCluster bootstrap from an existing cluster", Ordered, Label("ValkeyCluster", "Bootstrap"), func() {
	const (
		srcName     = "bootsrc"
		srcPassword = "src-secret"
		dstName     = "cluster-bootstrap"
		srcPods     = 6 // 3 shards, 1 replica each
	)

	srcExec := func(pod string, script string) (string, error) {
		return utils.Run(exec.Command("kubectl", "exec", pod, "--", "sh", "-c",
			fmt.Sprintf("export REDISCLI_AUTH=%s VALKEYCLI_AUTH=%s; %s", srcPassword, srcPassword, script)))
	}
	dstPod := func(shard, node int) string { return fmt.Sprintf("valkey-%s-%d-%d-0", dstName, shard, node) }
	dstExec := func(pod string, script string) (string, error) {
		return utils.Run(exec.Command("kubectl", "exec", pod, "-c", "server", "--", "sh", "-c",
			"unset VALKEYCLI_AUTH REDISCLI_AUTH; "+script))
	}
	// nodeLines returns the CLUSTER NODES lines seen from a new node, split into
	// the new cluster's own nodes and everyone else (the source).
	nodeLines := func(g Gomega) (ours, others []string) {
		out, err := dstExec(dstPod(0, 0), `valkey-cli cluster nodes | tr -d '\r'`)
		g.Expect(err).NotTo(HaveOccurred())
		pods, err := utils.Run(exec.Command("kubectl", "get", "pods", "-l", "valkey.io/cluster="+dstName,
			"-o", "jsonpath={range .items[*]}{.status.podIP} {end}"))
		g.Expect(err).NotTo(HaveOccurred())
		for _, line := range strings.Split(out, "\n") {
			f := strings.Fields(line)
			if len(f) < 3 {
				continue
			}
			if strings.Contains(pods, strings.Split(f[1], ":")[0]) {
				ours = append(ours, line)
			} else {
				others = append(others, line)
			}
		}
		return ours, others
	}
	// writeKeys writes prefix:1..n through the source, following redirects.
	writeKeys := func(prefix string, n int) {
		out, err := srcExec(srcName+"-0", fmt.Sprintf(
			`for i in $(seq 1 %d); do echo "SET %s:$i v$i"; done | valkey-cli -c`, n, prefix))
		Expect(err).NotTo(HaveOccurred(), out)
	}
	// countKeys returns how many of prefix:1..n read back with their value.
	countKeys := func(pod, prefix string, n int) int {
		out, err := dstExec(pod, fmt.Sprintf(
			`for i in $(seq 1 %d); do v=$(valkey-cli -c get %s:$i); [ "$v" = "v$i" ] && echo ok; done | wc -l`, n, prefix))
		Expect(err).NotTo(HaveOccurred(), out)
		var got int
		_, _ = fmt.Sscanf(strings.TrimSpace(out), "%d", &got)
		return got
	}
	// replicatedKeys sums DBSIZE over one node per shard. DBSIZE is local, so it
	// works on a replica; a GET would be redirected to the source primary, which
	// wants the source password.
	replicatedKeys := func() int {
		total := 0
		for shard := 0; shard < 3; shard++ {
			out, err := dstExec(dstPod(shard, 0), `valkey-cli dbsize | tr -d '\r'`)
			Expect(err).NotTo(HaveOccurred(), out)
			var n int
			_, _ = fmt.Sscanf(strings.TrimSpace(out), "%d", &n)
			total += n
		}
		return total
	}
	dstRole := func(pod string) string {
		out, err := dstExec(pod, `valkey-cli info replication | grep -E '^(role|master_link_status):' | tr -d '\r' | tr '\n' ' '`)
		Expect(err).NotTo(HaveOccurred(), out)
		return out
	}
	clusterStatus := func(g Gomega) *valkeyiov1alpha1.ValkeyCluster {
		cr, err := utils.GetValkeyClusterStatus(dstName)
		g.Expect(err).NotTo(HaveOccurred())
		return cr
	}

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.CollectDebugInfo(namespace)
		}
	})

	AfterAll(func() {
		for _, args := range [][]string{
			{"delete", "valkeycluster", dstName, "--ignore-not-found", "--wait=true", "--timeout=120s"},
			{"delete", "statefulset", srcName, "--ignore-not-found", "--wait=true", "--timeout=120s"},
			{"delete", "service", srcName, "--ignore-not-found"},
			{"delete", "secret", srcName + "-auth", "--ignore-not-found"},
		} {
			_, _ = utils.Run(exec.Command("kubectl", args...))
		}
	})

	It("creates the source cluster and seeds data", func() {
		manifest := fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %[1]s
spec:
  clusterIP: None
  selector: {app: %[1]s}
  ports: [{name: client, port: 6379}, {name: bus, port: 16379}]
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: %[1]s
spec:
  serviceName: %[1]s
  replicas: %[3]d
  podManagementPolicy: Parallel
  selector:
    matchLabels: {app: %[1]s}
  template:
    metadata:
      labels: {app: %[1]s}
    spec:
      containers:
      - name: valkey
        image: %[4]s
        env:
        - name: POD_IP
          valueFrom: {fieldRef: {fieldPath: status.podIP}}
        command: ["valkey-server"]
        args:
        - --port
        - "6379"
        - --cluster-enabled
        - "yes"
        - --cluster-config-file
        - /tmp/nodes.conf
        - --cluster-node-timeout
        - "5000"
        - --cluster-announce-ip
        - $(POD_IP)
        - --protected-mode
        - "no"
        - --appendonly
        - "no"
        - --requirepass
        - %[2]s
        - --masterauth
        - %[2]s
        ports: [{containerPort: 6379}, {containerPort: 16379}]
        resources:
          requests: {cpu: 50m, memory: 64Mi}
          limits: {memory: 256Mi}
        readinessProbe:
          exec: {command: ["sh", "-c", "valkey-cli -a %[2]s --no-auth-warning ping | grep -q PONG"]}
          periodSeconds: 2
`, srcName, srcPassword, srcPods, valkeyClientImage)
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(manifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		_, err = utils.Run(exec.Command("kubectl", "create", "secret", "generic", srcName+"-auth",
			"--from-literal=password="+srcPassword))
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "statefulset", srcName, "-o", "jsonpath={.status.readyReplicas}"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal(fmt.Sprint(srcPods)))
		}, 3*time.Minute).Should(Succeed())

		By("forming the source cluster")
		out, err := utils.Run(exec.Command("kubectl", "get", "pods", "-l", "app="+srcName,
			"-o", "jsonpath={range .items[*]}{.status.podIP}:6379 {end}"))
		Expect(err).NotTo(HaveOccurred())
		_, err = srcExec(srcName+"-0", fmt.Sprintf("valkey-cli --cluster create %s --cluster-replicas 1 --cluster-yes", strings.TrimSpace(out)))
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			out, err := srcExec(srcName+"-0", "valkey-cli cluster info | tr -d '\\r'")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(ContainSubstring("cluster_state:ok"))
		}, 2*time.Minute).Should(Succeed())

		writeKeys("pre", 100)
	})

	It("joins the source as replicas without promoting anything", func() {
		manifest := fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  acl:
    users:
      - name: default
        password:
          type: Any
        permissions: "~* &* +@all"
  shards: 3
  replicasPerShard: 1
  resources:
    requests: {cpu: 50m, memory: 64Mi}
    limits: {memory: 256Mi}
  bootstrap:
    source:
      endpoints: ["%s-0.%s.default.svc.cluster.local:6379"]
      passwordSecretRef: {name: %s-auth, key: password}
`, dstName, srcName, srcName, srcName)
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(manifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for every shard to report in sync")
		Eventually(func(g Gomega) {
			c := utils.FindCondition(clusterStatus(g).Status.Conditions, valkeyiov1alpha1.ConditionBootstrapSynced)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionTrue))
		}, 6*time.Minute).Should(Succeed())

		By("checking every new node is a linked replica and none was promoted")
		for shard := 0; shard < 3; shard++ {
			for node := 0; node < 2; node++ {
				role := dstRole(dstPod(shard, node))
				Expect(role).To(ContainSubstring("role:slave"), dstPod(shard, node))
				Expect(role).To(ContainSubstring("master_link_status:up"), dstPod(shard, node))
			}
		}
		Consistently(func(g Gomega) {
			g.Expect(dstRole(dstPod(0, 0))).To(ContainSubstring("role:slave"))
			g.Expect(clusterStatus(g).Spec.Bootstrap.Stage).To(Equal(valkeyiov1alpha1.BootstrapStageBootstrap))
		}, 20*time.Second, 5*time.Second).Should(Succeed())

		By("checking no new node can be elected primary, and the source is untouched")
		Eventually(func(g Gomega) {
			ours, others := nodeLines(g)
			g.Expect(ours).To(HaveLen(6))
			for _, line := range ours {
				g.Expect(line).To(ContainSubstring("nofailover"), line)
			}
			g.Expect(others).To(HaveLen(6))
			for _, line := range others {
				g.Expect(line).NotTo(ContainSubstring("nofailover"), "the source must not be modified before cutover: "+line)
			}
		}, time.Minute).Should(Succeed())

		By("reading the source's data from a new replica")
		Expect(replicatedKeys()).To(Equal(100))
	})

	It("keeps replicating writes made after the join", func() {
		writeKeys("mid", 100)
		Eventually(replicatedKeys, time.Minute).Should(Equal(200))
	})

	It("cuts over only when spec.bootstrap.stage is Cutover", func() {
		_, err := utils.Run(exec.Command("kubectl", "patch", "valkeycluster", dstName, "--type=merge",
			"-p", `{"spec":{"bootstrap":{"stage":"Cutover"}}}`))
		Expect(err).NotTo(HaveOccurred())

		By("waiting for the new cluster to become Ready")
		Eventually(func(g Gomega) {
			cr := clusterStatus(g)
			g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
			g.Expect(cr.Status.ReadyShards).To(Equal(int32(3)))
		}, 6*time.Minute).Should(Succeed())

		By("checking the new nodes own all slots: three primaries, all ours")
		Eventually(func(g Gomega) {
			ours, others := nodeLines(g)
			primaries := 0
			for _, line := range ours {
				if strings.Contains(strings.Fields(line)[2], "master") {
					primaries++
				}
			}
			g.Expect(primaries).To(Equal(3))
			for _, line := range others {
				g.Expect(strings.Fields(line)[2]).NotTo(ContainSubstring("master"), "a source node is still primary: "+line)
			}
		}, 3*time.Minute).Should(Succeed())

		By("checking the source nodes stay as replicas that cannot be elected, and the new nodes can")
		Eventually(func(g Gomega) {
			ours, others := nodeLines(g)
			g.Expect(others).To(HaveLen(6))
			for _, line := range others {
				g.Expect(line).To(ContainSubstring("nofailover"), "source node can be elected: "+line)
			}
			for _, line := range ours {
				g.Expect(line).NotTo(ContainSubstring("nofailover"), "new node still cannot fail over: "+line)
			}
			c := utils.FindCondition(clusterStatus(g).Status.Conditions, valkeyiov1alpha1.ConditionBootstrapSourceDetached)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(c.Reason).To(Equal(valkeyiov1alpha1.ReasonSourceNodesRemain))
		}, 2*time.Minute).Should(Succeed())

		By("checking every source node stays in sync with its new primary, so a rollback has something to roll back to")
		Eventually(func(g Gomega) {
			for i := 0; i < srcPods; i++ {
				out, err := srcExec(fmt.Sprintf("%s-%d", srcName, i), "valkey-cli info replication | tr -d '\\r'")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(ContainSubstring("role:slave"), "source pod %d", i)
				g.Expect(out).To(ContainSubstring("master_link_status:up"), "source pod %d lost sync with the new primary", i)
			}
		}, 2*time.Minute).Should(Succeed())

		By("checking no data was lost across cutover")
		Expect(countKeys(dstPod(0, 0), "pre", 100)).To(Equal(100))
		Expect(countKeys(dstPod(0, 0), "mid", 100)).To(Equal(100))
	})

	It("keeps the source nodes until the stage is Complete, then forgets them", func() {
		By("shutting the source down")
		_, err := utils.Run(exec.Command("kubectl", "scale", "statefulset", srcName, "--replicas=0"))
		Expect(err).NotTo(HaveOccurred())

		By("checking the operator keeps the failed source nodes while the stage is Cutover")
		Eventually(func(g Gomega) {
			_, others := nodeLines(g)
			failed := 0
			for _, line := range others {
				if strings.Contains(strings.Fields(line)[2], "fail") {
					failed++
				}
			}
			g.Expect(failed).To(Equal(6))
		}, 2*time.Minute).Should(Succeed())
		Consistently(func(g Gomega) {
			_, others := nodeLines(g)
			g.Expect(others).To(HaveLen(6), "source nodes were forgotten before stage Complete")
		}, 45*time.Second, 5*time.Second).Should(Succeed())

		By("moving the stage to Complete")
		_, err = utils.Run(exec.Command("kubectl", "patch", "valkeycluster", dstName, "--type=merge",
			"-p", `{"spec":{"bootstrap":{"stage":"Complete"}}}`))
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			_, others := nodeLines(g)
			g.Expect(others).To(BeEmpty(), "source nodes are still in the cluster")
			c := utils.FindCondition(clusterStatus(g).Status.Conditions, valkeyiov1alpha1.ConditionBootstrapSourceDetached)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionTrue))
		}, 4*time.Minute).Should(Succeed())

		By("checking the cluster is healthy and the data is intact")
		Eventually(func(g Gomega) {
			cr := clusterStatus(g)
			g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
			g.Expect(cr.Status.ReadyShards).To(Equal(int32(3)))
		}, 8*time.Minute).Should(Succeed())
		Expect(countKeys(dstPod(0, 0), "pre", 100)).To(Equal(100))
		Expect(countKeys(dstPod(0, 0), "mid", 100)).To(Equal(100))
	})
})
