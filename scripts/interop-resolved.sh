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

echo "== Go -> Python: a bundle built by evidencebook as resolved here, verified by Python"
go run "$eb/test/interop/bundle" "$work/book" "$work/bundle.json" "$work/refusal.json"
python "$py/python-aac/verify_bundle.py" "$work/bundle.json"
python "$py/python-cll/verify_bundle.py" "$work/bundle.json"
python "$py/python-cll/verify_refusal.py" "$work/refusal.json"
# Tampered copies of that bundle: a changed record (record identity is AAC's
# check) and a changed body digest (the range proof is cll's check).
python - "$work/bundle.json" "$work/record-changed.json" "$work/digest-changed.json" <<'EOF'
import json, sys
bundle = json.load(open(sys.argv[1]))
record_changed = json.loads(json.dumps(bundle))
record_changed["records"][1]["operator"] = "someone-else"
json.dump(record_changed, open(sys.argv[2], "w"))
digest_changed = json.loads(json.dumps(bundle))
digests = digest_changed["completeness_certificate"]["body_digests"]
digests[0] = ("0" if digests[0][0] != "0" else "1") + digests[0][1:]
json.dump(digest_changed, open(sys.argv[3], "w"))
EOF
expect_reject "Python AAC, a changed record" "rejected the bundle" \
  python "$py/python-aac/verify_bundle.py" "$work/record-changed.json"
expect_reject "Python cll, a changed body digest" "range proof" \
  python "$py/python-cll/verify_bundle.py" "$work/digest-changed.json"

echo "== Python -> Go: a bundle built by Python, verified by evidencebook as resolved here"
python "$py/python-cll/build_bundle.py" "$work/py-bundle.json" "$work/py-tampered.json"
go run "$eb/test/interop/verify" "$work/py-tampered.json"  # DELIBERATELY RED: the tampered copy as if genuine
expect_reject "evidencebook (resolved), a tampered Python bundle" "evidencebook rejected the bundle" \
  go run "$eb/test/interop/verify" "$work/py-tampered.json"

echo "== round trip passed at the resolved versions"
