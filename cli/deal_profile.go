package cli

import (
	_ "embed"
	"errors"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The x-deal-v0 names verify reads deal bundles by. EvidenceBook's
// dealprofile package defines the profile (evidencebook#93), and the
// capsulectl-deal plugin (evidencebook, capsulectl/) builds its records. The core
// may not depend on EvidenceBook (scripts/check-neutral-deps.sh), so it keeps
// only the names its verifier reads, here and beside the code that reads
// them; the plugin, which depends on both, tests that each equals the
// profile's.

// DealProfile is the x-deal-v0 record block's, and a bundle extension's,
// member name.
const DealProfile = "x-deal-v0"

// DealProfileV1 is the x-deal-v1 profile's, and its bundle extension's, name
// (evidencebook#461, dealprofile/x-deal-v1/PROFILE.md). A v1 bundle names its
// sealed report in this extension, as a v0 bundle does in x-deal-v0; the
// report is always standalone, in x-deal-report/v0.
const DealProfileV1 = "x-deal-v1"

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
