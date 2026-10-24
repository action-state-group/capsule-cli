#!/usr/bin/env bash
# build-evidence-graph-iife.sh -- rebuild the vendored browser runtime that
# `report build`, `deal report --html` and `presentation build` embed, from
# the capsule-viewer and agent-action-capsule commits evidenceGraphIIFESource
# records, and check it byte-for-byte.
#
#   scripts/build-evidence-graph-iife.sh           # rebuild and compare
#   scripts/build-evidence-graph-iife.sh --write   # rebuild and replace
#
# Needs git, node and npm. Install scripts are disabled; both lockfiles
# resolve to registry packages, which carry their built dist/.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
asset="$root/cli/assets/evidence-graph.iife.js"
src="$root/cli/evidence_graph_assets.go"
# The commits the vendored IIFE was built from, as evidenceGraphIIFESource
# records them. They are pinned apart from go.mod, so a Go module can move
# without the browser runtime moving.
cv_commit=$(sed -n 's/.*evidenceGraphIIFESource *= *"capsule-viewer [a-z]*@\([0-9a-f]\{40\}\) .*/\1/p' "$src")
aac_commit=$(sed -n 's/.*evidenceGraphIIFESource *= *".* agent-action-capsule [a-z]*@\([0-9a-f]\{40\}\) .*/\1/p' "$src")
[[ -n "$cv_commit" && -n "$aac_commit" ]] || { echo "build-evidence-graph-iife: no source commits in evidenceGraphIIFESource" >&2; exit 2; }
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
: >"$work/npmrc"
export npm_config_userconfig="$work/npmrc"

git clone -q https://github.com/action-state-group/capsule-viewer.git "$work/capsule-viewer"
git -C "$work/capsule-viewer" checkout -q "$cv_commit"
git clone -q https://github.com/action-state-group/agent-action-capsule.git "$work/agent-action-capsule"
git -C "$work/agent-action-capsule" checkout -q "$aac_commit"

printf 'capsule-viewer %s, agent-action-capsule %s\n' "$cv_commit" "$aac_commit"
if [[ "${1:-}" == "--write" ]]; then
  bash "$root/scripts/iife-sync.sh" "$work/capsule-viewer" "$work/agent-action-capsule"
else
  bash "$root/scripts/iife-sync.sh" "$work/capsule-viewer" "$work/agent-action-capsule" --check
fi
