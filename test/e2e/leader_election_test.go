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
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/valkey-io/valkey-operator/test/utils"
)

// A leader that shuts down releases the lease on its way out, so another
// operator pod takes over at once rather than after the lease duration (15s).
// Each spec sets up the replica count it needs, so one failing does not skip
// the other.
var _ = Describe("Leader election", Ordered, ContinueOnFailure, Label("leader-election"), func() {
	const (
		deployment = "valkey-operator-controller-manager"
		// LeaderElectionID in cmd/main.go.
		leaseName = "73d40801.valkey.io"
		// Well under the 15s lease duration a standby waits out when the
		// lease is not released.
		takeoverTimeout = 8 * time.Second
	)

	kubectl := func(args ...string) (string, error) {
		return utils.Run(exec.Command("kubectl", append(args, "-n", namespace)...))
	}

	// leaderPod returns the pod named in the lease. The holder identity is
	// "<pod>_<uuid>".
	leaderPod := func() (string, error) {
		holder, err := kubectl("get", "lease", leaseName, "-o", "jsonpath={.spec.holderIdentity}")
		if err != nil {
			return "", err
		}
		pod, _, _ := strings.Cut(strings.TrimSpace(holder), "_")
		return pod, nil
	}

	// livePods returns the controller-manager pods not being deleted.
	livePods := func() ([]string, error) {
		out, err := kubectl("get", "pods", "-l", "control-plane=controller-manager", "-o",
			`go-template={{ range .items }}{{ if not .metadata.deletionTimestamp }}{{ .metadata.name }}{{ "\n" }}{{ end }}{{ end }}`)
		return utils.GetNonEmptyLines(out), err
	}

	// scaleTo scales the Deployment and waits until the rollout completes.
	scaleTo := func(replicas string) {
		GinkgoHelper()
		_, err := kubectl("scale", "deployment", deployment, "--replicas="+replicas)
		Expect(err).NotTo(HaveOccurred())
		_, err = kubectl("rollout", "status", "deployment", deployment, "--timeout=3m")
		Expect(err).NotTo(HaveOccurred())
	}

	// waitForSingleLeader waits for one operator pod that holds the lease and
	// returns its name.
	waitForSingleLeader := func() string {
		GinkgoHelper()
		var leader string
		Eventually(func(g Gomega) {
			pods, err := livePods()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pods).To(HaveLen(1))
			leader, err = leaderPod()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(leader).To(Equal(pods[0]))
		}, 2*time.Minute, time.Second).Should(Succeed())
		return leader
	}

	// waitForTakeover waits for a pod other than oldLeader to hold the lease,
	// within takeoverTimeout of start, and returns it.
	waitForTakeover := func(oldLeader string, start time.Time) string {
		GinkgoHelper()
		By("waiting for another operator pod to take the lease")
		var newLeader string
		Eventually(func(g Gomega) {
			var err error
			newLeader, err = leaderPod()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(newLeader).NotTo(BeEmpty())
			g.Expect(newLeader).NotTo(Equal(oldLeader))
		}).WithTimeout(takeoverTimeout-time.Since(start)).WithPolling(250*time.Millisecond).Should(Succeed(),
			"no other pod took the lease within %s of the leader being stopped", takeoverTimeout)
		_, _ = GinkgoWriter.Write([]byte(newLeader + " took the lease after " +
			time.Since(start).Round(100*time.Millisecond).String() + "\n"))
		return newLeader
	}

	expectControllersStarted := func(pod string) {
		GinkgoHelper()
		By("checking the new leader started the controllers")
		Eventually(func(g Gomega) {
			logs, err := kubectl("logs", pod, "-c", "manager")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(logs).To(ContainSubstring("Successfully acquired lease"))
			g.Expect(logs).To(ContainSubstring(`"controller": "valkeycluster"`))
			g.Expect(logs).To(ContainSubstring("Starting workers"))
		}, time.Minute, time.Second).Should(Succeed())
	}

	AfterAll(func() {
		if CurrentSpecReport().Failed() {
			utils.CollectDebugInfo(namespace)
		}
		By("scaling the operator back to one replica")
		scaleTo("1")
		waitForSingleLeader()
	})

	It("hands the lease on as soon as the leader shuts down", func() {
		By("running two operator replicas")
		scaleTo("2")

		var leader string
		Eventually(func(g Gomega) {
			pods, err := livePods()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pods).To(HaveLen(2))
			leader, err = leaderPod()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pods).To(ContainElement(leader), "the lease holder is not a running operator pod")
		}, 2*time.Minute, time.Second).Should(Succeed())

		By("deleting the leader " + leader + " with its normal grace period")
		start := time.Now()
		_, err := kubectl("delete", "pod", leader, "--wait=false")
		Expect(err).NotTo(HaveOccurred())

		// The standby usually wins, but the Deployment's replacement pod can
		// too. Either way the lease must move before the old one would expire.
		newLeader := waitForTakeover(leader, start)
		expectControllersStarted(newLeader)
	})

	// A helm upgrade with one replica: the rolling update starts the new pod
	// first and stops the leader once the new pod is Ready.
	It("hands the lease to the new pod of a rolling update", func() {
		By("running one operator replica")
		scaleTo("1")
		leader := waitForSingleLeader()

		By("restarting the Deployment, which rolls its pod template")
		_, err := kubectl("rollout", "restart", "deployment", deployment)
		Expect(err).NotTo(HaveOccurred())

		// The leader gets SIGTERM when the rolling update deletes it.
		By("waiting for the rolling update to stop the leader " + leader)
		var start time.Time
		Eventually(func(g Gomega) {
			out, err := kubectl("get", "pod", leader, "--ignore-not-found",
				"-o", "jsonpath={.metadata.name} {.metadata.deletionTimestamp}")
			g.Expect(err).NotTo(HaveOccurred())
			name, deleted, _ := strings.Cut(strings.TrimSpace(out), " ")
			g.Expect(name == "" || deleted != "").To(BeTrue(), "the leader has not been stopped yet")
			start = time.Now()
		}, 3*time.Minute, 250*time.Millisecond).Should(Succeed())

		newLeader := waitForTakeover(leader, start)
		expectControllersStarted(newLeader)

		_, err = kubectl("rollout", "status", "deployment", deployment, "--timeout=3m")
		Expect(err).NotTo(HaveOccurred())
	})
})
