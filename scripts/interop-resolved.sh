#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# The Go <-> Python round trip, at the versions THIS module resolves.
#
# evidencebook runs the same round trip in its own CI, but against its own pins.
# Under minimal version selection capsulectl links newer versions (capsule-emit-go,
# agent-action-capsule) than evidencebook pins, so that green check is for a build
# capsulectl does not ship. Here evidencebook's interop drivers run as packages of
# the evidencebook module this go.mod resolves, compiled with this module's graph:
# the evidencebook code capsulectl links, with the dependencies capsulectl links.
#
#   scripts/interop-resolved.sh EVIDENCEBOOK_SRC
#
# EVIDENCEBOOK_SRC is a checkout of evidencebook at the version this module
# resolves (for its Python verifier scripts). Needs Python with checkpointed-local-log,
# agent-action-capsule and capsule-emit installed, as evidencebook's CI installs them.
set -euo pipefail

src=${1:?usage: scripts/interop-resolved.sh EVIDENCEBOOK_SRC}
py=$src/test/interop
eb=github.com/action-state-group/evidencebook

echo "== the module graph this module resolves (go list -m all)"
go list -m all
echo "== what the interop drivers link"
go list -deps -f '{{if .Module}}{{.Module.Path}} {{.Module.Version}}{{end}}' \
  "$eb/test/interop/bundle" "$eb/test/interop/verify" | grep action-state-group | sort -u

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# expect_reject NAME MESSAGE CMD...: CMD must fail, and say MESSAGE (not fail for another reason).
expect_reject() {
  local name=$1 message=$2 out
  shift 2
  if out=$("$@" 2>&1); then
    echo "$name: ACCEPTED, it must not be: $out"
    exit 1
  fi
  grep -q -- "$message" <<<"$out" || { echo "$name: failed for another reason: $out"; exit 1; }
  echo "$name: rejected ($(grep -m 1 -- "$message" <<<"$out"))"
}

# tamper BUNDLE: write BUNDLE.record-changed.json (one record changed: record identity)
# and BUNDLE.digest-changed.json (one body digest changed: the range proof). Each
# direction is tampered at both layers, separately.
tamper() {
  python - "$1" <<'EOF'
import json, sys
path = sys.argv[1]
bundle = json.load(open(path))
record_changed = json.loads(json.dumps(bundle))
record_changed["records"][1]["operator"] = "someone-else"
json.dump(record_changed, open(path.removesuffix(".json") + ".record-changed.json", "w"))
digest_changed = json.loads(json.dumps(bundle))
digests = digest_changed["completeness_certificate"]["body_digests"]
digests[0] = ("0" if digests[0][0] != "0" else "1") + digests[0][1:]
json.dump(digest_changed, open(path.removesuffix(".json") + ".digest-changed.json", "w"))
EOF
}

echo "== Go -> Python: a bundle built by evidencebook as resolved here, verified by Python"
go run "$eb/test/interop/bundle" "$work/book" "$work/bundle.json" "$work/refusal.json"
python "$py/python-aac/verify_bundle.py" "$work/bundle.json"
python "$py/python-cll/verify_bundle.py" "$work/bundle.json"
python "$py/python-cll/verify_refusal.py" "$work/refusal.json"
tamper "$work/bundle.json"
expect_reject "Python AAC, a changed record" "record_identity_invalid" \
  python "$py/python-aac/verify_bundle.py" "$work/bundle.record-changed.json"
expect_reject "Python cll, a changed body digest" "interval range proof" \
  python "$py/python-cll/verify_bundle.py" "$work/bundle.digest-changed.json"

echo "== Python -> Go: a bundle built by Python, verified by evidencebook as resolved here"
python "$py/python-cll/build_bundle.py" "$work/py-bundle.json" "$work/py-tampered.json"
go run "$eb/test/interop/verify" "$work/py-bundle.json"
expect_reject "evidencebook (resolved), the Python script's own tampered copy" "evidencebook rejected the bundle" \
  go run "$eb/test/interop/verify" "$work/py-tampered.json"
tamper "$work/py-bundle.json"
expect_reject "evidencebook (resolved), a changed record" "record_identity_invalid" \
  go run "$eb/test/interop/verify" "$work/py-bundle.record-changed.json"
expect_reject "evidencebook (resolved), a changed body digest" "range_proof_invalid" \
  go run "$eb/test/interop/verify" "$work/py-bundle.digest-changed.json"

echo "== round trip passed at the resolved versions"
