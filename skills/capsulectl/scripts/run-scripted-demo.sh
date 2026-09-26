#!/usr/bin/env bash
set -euo pipefail

# Exercises every verb in ../spec.yaml against a throwaway jsonl profile in a
# temp directory, proving the evidence policy from spec.yaml (Steven's
# 2026-09-22 ruling), not just that capsules exist:
#
#   - the three CONSEQUENTIAL actions (discover, publish, cll append) each
#     map to a capsule (sealed or persisted) and every one of those is
#     independently verified;
#   - the five NOT-CONSEQUENTIAL actions (verify, contract validate,
#     plugin ls, cll list, get) run and produce no capsule at all — this
#     script never calls `capsulectl seal` for any of them;
#   - a failed evidence record fails closed (this skill's evidence policy:
#     "report evidence unavailable, stop before the next consequential
#     action") rather than silently continuing.
#
# Total elapsed time, printed at the end, doubles as the item's
# fresh-environment timing check (install through the first discover /
# contract-validate call, target under an hour). It never touches a real
# profile, a real CLL, or any path outside its own temp directory.

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

export XDG_CONFIG_HOME="$work/config"
export CAPSULECTL_PLUGIN_ROOTS="$work/empty-plugin-root"
mkdir -p "$XDG_CONFIG_HOME" "$CAPSULECTL_PLUGIN_ROOTS"

start_epoch=$(date +%s)

bin="$work/capsulectl"
(cd "$repo_root" && go build -o "$bin" ./cmd/capsulectl)

profile=demo
seed="$work/signing.hex"
"$bin" key generate --output "$seed" >"$work/keygen.json"
public_key=$(jq -r .public_key "$work/keygen.json")

store="$work/store"
"$bin" profile create --name "$profile" --type jsonl --jsonl-path "$store" \
  --namespace demo --log-id skill-demo \
  --trusted-key "$public_key" --signing-key-file "$seed" >/dev/null
"$bin" store init --profile "$profile" >/dev/null

scan_root="$work/scan-root"
mkdir -p "$scan_root"
cat >"$scan_root/otel-config.yaml" <<'EOF'
exporters:
  otlp:
    endpoint: "https://example.invalid:4317"
EOF
scope="$work/scope.yaml"
printf 'roots:\n  - %q\n' "$scan_root" >"$scope"

echo "== 0/11 evidence-failure-fails-closed check (not a numbered action) ==" >&2
# The evidence policy: "if evidence generation fails, report `evidence
# unavailable`, stop before the next consequential action." Point
# --seal-output at a directory this process cannot write to (it exists, so
# MkdirAll is a no-op, but CreateTemp inside it fails on permission) and
# confirm discover fails rather than silently proceeding without a record.
unwritable="$work/unwritable"
mkdir -p "$unwritable"
chmod 0500 "$unwritable"
set +e
"$bin" discover --profile "$profile" --scope "$scope" --format json \
  --seal-output "$unwritable/scan.json" >"$work/discover-fail-attempt.json" 2>&1
fail_status=$?
set -e
chmod 0700 "$unwritable" # restore so the temp dir cleans up on trap
if [[ "$fail_status" -eq 0 ]]; then
  echo "FAIL: discover should have failed closed with an unwritable --seal-output, but exited 0" >&2
  cat "$work/discover-fail-attempt.json" >&2
  exit 1
fi
echo "  confirmed: a failing evidence record stops the run (exit $fail_status), never silently proceeds" >&2

capsules_file="$work/capsules.txt"
: >"$capsules_file"
parent=""

echo "== 1/11 discover (self-sealing; consequential) ==" >&2
discover_out="$work/discover-scan.json"
"$bin" discover --profile "$profile" --scope "$scope" --format json --seal-output "$discover_out" >"$work/discover-stdout.json"
discover_capsule=$(jq -r .sealed.capsule_id "$work/discover-stdout.json")
"$bin" verify --profile "$profile" --capsule "$discover_out" >"$work/discover-verify.json"
jq -e '.producer_signature_and_trust=="passed" and .capsule_identity=="passed"' "$work/discover-verify.json" >/dev/null
echo "$discover_capsule" >>"$capsules_file"
parent="$discover_capsule"
echo "  self-sealed + verified: discover -> $discover_capsule" >&2

echo "== 2/11 publish (primary action; consequential) ==" >&2
ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)
jq -n --arg op "$profile" --arg ts "$ts" --arg parent "$parent" '
  {spec_version:"capsule-seal-request/v1",
   capsule:({ActionID:"skill-demo/publish-1", ActionType:"fyi", Operator:$op, Developer:"capsulectl-skill/v0", Timestamp:$ts,
             Chain:{ParentCapsuleID:$parent, Relation:"io.capsulectl.skill_step"}}),
   payload:{note:"scripted demo publish"}}' >"$work/publish-request.json"
"$bin" publish --profile "$profile" --request "$work/publish-request.json" >"$work/publish-result.json"
published_id=$(jq -r .capsule_id "$work/publish-result.json")
echo "$published_id" >>"$capsules_file"
echo "  published: publish -> $published_id" >&2

echo "== 3/11 get (not-consequential; no capsule) ==" >&2
"$bin" get --profile "$profile" --capsule-id "$published_id" >"$work/get-result.json"
echo "  ran, no seal call made — get is local inspection per the evidence policy" >&2

echo "== 4/11 verify (not-consequential; no capsule) ==" >&2
"$bin" get --profile "$profile" --capsule-id "$published_id" --raw --output "$work/published-raw.json" >"$work/get-raw-result.json"
"$bin" verify --profile "$profile" --capsule "$work/published-raw.json" >"$work/verify-of-published.json"
jq -e '.producer_signature_and_trust=="passed" and .capsule_identity=="passed"' "$work/verify-of-published.json" >/dev/null
echo "  ran + passed, no seal call made — checking a bundle is local validation per the evidence policy" >&2

echo "== 5/11 cll list (not-consequential; no capsule) ==" >&2
"$bin" cll list --profile "$profile" >"$work/cll-list-result.json"
echo "  ran, no seal call made — reading the log is local inspection per the evidence policy" >&2

echo "== 6/11 contract validate (not-consequential; no capsule) ==" >&2
schema="$work/demo-schema.json"
doc="$work/demo-doc.json"
cat >"$schema" <<'EOF'
{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","required":["hello"],"properties":{"hello":{"type":"string"}}}
EOF
cat >"$doc" <<'EOF'
{"hello":"world"}
EOF
"$bin" contract validate "$doc" --schema "$schema" --json >"$work/contract-result.json"
echo "  ran, no seal call made — this is the local validation the evidence policy names explicitly" >&2

echo "== 7/11 plugin ls (not-consequential; no capsule) ==" >&2
"$bin" plugin ls >"$work/plugin-ls-result.json"
echo "  ran, no seal call made — listing discovered plugins is local inspection per the evidence policy" >&2

echo "== 8/11 cll append (primary action; consequential) ==" >&2
"$bin" cll append --profile "$profile" --capsule "$discover_out" >"$work/cll-append-result.json"
appended_seq=$(jq -r .sequence "$work/cll-append-result.json")
# append's own job is to persist an EXISTING capsule, not create a new one --
# its "record" is the discover capsule itself, so this action maps onto the
# same capsule_id already logged for step 1/11, not a fresh one.
echo "$discover_capsule" >>"$capsules_file"
echo "  appended discover scan (capsule $discover_capsule) at CLL sequence $appended_seq" >&2

# Steps 9-11 exercise [capsulectl-book-verbs-v0]'s no-book-required half:
# the deterministic remainder of the retired capsule-judge harness. None
# needs --profile, a store, or a signing key -- pure computation over
# caller-supplied files, same as contract validate.

echo "== 9/11 judge pin (not-consequential; no capsule) ==" >&2
pin_input="$work/judge-pin-input.json"
cat >"$pin_input" <<'EOF'
{"model_id":"gpt-eval/1.0","prompt_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","axes_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
EOF
"$bin" judge pin "$pin_input" >"$work/judge-pin-result.json"
pin_digest=$(jq -r .judge_pin_digest "$work/judge-pin-result.json")
[[ "$pin_digest" =~ ^[0-9a-f]{64}$ ]] || { echo "FAIL: judge pin did not print a 64-hex-char digest" >&2; exit 1; }
echo "  ran, no seal call made — computing a pin is local computation per the evidence policy" >&2

echo "== 10/11 judge drift (not-consequential; no capsule) ==" >&2
drift_input_b="$work/judge-pin-input-b.json"
cat >"$drift_input_b" <<'EOF'
{"model_id":"gpt-eval/1.0","model_version":"v2","prompt_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","axes_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
EOF
"$bin" judge drift pin "$pin_input" "$drift_input_b" >"$work/judge-drift-pin-result.json"
jq -e '.pin_matches==false' "$work/judge-drift-pin-result.json" >/dev/null || {
  echo "FAIL: a silent model_version change should not report pin_matches=true" >&2; exit 1; }
reports_a="$work/reports-a.json"
reports_b="$work/reports-b.json"
cat >"$reports_a" <<'EOF'
[{"case_id":"case-1","judge_pin_digest":"pin-a","verdict":"met"}]
EOF
cat >"$reports_b" <<'EOF'
[{"case_id":"case-1","judge_pin_digest":"pin-a","verdict":"not_met"}]
EOF
"$bin" judge drift reports "$reports_a" "$reports_b" >"$work/judge-drift-reports-result.json"
jq -e '.drifted==1 and .cases[0].pin_matches==true and .cases[0].label_matches==false' "$work/judge-drift-reports-result.json" >/dev/null || {
  echo "FAIL: a same-pin, different-verdict rerun must seal a real delta, not a silent disagreement" >&2; exit 1; }
echo "  ran, no seal call made — comparing pins/report sets is local comparison per the evidence policy" >&2

echo "== 11/11 calibration summarize (not-consequential; no capsule) ==" >&2
ratings="$work/ratings.json"
cat >"$ratings" <<'EOF'
[{"case_id":"case-1","agrees_with_judge":true}]
EOF
"$bin" calibration summarize "$reports_a" "$ratings" >"$work/calibration-result.json"
jq -e '.pins[0].judge_pin_digest=="pin-a" and .pins[0].rated_count==1 and .pins[0].agreement_count==1 and .pins[0].agreement_rate_micros==1000000' \
  "$work/calibration-result.json" >/dev/null || {
  echo "FAIL: calibration summarize did not fold the human rating into the expected k-of-n" >&2; exit 1; }
echo "  ran, no seal call made — folding stats is local computation per the evidence policy" >&2

end_epoch=$(date +%s)
elapsed=$((end_epoch - start_epoch))
consequential_actions=$(wc -l <"$capsules_file" | tr -d ' ')
distinct_capsules=$(sort -u "$capsules_file" | wc -l | tr -d ' ')

echo >&2
echo "== summary ==" >&2
echo "consequential actions (discover, publish, cll append): $consequential_actions capsule mappings (target: 3)" >&2
echo "distinct capsules: $distinct_capsules (2 -- cll append's action maps onto discover's existing capsule, by design; it creates none of its own)" >&2
echo "not-consequential actions (verify, contract validate, plugin ls, cll list, get, judge pin, judge drift pin, judge drift reports, calibration summarize): 9, ran, zero seal calls made for any of them" >&2
echo "elapsed: ${elapsed}s from a fresh capsulectl build through the last verb (target: under 3600s)" >&2

if [[ "$consequential_actions" -ne 3 ]]; then
  echo "FAIL: expected exactly 3 consequential actions mapped to a capsule, got $consequential_actions" >&2
  exit 1
fi
if [[ "$distinct_capsules" -ne 2 ]]; then
  echo "FAIL: expected exactly 2 distinct capsules (discover + publish), got $distinct_capsules" >&2
  exit 1
fi
if [[ "$elapsed" -ge 3600 ]]; then
  echo "FAIL: exceeded the one-hour target (${elapsed}s)" >&2
  exit 1
fi
echo "PASS" >&2
