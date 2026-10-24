#!/usr/bin/env bash
# Checks scripts/shard-tests.sh on small inputs: the shards are disjoint, cover
# every name, balance the listed times, and spread names with no time or no
# weights file. CI runs it in the quality job.
set -euo pipefail

cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

check() { # label weights-file shard-count names-file
  local label=$1 weights=$2 n=$3 names=$4 i
  : >"$work/all"
  for ((i = 1; i <= n; i++)); do
    bash scripts/shard-tests.sh "$i" "$n" "$weights" <"$names" >"$work/s$i"
    if [[ ! -s "$work/s$i" ]]; then
      printf '%s: shard %d/%d is empty\n' "$label" "$i" "$n" >&2
      exit 1
    fi
    cat "$work/s$i" >>"$work/all"
  done
  if ! diff <(sort "$work/all") "$names" >/dev/null; then
    printf '%s: the shards do not run each name exactly once\n' "$label" >&2
    exit 1
  fi
}

printf 'TestA\nTestB\nTestC\nTestD\nTestE\nTestF\nTestG\n' >"$work/names"
printf '# comment\n30 TestA\n10 TestB\n10 TestC\n10 TestD\n0.00 TestE\n' >"$work/weights"

# Listed, partly listed, all-zero and absent weights.
check listed "$work/weights" 3 "$work/names"
check missing /dev/null 3 "$work/names"
printf '0.00 TestA\n0.00 TestB\n0.00 TestC\n0.00 TestD\n0.00 TestE\n0.00 TestF\n0.00 TestG\n' >"$work/zero"
check zero "$work/zero" 7 "$work/names"

# The heavy test sits alone and the three 10s share the others.
[[ "$(bash scripts/shard-tests.sh 1 3 "$work/weights" <"$work/names" | head -1)" == TestA ]] || {
  printf 'the heaviest test is not first on shard 1\n' >&2
  exit 1
}
printf 'shard-tests: ok\n'
