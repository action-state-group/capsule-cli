#!/usr/bin/env bash
# Writes downgrade.db for PR2b's downgrade test (capsule-cli#242 note §3.3):
# this directory's rc16 store, moved onto its shelf by this build's
# `deal tick`, then ticked once by capsulectl v0.1.0-rc16, which writes a
# deal_cadence row and its cadence-log tick past the shelf.
#
# Usage: make-downgrade.sh CAPSULECTL_RC16 CAPSULECTL_DEAL, where
# CAPSULECTL_DEAL is plugins/cmd/capsulectl-deal built from this checkout.
# Both runs are on the wall clock, after the fixture's due time.
set -euo pipefail
rc16="$1" deal="$2"
here="$(cd "$(dirname "$0")" && pwd)"
work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT
export HOME="$work/home" XDG_CONFIG_HOME="$work/home/.config"
mkdir -p "$XDG_CONFIG_HOME/capsule/profiles" "$work/store"
cp "$here"/store/{deal.db,signing.seed,checkpoint.seed} "$work/store/"
chmod 600 "$work"/store/*
sed "s|@STORE@|$work/store|g" "$here/deal.yaml" > "$XDG_CONFIG_HOME/capsule/profiles/deal.yaml"
chmod 600 "$XDG_CONFIG_HOME/capsule/profiles/deal.yaml"
"$deal" --profile deal tick >"$work/new-tick.json"
cat "$work/new-tick.json"
"$rc16" --profile deal cll checkpoint cadence >"$work/rc16-tick.json" || true
cat "$work/rc16-tick.json"
cp "$work/store/deal.db" "$here/downgrade.db"
