#!/bin/sh
# A neutral stub rules checker for tests: it reads the one record a deal
# check gives it on stdin and prints an external-check-result/v0. Its one
# rule is a per-action limit on body.spend_authorized_minor (else
# body.amount_minor): $1 is the limit in minor units. With no limit it
# refuses the input, as a checker does: non-zero exit, the cause on stderr.
limit=$1
if [ -z "$limit" ]; then
  echo "no limit given" >&2
  exit 2
fi
record=$(cat)
value=$(printf '%s' "$record" | tr -d '\n ' | sed -n 's/.*"spend_authorized_minor":\([0-9]*\).*/\1/p')
if [ -z "$value" ]; then
  value=$(printf '%s' "$record" | tr -d '\n ' | sed -n 's/.*"amount_minor":\([0-9]*\).*/\1/p')
fi
if [ -z "$value" ]; then
  echo "the record has no amount" >&2
  exit 1
fi
if [ "$value" -le "$limit" ]; then
  verdict=allow finding=pass reason=""
else
  verdict=deny finding=fail reason=',"reason":"over the per-action limit"'
fi
printf '{"schema":"external-check-result/v0","ruleset_id":"stub-rules/0.1.0","definition_digest":"%s","verdict":"%s","findings":[{"id":"per-action","check":"limit","verdict":"%s"%s,"limit":{"per_action_minor":%s},"value":{"minor":%s}}]}\n' \
  "0000000000000000000000000000000000000000000000000000000000000001" "$verdict" "$finding" "$reason" "$limit" "$value"
