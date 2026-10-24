#!/usr/bin/env bash
# build-fixtures.sh -- (re)build the presentation goldens' fixtures: the
# synthetic bundles and the pages capsulectl emits for them, written to
# cli/testdata/presentation/. Run it once, when a fixture is added
# or changed on purpose; TestPresentationGoldens then holds every later
# version of the viewer to what these pages show.
#
#   scripts/presentation-goldens/build-fixtures.sh
#
# Each page is emitted by the real CLI, so the page gate
# (cli/page_gate.go) applies, and every fixture's page must pass it.
# Keys and capsule ids are fresh on every run, so a rebuild rewrites every
# fixture.
#
# Needs bash, go, jq, perl and python3. Writes only the fixtures' directory and its own
# temp directory.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
here="$root/scripts/presentation-goldens"
out="$root/cli/testdata/presentation"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

(cd "$root" && go build -o "$work/capsulectl" ./cmd/capsulectl)
# The deal and book commands are the capsulectl-deal and capsulectl-book
# plugins', found on a plugin root of this run's own.
mkdir -p "$work/plugins"
(cd "$root/plugins" && go build -o "$work/plugins/capsulectl-deal" ./cmd/capsulectl-deal)
(cd "$root/plugins" && go build -o "$work/plugins/capsulectl-book" ./cmd/capsulectl-book)
export CAPSULECTL_PLUGIN_ROOTS="$work/plugins"
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

# represent DEAL FILE [WORDS]: a seller's represent check of FILE; with WORDS,
# the user's approval of it in those words, on the card it showed. Prints the
# check id a claim is then noted against.
represent() {
  local out
  out=$(capsulectl --profile deal deal check --deal "$1" --input "$2")
  if [[ -n "${3:-}" ]]; then
    jq -r .card <<<"$out" >card.txt
    capsulectl --profile deal deal note --deal "$1" --kind approval --check "$(jq -r .check_id <<<"$out")" \
      --choice proceed --said "$3" --shown-card card.txt >/dev/null
  fi
  jq -r .check_id <<<"$out"
}

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
  def cite($id): {type: "agent-action-capsule", digest_alg: "SHA-256", digest: $id, citation_purpose: "acted_on"};
  {spec_version: "capsule-seal-request/v1",
   capsule: {ActionID: "rules-compare", ActionType: "fyi", Operator: "example-operator",
             Developer: "example-rules-checker@1", Timestamp: "2026-10-07T00:01:00Z",
             References: [$spend, $approval, $vendor] | map({Type: "agent-action-capsule", DigestAlg: "SHA-256",
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

# --- 7. a receipt whose rules checker declares rules it does not measure ----
# The same synthetic retail checkout, with a pinned stub rules checker that
# allows the purchase and reports three rules its ruleset declares and it
# does not measure (reason "declared, not measured"): the receipt says them
# quietly beside the passing check, never as a warning. Synthetic ids only.
fixture 7-receipt-declared-rules
demo="$root/skills/deal/demo/retail-checkout"
export CAPSULE_DEAL_CHECK_URL=
plugins="$work/7-receipt-declared-rules/plugins"
mkdir -p "$plugins"
cat >"$plugins/example-rules" <<'STUB'
#!/bin/sh
cat >/dev/null
printf '%s' '{"schema":"external-check-result/v0","ruleset_id":"example-rules/1.0.0","definition_digest":"abababababababababababababababababababababababababababababababab","verdict":"allow","tier":"recomputed","findings":[{"id":"r05-per-purchase","check":"caps/1","verdict":"pass","limit":2500,"value":627},{"id":"r14-commits-the-user","verdict":"not_evaluable","reason":"declared, not measured"},{"id":"r24-outside-instructions","verdict":"not_evaluable","reason":"declared, not measured"},{"id":"r25-no-bypassing","verdict":"not_evaluable","reason":"declared, not measured"}]}'
STUB
chmod 0755 "$plugins/example-rules"
export CAPSULECTL_PLUGIN_ROOTS="$plugins:$work/plugins"
capsulectl deal init --profile deal --dir ./deal --no-witness --materiality "$root/skills/deal/profile/materiality-predicate/neutral.json" >/dev/null
jq -n --arg c "$plugins/example-rules" '{command: [$c]}' >pin.json
capsulectl --profile deal profile update --rules-checker pin.json >/dev/null
id=$(capsulectl --profile deal deal open --input "$demo/open.json" | jq -r .deal_id)
capsulectl --profile deal deal check --deal "$id" --input "$demo/check-pay.json" >/dev/null
capsulectl --profile deal deal note --deal "$id" --kind act --input "$demo/act-pay.json" >/dev/null
capsulectl --profile deal deal report --deal "$id" --html page.html --bundle bundle.json >/dev/null
cp bundle.json page.html "$out/7-receipt-declared-rules/"
meta 7-receipt-declared-rules "$(jq -n --arg v "$(verdict bundle.json)" '{kind: "receipt-declared-rules", emitted_by: "capsulectl deal report --deal ID --html page.html --bundle bundle.json (share keep), with a pinned stub rules checker", rerender: "deal-report", page: "written", verify: $v}')"
export CAPSULECTL_PLUGIN_ROOTS="$work/plugins"

# --- 8. a seller's deal, the buyer's copy: what the seller's agent said ----
# The user sells an example bicycle and asks the agent to tell the buyer about
# the scratch. Their agent states the item's condition (ordered: checked, DO)
# and the refund terms (approved in the user's words), each a claim with a
# class, offers, records the buyer's acceptance of that exact offer, and
# commits. The buyer's copy opens those statements, labelled, and none of the
# user's own. Synthetic.
fixture 8-seller-buyer-copy
export CAPSULE_DEAL_CHECK_URL=
capsulectl deal init --profile deal --dir ./deal --no-witness --materiality "$root/skills/deal/profile/materiality-predicate/neutral.json" >/dev/null
cat >open.json <<'JSON'
{"type": "purchase", "demo": true, "channel": "marketplace",
 "intent": {"verbatim": "Sell my example bicycle; ask 1900. Tell them about the scratch.", "party_role": "seller",
            "asked": {"item": "example bicycle", "quantity": 1}, "allowed": ["offer", "commit"],
            "ordered_representations": ["condition"]},
 "who": {"name": "Example Buyer", "domain": "buyer.example"},
 "terms": {"item": "example bicycle", "quantity": 1, "price_minor": 190000, "currency": "USD"},
 "recourse": {"rail": "card", "refundable": false}}
JSON
id=$(capsulectl --profile deal deal open --records typed --input open.json | jq -r .deal_id)
note() { capsulectl --profile deal deal note --deal "$id" "$@" >/dev/null; }
echo '{"text": "The frame has one small scratch by the seat", "source_kind": "agent", "class": "condition"}' >claim-1.json
echo '{"text": "Returns accepted within 7 days if unridden", "source_kind": "agent", "class": "refund_terms"}' >claim-2.json
echo '{"text": "I bought it new two years ago", "source_kind": "user"}' >claim-3.json
jq '{action: "represent", representing: {class, text}}' claim-1.json >represent-1.json
jq '{action: "represent", representing: {class, text}}' claim-2.json >represent-2.json
note --kind claim --check "$(represent "$id" represent-1.json)" --input claim-1.json
note --kind claim --check "$(represent "$id" represent-2.json "yes, offer the 7-day returns")" --input claim-2.json
note --kind claim --input claim-3.json
echo '{"action": "offer", "amount_minor": 190000, "terms": {"item": "example bicycle", "quantity": 1, "price_minor": 190000}}' >offer.json
offer=$(capsulectl --profile deal deal check --deal "$id" --input offer.json | jq -r .check_id)
echo '{"action": "offer", "amount_minor": 190000}' >act-offer.json
note --kind act --input act-offer.json
note --kind acceptance --check "$offer" --words "Deal, 1900 works for me" --channel marketplace
echo '{"action": "commit", "amount_minor": 190000, "terms": {"item": "example bicycle", "quantity": 1, "price_minor": 190000}}' >commit.json
capsulectl --profile deal deal check --deal "$id" --input commit.json >/dev/null
echo '{"action": "commit", "amount_minor": 190000}' >act-commit.json
note --kind act --input act-commit.json
capsulectl --profile deal deal report --deal "$id" --share counterparty --to "the buyer" --html page.html >/dev/null
# A shared copy writes only its page; the bundle is the one the page carries.
perl -ne 'print "$1\n" if /^\s*<script>window\.__BUNDLE__ = (.*);<\/script>$/' page.html | jq . >bundle.json
cp bundle.json page.html "$out/8-seller-buyer-copy/"
meta 8-seller-buyer-copy "$(jq -n --arg v "$(verdict bundle.json)" '{kind: "seller-buyer-copy", emitted_by: "capsulectl deal report --deal ID --share counterparty --to \"the buyer\" --html page.html (bundle.json is the bundle the page carries)", rerender: "deal-report", page: "written", verify: $v}')"

# --- 9. a seller's Receipt with later evidence under the close -------------
# Seller PRD §5.4 and exit criterion 9, from the deal plugin's fixture
# (plugins/internal/plugin/testdata/deal/seller-later-evidence). The user
# sells the example bicycle; the agent states its condition (ordered) and its
# refund terms (approved in the user's words), offers, the buyer accepts, the
# agent commits, and the delivery is due. The sale closes with that due date
# carried. Later the carrier's scan
# (checked) resolves it, and the buyer's refund request (a claim, so
# unverified) is about the refund terms; both chain `confirms` to the close.
fixture 9-seller-later-evidence
export CAPSULE_DEAL_CHECK_URL=
src="$root/plugins/internal/plugin/testdata/deal/seller-later-evidence"
capsulectl deal init --profile deal --dir ./deal --no-witness --materiality "$root/skills/deal/profile/materiality-predicate/neutral.json" >/dev/null
id=$(capsulectl --profile deal deal open --records typed --input "$src/open.json" | jq -r .deal_id)
note() { capsulectl --profile deal deal note --deal "$id" "$@"; }
note --kind claim --check "$(represent "$id" "$src/represent-condition.json")" --input "$src/claim-condition.json" >/dev/null
note --kind claim --check "$(represent "$id" "$src/represent-refund-terms.json" "yes, offer the 7-day returns")" --input "$src/claim-refund-terms.json" >/dev/null
offer=$(capsulectl --profile deal deal check --deal "$id" --input "$src/check-offer.json" | jq -r .check_id)
note --kind act --input "$src/act-offer.json" >/dev/null
note --kind acceptance --check "$offer" --words "Deal, 1900 works for me" --channel marketplace >/dev/null
capsulectl --profile deal deal check --deal "$id" --input "$src/check-commit.json" >/dev/null
note --kind act --input "$src/act-commit.json" >/dev/null
# The clock is real, so the due date is set a few days ahead.
jq --arg due "$(python3 -c 'import datetime; print((datetime.date.today() + datetime.timedelta(days=5)).isoformat())')" \
  '.obligation.due_by = $due' "$src/deliver-by.json" >deliver-by.json
due=$(note --kind evidence --input deliver-by.json | jq -r .step)
capsulectl --profile deal deal close --deal "$id" --carry-open-obligations --input "$src/close.json" >/dev/null
jq --argjson step "$due" '.resolves_step = $step' "$src/later-delivered.json" >later-delivered.json
note --kind evidence --input later-delivered.json >/dev/null
note --kind evidence --input "$src/later-refund-claim.json" >/dev/null
capsulectl --profile deal deal report --deal "$id" --html page.html --bundle bundle.json >/dev/null
cp bundle.json page.html "$out/9-seller-later-evidence/"
meta 9-seller-later-evidence "$(jq -n --arg v "$(verdict bundle.json)" '{kind: "seller-later-evidence", emitted_by: "capsulectl deal report --deal ID --html page.html --bundle bundle.json (share keep), after a close and two later evidence records that confirm it", rerender: "deal-report", page: "written", verify: $v}')"

# --- 3. bilateral receipt: composed/v1 of the buyer's and seller's copies ---
# A buyer opens a deal and the seller opens its side under the same deal id.
# The buyer asks itself and the seller for their copies over the
# evidence-request carrier, records both answers under the pinned keys, and
# composes them with `deal compose`: the container's root is the composition
# record on the buyer's compositions log for the deal, under its checkpoint, and each member
# carries its responder's checkpoint, so `verify --bundle` says VALID
# (capsule-cli#149). The two copies join on the deal id and agree.
fixture 3-bilateral-composed
export CAPSULE_DEAL_CHECK_URL=
mkdir -p buyer seller
# side PARTY: act as that party, in its own config and directory.
side() { export XDG_CONFIG_HOME="$work/3-bilateral-composed/$1/config"; cd "$work/3-bilateral-composed/$1"; }
side buyer
capsulectl deal init --profile deal --dir ./deal --no-witness --materiality "$root/skills/deal/profile/materiality-predicate/neutral.json" >/dev/null
cat >open.json <<'JSON'
{"type": "purchase", "channel": "web",
 "intent": {"verbatim": "buy me the example bicycle for at most 1900", "max_total_minor": 190000, "allowed": ["pay"]},
 "who": {"name": "Example Seller", "domain": "seller.example"},
 "terms": {"item": "example bicycle", "price_minor": 190000, "currency": "USD"},
 "recourse": {"rail": "card", "refundable": false}}
JSON
id=$(capsulectl --profile deal deal open --input open.json | jq -r .deal_id)
# The signing and checkpoint public keys the other party pins for this one.
buyer_key=$(capsulectl profile show deal | jq -er '.TrustedKeys[0]')
buyer_checkpoint=$(capsulectl profile show deal | jq -er '.Checkpoint.TrustedKeys[0]')
side seller
capsulectl deal init --profile deal --dir ./deal --no-witness --materiality "$root/skills/deal/profile/materiality-predicate/neutral.json" >/dev/null
cat >open.json <<'JSON'
{"type": "purchase", "demo": true, "channel": "web",
 "intent": {"verbatim": "Sell my example bicycle; ask 1900", "party_role": "seller",
            "asked": {"item": "example bicycle", "quantity": 1}, "allowed": ["offer", "commit"]},
 "who": {"name": "Example Buyer", "domain": "buyer.example"},
 "terms": {"item": "example bicycle", "quantity": 1, "price_minor": 190000, "currency": "USD"},
 "recourse": {"rail": "card", "refundable": false}}
JSON
capsulectl --profile deal deal open --records typed --deal-id "$id" --input open.json >/dev/null
echo '{"action": "offer", "amount_minor": 190000, "terms": {"item": "example bicycle", "quantity": 1, "price_minor": 190000}}' >offer.json
capsulectl --profile deal deal check --deal "$id" --input offer.json >/dev/null
seller_key=$(capsulectl profile show deal | jq -er '.TrustedKeys[0]')
seller_checkpoint=$(capsulectl profile show deal | jq -er '.Checkpoint.TrustedKeys[0]')
# ask RESPONDER KEY CHECKPOINT: the buyer asks RESPONDER for its copy, the
# responder answers, and the buyer records the answer. Sets record to the
# request record id. It runs in this shell, not in $(...), so set -e stops
# the build at the step that fails.
ask() {
  local dir="$work/3-bilateral-composed"
  side buyer
  jq -n --arg id "$id" '{subject: {kind: "correlation", value: $id}, coverage: {min_freshness: {size: 1}}}' >"ask-$1.json"
  record=$(capsulectl --profile deal deal request --request "ask-$1.json" --responder "the $1" --output "$dir/request-$1.json" | jq -er .record_id)
  side "$1"
  capsulectl --profile deal deal respond --request "$dir/request-$1.json" --requester "the buyer" --output "$dir/response-$1.json" >/dev/null
  side buyer
  capsulectl --profile deal deal request --response "$dir/response-$1.json" --for "$record" --responder-key "$2" --responder-checkpoint-key "$3" >/dev/null
}
ask buyer "$buyer_key" "$buyer_checkpoint"
own=$record
ask seller "$seller_key" "$seller_checkpoint"
theirs=$record
side buyer
capsulectl --profile deal deal compose --deal "$id" --member "$own" --member "$theirs" --output bundle.json >/dev/null
# The page as anyone renders it: no plugin installed, no profile.
# An empty CAPSULECTL_PLUGIN_ROOTS would mean the default roots, so it names
# an empty directory.
mkdir -p "$work/3-bilateral-composed/no-plugins"
CAPSULECTL_PLUGIN_ROOTS="$work/3-bilateral-composed/no-plugins" XDG_CONFIG_HOME="$work/3-bilateral-composed/none" capsulectl presentation build --bundle bundle.json --out page.html >/dev/null
cp bundle.json page.html "$out/3-bilateral-composed/"
meta 3-bilateral-composed "$(jq -n --arg v "$(verdict bundle.json)" '{kind: "bilateral-composed", emitted_by: "capsulectl deal compose --deal ID --member BUYER_REQUEST --member SELLER_REQUEST --output bundle.json, then capsulectl presentation build --bundle bundle.json --out page.html", rerender: "presentation-build", page: "written", verify: $v}')"

# --- 4 and 5. monthly outcome and compliance reports: Result v0 roots ------
# A Result v0 sealed as the payload of a SQLite record. When these fixtures
# were first taken, `result build` sealed only into a jsonl book whose
# bundles left producer signatures out, and the page gate refused those
# (capsule-cli#148); the fixtures keep the SQLite route.
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
  capsulectl book report build --bundle bundle.json --card "$3" --out page.html >/dev/null
  cp bundle.json page.html "$out/$1/"
  meta "$1" "$(jq -n --arg k "$2" --arg card "$3" --arg v "$(verdict bundle.json)" '{kind: ("monthly-" + $k), emitted_by: ("capsulectl book report build --bundle bundle.json --card " + $card + " --out page.html"), rerender: "report-build", card: $card, page: "written", verify: $v}')"
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

printf 'fixtures written to %s\n' "$out"
