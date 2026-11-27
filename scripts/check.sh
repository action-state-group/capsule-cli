#!/usr/bin/env bash
# check.sh -- the local check before every push (`make check`): every pull
# request job, so a push never waits on GitHub CI to find a failure.
#
#   - the quality job's checks;
#   - scripts/test.sh with both passes, unsharded: the race pass (which CI runs
#     on push to main and the nightly), then every case without the race
#     detector (CI's plain shards);
#   - the report page in headless Chrome (CI's report-page job), on the
#     capsulectl this check built;
#   - the changelog-fragments workflow.
#
# seal-pr-capsule is the one pull request job not run here: it seals CI's result
# with the repository's signing keys and tests nothing. Needs Go, Docker
# (scripts/test.sh starts a disposable MySQL 8.4 unless
# CAPSULE_CLI_TEST_MYSQL_PORT names one), Chrome or Chromium (the page tests
# and scripts/check-html-render.sh), python3 with venv, and network access to
# github.com (scripts/check-contract-source.sh) and, on the first run, PyPI
# (scripts/python-verifiers.sh). Changes no tracked file; exits non-zero on
# the first failing step.
set -euo pipefail
cd "$(dirname "$0")/.."

if (($# > 0)); then
  echo "usage: scripts/check.sh" >&2
  exit 2
fi

step() { printf '\n== check: %s ==\n' "$*"; }

step no committed env files
bash scripts/check-env-files.sh
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
step "the contract case library's pins name the engine's files"
bash scripts/check-contract-source.sh
step vet
go vet ./...
step rendered skill files match spec.yaml
go run ./cmd/skillgen --check
step build
make build

step changelog fragments
python3 .github/changelog_fragments.py check
(cd .github && python3 -m unittest test_changelog_fragments)

step "tests: race pass, then plain pass"
CAPSULE_TEST_PASS=all CAPSULE_TEST_SHARD=1/1 bash scripts/test.sh

step report page renders and checks itself
# The binary make build just made, never one CAPSULECTL names from elsewhere.
CAPSULECTL="$PWD/capsulectl" bash scripts/check-html-render.sh

step passed
