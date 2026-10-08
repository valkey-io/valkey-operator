#!/usr/bin/env bash
# Fails if a Go file added since the merge base with $1 lacks the current
# year's copyright header. Existing files keep the year they were created.
set -euo pipefail

base="${1:-origin/main}"
header="Copyright $(date -u +%Y) Valkey Contributors."
status=0

while IFS= read -r f; do
  if ! head -n 5 "$f" | grep -qF "$header"; then
    echo "::error file=${f},line=2::new file must start with the header in hack/boilerplate.go.txt ('${header}')"
    status=1
  fi
done < <(git diff --name-only --diff-filter=A "${base}...HEAD" -- '*.go')

exit "$status"
