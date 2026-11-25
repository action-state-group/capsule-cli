#!/usr/bin/env bash
# Copied from the reporter in the pull request asg-agent-team-ethan#409 cites
# (number 932); keep in sync. Only comments differ: the core names no company
# plugin (cli/neutral_names_test.go), so the source's issue link is reworded.
# Reports a red run on main or a nightly so it reaches a person the same day
# (the source's issue 925): comments the run link $RUN on the one open issue titled
# $TITLE, or opens it, labelled ci-red (the coordinator's watchdog lists open
# ci-red issues) and assigned to $ASSIGNEE when that is set. Two reporters
# racing can both open one; after opening, the reporter keeps the
# lowest-numbered open issue and closes the others. Needs GH_TOKEN with
# issues: write.
set -euo pipefail
: "${TITLE:?}" "${RUN:?}"
repo=$GITHUB_REPOSITORY

open_issues() {
  gh issue list --repo "$repo" --state open --search "\"$TITLE\" in:title" --json number,title \
    --jq "map(select(.title == env.TITLE) | .number) | sort | .[]"
}

n=$(open_issues | head -n 1)
if [[ -n "$n" ]]; then
  gh issue comment "$n" --repo "$repo" --body "Failed again: $RUN"
  exit 0
fi
gh label create ci-red --repo "$repo" --color B60205 \
  --description "A workflow failed on main or a nightly" 2>/dev/null || true
args=(--repo "$repo" --title "$TITLE" --label ci-red
  --body "Failed: $RUN. Close this issue once the workflow passes again.")
[[ -z "${ASSIGNEE:-}" ]] || args+=(--assignee "$ASSIGNEE")
gh issue create "${args[@]}"

# Search is eventually consistent: give a racing reporter's issue time to show.
sleep "${REPORT_SETTLE_SECONDS:-20}"
mapfile -t all < <(open_issues)
if ((${#all[@]} > 1)); then
  keep=${all[0]}
  for dup in "${all[@]:1}"; do
    gh issue close "$dup" --repo "$repo" --comment "Duplicate of #$keep."
  done
  gh issue comment "$keep" --repo "$repo" --body "Also failed: $RUN"
fi
