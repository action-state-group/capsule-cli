package cli

import (
	"encoding/json"
)

// This file is the only place that knows the sealed wire shape of a deal step.
// Everything else works with dealEvent. The deal record profile is still a
// draft; when it is published, switching the wire shape (record type names,
// field names, how counterparty identifiers are fingerprinted) is a change to
// these two functions and dealEventSpec, not to the rules or the commands.
//
// Today's shape: one JSON object per step, `spec` = "deal-event/v0", fields as
// declared on dealEvent. Sealed payloads are canonicalized with RFC 8785 JCS
// by the emit library when their digest is computed, and carry no floats.

func encodeDealRecord(ev dealEvent) ([]byte, error) {
	return json.Marshal(ev)
}

func decodeDealRecord(raw []byte) (dealEvent, error) {
	var ev dealEvent
	err := decodeJSON(raw, &ev)
	return ev, err
}
