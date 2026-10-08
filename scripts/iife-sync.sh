#!/usr/bin/env bash
# Rebuild the browser runtime `report build` embeds from an agent-action-capsule
# checkout and compare it with the vendored copy.
#
#   scripts/iife-sync.sh <aac-checkout> [--check]
#
# Without --check the rebuilt file replaces internal/cli/assets/evidence-graph.iife.js
# and its digest is printed, to be pinned in internal/cli/report.go
# (evidenceGraphIIFEDigest / evidenceGraphIIFESource) and in assets/README.md
# together. With --check the vendored copy must already equal the rebuild
# (exit 1 otherwise) -- the drift guard for a vendored build artifact.
#
# Node is needed here, at build time of this repository, never at render
# time: `capsulectl report build` embeds the bytes this script vendored.
set -euo pipefail

aac="${1:-}"
mode="${2:-}"
if [[ -z "$aac" || ! -d "$aac/ts" ]]; then
  echo "usage: $0 <agent-action-capsule checkout> [--check]" >&2
  exit 2
fi
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
vendored="$repo_root/internal/cli/assets/evidence-graph.iife.js"

(cd "$aac/ts" && npm ci --silent && npm run --silent emitter:iife) >/dev/null
built="$aac/ts/dist/evidence-graph.iife.js"
commit="$(git -C "$aac" rev-parse HEAD)"
digest="$(shasum -a 256 "$built" | cut -d' ' -f1)"

if [[ "$mode" == "--check" ]]; then
  if cmp -s "$built" "$vendored"; then
    echo "vendored IIFE matches the rebuild at $commit ($digest)"
    exit 0
  fi
  echo "vendored IIFE differs from the rebuild at $commit ($digest); run $0 $aac to refresh, then pin the digest" >&2
  exit 1
fi
cp "$built" "$vendored"
echo "vendored $vendored from $commit"
echo "sha256: $digest -- pin it in internal/cli/report.go and internal/cli/assets/README.md"
