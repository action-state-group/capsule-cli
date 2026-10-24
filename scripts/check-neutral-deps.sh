#!/usr/bin/env bash
# check-neutral-deps.sh -- the donatable core depends only on neutral modules
# (capsule-cli#326). Every github.com/action-state-group module in the core
# module's build graph, tests included, must be on the list below. A
# company-OSS module (evidencebook, capsule-engine, ...) or any module not yet
# listed fails, so a new one is added here on purpose, after a boundary check.
set -euo pipefail
cd "$(dirname "$0")/.."

neutral=(
  github.com/action-state-group/agent-action-capsule/go
  github.com/action-state-group/capsule-cli
  github.com/action-state-group/capsule-emit/go
  github.com/action-state-group/capsule-viewer/go
  github.com/action-state-group/checkpointed-local-log/go
)

# Listed first, and on its own: a failed go list must fail the check, never
# leave it with nothing to compare.
if ! mods=$(go list -deps -test -f '{{with .Module}}{{.Path}}{{end}}' ./...); then
  echo "::error::go list failed; the core's build graph could not be read"
  exit 1
fi
bad=0
while read -r mod; do
  case " ${neutral[*]} " in
    *" $mod "*) ;;
    *) echo "::error::the core's build graph includes $mod, which is not on the neutral list in scripts/check-neutral-deps.sh"; bad=1 ;;
  esac
done < <(printf '%s\n' "$mods" | grep '^github.com/action-state-group/' | sort -u)
exit "$bad"
