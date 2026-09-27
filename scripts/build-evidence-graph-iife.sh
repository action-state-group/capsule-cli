#!/usr/bin/env bash
# build-evidence-graph-iife.sh -- rebuild the vendored browser verifier that
# `capsulectl deal report --html` embeds, from the agent-action-capsule commit
# this module already pins in go.mod, and check it byte-for-byte.
#
#   scripts/build-evidence-graph-iife.sh           # rebuild and compare
#   scripts/build-evidence-graph-iife.sh --write   # rebuild and replace
#
# Needs git, node and npm. Install scripts are disabled; the one git
# dependency (cll-ts) is built from the commit the TS lockfile pins.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
asset="$root/internal/cli/assets/evidence-graph.iife.js"
aac_commit=$(cd "$root" && go list -m -f '{{.Version}}' github.com/action-state-group/agent-action-capsule/go | sed 's/.*-//')
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
: >"$work/npmrc"
export npm_config_userconfig="$work/npmrc"

git clone -q https://github.com/action-state-group/agent-action-capsule.git "$work/aac"
git -C "$work/aac" checkout -q "$aac_commit"
cd "$work/aac/ts"
cll_commit=$(node -e 'const l=require("./package-lock.json");console.log(l.packages["node_modules/@action-state-group/cll"].resolved.split("#")[1])')
npm ci --ignore-scripts --no-audit --no-fund >/dev/null

git clone -q https://github.com/action-state-group/cll-ts.git "$work/cll"
git -C "$work/cll" fetch -q origin "$cll_commit"
git -C "$work/cll" checkout -q "$cll_commit"
(cd "$work/cll" && npm ci --ignore-scripts --no-audit --no-fund >/dev/null && npm run -s build >/dev/null)
cp -R "$work/cll/dist" node_modules/@action-state-group/cll/dist

npm run -s emitter:iife >/dev/null 2>&1
built="$work/aac/ts/dist/evidence-graph.iife.js"
printf 'agent-action-capsule %s, cll-ts %s\n' "$aac_commit" "$cll_commit"
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
