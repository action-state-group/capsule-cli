#!/usr/bin/env bash
# The repository settings the release process depends on (docs/RELEASING.md,
# "Repository settings"). A repository admin runs this; nothing else applies
# these settings, and the release workflow cannot.
#
#   scripts/release-settings.sh            print what would be applied
#   scripts/release-settings.sh --apply    apply it, then print what GitHub holds
#   scripts/release-settings.sh --show     print what GitHub holds
#
# It sets:
#   1. Two deployment environments, each approved by one of REVIEWERS:
#      `release` for full releases, where whoever pushed the tag cannot approve
#      the run (prevent self-review), accepting only v* tags; and
#      `release-candidate` for pre-releases, where they can, accepting only
#      v*-* tags. Admins cannot bypass either.
#   2. Two tag rulesets on refs/tags/v*: only the maintain and admin roles may
#      create a release tag, and nobody, admins included, may move, update or
#      delete one.
#
# Re-running it is safe: environments are replaced, rulesets are updated in
# place by name, and tag patterns already present are kept.
set -euo pipefail

REPO="${REPO:-action-state-group/capsule-cli}"
REVIEWERS="${REVIEWERS:-ethanyzhang StevenMih}"
mode="${1:-}"
case "$mode" in
  "" | --apply | --show) ;;
  *) echo "usage: $0 [--apply | --show]" >&2; exit 2 ;;
esac

show() {
  for env in release release-candidate; do
    echo "== environment $env"
    gh api "repos/$REPO/environments/$env" \
      --jq '{can_admins_bypass, protection_rules, deployment_branch_policy}' || true
    gh api "repos/$REPO/environments/$env/deployment-branch-policies" \
      --jq '[.branch_policies[] | {name, type}]' || true
  done
  echo "== tag rulesets"
  for id in $(gh api "repos/$REPO/rulesets?targets=tag" --jq '.[].id'); do
    gh api "repos/$REPO/rulesets/$id" \
      --jq '{name, enforcement, conditions, rules: [.rules[].type], bypass_actors}'
  done
}

if [ "$mode" = --show ]; then
  show
  exit 0
fi

reviewers_json="$(for login in $REVIEWERS; do
  gh api "users/$login" --jq '{type: "User", id: .id}'
done | jq -s .)"

# environment NAME PREVENT_SELF_REVIEW TAG_PATTERN
environment() {
  local body
  body="$(jq -n --argjson reviewers "$reviewers_json" --argjson prevent "$2" '{
    wait_timer: 0,
    prevent_self_review: $prevent,
    reviewers: $reviewers,
    can_admins_bypass: false,
    deployment_branch_policy: {protected_branches: false, custom_branch_policies: true}
  }')"
  if [ "$mode" != --apply ]; then
    printf 'PUT repos/%s/environments/%s\n%s\n' "$REPO" "$1" "$body"
    printf 'POST repos/%s/environments/%s/deployment-branch-policies {"name": "%s", "type": "tag"}\n\n' "$REPO" "$1" "$3"
    return
  fi
  gh api -X PUT "repos/$REPO/environments/$1" --input - <<<"$body" >/dev/null
  if ! gh api "repos/$REPO/environments/$1/deployment-branch-policies" \
    --jq '.branch_policies[] | select(.type == "tag") | .name' | grep -Fqx "$3"; then
    gh api -X POST "repos/$REPO/environments/$1/deployment-branch-policies" \
      -f name="$3" -f type=tag >/dev/null
  fi
}

# ruleset NAME RULES_JSON BYPASS_JSON
ruleset() {
  local body id
  body="$(jq -n --arg name "$1" --argjson rules "$2" --argjson bypass "$3" '{
    name: $name,
    target: "tag",
    enforcement: "active",
    conditions: {ref_name: {include: ["refs/tags/v*"], exclude: []}},
    rules: $rules,
    bypass_actors: $bypass
  }')"
  if [ "$mode" != --apply ]; then
    printf 'PUT or POST repos/%s/rulesets\n%s\n\n' "$REPO" "$body"
    return
  fi
  id="$(gh api "repos/$REPO/rulesets?targets=tag" --jq ".[] | select(.name == \"$1\") | .id")"
  if [ -n "$id" ]; then
    gh api -X PUT "repos/$REPO/rulesets/$id" --input - <<<"$body" >/dev/null
  else
    gh api -X POST "repos/$REPO/rulesets" --input - <<<"$body" >/dev/null
  fi
}

environment release true 'v*'
environment release-candidate false 'v*-*'

# Repository role ids: 2 is maintain, 5 is admin.
ruleset "release tags: maintainers create" '[{"type": "creation"}]' \
  '[{"actor_id": 2, "actor_type": "RepositoryRole", "bypass_mode": "always"},
    {"actor_id": 5, "actor_type": "RepositoryRole", "bypass_mode": "always"}]'
ruleset "release tags: immutable" \
  '[{"type": "update"}, {"type": "deletion"}, {"type": "non_fast_forward"}]' '[]'

if [ "$mode" = --apply ]; then
  show
fi
