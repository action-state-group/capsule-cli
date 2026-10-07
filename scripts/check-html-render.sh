#!/usr/bin/env bash
# check-html-render.sh -- build the README's "self-checking report of records
# you sealed" with capsulectl, open its page in headless Chrome, and check
# what a reader sees:
#   - the page renders the report (its title and every row) and says the
#     bundle verified, with no console error;
#   - a copy with one disclosed payload edited says verification failed;
#   - `capsulectl verify --bundle` agrees with each (VALID, exit 0; INVALID,
#     exit 1).
#
#   scripts/check-html-render.sh            # uses CAPSULECTL if set, else builds
#
# Needs bash, jq, perl and Chrome or Chromium (CHROME, or google-chrome, chromium,
# chromium-browser, or macOS's Google Chrome). Touches nothing outside its
# own temp directory.
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

bin="${CAPSULECTL:-}"
if [[ -z "$bin" ]]; then
  bin="$work/capsulectl"
  (cd "$root" && go build -o "$bin" ./cmd/capsulectl)
fi
cl() { "$bin" "$@"; }

# The README's worked example, step for step.
cd "$work"
public=$(cl key generate --output seed | jq -r .public_key)
cl profile create --name example --type sqlite --sqlite-path store.db \
  --operator "Example Operator" --signing-key-file seed --trusted-key "$public" \
  --log-id example-log --checkpoint-signing-key-file seed --checkpoint-trusted-key "$public" >/dev/null
cl store init --profile example >/dev/null
request() { # name payload [references]
  jq -n --arg id "$1" --argjson payload "$2" --argjson refs "${3:-[]}" '{spec_version: "capsule-seal-request/v1",
    capsule: ({ActionID: $id, ActionType: "fyi", Operator: "example-operator", Developer: "example-developer",
      Timestamp: "2026-10-07T00:00:00Z"} + (if $refs == [] then {} else {References: $refs} end)), payload: $payload}' >"$1.json"
}
request check-config '{"check":"config file","result":"same"}'
config=$(cl publish --profile example --request check-config.json | jq -r .capsule_id)
request check-permissions '{"check":"permissions","result":"different"}'
perms=$(cl publish --profile example --request check-permissions.json | jq -r .capsule_id)
cite() { jq -n --arg d "$1" '{type: "agent-action-capsule", digest_alg: "sha256", digest: $d, citation_purpose: "acted_on"}'; }
report=$(jq -n --argjson a "$(cite "$config")" --argjson b "$(cite "$perms")" '{spec_version: "report/v1", title: "Example checks", rows: [
  {row_id: "config", label: "Config file", status: "same", reason: "unchanged since the last run", references: [$a]},
  {row_id: "permissions", label: "Permissions", status: "different", references: [$b]}]}')
refs=$(jq -n --arg a "$config" --arg b "$perms" '[$a, $b] | map({Type: "agent-action-capsule", DigestAlg: "sha256", Digest: ., CitationPurpose: "acted_on"})')
request report "$report" "$refs"
root_id=$(cl publish --profile example --request report.json | jq -r .capsule_id)
cl cll checkpoint create --profile example >/dev/null
cl disclose --profile example --root "$root_id" --out bundle.json --html report.html >/dev/null

fail=0
check() { # description, then a command that must succeed
  local what="$1"; shift
  if "$@"; then echo "ok   $what"; else echo "FAIL $what" >&2; fail=1; fi
}

# render PAGE: the DOM after the page's own scripts ran, and its console.
# Headless Chrome uses its own throwaway profile (a --user-data-dir keeps it
# from exiting on macOS); the alarm bounds a run that hangs anyway, which
# then fails the checks below for want of a DOM.
render() {
  perl -e 'alarm 60; exec @ARGV' "$chrome" --headless --no-sandbox --disable-gpu --no-first-run \
    --enable-logging=stderr --v=0 --virtual-time-budget=10000 --dump-dom "file://$work/$1" \
    >"$work/$1.dom" 2>"$work/$1.log" || true
  grep -E 'CONSOLE' "$work/$1.log" >"$work/$1.console" || true
}
contains() { grep -q -F -- "$2" "$work/$1"; }
lacks() { ! contains "$@"; }
differs() { ! cmp -s "$work/$1" "$work/$2"; }
no_console_error() { ! grep -q -i -E 'uncaught|error' "$work/$1.console"; }

render report.html
check "the page says the bundle verified" contains report.html.dom 'data-verify="verified"'
check "the banner reads 'Bundle verification passed'" contains report.html.dom 'Bundle verification passed'
# The rendered markup, not the bundle's JSON the page also carries.
check "the page renders the report" contains report.html.dom 'data-page="report-rows"><h1>Example checks</h1>'
check "the page renders the row 'Config file'" contains report.html.dom 'data-row-id="config">Config file</button></td><td data-row-status="same">same<'
check "the page renders the row 'Permissions'" contains report.html.dom 'data-row-id="permissions">Permissions</button></td><td data-row-status="different">different<'
check "no console error" no_console_error report.html
[[ -s "$work/report.html.console" ]] && sed 's/^/     console: /' "$work/report.html.console"
cl verify --bundle bundle.json >/dev/null && verdict=0 || verdict=$?
check "verify --bundle: VALID, exit 0" test "$verdict" -eq 0

# Tampered: one disclosed payload edited, in the page and in the bundle.
sed 's/"check":"permissions","result":"different"/"check":"permissions","result":"same"/' report.html >tampered.html
check "the tampered copy differs" differs report.html tampered.html
render tampered.html
check "the tampered page says verification failed" contains tampered.html.dom 'Bundle verification failed'
check "the tampered page renders no rows" lacks tampered.html.dom 'data-page="report-rows"'
jq --arg id "$perms" '.disclosures[$id].agent_input.result = "same"' bundle.json >tampered.json
cl verify --bundle tampered.json >/dev/null 2>&1 && verdict=0 || verdict=$?
check "verify --bundle on the tampered bundle: INVALID, exit 1" test "$verdict" -eq 1

# The page should state the CLI's verdict, not only "passed": it does not yet
# (the viewer's banner reads "passed" for an INCOMPLETE bundle too). Checked
# once the vendored viewer sets data-verdict.
if contains report.html.dom 'data-verdict='; then
  check "the page states the CLI's verdict" contains report.html.dom 'data-verdict="valid"'
else
  echo "skip the page states the CLI's verdict: the vendored viewer sets no data-verdict yet"
fi

exit "$fail"
