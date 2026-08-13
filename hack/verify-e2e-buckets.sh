#!/usr/bin/env bash
# Verify that no e2e test maps to more than one CI matrix bucket.
#
# A test matching two buckets (via its own or inherited Describe/Context labels)
# would run twice across the matrix; this fails with the offending file:line.
# A test matching no named bucket is covered by the CI catch-all job, so the
# catch-all count is printed only as a diagnostic.
#
# Usage: verify-e2e-buckets.sh <bucket>...
set -euo pipefail

buckets=("$@")
# Same derivation as E2E_CATCHALL in the Makefile; for display only.
catchall_label="!( $(sed 's/ / || /g' <<<"${buckets[*]}") )"

report=$(mktemp)
dryrun_log=$(mktemp)
trap 'rm -f "$report" "$dryrun_log"' EXIT

echo ">> checking e2e label buckets"

# Enumerate every spec and its labels without running anything. A Ginkgo
# dry-run exits 197 (GINKGO_FOCUS_EXIT_CODE) if a stray FIt/FDescribe is present,
# even though it succeeds and writes the report; so we ignore the exit code and
# instead fail if no report was produced.
go test -tags=e2e ./test/e2e/ -ginkgo.dry-run --ginkgo.json-report="$report" >"$dryrun_log" 2>&1 || true
if [ ! -s "$report" ]; then
	echo "FAIL: e2e dry-run produced no report:"
	cat "$dryrun_log"
	exit 1
fi

# Emit one line per spec: "<file>:<line>\t<label>,<label>,..." with the spec's own
# labels merged with all inherited container labels.
spec_labels=$(jq -r '
	.[].SpecReports[]
	| select(.LeafNodeType=="It")
	| "\(.LeafNodeLocation.FileName):\(.LeafNodeLocation.LineNumber)\t\((((.LeafNodeLabels // []) + [.ContainerHierarchyLabels[]?[]?]) | unique) | join(","))"
' "$report")

# Collect the file:line of every spec that matches each bucket. A spec matching
# two buckets therefore appears twice across all buckets, which we detect below.
bucket_hits=""
empty_buckets=""
for b in "${buckets[@]}"; do
	hits=$(awk -F'\t' -v b="$b" '{split($2,a,","); for(i in a) if(a[i]==b){print $1; break}}' <<<"$spec_labels")
	n=$(grep -c . <<<"$hits" || true)
	printf '  %3d  %s\n' "$n" "$b"
	if [ "$n" -eq 0 ]; then
		empty_buckets+=" $b"
	fi
	bucket_hits+="${hits}"$'\n'
done

# Catch-all count (diagnostic only): specs matching none of the buckets.
catchall_n=$(awk -F'\t' -v bl="${buckets[*]}" '
	BEGIN{split(bl,bk," ")}
	{hit=0; split($2,a,","); for(i in a) for(j in bk) if(a[i]==bk[j]) hit=1; if(!hit) c++}
	END{print c+0}
' <<<"$spec_labels")
printf '  %3d  %s\n' "$catchall_n" "$catchall_label"

# Verify every named bucket matches at least one spec.
if [ -n "$empty_buckets" ]; then
	echo "FAIL: bucket(s) match no tests (check E2E_BUCKETS):$empty_buckets"
	exit 1
fi

# Verify no spec is in more than one bucket (it would otherwise run twice).
overlap=$(grep . <<<"$bucket_hits" | sort | uniq -d || true)
if [ -n "$overlap" ]; then
	echo "FAIL: test(s) in more than one bucket:"
	sed 's/^/  OVERLAP /' <<<"$overlap"
	exit 1
fi

echo "OK: every e2e test is in exactly one bucket"
