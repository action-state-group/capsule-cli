#!/usr/bin/env bash
# Writes a deal store the way capsulectl v0.1.0-rc16 does, for capsule-cli#242
# PR2b's migration tests: three deals (one closed, two open), two cadence ticks
# with deliveries pending to an unreachable witness, and an rc16 bundle of the
# closed deal.
#
# Usage: make.sh CAPSULECTL_RC16 SKILLS_DEAL_DEMO_RETAIL_CHECKOUT OUT_DIR; then
# replace the store directory in OUT_DIR/deal.yaml with @STORE@. CAPSULECTL_RC16
# is built from the v0.1.0-rc16 tag.
set -euo pipefail
bin="$1" demo="$2" out="$3"
work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT
export HOME="$work/home" XDG_CONFIG_HOME="$work/home/.config"
mkdir -p "$HOME"
c() { "$bin" --profile deal "$@"; }
"$bin" key generate --output "$work/witness.seed" > "$work/witness.json"
witness_pub=$(jq -r .public_key "$work/witness.json")
"$bin" deal init --profile deal --dir "$HOME/store" --no-witness >/dev/null
"$bin" profile update --profile deal --checkpoint-endpoint https://127.0.0.1:9 --checkpoint-public-key "$witness_pub" >/dev/null
printf 'cadence:\n  interval: 1m\n  jitter: 0s\n' >>"$XDG_CONFIG_HOME/capsule/profiles/deal.yaml"
paid() {
  local id
  id=$(c deal open --input "$demo/open.json" | jq -r .deal_id)
  c deal check --deal "$id" --input "$demo/check-pay.json" >/dev/null
  c deal note --deal "$id" --kind act --input "$demo/act-pay.json" >/dev/null
  echo "$id"
}
a=$(paid)
printf '%s' '{"status":"received","delivered":{"item":"cat sticker","quantity":1}}' >"$work/close.json"
c deal close --deal "$a" --input "$work/close.json" >/dev/null
b=$(paid)
c cll checkpoint cadence > "$work/tick1.json" || true
cc=$(paid)
c cll checkpoint cadence --wait-up-to 75s > "$work/tick2.json" || true
c bundle --deal "$a" --out "$work/bundle-a.json" >/dev/null
rm -rf "$out"; mkdir -p "$out"
cp -R "$HOME/store/." "$out/store/"
cp "$XDG_CONFIG_HOME/capsule/profiles/deal.yaml" "$out/deal.yaml"
cp "$work/witness.seed" "$out/witness.seed"
cp "$work/bundle-a.json" "$out/bundle-a.json"
jq -n --arg closed "$a" --arg open "$b" --arg later "$cc" --arg witness "$witness_pub" '{closed:$closed, open:$open, later:$later, witness_public_key:$witness}' > "$out/deals.json"
cat "$work/tick1.json" "$work/tick2.json"
