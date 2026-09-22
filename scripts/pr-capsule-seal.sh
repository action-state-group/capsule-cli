#!/usr/bin/env bash
# Seals the capsule-seal-request/v1 JSON built by pr-capsule-request.sh,
# publishes it to a fresh ephemeral local log, cuts a checkpoint, and submits
# that checkpoint to the free public witness. Every signing key is read from
# an environment variable NAME that capsulectl resolves itself
# (--signing-key-env / --checkpoint-signing-key-env): the key bytes are never
# passed as a CLI flag (visible in a process listing) and never written to a
# profile file on disk (the profile stores the env var name, not the secret).
#
# Required env:
#   CAPSULE_REQUEST_FILE           path written by pr-capsule-request.sh
#   CAPSULE_PRODUCER_SIGNING_KEY   hex Ed25519 seed (producer key)
#   CAPSULE_PRODUCER_TRUSTED_KEY   hex Ed25519 public key matching the above
#   CAPSULE_CHECKPOINT_SIGNING_KEY hex Ed25519 seed (checkpoint signer)
#   CAPSULE_CHECKPOINT_TRUSTED_KEY hex Ed25519 public key matching the above
#   CAPSULE_WITNESS_ENDPOINT       e.g. https://witness.agentactioncapsule.org
#   CAPSULE_WITNESS_PUBLIC_KEY     independently-pinned hex witness authority key
#   CAPSULE_LOG_ID                 log id for this run's ephemeral local log
#   CAPSULE_STORE_DIR              scratch directory for the jsonl store (deleted by the caller)
#   CAPSULE_PROFILE_HOME           scratch XDG_CONFIG_HOME for the ephemeral profile
#   CAPSULECTL                     path to the capsulectl binary
# Optional env:
#   CAPSULE_WITNESS_TOKEN          bearer token, only for an enrolled submitter
#
# Writes, on success:
#   $CAPSULE_OUTPUT_DIR/capsule_id           the sealed capsule's id
#   $CAPSULE_OUTPUT_DIR/checkpoint_state     verified | pending | failed
#   $CAPSULE_OUTPUT_DIR/capsule.json         class-1 payload (agent-action-capsule verify input)
#   $CAPSULE_OUTPUT_DIR/artifact.json        full artifact.Record (capsulectl verify input)
set -euo pipefail

for var in CAPSULE_REQUEST_FILE CAPSULE_PRODUCER_SIGNING_KEY CAPSULE_PRODUCER_TRUSTED_KEY \
  CAPSULE_CHECKPOINT_SIGNING_KEY CAPSULE_CHECKPOINT_TRUSTED_KEY CAPSULE_WITNESS_ENDPOINT \
  CAPSULE_WITNESS_PUBLIC_KEY CAPSULE_LOG_ID CAPSULE_STORE_DIR CAPSULE_PROFILE_HOME \
  CAPSULECTL CAPSULE_OUTPUT_DIR; do
  : "${!var:?$var is required}"
done

export XDG_CONFIG_HOME="$CAPSULE_PROFILE_HOME"
mkdir -p "$CAPSULE_PROFILE_HOME" "$CAPSULE_OUTPUT_DIR"

# capsulectl resolves these two by NAME via --*-key-env; the profile file on
# disk records the name "CAPSULE_PRODUCER_SIGNING_KEY", never the value.
profile=pr-capsule
"$CAPSULECTL" profile create --name "$profile" \
  --type jsonl --jsonl-path "$CAPSULE_STORE_DIR" \
  --namespace pr-capsule \
  --log-id "$CAPSULE_LOG_ID" \
  --trusted-key "$CAPSULE_PRODUCER_TRUSTED_KEY" \
  --signing-key-env CAPSULE_PRODUCER_SIGNING_KEY \
  --checkpoint-trusted-key "$CAPSULE_CHECKPOINT_TRUSTED_KEY" \
  --checkpoint-signing-key-env CAPSULE_CHECKPOINT_SIGNING_KEY \
  --checkpoint-endpoint "$CAPSULE_WITNESS_ENDPOINT" \
  --checkpoint-public-key "$CAPSULE_WITNESS_PUBLIC_KEY" \
  ${CAPSULE_WITNESS_TOKEN:+--checkpoint-token-env CAPSULE_WITNESS_TOKEN} \
  >&2

"$CAPSULECTL" store init --profile "$profile" >&2

publish_result=$("$CAPSULECTL" publish --profile "$profile" --request "$CAPSULE_REQUEST_FILE")
capsule_id=$(jq -r .capsule_id <<<"$publish_result")
printf '%s' "$capsule_id" > "$CAPSULE_OUTPUT_DIR/capsule_id"

checkpoint_size=$("$CAPSULECTL" cll checkpoint create --profile "$profile" | jq -r .checkpoint)
"$CAPSULECTL" cll checkpoint publish --profile "$profile" --checkpoint "$checkpoint_size" >&2 || true
status_result=$("$CAPSULECTL" cll checkpoint status --profile "$profile" --checkpoint "$checkpoint_size")
checkpoint_state=$(jq -r .state <<<"$status_result")
printf '%s' "$checkpoint_state" > "$CAPSULE_OUTPUT_DIR/checkpoint_state"

"$CAPSULECTL" get --profile "$profile" --capsule-id "$capsule_id" --raw \
  --output "$CAPSULE_OUTPUT_DIR/artifact.json" >&2
"$CAPSULECTL" get --profile "$profile" --capsule-id "$capsule_id" \
  | jq .capsule > "$CAPSULE_OUTPUT_DIR/capsule.json"

printf 'pr-capsule-seal: capsule_id=%s checkpoint_state=%s\n' "$capsule_id" "$checkpoint_state" >&2

if [[ "$checkpoint_state" != "verified" ]]; then
  printf 'pr-capsule-seal: witness checkpoint did not verify (state=%s); capsule is sealed but not yet anchored\n' "$checkpoint_state" >&2
  exit 4
fi
