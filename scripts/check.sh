#!/usr/bin/env bash
# check.sh -- the local check before every push (`make check`, `make check-full`).
#
#   scripts/check.sh          the quality job's checks, then scripts/test.sh with
#                             both passes: the race pass sampled as pull requests
#                             ran it before they stopped running it, then every
#                             case without the race detector
#   scripts/check.sh --full   the same with every case under the race detector
#                             (CAPSULE_TEST_RACE_FULL=1, as the nightly runs it),
#                             then the report page in headless Chrome
#
# Pull requests run only the quality job, the plain test shards and the report
# page; the race shards run on push to main and on the nightly. Needs Go and
# Docker (scripts/test.sh starts a disposable MySQL 8.4 unless
# CAPSULE_CLI_TEST_MYSQL_PORT names one); --full also needs Chrome or Chromium
# (scripts/check-html-render.sh). Changes no tracked file; exits non-zero on
# the first failing step.
set -euo pipefail
cd "$(dirname "$0")/.."

full=false
case "${1:-}" in
  "") ;;
  --full) full=true ;;
  *)
    echo "usage: scripts/check.sh [--full]" >&2
    exit 2
    ;;
esac

step() { printf '\n== check: %s ==\n' "$*"; }

step formatting and module files
unformatted=$(gofmt -l .)
if [[ -n "$unformatted" ]]; then
  printf 'gofmt would change:\n%s\n' "$unformatted" >&2
  exit 1
fi
go mod tidy -diff
step the core depends only on neutral modules
bash scripts/check-neutral-deps.sh
step nothing points into the trees that moved out
bash scripts/check-moved-paths.sh --fail
step the test sharding script splits every test once
bash scripts/check-shard-tests.sh
step vet
go vet ./...
step rendered skill files match spec.yaml
go run ./cmd/skillgen --check
step build
make build

if [[ "$full" == true ]]; then
  export CAPSULE_TEST_RACE_FULL=1
  step "tests: race pass (every case), then plain pass"
else
  unset CAPSULE_TEST_RACE_FULL
  step "tests: race pass (sampled), then plain pass"
fi
CAPSULE_TEST_PASS=all CAPSULE_TEST_SHARD=1/1 bash scripts/test.sh

if [[ "$full" == true ]]; then
  step report page renders and checks itself
  bash scripts/check-html-render.sh
fi
step passed
