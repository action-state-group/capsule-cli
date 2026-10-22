#!/usr/bin/env bash
# check-portability.sh -- a deal's shared copy and its exported records stand
# on their own (capsule-cli#243, Buyer Completion Part E):
#   1. run the deal plugin's flow on a deal store on an evidence-book shelf:
#      init, open (typed records), check, act;
#   2. share it (`deal report --share counterparty --html`) and export its
#      records (`deal export`);
#   3. delete the session: the profile, its keys, the deal store and the
#      plugins;
#   4. with capsulectl alone (no plugin, no profile, no network), check both
#      layers:
#        the page: opened offline in headless Chrome it renders the deal,
#          says its bundle verified and its text is sealed, and
#          `capsulectl verify --bundle` calls it VALID;
#        the records: each exported typed record is the exact payload a
#          bundle record commits to, the chain links, and the facts they
#          carry (what was authorized, checked and done, for how much) are
#          the ones the page shows.
#   5. prove the check catches a broken copy: an edited bundle, a bundle
#      with a record dropped and an edited exported record each fail it.
#
#   scripts/check-portability.sh            # uses CAPSULECTL if set, else builds
#
# Needs bash, awk, jq, perl, shasum or sha256sum, and Chrome or Chromium
# (CHROME, or google-chrome, chromium, chromium-browser, or macOS's Google
# Chrome). Touches nothing outside its own temp directory.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
# shellcheck source=lib/page-checks.sh
source "$root/scripts/lib/page-checks.sh"
find_chrome || { echo "check-portability: no Chrome or Chromium found (set CHROME)" >&2; exit 2; }
if command -v sha256sum >/dev/null 2>&1; then sha256() { sha256sum | cut -c1-64; }; else sha256() { shasum -a 256 | cut -c1-64; }; fi

mkdir -p "$work/bin"
if [[ -n "${CAPSULECTL:-}" ]]; then
  ln -s "$CAPSULECTL" "$work/bin/capsulectl"
else
  (cd "$root" && go build -o "$work/bin/capsulectl" ./cmd/capsulectl)
fi
export PATH="$work/bin:$PATH"

# The session: its own home, config, plugin root and deal store.
session="$work/session"
mkdir -p "$session/home" "$session/config"
mkdir -m 0700 "$session/plugins"
(cd "$root/plugins" && go build -o "$session/plugins/capsulectl-book" ./cmd/capsulectl-book &&
  go build -o "$session/plugins/capsulectl-deal" ./cmd/capsulectl-deal)
mkdir "$work/out"

# 1-2. The synthetic retail-checkout demo, sealed in typed records, shared
# with the counterparty and exported.
demo="$root/skills/deal/demo/retail-checkout"
(
  cd "$session"
  export HOME="$session/home" XDG_CONFIG_HOME="$session/config" CAPSULECTL_PLUGIN_ROOTS="$session/plugins" CAPSULE_DEAL_CHECK_URL=
  capsulectl deal init --profile deal --dir ./deal --no-witness --materiality "$root/skills/deal/profile/materiality-predicate/neutral.json" >/dev/null
  id=$(capsulectl --profile deal deal open --records typed --input "$demo/open.json" | jq -er .deal_id)
  verdict=$(capsulectl --profile deal deal check --deal "$id" --input "$demo/check-pay.json" | jq -r .verdict)
  [[ "$verdict" == pass ]] || { echo "the demo's check did not pass: $verdict" >&2; exit 1; }
  capsulectl --profile deal deal note --deal "$id" --kind act --input "$demo/act-pay.json" >/dev/null
  capsulectl --profile deal deal report --deal "$id" --html "$work/out/recipient.html" --share counterparty --to "Example Stickers" >/dev/null
  capsulectl --profile deal deal export --deal "$id" --output "$work/out/records.json" >/dev/null
  echo "$id" >"$work/out/deal-id"
) >"$work/session.log" 2>&1 || { echo "FAIL the deal flow ran" >&2; cat "$work/session.log" >&2; exit 1; }
echo "ok   the deal flow ran: open, check, act, share, export"
deal_id=$(cat "$work/out/deal-id")

# 3. Delete the session.
rm -rf "$session"
check "the session is deleted (profile, keys, deal store, plugins)" test ! -e "$session"

# 4. From here on capsulectl alone: an empty home, config and plugin root.
export HOME="$work/stock/home" XDG_CONFIG_HOME="$work/stock/config" CAPSULECTL_PLUGIN_ROOTS="$work/stock/plugins"
mkdir -p "$HOME" "$XDG_CONFIG_HOME" "$CAPSULECTL_PLUGIN_ROOTS"
stock() { ! capsulectl deal --help >/dev/null 2>&1; }
check "capsulectl here has no deal plugin" stock

# portable PAGE RECORDS: every check of the two layers on a shared page and
# an exported records file, in a directory of its own. It prints its checks
# and returns 1 if any failed.
portable() {
  local page="$1" records="$2" dir saved="$fail" status
  dir="$(mktemp -d "$work/check.XXXXXX")"
  cp "$page" "$dir/page.html"
  cp "$records" "$dir/records.json"
  fail=0
  (
    cd "$dir"
    portable_checks
    exit "$fail"
  ) && status=0 || status=1
  fail="$saved"
  return "$status"
}

portable_checks() {
  # The page layer.
  capsulectl verify --bundle page.html >verify.json 2>/dev/null && verdict=0 || verdict=$?
  check "verify --bundle: VALID, exit 0" test "$verdict" -eq 0
  check "the page loads nothing from the network" no_remote page.html
  render page.html
  check "the page ran offline with no console.error, uncaught error or unhandled rejection" no_render_errors page.html.dom
  # The pill element itself: the page's stylesheet names data-verify values too.
  check "the page says its bundle verified" contains page.html.dom 'class="cv-pill" data-verify="verified"'
  check "the page renders the deal" contains page.html.dom '<h1>Deal report</h1>'
  check "the page says its text is sealed and checked" contains page.html.dom 'data-sealed="x-deal-v0"'
  page_bundle page.html >bundle.json
  check "every bundle record is on the deal's book" on_book bundle.json

  # The record layer: the exported records, read as neutral typed records.
  check "the exported records are the payloads the bundle's records commit to, in order" bound records.json bundle.json
  check "each record's prev is the digest of the one before it" chained records.json
  facts records.json >facts.txt || true
  check "the typed records carry the facts: authorized, proposed, checked DO, paid \$6.27 by card" \
    contains facts.txt "authorized=pay proposed=pay:627:USD:max627 evaluated=DO paid=out:pay:627:USD:card"
  # Both layers state the same thing: each line the page shows for a step
  # is the one the typed record of that step implies.
  check "the page lists as many records as were exported" \
    contains page.html.dom "<summary>$(jq length records.json) records, by capsule id</summary>"
  local line
  line=$(record_line records.json proposed-action/v0 '"About to \(.action) \(money)"')
  check "the page shows what the proposed action says ($line)" contains page.html.dom "<span>$line</span>"
  line=$(record_line records.json action-evaluation/v0 'if .disposition == "DO" and .findings == [] then "Check: no differences" else "(evaluated \(.disposition))" end')
  check "the page shows what the evaluation says ($line)" contains page.html.dom "<span>$line</span>"
  # An act is its own item: the page says it once, as the item's line
  # (capsule-cli#341), and that line says what the action record says.
  local did
  did=$(grep -o '<details data-self="[0-9a-f]*"><summary>[^<]*</summary>' page.html.dom | sed 's/.*<summary>//; s/<\/summary>//')
  line=$(record_line records.json action-record/v0 '"Did: \(.action) \(money)"')
  check "the page's line for the act says what the action record says ($line)" contains_text "$did" "$line "
  line=$(record_line records.json action-record/v0 '" by \(.rail)"')
  check "and says how it was paid ($line)" contains_text "$did" "$line"
}

# contains_text TEXT PART: TEXT holds PART.
contains_text() { [[ -n "$1" && "$1" == *"$2"* ]]; }

# record_line RECORDS TYPE FILTER: FILTER (jq) on the body of the one record
# of TYPE, with money its amount as the page writes it ($6.27); a stand-in
# that no page shows when there is not exactly one.
record_line() {
  jq -r --arg type "$2" 'def money: "$\(.amount_minor / 100 | floor).\(.amount_minor % 100 + 100 | tostring | .[1:])";
    [.[] | select(.type == $type) | .body] | if length == 1 then .[0] | '"$3"' else "(not one \($type))" end' "$1" 2>/dev/null || echo "(unreadable records)"
}

# on_book BUNDLE: every record is on the deal's evidence book (the deal store
# is a shelf of books, capsule-cli#242).
on_book() {
  jq -e --arg book "deal/$deal_id" '(.records | length) > 0 and all(.records[]; .evidence_record_header.book_id == $book)' "$1" >/dev/null
}

# no_remote PAGE: no element loads a resource from another origin.
no_remote() { ! grep -Eiq '(src|href|action)="(https?:)?//' "$1"; }

# record_digest: the SHA-256 of each exported record's exact bytes, one per
# line. jq re-serializes, so the bytes are first checked to be exactly the
# file's: "[" + the records, compact, comma-joined + "]".
record_digests() {
  local n i
  [[ "$(printf '[%s]' "$(jq -r '[.[] | tojson] | join(",")' "$1")")" == "$(cat "$1")" ]] || return 1
  n=$(jq length "$1")
  for ((i = 0; i < n; i++)); do jq -cj ".[$i]" "$1" | sha256; done
}
bound() { # records bundle
  local got want
  got=$(record_digests "$1") || return 1
  want=$(jq -r '.records | sort_by(.evidence_record_header.seq)[] | .evidence_record_header.payload_commitments | if length == 1 then .[0] else error("not one payload") end' "$2") || return 1
  [[ -n "$got" && "$got" == "$want" ]]
}
chained() { # records
  local digests
  digests=$(record_digests "$1") || return 1
  jq -e --argjson d "$(printf '%s\n' "$digests" | jq -R . | jq -s .)" '
    . as $r | all(range(0; length); . as $i | ($r[$i].prev // $r[$i]["x-deal-v0"].prev) as $p
      | if $i == 0 then $p == null else $p.digest == $d[$i - 1] end)' "$1" >/dev/null
}
# facts records: what the typed records say, by their own references: the
# evaluation checked the proposed action, the action record relied on that
# evaluation and on the task authority, which allowed the action.
facts() {
  local digests
  digests=$(record_digests "$1") || return 1
  jq -er --argjson d "$(printf '%s\n' "$digests" | jq -R . | jq -s .)" '
    [to_entries[] | {digest: $d[.key], type: .value.type, body: .value.body}] as $r
    | def one($t): [$r[] | select(.type == $t)] | if length == 1 then .[0] else error("not one \($t)") end;
    one("task-authority/v0") as $ta | one("proposed-action/v0") as $pa
    | one("action-evaluation/v0") as $ev | one("action-record/v0") as $ar
    | if $ev.body.proposed_action_digest != $pa.digest then error("the evaluation checks another proposal")
      elif $ev.body.task_authority_ref.digest != $ta.digest then error("the evaluation rests on another authority")
      elif $ar.body.evaluation_ref.digest != $ev.digest then error("the action rests on another evaluation")
      elif ([$ar.body.authority_basis[].ref.digest] | index($ta.digest)) == null then error("the action names no task authority")
      elif ($ta.body.allowed_actions | index($ar.body.action)) == null then error("the authority does not allow the action")
      else "authorized=\($ta.body.allowed_actions | join(",")) proposed=\($pa.body.action):\($pa.body.amount_minor):\($pa.body.currency):max\($pa.body.authorized_max_minor) evaluated=\($ev.body.disposition) paid=\($ar.body.direction):\($ar.body.action):\($ar.body.amount_minor):\($ar.body.currency):\($ar.body.rail)"
      end' "$1"
}

if portable "$work/out/recipient.html" "$work/out/records.json" >"$work/portable.log" 2>&1; then
  sed 's/^/     /' "$work/portable.log"
  echo "ok   the shared copy and its records stand on their own"
else
  sed 's/^/     /' "$work/portable.log" >&2
  echo "FAIL the shared copy and its records stand on their own" >&2
  fail=1
fi

# 5. Broken copies: each must fail the same check, and fail it where the
# break is: the checks named after the page and records must be among those
# that failed.
broken() { # name, page, records, then the checks that must fail
  local name="$1" page="$2" records="$3" want missing=""
  shift 3
  if portable "$page" "$records" >"$work/$name.log" 2>&1; then
    sed 's/^/     /' "$work/$name.log" >&2
    echo "FAIL $name: the check fails" >&2
    fail=1
    return
  fi
  for want in "$@"; do
    grep -qxF "FAIL $want" "$work/$name.log" || missing="$missing; $want"
  done
  if [[ -n "$missing" ]]; then
    sed 's/^/     /' "$work/$name.log" >&2
    echo "FAIL $name: the check fails where it should (passed:${missing#;})" >&2
    fail=1
  else
    echo "ok   $name: the check fails ($(grep -c '^FAIL' "$work/$name.log") failed, among them: $(printf '%s; ' "$@" | sed 's/; $//'))"
  fi
}
# refused PAGE: opened offline, the page itself says its bundle failed.
refused() {
  render "$1"
  contains "$1.dom" 'class="cv-pill" data-verify="failed"' && lacks "$1.dom" 'class="cv-pill" data-verify="verified"'
}
cd "$work/out"
page_bundle recipient.html >bundle.json

# broken-bundle: the action record's payload commitment edited in the page's
# bundle, and its CSP re-pinned so the page runs it.
jq -c --arg last "$(jq -r '.records | max_by(.evidence_record_header.seq) | .capsule_id' bundle.json)" \
  '(.records[] | select(.capsule_id == $last)).evidence_record_header.payload_commitments[0] |= (if startswith("0") then "1" else "0" end + .[1:])' bundle.json>broken-bundle.json
replace_bundle recipient.html broken-bundle.json >broken-bundle.html
rehash_bundle_csp recipient.html broken-bundle.html
check "broken-bundle: the copy differs" differs recipient.html broken-bundle.html
broken broken-bundle broken-bundle.html records.json "verify --bundle: VALID, exit 0" "the page says its bundle verified"
check "broken-bundle: the page says its bundle failed" refused broken-bundle.html

# dropped-record: the bundle without its last record (the action taken).
jq -c '.records |= (sort_by(.evidence_record_header.seq) | .[:-1])' bundle.json >dropped-record.json
replace_bundle recipient.html dropped-record.json >dropped-record.html
rehash_bundle_csp recipient.html dropped-record.html
broken dropped-record dropped-record.html records.json "verify --bundle: VALID, exit 0" "the page says its bundle verified"
check "dropped-record: the page says its bundle failed" refused dropped-record.html

# broken-records: the exported action record says $62.70 was paid.
jq -c '(.[] | select(.type == "action-record/v0") | .body.amount_minor) |= 6270' records.json | tr -d '\n' >broken-records.json
check "broken-records: the records differ" differs records.json broken-records.json
broken broken-records recipient.html broken-records.json \
  "the exported records are the payloads the bundle's records commit to, in order" \
  "the page's line for the act says what the action record says (Did: pay \$62.70)"

exit "$fail"
