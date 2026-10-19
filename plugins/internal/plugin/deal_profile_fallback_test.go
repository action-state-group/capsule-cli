package plugin

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fallbackSchemaStage runs the profile checker's schema stage with jsonschema
// made unimportable, so the reduced structural check runs whether or not the
// package is installed, on one minimal record per given record_type. It
// returns, per type, "" for accepted or the stage error.
const fallbackSchemaStage = `
import json, sys
sys.modules["jsonschema"] = None
sys.path.insert(0, sys.argv[1])
import check_profile as cp
out = {}
for rt in json.load(sys.stdin):
    rec = {"x-deal-v0": {"profile": "x-deal-v0", "canonicalization": "jcs", "deal_id": "deal-0123456789abcdef",
                         "record_type": rt, "seq": 1, "at": "2026-01-01T00:00:00Z"}, "body": {}}
    try:
        cp.stage_schema(rec)
        out[rt] = ""
    except cp.StageError as e:
        out[rt] = str(e)
assert cp.SCHEMA_MODE.startswith("fallback"), cp.SCHEMA_MODE
print(json.dumps(out))
`

// Without jsonschema the checker's reduced check accepts every record_type
// the schema declares, and still rejects one it does not.
func TestProfileFallbackAcceptsEverySchemaRecordType(t *testing.T) {
	python := profilePython(t)
	if python == "" {
		return
	}
	var schema struct {
		Defs struct {
			RecordType struct {
				Enum []string `json:"enum"`
			} `json:"record_type"`
		} `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(dealProfileSchema, &schema))
	types := schema.Defs.RecordType.Enum
	require.Contains(t, types, "counterparty_profile")

	input, err := json.Marshal(append(append([]string{}, types...), "payment"))
	require.NoError(t, err)
	cmd := exec.Command(python, "-I", "-c", fallbackSchemaStage, dealProfileDir)
	cmd.Stdin = strings.NewReader(string(input))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	require.NoError(t, err, stderr.String())
	var got map[string]string
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Len(t, got, len(types)+1)

	for _, rt := range types {
		assert.Empty(t, got[rt], "record_type %q", rt)
	}
	assert.Contains(t, got["payment"], "record_type")
}
