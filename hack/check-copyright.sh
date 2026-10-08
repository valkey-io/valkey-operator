#!/usr/bin/env bash
# Fails if a Go file added since the merge base with $1 does not carry the
# header in hack/boilerplate.go.txt with the current year. Existing files keep
# the year they were created.
set -euo pipefail

base="${1:-origin/main}"
year="$(date -u +%Y)"
expected="$(sed -E "s/Copyright [0-9]{4}/Copyright ${year}/" hack/boilerplate.go.txt)"

# Not piped into the loop, so a bad base ref fails the script.
files="$(git diff --name-only --diff-filter=A "${base}...HEAD" -- '*.go')"

status=0
while IFS= read -r f; do
  [ -n "$f" ] || continue
  # 25 lines leaves room for a //go:build line above the header.
  if [[ "$(head -n 25 "$f")" != *"$expected"* ]]; then
    echo "::error file=${f}::new file must start with the header in hack/boilerplate.go.txt, with the year ${year}"
    status=1
  fi
done <<< "$files"

exit "$status"
