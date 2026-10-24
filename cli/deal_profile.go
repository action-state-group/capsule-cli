package cli

import (
	_ "embed"
	"errors"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The x-deal-v0 record's names, which verify reads deal bundles by. The
// capsulectl-deal plugin builds the records, and its deal skill's
// profile/PROFILE.md defines them (evidencebook-deal, capsule-cli#326).

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
