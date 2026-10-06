package cli

import (
	"bytes"
	"os"
	"os/exec"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rc6Records is a deal sealed and exported by v0.1.0-rc6, whose check and
// verdict records carry pack_id (see its README).
const rc6Records = "testdata/deal/rc6-records/jet-ski-deal.json"

// Dropping pack_id from the required lists loosens the profile: every record
// sealed while it was written still validates, and still passes the profile's
// own checker.
func TestDealSchemaStillAcceptsRecordsThatNameAPack(t *testing.T) {
	schema, err := compiledDealSchema()
	require.NoError(t, err)
	raw, err := os.ReadFile(rc6Records)
	require.NoError(t, err)
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	require.NoError(t, err)

	named := map[string]bool{}
	for i, r := range doc.([]interface{}) {
		rec := r.(map[string]interface{})
		rt := rec["x-deal-v0"].(map[string]interface{})["record_type"].(string)
		if _, ok := rec["body"].(map[string]interface{})["pack_id"]; ok {
			named[rt] = true
		}
		assert.NoError(t, schema.Validate(r), "rc6 record %d (%s)", i+1, rt)
	}
	assert.Equal(t, map[string]bool{"check": true, "verdict": true}, named, "the rc6 deal's check and verdict name a pack")

	python := profilePython(t)
	if python == "" {
		return
	}
	result, err := exec.Command(python, dealProfileDir+"/check_profile.py", rc6Records).CombinedOutput()
	require.NoError(t, err, string(result))
	assert.Contains(t, string(result), "ALL OK")
}
