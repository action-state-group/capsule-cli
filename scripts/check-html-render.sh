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
# The README's example needs no plugin: the core alone, on no plugin root.
export CAPSULECTL_PLUGIN_ROOTS="$work/no-plugins"
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

# A jsonl (book) profile's page refusal needs the book plugin; its tests
# check it (evidencebook's TestPageGateRefusesAJSONLProfilesPage).

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

# A deal receipt's page (the deal plugin's) is checked in evidencebook-deal
# (scripts/check-deal-pages.sh) since the plugin moved there (capsule-cli#326).

exit "$fail"
