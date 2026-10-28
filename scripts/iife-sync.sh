#!/usr/bin/env bash
# Rebuild the browser runtime `report build` embeds from a capsule-viewer
# checkout (the page code) and an agent-action-capsule checkout (the
# verifier), and compare it with the vendored copy.
#
#   scripts/iife-sync.sh <capsule-viewer checkout> <agent-action-capsule checkout> [--check]
#
# The two checkouts should sit at the commits evidenceGraphIIFESource in
# cli/evidence_graph_assets.go names (scripts/build-evidence-graph-iife.sh
# fetches them itself). scripts/build-report-runtime.mjs does the bundling.
# Without --check the rebuilt file replaces cli/assets/evidence-graph.iife.js
# and its digest is printed, to be pinned in cli/evidence_graph_assets.go
# (EvidenceGraphIIFEDigest / evidenceGraphIIFESource), evidence-graph.iife.js.sha256
# and assets/README.md together. With --check the vendored copy must already
# equal the rebuild (exit 1 otherwise) -- the drift guard for a vendored
# build artifact.
#
# Node is needed here, at build time of this repository, never at render
# time: `capsulectl book report build` embeds the bytes this script vendored.
set -euo pipefail

cv="${1:-}"
aac="${2:-}"
mode="${3:-}"
if [[ -z "$cv" || ! -d "$cv/ts" || -z "$aac" || ! -d "$aac/ts" ]]; then
  echo "usage: $0 <capsule-viewer checkout> <agent-action-capsule checkout> [--check]" >&2
  exit 2
fi
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
vendored="$repo_root/cli/assets/evidence-graph.iife.js"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# Install scripts stay off: both lockfiles resolve to registry packages.
for d in "$cv/ts" "$aac/ts"; do
  (cd "$d" && npm ci --silent --ignore-scripts --no-audit --no-fund) >/dev/null
done
built="$work/evidence-graph.iife.js"
node "$repo_root/scripts/build-report-runtime.mjs" "$cv" "$aac" "$built"
commits="capsule-viewer $(git -C "$cv" rev-parse HEAD), agent-action-capsule $(git -C "$aac" rev-parse HEAD)"
digest="$(shasum -a 256 "$built" | cut -d' ' -f1)"

if [[ "$mode" == "--check" ]]; then
  if cmp -s "$built" "$vendored"; then
    echo "vendored IIFE matches the rebuild at $commits ($digest)"
    exit 0
  fi
  echo "vendored IIFE differs from the rebuild at $commits ($digest); run $0 $cv $aac to refresh, then pin the digest" >&2
  exit 1
fi
cp "$built" "$vendored"
echo "$digest" >"$vendored.sha256"
echo "vendored $vendored from $commits"
echo "sha256: $digest -- pin it in cli/evidence_graph_assets.go and cli/assets/README.md"
