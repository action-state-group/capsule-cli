package cli

import (
	_ "embed"
	"errors"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The x-deal-v0 names verify reads deal bundles by. EvidenceBook's
// dealprofile package defines the profile (evidencebook#93), and the
// capsulectl-deal plugin (evidencebook-deal) builds its records. The core
// may not depend on EvidenceBook (scripts/check-neutral-deps.sh), so it keeps
// only the names its verifier reads, here and beside the code that reads
// them; the plugin, which depends on both, tests that each equals the
// profile's.

// DealProfile is the x-deal-v0 record block's, and a bundle extension's,
// member name.
const DealProfile = "x-deal-v0"

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
