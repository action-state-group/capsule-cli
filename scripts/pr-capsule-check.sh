#!/usr/bin/env bash
# Checks that a PR capsule's references decode under docs/PR-CAPSULE-PROFILE.md:
# each entry's role is read from its position and type, so a list that does
# not decode has no defined roles and is rejected.
#
#   references[0]  text/x-diff       the diff, always
#   references[1]  application/json  the CI result, always
#   then           application/json  the reviews, at most one, decision capsule only
#   then           text/plain        the declared prompt, at most one, last
#
# Usage: pr-capsule-check.sh FILE...
# Each FILE is a sealed capsule payload (capsule.json) or the
# capsule-seal-request/v1 request pr-capsule-request.sh writes. Exits 0 when
# every file decodes, 1 when any does not (each reason on stderr), 2 on usage.
set -euo pipefail

if [[ $# -eq 0 ]]; then
  printf 'usage: pr-capsule-check.sh FILE...\n' >&2
  exit 2
fi

# Prints why the references do not decode, or nothing when they do.
check='
  (if .spec_version == "capsule-seal-request/v1"
   then {t: .capsule.ActionType, r: [.capsule.References[]? | .Type]}
   else {t: .action_type, r: [.references[]? | .type]} end) as $c
  | $c.r as $r
  | ($r[2:]) as $rest
  | if $c.t != "fyi" and $c.t != "decide" then
      "action_type \($c.t | tojson) is neither fyi (head capsule) nor decide (decision capsule)"
    elif ($r | length) > 4 then
      "\($r | length) references; a PR capsule has at most 4"
    elif ($r | length) < 1 or $r[0] != "text/x-diff" then
      "references[0] must be the diff (text/x-diff), got \($r[0] | tojson)"
    elif ($r | length) < 2 or $r[1] != "application/json" then
      "references[1] must be the CI result (application/json), got \($r[1] | tojson)"
    elif $rest == [] or $rest == ["text/plain"] then
      empty
    elif $rest[0] == "application/json" and $c.t == "fyi" then
      "references[2] is a review entry (application/json) on a head capsule; reviews are recorded on the decision capsule only"
    elif $rest == ["application/json"] or $rest == ["application/json", "text/plain"] then
      empty
    else
      "references after the CI result are \($rest | tojson); only a review (application/json) then a prompt (text/plain) may follow, in that order"
    end
'

failed=0
for file in "$@"; do
  if ! reason=$(jq -r "$check" "$file"); then
    printf 'pr-capsule-check: %s: not a JSON object\n' "$file" >&2
    failed=1
  elif [[ -n "$reason" ]]; then
    printf 'pr-capsule-check: %s: %s\n' "$file" "$reason" >&2
    failed=1
  fi
done
exit "$failed"
