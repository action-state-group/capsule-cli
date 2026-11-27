#!/usr/bin/env bash
# check-contract-source.sh -- cli/testdata/contract/cases/SOURCE.json names the
# capsule-engine commit the Evidence Contract case library and the embedded
# contract schema were copied from, with the digests of its SHA256SUMS and its
# schema. TestContractCaseLibraryMatchesPinnedSource and
# TestEmbeddedContractSchemaMatchesPin hold the copies to those digests; this
# check holds the digests to the commit: it fetches that one commit from the
# public capsule-engine repository and compares both files. Needs git, python3
# and network access to github.com. Touches nothing outside a temporary
# directory.
set -euo pipefail
cd "$(dirname "$0")/.."

if (($# > 0)); then
  echo "usage: scripts/check-contract-source.sh" >&2
  exit 2
fi

source_json=cli/testdata/contract/cases/SOURCE.json
field() { python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))[sys.argv[2]])' "$source_json" "$1"; }
repo=$(field repo)
commit=$(field commit)
cases=$(field path)
schema=$(field contract_schema_path)

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
git -C "$work" init --quiet
git -C "$work" fetch --quiet --depth 1 "https://github.com/$repo" "$commit"

sha256() { git -C "$work" show "$commit:$1" | python3 -c 'import hashlib, sys; print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest())'; }
status=0
for pair in "$cases/SHA256SUMS sha256sums_digest" "$schema contract_schema_sha256"; do
  path=${pair% *}
  want=$(field "${pair#* }")
  got=$(sha256 "$path")
  if [[ "$got" == "$want" ]]; then
    echo "ok   $repo@${commit:0:12}:$path"
  else
    echo "FAIL $repo@${commit:0:12}:$path is sha256 $got, SOURCE.json pins $want" >&2
    status=1
  fi
done
exit "$status"
