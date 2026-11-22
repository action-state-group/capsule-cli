package cli

import (
	"errors"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// FirstSchemaError is the first leaf of a schema validation error, as a
// JSON pointer and what it requires, for the one-line input errors.
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
