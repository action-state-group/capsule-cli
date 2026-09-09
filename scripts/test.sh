#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

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

printf 'Running all tests, including MySQL integration, on loopback port %s\n' "$CAPSULE_CLI_TEST_MYSQL_PORT"
go test -race -count=1 -timeout=15m ./...
