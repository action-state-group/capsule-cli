#!/usr/bin/env bash
# Vendors Unicode's official TR39 confusables data (confusables.txt) for the
# deal receipt's shared-copy matching, then regenerates the compiled table.
# Pass the Unicode version to vendor. The file is kept unmodified in
# third_party/unicode/ with its digest beside it; only the mappings are
# compiled in (internal/cli/confusables_table.go), and a test checks the
# digest and regenerates the table, so neither can drift silently.
set -euo pipefail
cd "$(dirname "$0")/.."
version="${1:?usage: scripts/update-confusables.sh UNICODE_VERSION (e.g. 18.0.0)}"
out=third_party/unicode/confusables.txt
curl -sSfL -o "$out" "https://www.unicode.org/Public/${version}/security/confusables.txt"
grep -q "^# Version: ${version}\$" "$out" || { echo "downloaded file is not version ${version}" >&2; exit 1; }
shasum -a 256 "$out" | cut -d' ' -f1 > "$out.sha256"
go run ./scripts/genconfusables
echo "vendored confusables.txt ${version}: $(cat "$out.sha256")"
