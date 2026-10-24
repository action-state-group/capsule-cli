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

bad=0
while read -r mod; do
  case " ${neutral[*]} " in
    *" $mod "*) ;;
    *) echo "::error::the core's build graph includes $mod, which is not on the neutral list in scripts/check-neutral-deps.sh"; bad=1 ;;
  esac
done < <(go list -deps -test -f '{{with .Module}}{{.Path}}{{end}}' ./... | grep '^github.com/action-state-group/' | sort -u)
exit "$bad"
