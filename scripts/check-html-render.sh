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

# rehash_bundle_csp ORIGINAL EDITED: the page's CSP pins the SHA-256 of every
# inline script, the bundle's included, so a page whose bundle text was edited
# does not run it at all. Whoever edits the file can edit the policy too: this
# puts the edited bundle script's hash where the original's was, so the checks
# below test what the page itself says about an edited bundle.
rehash_bundle_csp() {
  perl -MDigest::SHA=sha256_base64 -e '
    sub h { my $b = sha256_base64($_[0]); $b .= "=" while length($b) % 4; return "sha256-$b" }
    local $/; open my $o, "<", $ARGV[0] or die; my $orig = <$o>; close $o;
    open my $e, "<", $ARGV[1] or die; my $page = <$e>; close $e;
    my ($old) = $orig =~ /<script>(window\.__BUNDLE__ = .*?;)<\/script>/s or die "no bundle script";
    my ($new) = $page =~ /<script>(window\.__BUNDLE__ = .*?;)<\/script>/s or die "no bundle script";
    my ($ho, $hn) = (h($old), h($new));
    $page =~ s/\Q$ho\E/$hn/ or die "the bundle script is not pinned in the CSP";
    open my $w, ">", $ARGV[1] or die; print $w $page; close $w;' "$1" "$2"
}

# incomplete-bundle: the README page with its records' producer signatures
# removed is INCOMPLETE by `verify --bundle`; its page must never say the
# bundle passed. capsulectl writes no such page; this checks the vendored
# viewer itself, once it states a verdict (data-verdict).
jq -c '.records |= map(del(.signature, .key_id))' bundle.json >incomplete.json
perl -0pe 'BEGIN{open my $f,"<","incomplete.json" or die; local $/; $j=<$f>; chomp $j; $j=~s/</\\u003c/g} s/(window\.__BUNDLE__ = ).*?(;<\/script>)/$1$j$2/s' report.html >incomplete.html
rehash_bundle_csp report.html incomplete.html
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

# A deal receipt: its deal text (summary lines, step lines, amounts) is
# sealed as a record in the bundle (internal/cli/deal_sealed_report.go). The
# page renders it and says it checked it; a copy with one sealed line edited
# says it did not verify and shows none of it, and verify --bundle calls it
# INVALID. The deal is the synthetic retail-checkout demo.
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
check "deal-receipt: the page says its text is sealed and checked" contains receipt.html.dom 'data-sealed="x-deal-v0"'
capsulectl verify --bundle receipt.html >receipt-verify.json && verdict=0 || verdict=$?
check "deal-receipt: verify --bundle: VALID, exit 0" test "$verdict" -eq 0
perl -0ne 'print $1 if /window\.__BUNDLE__ = (.*?);<\/script>/s' receipt.html >receipt.json
did_line=$(jq -r '.extensions["x-deal-v0"].sealed_report as $id | .disclosures[$id].agent_input.report.did_line' receipt.json)
check "deal-receipt: the page renders its sealed summary line" contains receipt.html.dom "<p class=\"deal-note\">$did_line</p>"
jq -c '.extensions["x-deal-v0"].sealed_report as $id | .disclosures[$id].agent_input.report.did_line = "A line nobody sealed."' receipt.json >receipt-edited.json
perl -0pe 'BEGIN{open my $f,"<","receipt-edited.json" or die; local $/; $j=<$f>; chomp $j; $j=~s/</\\u003c/g} s/(window\.__BUNDLE__ = ).*?(;<\/script>)/$1$j$2/s' receipt.html >receipt-edited.html
rehash_bundle_csp receipt.html receipt-edited.html
check "deal-receipt-edited: the page carries the edited line" contains receipt-edited.html 'A line nobody sealed.'
# The same edit without touching the policy: the edited bundle script is not
# the one the CSP pins, so the browser does not run it and the page shows no
# deal text at all.
perl -0pe 'BEGIN{open my $f,"<","receipt-edited.json" or die; local $/; $j=<$f>; chomp $j; $j=~s/</\\u003c/g} s/(window\.__BUNDLE__ = ).*?(;<\/script>)/$1$j$2/s' receipt.html >receipt-unpinned.html
render receipt-unpinned.html
check "deal-receipt-unpinned: an edited bundle the CSP does not pin shows no deal text" lacks receipt-unpinned.html.dom '<p class="deal-note">A line nobody sealed.</p>'
check "deal-receipt-unpinned: and no deal section" lacks receipt-unpinned.html.dom '<h1>Deal report</h1>'
render receipt-edited.html
# The page's runtime says the bundle failed and selects no presentation: the
# deal view never runs on a bundle that did not verify.
check "deal-receipt-edited: the page says the bundle did not verify" contains receipt-edited.html.dom 'data-refusal="unverified-bundle"'
check "deal-receipt-edited: and no deal section" lacks receipt-edited.html.dom '<h1>Deal report</h1>'
check "deal-receipt-edited: the page shows no edited line" lacks receipt-edited.html.dom '<p class="deal-note">A line nobody sealed.</p>'
capsulectl verify --bundle receipt-edited.html >/dev/null 2>&1 && verdict=0 || verdict=$?
check "deal-receipt-edited: verify --bundle: INVALID, exit 1" test "$verdict" -eq 1

# deal-receipt-downgraded: the sealed report's record kept, its disclosure
# dropped, and edited text put inline in the extension, as a receipt from
# before reports were sealed carries it. The page refuses rather than show
# it under the "not checked" label; verify --bundle calls it INVALID.
jq -c '.extensions["x-deal-v0"].sealed_report as $id | .disclosures[$id].agent_input.report as $text
  | .extensions["x-deal-v0"] = ($text | .did_line = "A line nobody sealed.") | del(.disclosures[$id])' receipt.json >receipt-downgraded.json
perl -0pe 'BEGIN{open my $f,"<","receipt-downgraded.json" or die; local $/; $j=<$f>; chomp $j; $j=~s/</\\u003c/g} s/(window\.__BUNDLE__ = ).*?(;<\/script>)/$1$j$2/s' receipt.html >receipt-downgraded.html
rehash_bundle_csp receipt.html receipt-downgraded.html
check "deal-receipt-downgraded: the page carries the inline line" contains receipt-downgraded.html 'A line nobody sealed.'
render receipt-downgraded.html
check "deal-receipt-downgraded: the page says the text could not be checked" contains receipt-downgraded.html.dom "<p class=\"deal-bad\">⚠️ The deal's text could not be checked against its sealed record"
check "deal-receipt-downgraded: the page shows no inline line" lacks receipt-downgraded.html.dom '<p class="deal-note">A line nobody sealed.</p>'
check "deal-receipt-downgraded: no \"not checked\" label" lacks receipt-downgraded.html.dom 'data-unchecked="x-deal-v0"'
capsulectl verify --bundle receipt-downgraded.html >/dev/null 2>&1 && verdict=0 || verdict=$?
check "deal-receipt-downgraded: verify --bundle: INVALID, exit 1" test "$verdict" -eq 1

exit "$fail"
