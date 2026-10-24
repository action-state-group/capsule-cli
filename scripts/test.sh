#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

# Which pass and shard to run (see below), checked before any MySQL starts.
pass="${CAPSULE_TEST_PASS:-all}"
shard="${CAPSULE_TEST_SHARD:-1/1}"
budget="${CAPSULE_TEST_BUDGET:-420}"
case "$pass" in all | race | plain) ;; *)
  printf 'CAPSULE_TEST_PASS must be race, plain or all, not %s\n' "$pass" >&2
  exit 1
  ;;
esac
if [[ ! "$shard" =~ ^([0-9]+)/([0-9]+)$ ]] || ((BASH_REMATCH[1] < 1 || BASH_REMATCH[1] > BASH_REMATCH[2])); then
  printf 'CAPSULE_TEST_SHARD must be i/n with 1 <= i <= n, not %s\n' "$shard" >&2
  exit 1
fi
shard_index=${BASH_REMATCH[1]}
shard_count=${BASH_REMATCH[2]}

# CI supplies an isolated MySQL service. Local runs create and clean up their
# own container rather than silently skipping the SDK integration tests.
container=""
cleanup() {
  if [[ -n "$container" ]]; then
    docker rm --force "$container" >/dev/null
  fi
}
trap cleanup EXIT

if [[ -z "${CAPSULE_CLI_TEST_MYSQL_PORT:-}" ]]; then
  command -v docker >/dev/null || {
    printf '%s\n' 'Docker is required for the complete test suite.' >&2
    exit 1
  }
  container=$(docker run --detach --rm \
    --publish 127.0.0.1::3306 \
    --env MYSQL_ALLOW_EMPTY_PASSWORD=yes \
    --env MYSQL_DATABASE=capsule_cli_test \
    --health-cmd='mysqladmin ping --silent' \
    --health-interval=2s --health-timeout=2s --health-retries=45 \
    mysql:8.4)
  ready=false
  for ((attempt = 0; attempt < 90; attempt++)); do
    if [[ "$(docker inspect --format '{{.State.Health.Status}}' "$container")" == healthy ]]; then
      ready=true
      break
    fi
    sleep 2
  done
  if [[ "$ready" != true ]]; then
    printf '%s\n' 'Isolated MySQL did not become healthy.' >&2
    docker logs "$container" >&2
    exit 1
  fi
  CAPSULE_CLI_TEST_MYSQL_PORT=$(docker inspect --format '{{(index (index .NetworkSettings.Ports "3306/tcp") 0).HostPort}}' "$container")
  export CAPSULE_CLI_TEST_MYSQL_PORT
fi

# Arguments are passed to go test. Two passes: the race detector, under which the
# slowest single-goroutine case loops run a fixed sample (see
# cli/race_sample_test.go; CAPSULE_TEST_RACE_FULL=1 runs them all),
# then every case without it.
#
# CI runs the passes as separate jobs and splits the race pass across several:
#   CAPSULE_TEST_PASS=race|plain   one pass only (default: both)
#   CAPSULE_TEST_SHARD=i/n         only the top-level tests in shard i of n
# A shard is every n-th name of the sorted `go test -list` output, so the n
# shards together run every test; a name two packages share runs in each.
# Each pass ends with its per-package times and slowest tests, and warns when
# a package takes longer than CAPSULE_TEST_BUDGET seconds (default 420).
# Both modules: the core (the repository root) and the first-party plugins
# (plugins/, capsule-cli#236). go test ./... in a module does not reach into a
# nested one, so every go command below runs once per module.
modules=(. plugins)
run=()
if ((shard_count > 1)); then
  listed=$(for m in "${modules[@]}"; do (cd "$m" && go test -list '.*' ./...); done)
  names=$(printf '%s\n' "$listed" | grep -E '^(Test|Example|Fuzz)' | sort -u |
    awk -v i="$shard_index" -v n="$shard_count" '(NR - 1) % n == i - 1')
  if [[ -z "$names" ]]; then
    printf 'Shard %s has no tests.\n' "$shard" >&2
    exit 1
  fi
  printf 'Shard %s: %d top-level tests\n' "$shard" "$(printf '%s\n' "$names" | wc -l)"
  run=(-run "^($(printf '%s\n' "$names" | paste -sd '|'))\$")
fi

log=$(mktemp)
trap 'cleanup; rm -f "$log"' EXIT

# go test with -v, teed to $log; then the pass's per-package times and its
# slowest top-level tests, from that log.
timed() {
  local label=$1 status=0
  shift
  go test -v "$@" 2>&1 | tee "$log" || status=$?
  printf '\n== %s: per-package time (budget %ss) ==\n' "$label" "$budget"
  awk -v budget="$budget" '$1 ~ /^(ok|FAIL)$/ && $2 ~ /\// && $3 ~ /s$/ {
    t = $3; sub(/s$/, "", t); printf "%9.1fs  %s  %s\n", t, $1, $2
    if (t + 0 > budget + 0) printf "::warning::%s took %.0fs, over the %ss budget\n", $2, t, budget
  }' "$log"
  printf '== %s: slowest tests ==\n' "$label"
  awk '/^--- (PASS|FAIL)/ { t = $4; gsub(/[()s]/, "", t); printf "%9.2fs  %s  %s\n", t, $2, $3 }' "$log" |
    sort -rn | awk 'NR <= 15'
  return "$status"
}

printf 'Running all tests, including MySQL integration, on loopback port %s\n' "$CAPSULE_CLI_TEST_MYSQL_PORT"
status=0
for m in "${modules[@]}"; do
  if [[ "$pass" != plain ]]; then
    (cd "$m" && timed "race $shard ($m)" ${@+"$@"} ${run[@]+"${run[@]}"} -race -count=1 -timeout=15m ./...) || status=$?
  fi
  if [[ "$pass" != race ]]; then
    (cd "$m" && timed "plain $shard ($m)" ${@+"$@"} ${run[@]+"${run[@]}"} -count=1 -timeout=10m ./...) || status=$?
  fi
done
exit "$status"
