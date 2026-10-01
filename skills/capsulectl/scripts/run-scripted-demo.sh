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
#   - the book verbs (request, respond, close) each return the id of the
#     signed book record that is their evidence, and reconcile adds no
#     record; the counterparty's Close verifies with `verify` and its
#     bundle with the neutral AAC bundle verifier;
#   - a failed evidence record fails closed (this skill's evidence policy:
#     "report evidence unavailable, stop before the next consequential
#     action") rather than silently continuing.
#
# A contributor check: it builds capsulectl from this checkout, so it needs
# Go and the repository. It is not an install test; a user installs a
# release binary and the skill from the same tag (README, "Agent skill").
# It never touches a real profile, a real CLL, or any path outside its own
# temp directory.

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
checkpoint_seed="$work/checkpoint-signing.hex"
"$bin" key generate --output "$checkpoint_seed" >"$work/checkpoint-keygen.json"
checkpoint_public_key=$(jq -r .public_key "$work/checkpoint-keygen.json")
"$bin" profile create --name "$profile" --type jsonl --jsonl-path "$store" \
  --namespace demo --log-id skill-demo --operator demo-operator \
  --trusted-key "$public_key" --signing-key-file "$seed" \
  --checkpoint-signing-key-file "$checkpoint_seed" --checkpoint-trusted-key "$checkpoint_public_key" >/dev/null
"$bin" store init --profile "$profile" >/dev/null
# This profile's one log is its evidence book. Yesterday's exchanges go in
# first, before anything today: `close` refuses a period that has not ended,
# and a log whose commit times go backwards cannot be closed at all.
# bookdemo is a fixture helper, not part of capsulectl.
bookdemo="$work/bookdemo"
(cd "$repo_root" && go build -o "$bookdemo" ./skills/capsulectl/scripts/bookdemo)
yesterday=$(date -u -v-1d +%Y-%m-%d 2>/dev/null || date -u -d yesterday +%Y-%m-%d)
"$bookdemo" seed --store "$store" --log-id skill-demo --operator demo-operator \
  --signing-key-file "$seed" --checkpoint-key-file "$checkpoint_seed" --at "${yesterday}T12:00:00Z" \
  booking-1:req-1:confirmed booking-2:req-2:refunded

scan_root="$work/scan-root"
mkdir -p "$scan_root"
cat >"$scan_root/otel-config.yaml" <<'EOF'
exporters:
  otlp:
    endpoint: "https://example.invalid:4317"
EOF
scope="$work/scope.yaml"
printf 'roots:\n  - %q\n' "$scan_root" >"$scope"

echo "== 0/15 evidence-failure-fails-closed check (not a numbered action) ==" >&2
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

echo "== 1/15 discover (self-sealing; consequential) ==" >&2
discover_out="$work/discover-scan.json"
"$bin" discover --profile "$profile" --scope "$scope" --format json --seal-output "$discover_out" >"$work/discover-stdout.json"
discover_capsule=$(jq -r .sealed.capsule_id "$work/discover-stdout.json")
"$bin" verify --profile "$profile" --capsule "$discover_out" >"$work/discover-verify.json"
jq -e '.producer_signature_and_trust=="passed" and .capsule_identity=="passed"' "$work/discover-verify.json" >/dev/null
echo "$discover_capsule" >>"$capsules_file"
parent="$discover_capsule"
echo "  self-sealed + verified: discover -> $discover_capsule" >&2

echo "== 2/15 publish (primary action; consequential) ==" >&2
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

echo "== 3/15 get (not-consequential; no capsule) ==" >&2
"$bin" get --profile "$profile" --capsule-id "$published_id" >"$work/get-result.json"
echo "  ran, no seal call made — get is local inspection per the evidence policy" >&2

echo "== 4/15 verify (not-consequential; no capsule) ==" >&2
"$bin" get --profile "$profile" --capsule-id "$published_id" --raw --output "$work/published-raw.json" >"$work/get-raw-result.json"
"$bin" verify --profile "$profile" --capsule "$work/published-raw.json" >"$work/verify-of-published.json"
jq -e '.producer_signature_and_trust=="passed" and .capsule_identity=="passed"' "$work/verify-of-published.json" >/dev/null
echo "  ran + passed, no seal call made — checking a bundle is local validation per the evidence policy" >&2

echo "== 5/15 cll list (not-consequential; no capsule) ==" >&2
"$bin" cll list --profile "$profile" >"$work/cll-list-result.json"
jq -e --arg id "$published_id" 'any(.entries[]; .record_type=="published_capsule" and .capsule_id==$id and .capsule_carried)' \
  "$work/cll-list-result.json" >/dev/null || {
  echo "FAIL: the capsule publish committed is not in the log cll list reads" >&2; exit 1; }
[[ ! -e "$store/cll.jsonl" ]] || { echo "FAIL: a jsonl profile must have one log, but cll.jsonl exists" >&2; exit 1; }
echo "  ran, no seal call made — the published capsule is in the profile's one log (its evidence book)" >&2

echo "== 6/15 contract validate (not-consequential; no capsule) ==" >&2
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

echo "== 7/15 plugin ls (not-consequential; no capsule) ==" >&2
"$bin" plugin ls >"$work/plugin-ls-result.json"
echo "  ran, no seal call made — listing discovered plugins is local inspection per the evidence policy" >&2

echo "== 8/15 cll append (primary action; consequential) ==" >&2
"$bin" cll append --profile "$profile" --capsule "$discover_out" >"$work/cll-append-result.json"
appended_seq=$(jq -r .sequence "$work/cll-append-result.json")
# append's own job is to persist an EXISTING capsule, not create a new one --
# its "record" is the discover capsule itself, so this action maps onto the
# same capsule_id already logged for step 1/15, not a fresh one.
echo "$discover_capsule" >>"$capsules_file"
echo "  appended discover scan (capsule $discover_capsule) at CLL sequence $appended_seq" >&2

# Steps 9-11 exercise [capsulectl-book-verbs-v0]'s no-book-required half:
# the deterministic remainder of the retired capsule-judge harness. None
# needs --profile, a store, or a signing key -- pure computation over
# caller-supplied files, same as contract validate.

echo "== 9/15 judge pin (not-consequential; no capsule) ==" >&2
pin_input="$work/judge-pin-input.json"
cat >"$pin_input" <<'EOF'
{"model_id":"gpt-eval/1.0","prompt_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","axes_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
EOF
"$bin" judge pin "$pin_input" >"$work/judge-pin-result.json"
pin_digest=$(jq -r .judge_pin_digest "$work/judge-pin-result.json")
[[ "$pin_digest" =~ ^[0-9a-f]{64}$ ]] || { echo "FAIL: judge pin did not print a 64-hex-char digest" >&2; exit 1; }
echo "  ran, no seal call made — computing a pin is local computation per the evidence policy" >&2

echo "== 10/15 judge drift (not-consequential; no capsule) ==" >&2
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

echo "== 11/15 calibration summarize (not-consequential; no capsule) ==" >&2
ratings="$work/ratings.json"
cat >"$ratings" <<'EOF'
[{"case_id":"case-1","agrees_with_judge":true}]
EOF
"$bin" calibration summarize "$reports_a" "$ratings" >"$work/calibration-result.json"
jq -e '.pins[0].judge_pin_digest=="pin-a" and .pins[0].rated_count==1 and .pins[0].agreement_count==1 and .pins[0].agreement_rate_micros==1000000' \
  "$work/calibration-result.json" >/dev/null || {
  echo "FAIL: calibration summarize did not fold the human rating into the expected k-of-n" >&2; exit 1; }
echo "  ran, no seal call made — folding stats is local computation per the evidence policy" >&2

# Steps 12-15 run the book verbs over two local books: this profile's and a
# second profile standing in for the counterparty. `close` refuses a period
# that has not ended, so bookdemo (a fixture helper, not part of capsulectl)
# seeds both books with yesterday's exchanges first.
peer=demo-peer
peer_seed="$work/peer-signing.hex"
peer_checkpoint_seed="$work/peer-checkpoint-signing.hex"
"$bin" key generate --output "$peer_seed" >"$work/peer-keygen.json"
"$bin" key generate --output "$peer_checkpoint_seed" >"$work/peer-checkpoint-keygen.json"
peer_public_key=$(jq -r .public_key "$work/peer-keygen.json")
peer_checkpoint_key=$(jq -r .public_key "$work/peer-checkpoint-keygen.json")
peer_store="$work/peer-store"
"$bin" profile create --name "$peer" --type jsonl --jsonl-path "$peer_store" \
  --namespace demo --log-id skill-demo-peer --operator demo-peer-operator \
  --trusted-key "$peer_public_key" --signing-key-file "$peer_seed" \
  --checkpoint-signing-key-file "$peer_checkpoint_seed" --checkpoint-trusted-key "$peer_checkpoint_key" >/dev/null
"$bookdemo" seed --store "$peer_store" --log-id skill-demo-peer --operator demo-peer-operator \
  --signing-key-file "$peer_seed" --checkpoint-key-file "$peer_checkpoint_seed" --at "${yesterday}T12:00:00Z" \
  booking-1:req-1:confirmed booking-2:req-2:cancelled

book_records_file="$work/book-records.txt"
: >"$book_records_file"
record_of() { # FILE: log the record_id a consequential book verb returned, or fail closed
  local id
  id=$(jq -r .record_id "$1")
  [[ "$id" =~ ^[0-9a-f]{64}$ ]] || { echo "FAIL: evidence unavailable -- no record_id in $1" >&2; exit 1; }
  echo "$id" >>"$book_records_file"
}

echo "== 12/15 request + respond (each a signed book record; consequential) ==" >&2
cat >"$work/evidence-request.json" <<'EOF'
{"subject":{"kind":"full_history"},"coverage":{"min_freshness":{"size":1}}}
EOF
"$bin" request --profile "$profile" --request "$work/evidence-request.json" --responder "$peer" \
  --output "$work/request.sent" >"$work/request-result.json"
record_of "$work/request-result.json"
"$bin" respond --profile "$peer" --request "$work/request.sent" --requester "$profile" \
  --output "$work/response.json" >"$work/respond-result.json"
record_of "$work/respond-result.json"
jq -e '.outcome=="artifact"' "$work/respond-result.json" >/dev/null || {
  echo "FAIL: an identified requester asking for full history under the default policy should get an artifact" >&2; exit 1; }
"$bin" request --profile "$profile" --for "$(jq -r .record_id "$work/request-result.json")" \
  --response "$work/response.json" --responder-key "$peer_public_key" \
  --responder-checkpoint-key "$peer_checkpoint_key" >"$work/response-recorded.json"
record_of "$work/response-recorded.json"
jq -e '.outcome=="artifact"' "$work/response-recorded.json" >/dev/null || {
  echo "FAIL: the requester did not record the verified artifact" >&2; exit 1; }
echo "  asked, answered, and recorded the answer: three book records" >&2

echo "== 13/15 the counterparty closes first, no peer bundle (primary action; signs) ==" >&2
# A book's counterparty is named by its book id (the other profile's
# log_id). The peer holds only its own account, so every exchange reads
# INSUFFICIENT; its bundle is what it hands over.
"$bin" close --profile "$peer" --period day --date "$yesterday" --counterparty skill-demo \
  --capsule-out "$work/peer-close.json" --bundle-out "$work/peer-close-bundle.json" >"$work/peer-close-result.json"
record_of "$work/peer-close-result.json"
jq -e '.reconciliation.states.INSUFFICIENT==2 and .reconciliation.states.CONFLICTING==0 and .reconciliation.peer_complete==false' \
  "$work/peer-close-result.json" >/dev/null || {
  echo "FAIL: with no peer bundle each exchange must read INSUFFICIENT, never CONFLICTING" >&2; exit 1; }
"$bin" verify --profile "$peer" --capsule "$work/peer-close.json" >"$work/peer-close-verify.json"
"$bookdemo" aac-verify "$work/peer-close-bundle.json" >"$work/peer-close-bundle-verify.json"
"$bin" close --profile "$peer" --period day --date "$yesterday" --counterparty skill-demo >"$work/peer-close-again.json"
jq -e --arg id "$(jq -r .record_id "$work/peer-close-result.json")" '.already_closed==true and .record_id==$id' \
  "$work/peer-close-again.json" >/dev/null || {
  echo "FAIL: a second close of the same period and counterparty must return the first, not seal another" >&2; exit 1; }
echo "  sealed; verify accepts the Close; the AAC bundle verifier passes its bundle; a repeat seals no second Close" >&2

echo "== 14/15 close against the counterparty's held bundle (primary action; signs) ==" >&2
"$bin" close --profile "$profile" --period day --date "$yesterday" --counterparty skill-demo-peer \
  --peer "$work/peer-close-bundle.json" --peer-checkpoint-key "$peer_checkpoint_key" >"$work/close-peer-result.json"
record_of "$work/close-peer-result.json"
jq -e '.reconciliation.peer_complete==true and .reconciliation.states.MATCHED==1 and .reconciliation.states.CONFLICTING==1' \
  "$work/close-peer-result.json" >/dev/null || {
  echo "FAIL: one matching and one differing exchange should reconcile MATCHED + CONFLICTING against a complete peer account" >&2; exit 1; }
echo "  one exchange MATCHED, one CONFLICTING, from the held bundle alone" >&2

echo "== 15/15 reconcile (not-consequential; adds no record) ==" >&2
"$bin" reconcile --profile "$profile" --period day --date "$yesterday" --counterparty skill-demo-peer \
  --peer "$work/peer-close-bundle.json" --peer-checkpoint-key "$peer_checkpoint_key" >"$work/reconcile-result.json"
jq -e --slurpfile closed "$work/close-peer-result.json" '.reconciliation.states==$closed[0].reconciliation.states' \
  "$work/reconcile-result.json" >/dev/null || {
  echo "FAIL: reconcile after the Close must compute what the Close sealed" >&2; exit 1; }
for wrong in "--peer-checkpoint-key $checkpoint_public_key --counterparty skill-demo-peer" \
  "--peer-checkpoint-key $peer_checkpoint_key --counterparty someone-else"; do
  set +e
  # shellcheck disable=SC2086 # $wrong is two flag pairs by design
  "$bin" reconcile --profile "$profile" --period day --date "$yesterday" \
    --peer "$work/peer-close-bundle.json" $wrong >/dev/null 2>&1
  wrong_status=$?
  set -e
  [[ "$wrong_status" -eq 2 ]] || {
    echo "FAIL: a peer bundle under another key, or from another book, must be refused (exit 2), got $wrong_status" >&2; exit 1; }
done
echo "  same states as the sealed Close; a bundle under the wrong key or from another book is refused" >&2

end_epoch=$(date +%s)
elapsed=$((end_epoch - start_epoch))
consequential_actions=$(wc -l <"$capsules_file" | tr -d ' ')
distinct_capsules=$(sort -u "$capsules_file" | wc -l | tr -d ' ')

echo >&2
echo "== summary ==" >&2
echo "consequential actions (discover, publish, cll append): $consequential_actions capsule mappings (target: 3)" >&2
echo "distinct capsules: $distinct_capsules (2 -- cll append's action maps onto discover's existing capsule, by design; it creates none of its own)" >&2
echo "not-consequential actions (verify, contract validate, plugin ls, cll list, get, judge pin, judge drift pin, judge drift reports, calibration summarize): 9, ran, zero seal calls made for any of them" >&2
book_records=$(wc -l <"$book_records_file" | tr -d ' ')
echo "book records from consequential book verbs (request x2, respond, close x2): $book_records (target: 5)" >&2
echo "not-consequential book verb (reconcile): ran, added no record" >&2
echo "elapsed: ${elapsed}s from a fresh capsulectl build through the last verb (target: under 3600s)" >&2

if [[ "$consequential_actions" -ne 3 ]]; then
  echo "FAIL: expected exactly 3 consequential actions mapped to a capsule, got $consequential_actions" >&2
  exit 1
fi
if [[ "$distinct_capsules" -ne 2 ]]; then
  echo "FAIL: expected exactly 2 distinct capsules (discover + publish), got $distinct_capsules" >&2
  exit 1
fi
if [[ "$book_records" -ne 5 ]]; then
  echo "FAIL: expected exactly 5 book records from consequential book verbs, got $book_records" >&2
  exit 1
fi
if [[ "$elapsed" -ge 3600 ]]; then
  echo "FAIL: exceeded the one-hour target (${elapsed}s)" >&2
  exit 1
fi
echo "PASS" >&2
