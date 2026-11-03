#!/usr/bin/env bash
# Verify that every e2e test runs in exactly one CI matrix job.
#
# Each matrix job selects its tests with a Ginkgo --label-filter, so this asks
# Ginkgo which tests each filter selects (via dry-run reports) rather than
# matching labels itself. Ginkgo label matching is case-insensitive, so a plain
# string comparison would miss a test labelled e.g. "valkeycluster" that the
# "ValkeyCluster" job also runs.
#
# Fails if a bucket selects no tests, if a test is selected by two jobs (it
# would run twice), or if a test is selected by no job (it would never run).
#
# Usage: verify-e2e-buckets.sh <catch-all label filter> <bucket>...
set -euo pipefail

if [ "$#" -lt 2 ]; then
	echo "usage: $0 <catch-all label filter> <bucket>..." >&2
	exit 2
fi
catchall=$1
shift
buckets=("$@")

cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

echo ">> checking e2e label buckets"

if ! go test -tags=e2e -c ./test/e2e/ -o "$work/e2e.test" >"$work/build.log" 2>&1; then
	echo "FAIL: could not build the e2e test binary:"
	cat "$work/build.log"
	exit 1
fi

# select_specs <name> [ginkgo flags...] writes the sorted file:line of each
# selected test to "$work/<name>.txt". A dry-run exits 197 when a FIt/FDescribe
# is present but still writes the report, so only a missing report fails.
select_specs() {
	local name=$1
	shift
	local report="$work/$name.json"
	"$work/e2e.test" -ginkgo.dry-run -ginkgo.json-report="$report" "$@" >"$work/$name.log" 2>&1 || true
	if [ ! -s "$report" ]; then
		echo "FAIL: e2e dry-run produced no report for $name:"
		cat "$work/$name.log"
		exit 1
	fi
	jq -r --arg root "$PWD/" '.[].SpecReports[]
		| select(.LeafNodeType == "It" and .State != "skipped" and .State != "pending")
		| "\(.LeafNodeLocation.FileName | ltrimstr($root)):\(.LeafNodeLocation.LineNumber)"' \
		"$report" | sort >"$work/$name.txt"
}

select_specs all

# jobs.tsv gets one "file:line<TAB>job" row for every test each job selects.
empty_buckets=""
for i in "${!buckets[@]}"; do
	b=${buckets[$i]}
	select_specs "bucket-$i" -ginkgo.label-filter="$b"
	n=$(wc -l <"$work/bucket-$i.txt")
	printf '  %3d  %s\n' "$n" "$b"
	if [ "$n" -eq 0 ]; then
		empty_buckets+=" $b"
	fi
	awk -v job="$b" '{print $0 "\t" job}' "$work/bucket-$i.txt" >>"$work/jobs.tsv"
done

# The catch-all may legitimately be empty once every test is labelled.
select_specs catchall -ginkgo.label-filter="$catchall"
printf '  %3d  %s\n' "$(wc -l <"$work/catchall.txt")" "$catchall"
awk '{print $0 "\tcatch-all"}' "$work/catchall.txt" >>"$work/jobs.tsv"
printf '  %3d  total\n' "$(wc -l <"$work/all.txt")"

failed=0

if [ -n "$empty_buckets" ]; then
	echo "FAIL: bucket(s) match no tests (check E2E_BUCKETS):$empty_buckets"
	failed=1
fi

overlap=$(awk -F'\t' '{jobs[$1] = jobs[$1] " " $2; n[$1]++}
	END {for (s in n) if (n[s] > 1) print "  OVERLAP " s " (" substr(jobs[s], 2) ")"}' "$work/jobs.tsv" | sort)
if [ -n "$overlap" ]; then
	echo "FAIL: test(s) selected by more than one job (would run twice):"
	echo "$overlap"
	failed=1
fi

missing=$(cut -f1 "$work/jobs.tsv" | sort -u | comm -23 "$work/all.txt" -)
if [ -n "$missing" ]; then
	echo "FAIL: test(s) selected by no job (would never run):"
	while IFS= read -r s; do echo "  MISSING $s"; done <<<"$missing"
	failed=1
fi

if [ "$failed" -ne 0 ]; then
	exit 1
fi
echo "OK: every e2e test is in exactly one job"
