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
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/valkey-io/valkey-operator/internal/aclscan"
	"github.com/valkey-io/valkey-operator/test/utils"
)

// maxAclDryRunPlaceholders bounds how many placeholder arguments aclDryRun
// will pad a command with to satisfy its arity, as a safety net for
// commands commandArities couldn't resolve (see its doc comment). Since ACL
// DRYRUN never executes the command, placeholder values are never
// interpreted; only the argument count matters for admin/cluster commands.
const maxAclDryRunPlaceholders = 8

// commandName is the "container|subcommand" form Valkey uses to name a
// command in COMMAND INFO and in ACL rules (e.g. "+cluster|info"). aclscan
// discovers commands as token slices (e.g. []string{"CLUSTER", "INFO"}); this
// is the inverse of that for the handful of places that need the string form.
func commandName(tokens []string) string {
	return strings.ToLower(strings.Join(tokens, "|"))
}

// commandArities looks up the arity Valkey's own command table reports for
// each discovered command, via a single COMMAND INFO call, so aclDryRun can
// pad a command with exactly the right number of placeholder arguments up
// front instead of probing one at a time (a wrong-arity ACL DRYRUN call for
// e.g. CLUSTER MEET is itself a round-trip to the cluster over kubectl exec).
//
// arity follows Valkey's COMMAND INFO convention: a positive value is the
// exact total token count (command name and subcommand included), a
// negative value is the minimum. Commands missing from the result (e.g. if
// the output format doesn't parse as expected) are simply absent from the
// returned map; callers fall back to aclDryRun's own probing for those.
func commandArities(podName string, valkeyCli []string, commands []aclscan.Command) (map[string]int, error) {
	wanted := make(map[string]bool, len(commands))
	names := make([]string, 0, len(commands))
	for _, c := range commands {
		name := commandName(c.Tokens)
		if !wanted[name] {
			wanted[name] = true
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}

	kubectlArgs := []string{"exec", podName, "-c", "server", "--"}
	kubectlArgs = append(kubectlArgs, valkeyCli...)
	kubectlArgs = append(kubectlArgs, "COMMAND", "INFO")
	kubectlArgs = append(kubectlArgs, names...)

	output, err := utils.Run(exec.Command("kubectl", kubectlArgs...))
	if err != nil {
		return nil, err
	}

	// valkey-cli isn't attached to a terminal under kubectl exec, so it
	// prints each COMMAND INFO reply element on its own line rather than the
	// "N) M) ..." bulleted form it uses interactively: a command's own name,
	// immediately followed by its arity, then a variable number of lines for
	// its flags/key-spec/acl-category/tip/subcommand fields that we don't
	// need. Scanning for our own requested names is therefore both
	// sufficient and robust to that variable-length tail.
	lines := strings.Split(output, "\n")
	arities := make(map[string]int, len(names))
	for i := 0; i < len(lines)-1; i++ {
		name := strings.TrimSpace(lines[i])
		if !wanted[name] {
			continue
		}
		if arity, err := strconv.Atoi(strings.TrimSpace(lines[i+1])); err == nil {
			arities[name] = arity
		}
	}
	return arities, nil
}

// placeholdersNeeded returns how many placeholder arguments must be
// appended to tokens to satisfy arity (as returned by commandArities), or 0
// if arity is unknown (ok is false) or already satisfied.
func placeholdersNeeded(tokens []string, arity int, ok bool) int {
	if !ok {
		return 0
	}
	minTokens := arity
	if arity < 0 {
		minTokens = -arity
	}
	if needed := minTokens - len(tokens); needed > 0 {
		return needed
	}
	return 0
}

// aclDryRun execs `<valkeyCli...> ACL DRYRUN <user> <tokens...>` in
// podName's server container, padding tokens with minArgs placeholder
// arguments up front (see commandArities/placeholdersNeeded) and, if that
// wasn't enough, growing the padding further until Valkey stops reporting a
// wrong-arity error. It returns the command's own last line of output (i.e.
// with any valkey-cli warnings, such as the insecure-password-on-the-
// command-line notice, stripped).
func aclDryRun(podName string, valkeyCli []string, user string, tokens []string, minArgs int) (string, error) {
	args := append([]string{}, tokens...)
	for i := 0; i < minArgs; i++ {
		args = append(args, "x")
	}
	for extra := 0; extra <= maxAclDryRunPlaceholders; extra++ {
		kubectlArgs := []string{"exec", podName, "-c", "server", "--"}
		kubectlArgs = append(kubectlArgs, valkeyCli...)
		kubectlArgs = append(kubectlArgs, "ACL", "DRYRUN", user)
		kubectlArgs = append(kubectlArgs, args...)

		output, err := utils.Run(exec.Command("kubectl", kubectlArgs...))
		if err != nil {
			return "", err
		}
		result := lastNonEmptyLine(output)
		if !strings.Contains(result, "wrong number of arguments") {
			return result, nil
		}
		args = append(args, "x")
	}
	return "", fmt.Errorf("exceeded %d placeholder arguments without resolving arity for %q", maxAclDryRunPlaceholders, strings.Join(tokens, " "))
}

// lastNonEmptyLine returns the last non-empty line of output, trimmed. It's
// used to strip valkey-cli's warnings (printed on their own line before the
// actual reply) from combined stdout+stderr output.
func lastNonEmptyLine(output string) string {
	lines := utils.GetNonEmptyLines(output)
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[len(lines)-1])
}
