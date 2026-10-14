---
name: pre-pr-review
description: Use before opening a valkey-operator PR, to validate the current branch on a fresh kind cluster and review its diff (e.g. "/pre-pr-review", "check my branch before I raise a PR", "kind-validate my change").
---

# Pre-PR review

Validate the contributor's branch on a fresh kind cluster, review its diff, and write a report plus a Testing block for the PR description. Report findings; the contributor decides what to change.

## Workflow

1. **Resolve the target.** The current branch, as it is on disk. Run `git fetch https://github.com/valkey-io/valkey-operator.git main` and take `git merge-base HEAD FETCH_HEAD` as `<base>`. Fetching by URL gets upstream `main` whatever the contributor's remotes are called, and avoids diffing against a stale fork `main`. Stop and tell the contributor if HEAD is on `main`, or if `git rev-list --count <base>..HEAD` prints 0 and `git status --porcelain` prints nothing.

   Save `git status --porcelain` to `/tmp/review-<slug>-status.before` and `git diff | sha256sum` to `/tmp/review-<slug>-diff.before`. If the status prints anything, carry on and mark the run dirty: the image builds from the working tree, so uncommitted changes are part of the review.

   Derive `<slug>` from the branch name: lowercase it, replace each run of characters outside `a-z0-9` with `-`, trim to 40 characters, strip leading and trailing `-`. `feat/Zone-Pinning` becomes `feat-zone-pinning`. The cluster is `review-<slug>`.
2. **Understand the change.** Read `git diff <base>` (commits plus uncommitted changes) and `git log <base>..HEAD`. `git diff` leaves out untracked files, which the image still builds from, so also read each file `git ls-files --others --exclude-standard` lists that belongs to the change, and include them in step 4's review. For each issue referenced in the commit messages (`Fixes #n`, `Closes #n`), read it with `gh issue view <n> --repo valkey-io/valkey-operator --comments` when `gh` is installed; otherwise ask the contributor for the issue and its repro steps. Done when you can list (a) every behaviour the diff changes, each with a scenario that exercises it, including one that reproduces the issue's original failure, and (b) the cluster shape those scenarios need: node count, zone labels, cert-manager.
3. **Fast checks.** Run `make lint test`. Record pass or fail, and the failing output, for the report. Continue to step 4 on failure.

   `make test` runs `manifests generate fmt`, so it rewrites files the contributor forgot to regenerate or format. Compare `git status --porcelain` with `/tmp/review-<slug>-status.before` and `git diff | sha256sum` with `/tmp/review-<slug>-diff.before`; the hash catches a rewrite of a file that was already modified, whose status line reads ` M` both times. Run `git diff` on anything that changed. Report these files that make changed as a "fix first" finding: CI's generated-code check fails on them. Repeat this check after step 6.
4. **Start the static review in the background.** On Claude Code, invoke the `code-review` skill on the diff at `medium` effort. On other agents, use the agent's own review command, or dispatch a subagent to review `git diff <base>` for correctness bugs. When the diff touches `api/`, a `*_types.go`, or `config/crd/`, also apply every item of the [API checklist](#api-checklist) to those files. The findings feed steps 7 and 9.
5. **Create the cluster.** Always a fresh one, with its own kubeconfig. The contributor's ambient kubectl context may point at a real cluster, and `make install`/`deploy`/`undeploy` act on whatever context is current, so the review cluster never touches `~/.kube/config`:

   ```sh
   kind delete cluster --name review-<slug>   # clears a leftover from an earlier run
   echo '{"kind": "Cluster", "apiVersion": "kind.x-k8s.io/v1alpha4", "nodes": [{"role": "control-plane"}, {"role": "worker"}, {"role": "worker"}]}' \
     | kind create cluster --name review-<slug> --kubeconfig /tmp/kind-review-<slug>.kubeconfig --config -
   ```

   Extend the node list when step 2's cluster shape needs more (zone labels, extra workers). The cluster outlives the review.

   From here on, every cluster-touching command, `make` and `kubectl` alike, runs with `KUBECONFIG=/tmp/kind-review-<slug>.kubeconfig`. Shell state may not carry between commands, so set it at the top of each block.
6. **Build and deploy.** `make deploy` rewrites `config/manager/kustomization.yaml`; the backup and the `EXIT` trap put back the contributor's version, uncommitted edits included, even when the block fails or is killed. `set -e` stops the block at the first failure, so a failed build never deploys an image left over from an earlier run.

   ```sh
   set -e
   export KUBECONFIG=/tmp/kind-review-<slug>.kubeconfig
   [ "$(kubectl config current-context)" = kind-review-<slug> ] || exit 1
   cp config/manager/kustomization.yaml /tmp/review-<slug>-kustomization.yaml.bak
   trap 'cp /tmp/review-<slug>-kustomization.yaml.bak config/manager/kustomization.yaml' EXIT
   make docker-build IMG=valkey-operator:review-<slug>
   kind load docker-image valkey-operator:review-<slug> --name review-<slug>
   make install
   make deploy IMG=valkey-operator:review-<slug>
   kubectl -n valkey-operator-system rollout status deploy --timeout=180s
   ```

   When a fix forces a rebuild mid-run, tag each new image `review-<slug>-r2`, `-r3` and so on. Reloading the same tag into kind can leave running pods on the old image.
7. **Validate.** Exercise each behaviour from step 2 directly: apply CRs, mutate specs, watch conditions and operator logs. At least one scenario must fail without the change. When the static findings arrive, add a scenario for each one the cluster can confirm or refute. Capture every command and its observed output verbatim as you go; the report quotes them. Done when every behaviour from step 2 and every cluster-checkable finding has a scenario with a recorded pass or fail. Delete the CRs you created; keep the cluster.

   When the diff changes what the operator renders into pods or their ConfigMaps (pod template, volumes, scripts, ConfigMap keys), include an upgrade scenario. Build a second image from `<base>` (`git worktree add` at `<base>`, then `make docker-build IMG=valkey-operator:review-<slug>-base` there), deploy it from that worktree with step 6's commands, and create a cluster with some data. Then redeploy the branch image from the branch with step 6's commands and check that pods roll one at a time with the data kept. This is the only scenario that checks the order of operations on upgrade, such as the ConfigMap updating before the first pod rolls.

   Before marking a static finding refuted, grep `test/e2e/` and `internal/controller/*_test.go` for its scenario and reuse their setup (Secrets, ConfigMaps, cluster shape). Refute only when a working repro shows the behaviour is fine, or the code path is unreachable. A repro that fails for an unrelated reason refutes nothing.
8. **Run the related e2e specs.** Grep `test/e2e/` for the functions, fields, files and resource names the diff touches, and list the `Label(...)` values on the matching `Describe`/`Context`/`It` blocks. Pick the narrowest label that covers each match (`external-cm`, not `valkeynode`). Each label takes about two minutes. AfterSuite removes the review deployment and the CRDs, so this is the last cluster step:

   ```sh
   set -e
   export KUBECONFIG=/tmp/kind-review-<slug>.kubeconfig
   [ "$(kubectl config current-context)" = kind-review-<slug> ] || exit 1
   cp config/manager/kustomization.yaml /tmp/review-<slug>-kustomization.yaml.bak
   trap 'cp /tmp/review-<slug>-kustomization.yaml.bak config/manager/kustomization.yaml' EXIT
   make undeploy
   KIND_CLUSTER=review-<slug> CERT_MANAGER_INSTALL_SKIP=true go test -tags=e2e ./test/e2e/ -v -ginkgo.label-filter "<label> || <label>"
   ```

   BeforeSuite runs `make deploy`, which rewrites `kustomization.yaml`; the trap restores it as in step 6. Several labels run for longer than a default tool timeout, so run this block in the background or with a long timeout. Drop `CERT_MANAGER_INSTALL_SKIP=true` when a selected label needs TLS. Record pass or fail per label.
9. **Report.** Run `git check-ignore -q .review`. If it succeeds, write `.review/<slug>.md`. Otherwise the branch predates the ignore rule, so write `/tmp/review-<slug>.md` and say so in the output. Sections, in order:
   1. Verdict: "ready to open" or "fix first", with the reasons
   2. Change summary
   3. Environment: cluster name and config, every image tag built (`-base`, `-r2`, ...), `<base>` and HEAD SHAs, dirty or clean
   4. Fast checks
   5. Validation scenarios: per scenario, purpose, commands, observed output, pass or fail
   6. Related e2e specs: per label, pass or fail
   7. Code review findings
   8. API checklist results, when the diff touches API types. State whether the shape is right for the change, not only whether it works.
   9. Follow-ups

   Then print to the contributor, in this order:
   - A Testing block in a fenced `markdown` code block, ready to paste under the PR template's `### Testing`:

     ```markdown
     Validated on kind (`review-<slug>`, image `valkey-operator:review-<slug>`, HEAD `<short-sha>`):

     - `make lint test`: pass
     - <scenario purpose>: pass
     - e2e `<label>`: pass
     ```
   - The report path.
   - The teardown command: `kind delete cluster --name review-<slug>`.

## API checklist

Apply each item to the changed API types and CRDs. The [Kubernetes API conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md) are the reference.

- New fields are optional with a documented default, or required with a stated reason.
- A bool that could grow a third state is an enum.
- Structured config uses typed fields or a list of named entries, not `map[string]string`.
- Enums use `+kubebuilder:validation:Enum` with CamelCase values.
- Fields sharing a prefix sit in a nested struct.
- Status reports state through conditions per `docs/status-conditions.md`, not ad-hoc bools or message strings.
- Cross-namespace references carry a stated reason.
- Spec changes stay compatible with existing CRs; renames and type changes carry a migration note.
- The change belongs in the API at all, rather than as an operator default.
- `make manifests generate` was run, and the CRD diff matches the type diff.

## Gotchas

- `make test-e2e` chains `cleanup-test-e2e`, which deletes the kind cluster mid-review; step 8 runs `go test` directly. The suite brings its own operator: BeforeSuite builds an image, creates `valkey-operator-system` (failing if it exists), and runs `make deploy`; AfterSuite runs `make undeploy` and `make uninstall`, removing the review deployment, the CRDs and every CR.
- Tag images with something other than `latest`: `:latest` implies `imagePullPolicy: Always`, which breaks kind-loaded images.
- TLS-related changes need cert-manager in the cluster. The e2e suite installs it; see `test/e2e/e2e_suite_test.go`.
