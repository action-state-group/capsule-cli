package cli

import (
	_ "embed"
	"errors"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// This file is the only place that knows the sealed wire shape of a deal step:
// the x-deal-v0 record (skills/deal/profile/PROFILE.md). Everything else works
// with dealEvent, which holds raw values and lives only in the local store.
// A record is derived deterministically from its local event, the steps before
// it, and the deal key, so the local store can be re-checked against every
// sealed record on every read.

const (
	DealProfile   = "x-deal-v0"
	DealFPAlg     = "hmac-sha256-deal-key"
	DealRecordRef = "deal-record"
)

func FirstSchemaError(err error) string {
	var v *jsonschema.ValidationError
	if errors.As(err, &v) {
		leaves := leaves(v)
		if len(leaves) > 0 {
			l := leaves[0]
			return "/" + strings.Join(l.InstanceLocation, "/") + ": " + describeLeaf(l.ErrorKind)
		}
	}
	return "invalid record"
}
