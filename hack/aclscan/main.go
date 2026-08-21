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

// Command aclscan prints the Valkey commands the operator's reconciliation
// code issues, as discovered by statically scanning cmd/ and internal/ for
// valkey-go client calls. It's a manual-inspection aid for keeping the
// "_operator" system user's ACL (internal/controller/users.go) in sync with
// the code; test/e2e uses the same package to verify ACL coverage against a
// live cluster via ACL DRYRUN.
//
// Usage: go run ./hack/aclscan
package main

import (
	"fmt"
	"os"

	"github.com/valkey-io/valkey-operator/internal/aclscan"
)

func main() {
	commands, err := aclscan.OperatorCommands()
	if err != nil {
		fmt.Fprintln(os.Stderr, "aclscan:", err)
		os.Exit(1)
	}
	for _, c := range commands {
		fmt.Println(c.String())
	}
}
