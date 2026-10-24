#!/usr/bin/env bash
# check-html-render.sh -- run the README's "self-checking report of records
# you sealed" exactly as written, open its page in headless Chrome, and check
# what a reader sees:
#   - the page renders the report (its title and every row) and says the
#     bundle verified, with no error logged or thrown;
#   - a copy with one disclosed payload edited says verification failed and
#     renders no rows;
#   - `capsulectl verify --bundle` agrees with each (VALID, exit 0; INVALID,
#     exit 1).
#
#   scripts/check-html-render.sh            # uses CAPSULECTL if set, else builds
#
# Needs bash, awk, jq, perl and Chrome or Chromium (CHROME, or google-chrome,
# chromium, chromium-browser, or macOS's Google Chrome). Touches nothing
# outside its own temp directory.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
export XDG_CONFIG_HOME="$work/config"
# shellcheck source=lib/page-checks.sh
source "$root/scripts/lib/page-checks.sh"
find_chrome || { echo "check-html-render: no Chrome or Chromium found (set CHROME)" >&2; exit 2; }

mkdir -p "$work/bin"
if [[ -n "${CAPSULECTL:-}" ]]; then
  ln -s "$CAPSULECTL" "$work/bin/capsulectl"
else
  (cd "$root" && go build -o "$work/bin/capsulectl" ./cmd/capsulectl)
fi
export PATH="$work/bin:$PATH"
# The deal and book commands are the first-party plugins (capsule-cli#236):
# build both onto a plugin root of the run's own, unless one is given.
if [[ -z "${CAPSULECTL_PLUGIN_ROOTS:-}" ]]; then
  mkdir -m 0700 "$work/plugins"
  (cd "$root/plugins" && go build -o "$work/plugins/capsulectl-book" ./cmd/capsulectl-book &&
    go build -o "$work/plugins/capsulectl-deal" ./cmd/capsulectl-deal)
  export CAPSULECTL_PLUGIN_ROOTS="$work/plugins"
fi
cd "$work"

# The README's worked example: the first bash block under its heading, run as
# written in an empty directory.
awk '/^## Example: a self-checking report of records you sealed$/ {in_section = 1; next}
     in_section && /^## / {exit}
     in_section && !started && /^```bash$/ {started = 1; next}
     started && /^```$/ {exit}
     started {print}' "$root/README.md" >example.sh
[[ -s example.sh ]] || { echo "check-html-render: the README's report example was not found" >&2; exit 2; }
mkdir example
if ! (cd example && bash -e -o pipefail ../example.sh >../example.log 2>&1); then
  echo "FAIL the README's example runs as written" >&2
  cat example.log >&2
  exit 1
fi
echo "ok   the README's example runs as written"
cp example/bundle.json example/report.html .

render report.html
check "the error hook ran in the page" contains report.html.dom 'data-render-hook="installed"'
# The pill element itself: the page's stylesheet names data-verify values too.
check "the page says the bundle verified" contains report.html.dom 'class="cv-pill" data-verify="verified"'
check "the pill reads 'Contents checked'" contains report.html.dom '>Contents checked</span>'
# The rendered markup, not the bundle's JSON the page also carries.
check "the page renders the report" contains report.html.dom 'data-page="report-rows"><h1>Example checks</h1>'
check "the page renders the row 'Config file'" contains report.html.dom 'data-row-id="config">Config file</button></td><td data-row-status="same">same<'
check "the page renders the row 'Permissions'" contains report.html.dom 'data-row-id="permissions">Permissions</button></td><td data-row-status="different">different<'
check "no console.error, uncaught error or unhandled rejection" no_render_errors report.html.dom
capsulectl verify --bundle bundle.json >verify.json && verdict=0 || verdict=$?
check "verify --bundle: VALID, exit 0" test "$verdict" -eq 0

# Tampered: one disclosed payload edited, in the page and in the bundle.
sed 's/"check":"permissions","result":"different"/"check":"permissions","result":"same"/' report.html >tampered.html
check "the tampered copy differs" differs report.html tampered.html
render tampered.html
check "the tampered page says verification failed" contains tampered.html.dom 'class="cv-pill" data-verify="failed"'
check "the tampered page's pill reads 'Check failed'" contains tampered.html.dom '>Check failed</span>'
check "the tampered page renders no rows" lacks tampered.html.dom 'data-page="report-rows"'
permissions=$(jq -r '.disclosures | to_entries[] | select(.value.agent_input.check == "permissions") | .key' bundle.json)
jq --arg id "$permissions" '.disclosures[$id].agent_input.result = "same"' bundle.json >tampered.json
capsulectl verify --bundle tampered.json >tampered-verify.json 2>/dev/null && verdict=0 || verdict=$?
check "verify --bundle on the tampered bundle: INVALID, exit 1" test "$verdict" -eq 1

# The page should state a verdict (data-verdict: valid, incomplete or
# invalid) that claims no more than verify --bundle's on the same bundle. It
# may claim less: a browser that cannot authenticate the checkpoint can
# honestly say incomplete for a bundle the CLI calls VALID; it must never say
# valid for one the CLI calls INCOMPLETE or INVALID. Checked once the vendored
# viewer sets data-verdict: until then its banner reads "passed" for an
# INCOMPLETE bundle too.
rank() { case "$1" in invalid) echo 0 ;; incomplete) echo 1 ;; valid) echo 2 ;; *) echo -1 ;; esac; }
claims_no_more() { # DOM, verify --bundle output
  local page cli
  page=$(grep -o 'data-verdict="[a-z]*"' "$1" | head -1 | sed 's/data-verdict="//; s/"$//')
  cli=$(jq -r '.verdict | ascii_downcase' "$2")
  echo "     page: ${page:-none}, verify --bundle: $cli"
  [[ $(rank "$page") -ge 0 && $(rank "$cli") -ge 0 && $(rank "$page") -le $(rank "$cli") ]]
}
if contains report.html.dom 'data-verdict='; then
  check "the page's verdict claims no more than verify --bundle's" claims_no_more report.html.dom verify.json
  check "the tampered page's verdict claims no more than verify --bundle's" claims_no_more tampered.html.dom tampered-verify.json
else
  echo "skip the page's verdict against verify --bundle's: the vendored viewer sets no data-verdict yet (see agent-action-capsule's issue on the viewer's verdict wording)"
fi


# A page is written only for a bundle `verify --bundle` calls VALID whose
# report shows every row (cli/page_gate.go). Each variant of the
# README's example below runs as written up to its refused step.
variant() { # name, then perl substitutions applied to the README's example
  local name="$1"; shift
  mkdir "$name"
  perl -0p "$@" example.sh >"$name/example.sh"
  # Its own config directory: each variant creates the README's profile anew.
  (cd "$name" && XDG_CONFIG_HOME="$work/$name/config" bash -e -o pipefail example.sh >log 2>&1) && echo 0 >"$name/exit" || echo $? >"$name/exit"
}
refused() { # name, words the refusal must carry
  [[ $(cat "$1/exit") -ne 0 ]] && [[ ! -e "$1/report.html" ]] && contains "$1/log" "$2"
}

# legacy-jsonl-profile: an rc7/rc8-style jsonl profile's bundle carries its
# evidence book's records, not the records sealed, so its page could show
# none of the report's rows (it once said "verification passed" over 0 of 6).
variant legacy-jsonl-profile -e 's#--type sqlite --sqlite-path \./store\.db#--type jsonl --jsonl-path ./store --namespace example#'
check "legacy-jsonl-profile: no page is written" refused legacy-jsonl-profile "a book profile's bundle carries its evidence book's own records"

# zero-rows: a VALID bundle whose report row cites a record it does not carry.
variant zero-rows -e 's#references: \[cite\(\$permissions\)\]#references: [cite("0000000000000000000000000000000000000000000000000000000000000000")]#'
check "zero-rows: no page is written for a report that would show 1 of 2 rows" refused zero-rows "would show 1 of 2 rows"

# incomplete-bundle: the README page with its records' producer signatures
# removed is INCOMPLETE by `verify --bundle`; its page must never say the
# bundle passed. capsulectl writes no such page; this checks the vendored
# viewer itself, once it states a verdict (data-verdict).
jq -c '.records |= map(del(.signature, .key_id))' bundle.json >incomplete.json
replace_bundle report.html incomplete.json >incomplete.html
rehash_bundle_csp report.html incomplete.html
check "incomplete-bundle: the page carries the changed bundle" differs report.html incomplete.html
capsulectl verify --bundle incomplete.html >incomplete-verify.json 2>/dev/null && verdict=0 || verdict=$?
check "incomplete-bundle: verify --bundle calls it INCOMPLETE, exit 3" test "$verdict" -eq 3
render incomplete.html
if contains incomplete.html.dom 'data-verdict='; then
  check "incomplete-bundle: the page's verdict claims no more than verify --bundle's" claims_no_more incomplete.html.dom incomplete-verify.json
  # The pill's accessible name is where the page says "passed" (the attribute,
  # not the viewer's inline script, which holds the same words).
  check "incomplete-bundle: the page never says the bundle passed" lacks incomplete.html.dom 'aria-label="Contents checked: bundle verification passed"'
else
  echo "skip incomplete-bundle's page: the vendored viewer sets no data-verdict yet, and says 'Contents checked' for it (agent-action-capsule's issue on the viewer's verdict wording)"
fi

# A deal receipt: its deal text (summary lines, step lines, amounts) is
# sealed as a standalone signed report the copy carries in x-deal-report/v0
# (cli/deal_sealed_report.go). The page renders it and says it
# checked it; a copy with one sealed line edited, or its report stripped to
# inline text, is refused and shows none of it, and verify --bundle calls it
# INVALID. The deal is the synthetic retail-checkout demo. The page's refusal
# of a downgraded receipt sealed on the deal's log, as capsulectl wrote before
# capsule-cli#294, is checked under node in
# plugins/internal/deal/deal_page_standalone_test.go.
demo="$root/skills/deal/demo/retail-checkout"
mkdir deal-receipt
(
  cd deal-receipt
  export XDG_CONFIG_HOME="$work/deal-receipt/config" CAPSULE_DEAL_CHECK_URL=
  capsulectl deal init --profile deal --dir ./deal --no-witness --materiality "$root/skills/deal/profile/materiality-predicate/neutral.json" >/dev/null
  id=$(capsulectl --profile deal deal open --input "$demo/open.json" | jq -r .deal_id)
  capsulectl --profile deal deal check --deal "$id" --input "$demo/check-pay.json" >/dev/null
  capsulectl --profile deal deal note --deal "$id" --kind act --input "$demo/act-pay.json" >/dev/null
  capsulectl --profile deal deal report --deal "$id" --html receipt.html >/dev/null
) >deal-receipt/log 2>&1 || { echo "FAIL deal-receipt: the demo deal's receipt was written" >&2; cat deal-receipt/log >&2; exit 1; }
cp deal-receipt/receipt.html receipt.html
render receipt.html
check "deal-receipt: no console.error, uncaught error or unhandled rejection" no_render_errors receipt.html.dom
check "deal-receipt: the page renders the deal" contains receipt.html.dom '<h1>Deal report</h1>'
check "deal-receipt: the deal view inserted its own stylesheet" contains receipt.html.dom '<style id="capsulectl-deal-view-style">'
check "deal-receipt: the page says its text is sealed and checked" contains receipt.html.dom 'data-sealed="x-deal-v0"'
capsulectl verify --bundle receipt.html >receipt-verify.json && verdict=0 || verdict=$?
check "deal-receipt: verify --bundle: VALID, exit 0" test "$verdict" -eq 0
page_bundle receipt.html >receipt.json
did_line=$(jq -r '.extensions["x-deal-report/v0"].agent_input.report.did_line' receipt.json)
check "deal-receipt: the page renders its sealed summary line" contains receipt.html.dom "<p class=\"deal-note\">$did_line</p>"

# deal-receipt-edited: one line of the signed report edited. The bundle's own
# records still verify, so the page's runtime runs the deal view, which
# refuses: the report's signature no longer verifies.
jq -c '.extensions["x-deal-report/v0"].agent_input.report.did_line = "A line nobody sealed."' receipt.json >receipt-edited.json
replace_bundle receipt.html receipt-edited.json >receipt-edited.html
rehash_bundle_csp receipt.html receipt-edited.html
check "deal-receipt-edited: the page carries the edited line" contains receipt-edited.html 'A line nobody sealed.'
# The same edit without touching the policy: the edited bundle script is not
# the one the CSP pins, so the browser does not run it and the page shows no
# deal text at all.
replace_bundle receipt.html receipt-edited.json >receipt-unpinned.html
render receipt-unpinned.html
check "deal-receipt-unpinned: an edited bundle the CSP does not pin shows no deal text" lacks receipt-unpinned.html.dom '<p class="deal-note">A line nobody sealed.</p>'
check "deal-receipt-unpinned: and no deal section" lacks receipt-unpinned.html.dom '<h1>Deal report</h1>'
render receipt-edited.html
check "deal-receipt-edited: the page says the text could not be checked" contains receipt-edited.html.dom "<p class=\"deal-bad\">⚠️ The deal's text could not be checked against its sealed report"
check "deal-receipt-edited: the page shows no edited line" lacks receipt-edited.html.dom '<p class="deal-note">A line nobody sealed.</p>'
check "deal-receipt-edited: no \"not checked\" label" lacks receipt-edited.html.dom 'data-unchecked="x-deal-v0"'
capsulectl verify --bundle receipt-edited.html >/dev/null 2>&1 && verdict=0 || verdict=$?
check "deal-receipt-edited: verify --bundle: INVALID, exit 1" test "$verdict" -eq 1

# deal-receipt-downgraded: the standalone report dropped and edited text put
# inline in x-deal-v0, as a receipt from before reports were sealed carries
# it. The page refuses rather than show it under the "not checked" label;
# verify --bundle calls it INVALID: the copy names no sealed report.
jq -c '.extensions["x-deal-report/v0"].agent_input.report as $text
  | .extensions["x-deal-v0"] = ($text | .did_line = "A line nobody sealed.") | del(.extensions["x-deal-report/v0"])' receipt.json >receipt-downgraded.json
replace_bundle receipt.html receipt-downgraded.json >receipt-downgraded.html
rehash_bundle_csp receipt.html receipt-downgraded.html
check "deal-receipt-downgraded: the page carries the inline line" contains receipt-downgraded.html 'A line nobody sealed.'
render receipt-downgraded.html
check "deal-receipt-downgraded: the page says the text could not be checked" contains receipt-downgraded.html.dom "<p class=\"deal-bad\">⚠️ The deal's text could not be checked against its sealed report"
check "deal-receipt-downgraded: the page shows no inline line" lacks receipt-downgraded.html.dom '<p class="deal-note">A line nobody sealed.</p>'
check "deal-receipt-downgraded: no \"not checked\" label" lacks receipt-downgraded.html.dom 'data-unchecked="x-deal-v0"'
capsulectl verify --bundle receipt-downgraded.html >receipt-downgraded-verify.json 2>/dev/null && verdict=0 || verdict=$?
check "deal-receipt-downgraded: verify --bundle: INVALID, exit 1" test "$verdict" -eq 1
check "deal-receipt-downgraded: verify --bundle names the missing sealed report" contains receipt-downgraded-verify.json 'sealed_report_not_named'

exit "$fail"
