#!/usr/bin/env bash
# Regenerates scripts/test-weights from a CI run: the seconds each top-level
# test took in the run's race shards, which scripts/test.sh uses to cut the
# shards by time rather than by name. Run it after a change that moves a lot of
# test time, or when a shard drifts past about 10 minutes:
#
#   scripts/test-weights.sh <run-id> [owner/repo]
#
# Pick a pull-request run on a green head: a nightly run samples nothing, so its
# times are not what the pull-request shards spend. A name two packages share
# gets the sum of its times.
set -euo pipefail

cd "$(dirname "$0")/.."

run=${1:?usage: scripts/test-weights.sh <run-id> [owner/repo]}
repo=${2:-$(gh repo view --json nameWithOwner --jq .nameWithOwner)}

out=scripts/test-weights
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

# Log lines read "<job>\t<step>\t<timestamp> <go test -v output>"; a top-level
# result has no indentation after the timestamp, a subtest's is indented.
gh run view "$run" --repo "$repo" --log |
  awk -F'\t' '$1 ~ /^test \(race / { sub(/^[^ ]+ /, "", $3); print $3 }' |
  awk '/^--- (PASS|FAIL)/ { t = $4; gsub(/[()s]/, "", t); sum[$3] += t }
       END { for (n in sum) printf "%.2f %s\n", sum[n], n }' |
  sort -k2 >"$tmp"

if [[ ! -s "$tmp" ]]; then
  printf 'No race-shard test results in run %s.\n' "$run" >&2
  exit 1
fi
{
  printf '# Seconds per top-level test under -race, from CI run %s; see scripts/test-weights.sh.\n' "$run"
  cat "$tmp"
} >"$out"
printf '%d tests, %.0fs in total, written to %s\n' "$(wc -l <"$tmp")" "$(awk '{ s += $1 } END { print s }' "$tmp")" "$out"
