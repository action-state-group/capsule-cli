#!/usr/bin/env bash
# build-evidence-graph-iife.sh -- rebuild the vendored browser verifier that
# `capsulectl deal report --html` embeds, from the agent-action-capsule commit
# evidenceGraphIIFESource records, and check it byte-for-byte.
#
#   scripts/build-evidence-graph-iife.sh           # rebuild and compare
#   scripts/build-evidence-graph-iife.sh --write   # rebuild and replace
#
# Needs git, node and npm. Install scripts are disabled; the one git
# dependency is built only for historical AAC locks that pin CLL from Git.
# Registry locks install the published package, including its built dist/.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
asset="$root/internal/cli/assets/evidence-graph.iife.js"
# The agent-action-capsule commit the vendored IIFE was built from, as
# evidenceGraphIIFESource records it. It is pinned apart from go.mod, so the
# Go module can move to a release tag without the browser verifier moving.
aac_commit=$(sed -n 's/.*evidenceGraphIIFESource *= *"agent-action-capsule [a-z]*@\([0-9a-f]\{40\}\) .*/\1/p' "$root/internal/cli/evidence_graph_assets.go")
[[ -n "$aac_commit" ]] || { echo "build-evidence-graph-iife: no source commit in evidenceGraphIIFESource" >&2; exit 2; }
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
: >"$work/npmrc"
export npm_config_userconfig="$work/npmrc"

git clone -q https://github.com/action-state-group/agent-action-capsule.git "$work/aac"
git -C "$work/aac" checkout -q "$aac_commit"
cd "$work/aac/ts"
cll_resolved=$(node -e 'const l=require("./package-lock.json");console.log(l.packages["node_modules/@action-state-group/cll"].resolved)')
npm ci --ignore-scripts --no-audit --no-fund >/dev/null

if [[ "$cll_resolved" == git+* ]]; then
  # Preserve reproducibility of the pre-migration AAC commit and its Git pin.
  cll_commit="${cll_resolved##*#}"
  git clone -q https://github.com/action-state-group/cll-ts.git "$work/cll"
  git -C "$work/cll" fetch -q origin "$cll_commit"
  git -C "$work/cll" checkout -q "$cll_commit"
  (cd "$work/cll" && npm ci --ignore-scripts --no-audit --no-fund >/dev/null && npm run -s build >/dev/null)
  cp -R "$work/cll/dist" node_modules/@action-state-group/cll/dist
fi

npm run -s emitter:iife >/dev/null 2>&1
built="$work/aac/ts/dist/evidence-graph.iife.js"
printf 'agent-action-capsule %s, CLL %s\n' "$aac_commit" "$cll_resolved"
shasum -a 256 "$built" | cut -d' ' -f1
if [[ "${1:-}" == "--write" ]]; then
  cp "$built" "$asset"
  shasum -a 256 "$asset" | cut -d' ' -f1 >"$asset.sha256"
  echo "wrote $asset"
elif ! cmp -s "$built" "$asset"; then
  echo "vendored asset differs from the rebuild" >&2
  exit 1
else
  echo "vendored asset matches"
fi
