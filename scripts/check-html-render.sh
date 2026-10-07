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

chrome="${CHROME:-}"
if [[ -z "$chrome" ]]; then
  for c in google-chrome google-chrome-stable chromium chromium-browser "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"; do
    if command -v "$c" >/dev/null 2>&1 || [[ -x "$c" ]]; then chrome="$c"; break; fi
  done
fi
[[ -n "$chrome" ]] || { echo "check-html-render: no Chrome or Chromium found (set CHROME)" >&2; exit 2; }

mkdir -p "$work/bin"
if [[ -n "${CAPSULECTL:-}" ]]; then
  ln -s "$CAPSULECTL" "$work/bin/capsulectl"
else
  (cd "$root" && go build -o "$work/bin/capsulectl" ./cmd/capsulectl)
fi
export PATH="$work/bin:$PATH"
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

fail=0
check() { # description, then a command that must succeed
  local what="$1"; shift
  if "$@"; then echo "ok   $what"; else echo "FAIL $what" >&2; fail=1; fi
}
contains() { grep -q -F -- "$2" "$1"; }
lacks() { ! contains "$@"; }
differs() { ! cmp -s "$1" "$2"; }

# render PAGE: the DOM after the page's own scripts ran. A copy of the page
# gets one script first in its head that records, by kind, every
# console.error, uncaught error and unhandled rejection into
# data-render-errors on <html>, which the DOM dump carries; it marks itself
# installed (data-render-hook), so a hook that never ran fails the check. Headless Chrome
# uses its own throwaway profile (a --user-data-dir keeps it from exiting on
# macOS); the alarm bounds a run that hangs anyway, which then fails for want
# of a DOM.
hook='<script>(function(){document.documentElement.setAttribute("data-render-hook","installed");var seen=[];function note(kind,detail){seen.push(kind+": "+String(detail));document.documentElement.setAttribute("data-render-errors",JSON.stringify(seen));}var error=console.error;console.error=function(){note("console.error",Array.prototype.join.call(arguments," "));return error.apply(console,arguments);};window.addEventListener("error",function(e){note("uncaught",e.message);});window.addEventListener("unhandledrejection",function(e){note("unhandled rejection",e.reason?e.reason.message||e.reason:e.reason);});})();</script>'
render() {
  awk -v hook="$hook" '!done && (i = index($0, "<head>")) {$0 = substr($0, 1, i + 5) hook substr($0, i + 6); done = 1} {print}' "$1" >"$1.probe.html"
  perl -e 'alarm 60; exec @ARGV' "$chrome" --headless --no-sandbox --disable-gpu --no-first-run \
    --virtual-time-budget=10000 --dump-dom "file://$work/$1.probe.html" >"$1.dom" 2>/dev/null || true
}
no_render_errors() {
  if contains "$1" 'data-render-errors='; then
    grep -o 'data-render-errors="[^"]*"' "$1" | sed 's/^/     /' >&2
    return 1
  fi
  contains "$1" 'data-render-hook="installed"'
}

render report.html
check "the error hook ran in the page" contains report.html.dom 'data-render-hook="installed"'
check "the page says the bundle verified" contains report.html.dom 'data-verify="verified"'
check "the banner reads 'Bundle verification passed'" contains report.html.dom 'Bundle verification passed'
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
check "the tampered page says verification failed" contains tampered.html.dom 'Bundle verification failed'
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
# report shows every row (internal/cli/page_gate.go). Each variant of the
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
check "legacy-jsonl-profile: no page is written" refused legacy-jsonl-profile "a jsonl profile's bundle carries its evidence book's records"

# zero-rows: a VALID bundle whose report row cites a record it does not carry.
variant zero-rows -e 's#references: \[cite\(\$permissions\)\]#references: [cite("0000000000000000000000000000000000000000000000000000000000000000")]#'
check "zero-rows: no page is written for a report that would show 1 of 2 rows" refused zero-rows "would show 1 of 2 rows"

# incomplete-bundle: the README page with its records' producer signatures
# removed is INCOMPLETE by `verify --bundle`; its page must never say the
# bundle passed. capsulectl writes no such page; this checks the vendored
# viewer itself, once it states a verdict (data-verdict).
jq -c '.records |= map(del(.signature, .key_id))' bundle.json >incomplete.json
perl -0pe 'BEGIN{open my $f,"<","incomplete.json" or die; local $/; $j=<$f>; chomp $j; $j=~s/</\\u003c/g} s/(window\.__BUNDLE__ = ).*?(;<\/script>)/$1$j$2/s' report.html >incomplete.html
check "incomplete-bundle: the page carries the changed bundle" differs report.html incomplete.html
capsulectl verify --bundle incomplete.html >incomplete-verify.json 2>/dev/null && verdict=0 || verdict=$?
check "incomplete-bundle: verify --bundle calls it INCOMPLETE, exit 3" test "$verdict" -eq 3
render incomplete.html
if contains incomplete.html.dom 'data-verdict='; then
  check "incomplete-bundle: the page's verdict claims no more than verify --bundle's" claims_no_more incomplete.html.dom incomplete-verify.json
  check "incomplete-bundle: the page never says the bundle passed" lacks incomplete.html.dom 'Bundle verification passed'
else
  echo "skip incomplete-bundle's page: the vendored viewer sets no data-verdict yet, and says 'passed' for it (agent-action-capsule's issue on the viewer's verdict wording)"
fi

exit "$fail"
