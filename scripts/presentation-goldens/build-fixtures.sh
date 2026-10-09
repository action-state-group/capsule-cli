#!/usr/bin/env bash
# build-fixtures.sh -- (re)build the presentation goldens' fixtures: six
# synthetic bundles and the pages capsulectl emits for them, written to
# internal/cli/testdata/presentation/. Run it once, when a fixture is added
# or changed on purpose; TestPresentationGoldens then holds every later
# version of the viewer to what these pages show.
#
#   scripts/presentation-goldens/build-fixtures.sh AAC_CHECKOUT
#
# AAC_CHECKOUT is an agent-action-capsule clone that has the commit go.mod
# pins (fixture 3 is built with its composed/v1 vector generator). Each page
# is emitted by the real CLI, so the page gate (internal/cli/page_gate.go)
# applies: a refused page is recorded with the refusal instead. Keys and
# capsule ids are fresh on every run, so a rebuild rewrites every fixture.
#
# Needs bash, jq, python3 (with `cryptography`) and git. Writes only the
# fixtures' directory and its own temp directory.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
here="$root/scripts/presentation-goldens"
out="$root/internal/cli/testdata/presentation"
aac="${1:?usage: build-fixtures.sh AAC_CHECKOUT}"
aac_commit=$(cd "$root" && go list -m -f '{{.Version}}' github.com/action-state-group/agent-action-capsule/go | sed 's/.*-//')
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

(cd "$root" && go build -o "$work/capsulectl" ./cmd/capsulectl)
capsulectl() { "$work/capsulectl" "$@"; }

# fixture DIR: a fresh directory for one fixture, with its own config.
fixture() {
  rm -rf "${out:?}/$1"
  mkdir -p "$out/$1" "$work/$1"
  cd "$work/$1"
  export XDG_CONFIG_HOME="$work/$1/config"
}

# sqlite_profile: one SQLite profile with a log; its bundles carry the
# sealed records themselves, so they verify VALID.
sqlite_profile() {
  local public
  public=$(capsulectl key generate --output ./seed | jq -r .public_key)
  capsulectl profile create --name example --type sqlite --sqlite-path ./store.db \
    --operator "Example Operator" --signing-key-file ./seed --trusted-key "$public" \
    --log-id example-log --checkpoint-signing-key-file ./seed --checkpoint-trusted-key "$public" >/dev/null
  capsulectl store init --profile example >/dev/null
}

publish() { capsulectl publish --profile example --request "$1" | jq -r .capsule_id; }

# meta DIR JSON: what the golden test needs to re-emit the page.
meta() { jq -S . <<<"$2" >"$out/$1/meta.json"; }

# verdict BUNDLE: what `verify --bundle` calls it.
verdict() {
  capsulectl verify --bundle "$1" >"$work/verify.json" 2>/dev/null || true
  jq -r .verdict "$work/verify.json"
}

# --- 1. rules comparison: report/v1 rows, under a neutral record type ------
fixture 1-rules-comparison
sqlite_profile
rule() { # row id, rule text, status
  jq -n --arg id "$1" --arg rule "$2" --arg status "$3" '
    {spec_version: "capsule-seal-request/v1",
     capsule: {ActionID: ("rule/" + $id), ActionType: "fyi", Operator: "example-operator",
               Developer: "example-rules-checker@1", Timestamp: "2026-10-07T00:00:00Z"},
     payload: {rule: $rule, before: "2026-09", after: "2026-10", result: $status}}' >"rule-$1.json"
  publish "rule-$1.json"
}
spend=$(rule spend-limit "Spend per order stays under the example limit" same)
approval=$(rule approval "Orders over the example threshold need a second approval" different)
vendor=$(rule vendor "Only vendors on the example list are paid" same)
jq -n --arg spend "$spend" --arg approval "$approval" --arg vendor "$vendor" '
  def cite($id): {type: "agent-action-capsule", digest_alg: "sha256", digest: $id, citation_purpose: "acted_on"};
  {spec_version: "capsule-seal-request/v1",
   capsule: {ActionID: "rules-compare", ActionType: "fyi", Operator: "example-operator",
             Developer: "example-rules-checker@1", Timestamp: "2026-10-07T00:01:00Z",
             References: [$spend, $approval, $vendor] | map({Type: "agent-action-capsule", DigestAlg: "sha256",
                                                             Digest: ., CitationPurpose: "acted_on"})},
   payload: {spec_version: "report/v1", type: "example.rules_compare/v0",
             title: "Example rules: September against October", rows: [
     {row_id: "spend-limit", label: "Spend limit per order", status: "same", references: [cite($spend)]},
     {row_id: "approval", label: "Second approval threshold", status: "different",
      reason: "the example threshold changed between the two months", references: [cite($approval)]},
     {row_id: "vendor", label: "Approved vendors", status: "same", references: [cite($vendor)]}]}}' >report-request.json
report=$(publish report-request.json)
capsulectl cll checkpoint create --profile example >/dev/null
capsulectl disclose --profile example --root "$report" --out bundle.json --html page.html >/dev/null
cp bundle.json page.html "$out/1-rules-comparison/"
meta 1-rules-comparison "$(jq -n --arg v "$(verdict bundle.json)" '{kind: "rules-comparison", emitted_by: "capsulectl disclose --root REPORT --out bundle.json --html page.html", rerender: "page", page: "written", verify: $v}')"

# --- 2. unilateral consumer receipt: x-deal-v0, audience keep -------------
fixture 2-unilateral-receipt
demo="$root/skills/deal/demo/retail-checkout"
export CAPSULE_DEAL_CHECK_URL=
capsulectl deal init --profile deal --dir ./deal --no-witness --materiality "$root/skills/deal/profile/materiality-predicate/neutral.json" >/dev/null
id=$(capsulectl --profile deal deal open --input "$demo/open.json" | jq -r .deal_id)
capsulectl --profile deal deal check --deal "$id" --input "$demo/check-pay.json" >/dev/null
capsulectl --profile deal deal note --deal "$id" --kind act --input "$demo/act-pay.json" >/dev/null
capsulectl --profile deal deal report --deal "$id" --html page.html --bundle bundle.json >/dev/null
cp bundle.json page.html "$out/2-unilateral-receipt/"
meta 2-unilateral-receipt "$(jq -n --arg v "$(verdict bundle.json)" '{kind: "unilateral-receipt", emitted_by: "capsulectl deal report --deal ID --html page.html --bundle bundle.json (share keep)", rerender: "deal-report", page: "written", verify: $v}')"

# --- 3. bilateral receipt: composed/v1 joined, two members, one mismatch ---
fixture 3-bilateral-composed
git -C "$aac" show "${aac_commit}:python/scripts/generate_bundle_composed_vectors.py" >generator.py
python3 "$here/composed_mismatch.py" generator.py >bundle.json
cp bundle.json "$out/3-bilateral-composed/"
meta 3-bilateral-composed "$(jq -n --arg v "$(verdict bundle.json)" --arg c "$aac_commit" '{kind: "bilateral-composed", emitted_by: ("composed_mismatch.py over agent-action-capsule " + $c + " python/scripts/generate_bundle_composed_vectors.py"), rerender: "page", page: "refused", verify: $v,
  refused_because: "no capsulectl command renders a bundle file to a page; the page gate refuses this bundle (verify --bundle: INCOMPLETE, no checkpoint), as it does every published composed/v1 vector; the vendored viewer has no composed/v1 view"}')"

# --- 4 and 5. monthly outcome and compliance reports: Result v0 roots ------
# A Result v0 sealed as the payload of a SQLite record: `result build` seals
# only into a jsonl book, whose bundles leave producer signatures out, and
# the page gate refuses those (INCOMPLETE).
month() { # fixture dir, kind, card
  fixture "$1"
  sqlite_profile
  python3 "$here/month.py" requests "$2" requests
  : >ids.jsonl
  while IFS=$'\t' read -r key file; do
    jq -n --arg k "$key" --arg v "$(publish "requests/$file")" '{($k): $v}' >>ids.jsonl
  done < <(jq -r '.[] | [.key, .file] | @tsv' requests/manifest.json)
  jq -s 'add' ids.jsonl >ids.json
  python3 "$here/month.py" result "$2" ids.json >result-request.json
  root_id=$(publish result-request.json)
  capsulectl cll checkpoint create --profile example >/dev/null
  capsulectl disclose --profile example --root "$root_id" --out disclosed.json >/dev/null
  jq --argjson ext "$(python3 "$here/month.py" extension "$2")" '.extensions = ((.extensions // {}) + $ext)' disclosed.json >bundle.json
  capsulectl report build --bundle bundle.json --card "$3" --out page.html >/dev/null
  cp bundle.json page.html "$out/$1/"
  meta "$1" "$(jq -n --arg k "$2" --arg card "$3" --arg v "$(verdict bundle.json)" '{kind: ("monthly-" + $k), emitted_by: ("capsulectl report build --bundle bundle.json --card " + $card + " --out page.html"), rerender: "report-build", card: $card, page: "written", verify: $v}')"
}
month 4-monthly-outcome outcome outcome
month 5-monthly-compliance compliance obligation

# --- 6. a generic bundle, and the same bundle with an unknown extension ----
fixture 6-generic-fallback
sqlite_profile
cat >note.json <<'EOF'
{"spec_version": "capsule-seal-request/v1",
 "capsule": {"ActionID": "example-note", "ActionType": "fyi", "Operator": "example-operator",
             "Developer": "example-developer", "Timestamp": "2026-10-07T00:00:00Z"},
 "payload": {"note": "an example record with no report and no aggregate"}}
EOF
note=$(publish note.json)
capsulectl cll checkpoint create --profile example >/dev/null
capsulectl disclose --profile example --root "$note" --out bundle.json --html page.html >/dev/null
cp bundle.json page.html "$out/6-generic-fallback/"
meta 6-generic-fallback "$(jq -n --arg v "$(verdict bundle.json)" '{kind: "generic-fallback", emitted_by: "capsulectl disclose --root NOTE --out bundle.json --html page.html", rerender: "page", page: "written", verify: $v}')"

mkdir -p "$out/6-generic-fallback-unknown-extension"
jq '.extensions = ((.extensions // {}) + {"x-example-unknown/v1": {"anything": "a block no reader knows"}})' bundle.json >"$out/6-generic-fallback-unknown-extension/bundle.json"
meta 6-generic-fallback-unknown-extension "$(jq -n --arg v "$(verdict "$out/6-generic-fallback-unknown-extension/bundle.json")" '{kind: "generic-fallback-unknown-extension", emitted_by: "fixture 6 bundle + extensions[\"x-example-unknown/v1\"]; no capsulectl command renders a bundle file, so its page is emitted by the gate and renderer disclose --html uses (see the golden test)", rerender: "page", page: "written", verify: $v}')"

printf 'fixtures written to %s (agent-action-capsule %s)\n' "$out" "$aac_commit"
