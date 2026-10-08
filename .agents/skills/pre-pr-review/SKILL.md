---
name: pre-pr-review
description: Use before opening a valkey-operator PR, to validate the current branch on a fresh kind cluster and review its diff (e.g. "/pre-pr-review", "check my branch before I raise a PR", "kind-validate my change").
---

# Pre-PR review

Validate the contributor's branch on a fresh kind cluster, review its diff, and write a report plus a Testing block for the PR description. Report findings; the contributor decides what to change.

## Workflow

1. **Resolve the target.** The current branch, as it is on disk. Run `git fetch https://github.com/valkey-io/valkey-operator.git main` and take `git merge-base HEAD FETCH_HEAD` as `<base>`. Fetching by URL gets upstream `main` whatever the contributor's remotes are called, and avoids diffing against a stale fork `main`. Stop and tell the contributor if HEAD is on `main` or `git rev-list --count <base>..HEAD` prints 0.

   Save `git status --porcelain` to `/tmp/review-<slug>-status.before`. If it prints anything, carry on and mark the run dirty: the image builds from the working tree, so uncommitted changes are part of the review.

   Derive `<slug>` from the branch name: lowercase it, replace each run of characters outside `a-z0-9` with `-`, trim to 40 characters, strip leading and trailing `-`. `feat/Zone-Pinning` becomes `feat-zone-pinning`. The cluster is `review-<slug>`.
2. **Understand the change.** Read `git diff <base>` (commits plus uncommitted changes) and `git log <base>..HEAD`. For each issue referenced in the commit messages (`Fixes #n`, `Closes #n`), read it with `gh issue view <n> --repo valkey-io/valkey-operator --comments` when `gh` is installed; otherwise ask the contributor for the issue and its repro steps. Done when you can list (a) every behaviour the diff changes, each with a scenario that exercises it, including one that reproduces the issue's original failure, and (b) the cluster shape those scenarios need: node count, zone labels, cert-manager.
3. **Fast checks.** Run `make lint test`. Record pass or fail, and the failing output, for the report. Continue to step 4 on failure.

   `make test` runs `manifests generate fmt`, so it rewrites files the contributor forgot to regenerate or format. Diff `git status --porcelain` against `/tmp/review-<slug>-status.before`, and run `git diff` on any file that changed. Report these files that make changed as a "fix first" finding: CI's generated-code check fails on them. Repeat this check after step 6.
4. **Start the static review in the background.** On Claude Code, invoke the `code-review` skill on the diff at `medium` effort. On other agents, use the agent's own review command, or dispatch a subagent to review `git diff <base>` for correctness bugs. When the diff touches `api/`, a `*_types.go`, or `config/crd/`, also apply every item of the [API checklist](#api-checklist) to those files. The findings feed steps 7 and 8.
5. **Create the cluster.** Always a fresh one, with its own kubeconfig. The contributor's ambient kubectl context may point at a real cluster, and `make install`/`deploy`/`undeploy` act on whatever context is current, so the review cluster never touches `~/.kube/config`:

   ```sh
   kind delete cluster --name review-<slug>   # clears a leftover from an earlier run
   echo '{"kind": "Cluster", "apiVersion": "kind.x-k8s.io/v1alpha4", "nodes": [{"role": "control-plane"}, {"role": "worker"}, {"role": "worker"}]}' \
     | kind create cluster --name review-<slug> --kubeconfig /tmp/kind-review-<slug>.kubeconfig --config -
   ```

   Extend the node list when step 2's cluster shape needs more (zone labels, extra workers). The cluster outlives the review.

   From here on, every cluster-touching command, `make` and `kubectl` alike, runs with `KUBECONFIG=/tmp/kind-review-<slug>.kubeconfig`. Shell state may not carry between commands, so set it at the top of each block.
6. **Build and deploy.** `make deploy` rewrites `config/manager/kustomization.yaml`; the copy and restore around it keep the contributor's version, uncommitted edits included.

   ```sh
   export KUBECONFIG=/tmp/kind-review-<slug>.kubeconfig
   [ "$(kubectl config current-context)" = kind-review-<slug> ] || exit 1
   make docker-build IMG=valkey-operator:review-<slug>
   kind load docker-image valkey-operator:review-<slug> --name review-<slug>
   make install
   cp config/manager/kustomization.yaml /tmp/review-<slug>-kustomization.yaml.bak
   make deploy IMG=valkey-operator:review-<slug>
   cp /tmp/review-<slug>-kustomization.yaml.bak config/manager/kustomization.yaml
   kubectl -n valkey-operator-system rollout status deploy --timeout=180s
   ```
7. **Validate.** Exercise each behaviour from step 2 directly: apply CRs, mutate specs, watch conditions and operator logs. At least one scenario must fail without the change. When the static findings arrive, add a scenario for each one the cluster can confirm or refute. Capture every command and its observed output verbatim as you go; the report quotes them. Done when every behaviour from step 2 and every cluster-checkable finding has a scenario with a recorded pass or fail. Delete the CRs you created; keep the cluster.
8. **Report.** Write `.review/<slug>.md` (git-ignored). Sections, in order:
   1. Verdict: "ready to open" or "fix first", with the reasons
   2. Change summary
   3. Environment: cluster name and config, image tag, `<base>` and HEAD SHAs, dirty or clean
   4. Fast checks
   5. Validation scenarios: per scenario, purpose, commands, observed output, pass or fail
   6. Code review findings
   7. API checklist results, when the diff touches API types. State whether the shape is right for the change, not only whether it works.
   8. Follow-ups

   Then print to the contributor, in this order:
   - A Testing block in a fenced `markdown` code block, ready to paste under the PR template's `### Testing`:

     ```markdown
     Validated on kind (`review-<slug>`, image `valkey-operator:review-<slug>`, HEAD `<short-sha>`):

     - `make lint test`: pass
     - <scenario purpose>: pass
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

- `make test-e2e` chains `cleanup-test-e2e`, which deletes the kind cluster mid-review. To run e2e specs against the review cluster: `KIND_CLUSTER=review-<slug> go test -tags=e2e ./test/e2e/ -v -ginkgo.label-filter "<label>"`. The suite brings its own operator: BeforeSuite builds an image, creates `valkey-operator-system` (failing if it exists), and runs `make deploy`; AfterSuite runs `make undeploy` and `make uninstall`, removing the review deployment, the CRDs and every CR. Run e2e specs last, after `make undeploy`, wrapped in the same `kustomization.yaml` copy and restore as step 6.
- Tag images with something other than `latest`: `:latest` implies `imagePullPolicy: Always`, which breaks kind-loaded images.
- TLS-related changes need cert-manager in the cluster. The e2e suite installs it; see `test/e2e/e2e_suite_test.go`.
