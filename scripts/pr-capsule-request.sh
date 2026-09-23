#!/usr/bin/env bash
# Builds a capsule-seal-request/v1 JSON document for one pull_request event.
#
# Every field that could carry free-form text (the diff, a prompt, CI job
# output) is reduced to a sha256 digest before it is ever written to a
# variable that could be logged; the digest, not the text, is what crosses
# into the request. Everything else (repo slug, PR number, SHAs, login,
# job-name -> conclusion) is already public GitHub metadata, not a secret.
#
# Required env:
#   CAPSULE_PR_REPO       owner/repo
#   CAPSULE_PR_NUMBER     pull request number
#   CAPSULE_PR_HEAD_SHA   head commit sha
#   CAPSULE_PR_BASE_SHA   merge-base / base commit sha
#   CAPSULE_PR_AUTHOR     PR author's declared GitHub login
#   CAPSULE_CI_JOBS_JSON  compact JSON object, job name -> conclusion
#   CAPSULE_REQUEST_OUTPUT  output path for the capsule-seal-request/v1 JSON
# Optional env:
#   CAPSULE_PR_REPO_DIR   git checkout to diff (default: cwd)
#   CAPSULE_PR_BODY       PR body text, passed via env (never interpolated
#                         into a shell command) so an agent/model declaration
#                         can be parsed out of it. A self-attested block looks
#                         like:
#                           <!-- capsule-declare
#                           agent_provider: anthropic
#                           agent_model: claude-sonnet-5
#                           prompt_digest_sha256: <64 hex, computed by the PR author locally>
#                           -->
#                         Every field is optional and unverified: it is the PR
#                         author's own claim, carried through as declared.
set -euo pipefail

: "${CAPSULE_PR_REPO:?CAPSULE_PR_REPO is required}"
: "${CAPSULE_PR_NUMBER:?CAPSULE_PR_NUMBER is required}"
: "${CAPSULE_PR_HEAD_SHA:?CAPSULE_PR_HEAD_SHA is required}"
: "${CAPSULE_PR_BASE_SHA:?CAPSULE_PR_BASE_SHA is required}"
: "${CAPSULE_PR_AUTHOR:?CAPSULE_PR_AUTHOR is required}"
: "${CAPSULE_CI_JOBS_JSON:?CAPSULE_CI_JOBS_JSON is required}"
: "${CAPSULE_REQUEST_OUTPUT:?CAPSULE_REQUEST_OUTPUT is required}"

repo_dir="${CAPSULE_PR_REPO_DIR:-.}"
pr_body="${CAPSULE_PR_BODY:-}"

# Diff digest: computed straight from git into sha256sum: the diff text
# itself is never assigned to a shell variable or written to disk.
diff_digest=$(git -C "$repo_dir" diff --no-color "${CAPSULE_PR_BASE_SHA}...${CAPSULE_PR_HEAD_SHA}" | sha256sum | cut -d' ' -f1)

# CI result digest: caller supplies job name -> conclusion only (small
# structured data, not log output); canonicalize key order before digesting
# so the same conclusions always produce the same digest.
ci_result_digest=$(jq -S -c . <<<"$CAPSULE_CI_JOBS_JSON" | sha256sum | cut -d' ' -f1)

# Self-attested agent/model declaration: parsed only from an explicit
# HTML-comment block the PR author opts into. Nothing else in the PR body is
# read, so an ordinary human PR with no such block yields no Model field.
declare_block=""
if [[ "$pr_body" == *"<!-- capsule-declare"* ]]; then
  declare_block=$(sed -n '/<!-- capsule-declare/,/-->/p' <<<"$pr_body")
fi
agent_provider=""
agent_model=""
prompt_digest=""
if [[ -n "$declare_block" ]]; then
  agent_provider=$(sed -n 's/^[[:space:]]*agent_provider:[[:space:]]*//p' <<<"$declare_block" | head -n1 | tr -d '\r')
  agent_model=$(sed -n 's/^[[:space:]]*agent_model:[[:space:]]*//p' <<<"$declare_block" | head -n1 | tr -d '\r')
  prompt_digest=$(sed -n 's/^[[:space:]]*prompt_digest_sha256:[[:space:]]*//p' <<<"$declare_block" | head -n1 | tr -d '\r')
  if [[ -n "$prompt_digest" && ! "$prompt_digest" =~ ^[0-9a-f]{64}$ ]]; then
    printf 'pr-capsule-request: prompt_digest_sha256 must be 64 lowercase hex chars; ignoring declared value\n' >&2
    prompt_digest=""
  fi
fi

short_head="${CAPSULE_PR_HEAD_SHA:0:12}"
action_id="pr-${CAPSULE_PR_REPO//\//-}-${CAPSULE_PR_NUMBER}-${short_head}"
# The head commit's own timestamp, not wall-clock: capsule_id is a digest
# over the whole payload including Timestamp (capsule-emit-go build.go),
# so a wall-clock value would mint a distinct capsule on every re-run of the
# same PR/head-sha even when nothing else changed. Using the commit's own
# timestamp makes the request (and so capsule_id) deterministic given the
# same head_sha and the same CI outcome — a re-run is then a true retry, not
# a new capsule.
timestamp=$(git -C "$repo_dir" show -s --format=%cI "$CAPSULE_PR_HEAD_SHA")

references='[]'
references=$(jq -c --arg d "$diff_digest" '. + [{"Type":"text/x-diff","DigestAlg":"sha256","Digest":$d,"CitationPurpose":"diff_digest"}]' <<<"$references")
references=$(jq -c --arg d "$ci_result_digest" '. + [{"Type":"application/json","DigestAlg":"sha256","Digest":$d,"CitationPurpose":"ci_result_digest"}]' <<<"$references")
if [[ -n "$prompt_digest" ]]; then
  references=$(jq -c --arg d "$prompt_digest" '. + [{"Type":"text/plain","DigestAlg":"sha256","Digest":$d,"CitationPurpose":"prompt_digest"}]' <<<"$references")
fi

model='null'
if [[ -n "$agent_provider" || -n "$agent_model" ]]; then
  model=$(jq -n --arg p "$agent_provider" --arg m "$agent_model" '{"Provider":$p,"ModelID":$m}')
fi

# emit.Seal rejects a request whose capsule.Model/Compute is set directly
# (it derives both from the request's top-level model/runtime fields), so the
# declared model goes on $.model, never nested under $.capsule.
jq -n \
  --arg action_id "$action_id" \
  --arg operator "$CAPSULE_PR_REPO" \
  --arg developer "$CAPSULE_PR_AUTHOR" \
  --arg timestamp "$timestamp" \
  --argjson references "$references" \
  --argjson model "$model" \
  '{
    "spec_version": "capsule-seal-request/v1",
    "capsule": {
      "ActionID": $action_id,
      "ActionType": "fyi",
      "Operator": $operator,
      "Developer": $developer,
      "Timestamp": $timestamp,
      "Domain": "action",
      "Provenance": "gate",
      "References": $references
    }
  } + (if $model == null then {} else {"model": $model} end)' > "$CAPSULE_REQUEST_OUTPUT"

printf 'pr-capsule-request: wrote %s (action_id=%s, diff_digest=%s, ci_result_digest=%s, agent_declared=%s)\n' \
  "$CAPSULE_REQUEST_OUTPUT" "$action_id" "$diff_digest" "$ci_result_digest" \
  "$([[ "$model" == "null" ]] && echo false || echo true)" >&2
