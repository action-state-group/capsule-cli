#!/usr/bin/env bash
set -euo pipefail

# Exercises every verb in ../spec.yaml against a throwaway jsonl profile in a
# temp directory: one capsule sealed, chained (via Chain.ParentCapsuleID) to
# the previous action, and independently verified, per action. This is the
# proof-of-the-invariant DONE requires ("every skill-driven action emits a
# capsule ... test with a scripted run"), and its total elapsed time --
# printed at the end -- doubles as the item's fresh-environment timing check
# (Do item 5: install through the first discover/contract-validate call,
# target under an hour). It never touches a real profile, a real CLL, or any
# path outside its own temp directory.

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

capsules_file="$work/capsules.txt"
: >"$capsules_file"
parent=""

# seal_step wraps a "skill-wraps" verb's own JSON result in a
# capsule-seal-request/v1, chains it to $parent when one is set, seals it
# with the base `seal` command (no store needed), verifies the result, and
# records the new capsule_id as the parent for the next step.
seal_step() {
  local action_id="$1" payload="$2"
  local req="$work/req.json" ts
  ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  jq -n --arg id "$action_id" --arg op "$profile" --arg dev "capsulectl-skill/v0" \
    --arg ts "$ts" --arg parent "$parent" --slurpfile payload "$payload" '
    {
      spec_version: "capsule-seal-request/v1",
      capsule: ({ActionID:$id, ActionType:"fyi", Operator:$op, Developer:$dev, Timestamp:$ts}
                + (if $parent == "" then {} else {Chain:{ParentCapsuleID:$parent, Relation:"io.capsulectl.skill_step"}} end)),
      payload: $payload[0]
    }' >"$req"
  local out="$work/out-$(echo "$action_id" | tr '/ ' '__').json"
  "$bin" seal --profile "$profile" --request "$req" --output "$out" >"$work/seal-result.json"
  local capsule_id
  capsule_id=$(jq -r .capsule_id "$work/seal-result.json")
  "$bin" verify --profile "$profile" --capsule "$out" >"$work/verify-result.json"
  jq -e '.producer_signature_and_trust=="passed" and .capsule_identity=="passed"' "$work/verify-result.json" >/dev/null
  echo "$capsule_id" >>"$capsules_file"
  parent="$capsule_id"
  echo "  sealed + verified: $action_id -> $capsule_id" >&2
}

echo "== 1/8 discover (self-sealing) ==" >&2
scan_root="$work/scan-root"
mkdir -p "$scan_root"
cat >"$scan_root/otel-config.yaml" <<'EOF'
exporters:
  otlp:
    endpoint: "https://example.invalid:4317"
EOF
scope="$work/scope.yaml"
printf 'roots:\n  - %q\n' "$scan_root" >"$scope"
discover_out="$work/discover-scan.json"
"$bin" discover --profile "$profile" --scope "$scope" --format json --seal-output "$discover_out" >"$work/discover-stdout.json"
discover_capsule=$(jq -r .sealed.capsule_id "$work/discover-stdout.json")
"$bin" verify --profile "$profile" --capsule "$discover_out" >"$work/discover-verify.json"
jq -e '.producer_signature_and_trust=="passed" and .capsule_identity=="passed"' "$work/discover-verify.json" >/dev/null
echo "$discover_capsule" >>"$capsules_file"
parent="$discover_capsule"
echo "  self-sealed + verified: discover -> $discover_capsule" >&2

echo "== 2/8 publish (primary action) ==" >&2
ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)
jq -n --arg op "$profile" --arg ts "$ts" --arg parent "$parent" '
  {spec_version:"capsule-seal-request/v1",
   capsule:({ActionID:"skill-demo/publish-1", ActionType:"fyi", Operator:$op, Developer:"capsulectl-skill/v0", Timestamp:$ts,
             Chain:{ParentCapsuleID:$parent, Relation:"io.capsulectl.skill_step"}}),
   payload:{note:"scripted demo publish"}}' >"$work/publish-request.json"
"$bin" publish --profile "$profile" --request "$work/publish-request.json" >"$work/publish-result.json"
published_id=$(jq -r .capsule_id "$work/publish-result.json")
echo "$published_id" >>"$capsules_file"
parent="$published_id"
echo "  published: publish -> $published_id" >&2

echo "== 3/8 get (skill-wraps) ==" >&2
"$bin" get --profile "$profile" --capsule-id "$published_id" >"$work/get-result.json"
seal_step "skill-demo/get-1" "$work/get-result.json"

echo "== 4/8 verify (skill-wraps; verify the raw record just fetched) ==" >&2
"$bin" get --profile "$profile" --capsule-id "$published_id" --raw --output "$work/published-raw.json" >"$work/get-raw-result.json"
"$bin" verify --profile "$profile" --capsule "$work/published-raw.json" >"$work/verify-of-published.json"
seal_step "skill-demo/verify-1" "$work/verify-of-published.json"

echo "== 5/8 cll list (skill-wraps) ==" >&2
"$bin" cll list --profile "$profile" >"$work/cll-list-result.json"
seal_step "skill-demo/cll-list-1" "$work/cll-list-result.json"

echo "== 6/8 contract validate (skill-wraps) ==" >&2
schema="$work/demo-schema.json"
doc="$work/demo-doc.json"
cat >"$schema" <<'EOF'
{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","required":["hello"],"properties":{"hello":{"type":"string"}}}
EOF
cat >"$doc" <<'EOF'
{"hello":"world"}
EOF
"$bin" contract validate "$doc" --schema "$schema" --json >"$work/contract-result.json"
seal_step "skill-demo/contract-validate-1" "$work/contract-result.json"

echo "== 7/8 plugin ls (skill-wraps) ==" >&2
"$bin" plugin ls >"$work/plugin-ls-result.json"
seal_step "skill-demo/plugin-ls-1" "$work/plugin-ls-result.json"

echo "== 8/8 cll append (primary action; append an already-sealed capsule) ==" >&2
"$bin" cll append --profile "$profile" --capsule "$discover_out" >"$work/cll-append-result.json"
appended_seq=$(jq -r .sequence "$work/cll-append-result.json")
# append's own job is to persist an EXISTING capsule, not create a new one --
# its "record" is the discover capsule itself, so this action maps onto the
# same capsule_id already logged for step 1/8, not a fresh one.
echo "$discover_capsule" >>"$capsules_file"
echo "  appended discover scan (capsule $discover_capsule) at CLL sequence $appended_seq" >&2

end_epoch=$(date +%s)
elapsed=$((end_epoch - start_epoch))
actions=$(wc -l <"$capsules_file" | tr -d ' ')
distinct=$(sort -u "$capsules_file" | wc -l | tr -d ' ')

echo >&2
echo "== summary ==" >&2
echo "actions run: $actions (target: one capsule association per documented action = 8)" >&2
echo "distinct capsules: $distinct (7 -- cll append's action maps onto discover's existing capsule, by design; it creates none of its own)" >&2
echo "elapsed: ${elapsed}s from a fresh capsulectl build through the last verb (target: under 3600s)" >&2

if [[ "$actions" -lt 8 ]]; then
  echo "FAIL: expected 8 actions each mapped to a capsule, got $actions" >&2
  exit 1
fi
if [[ "$elapsed" -ge 3600 ]]; then
  echo "FAIL: exceeded the one-hour target (${elapsed}s)" >&2
  exit 1
fi
echo "PASS" >&2
