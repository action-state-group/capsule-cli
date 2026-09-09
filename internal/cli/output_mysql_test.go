package cli

import (
	"encoding/json"
	"testing"

	"github.com/action-state-group/capsule-emit-go/artifact"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMySQLGetFlatOutput(t *testing.T) {
	p, key := mysqlProfile(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	target, err := openTarget(t.Context(), p, useInitialization)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.close()) })
	request, err := parseRequest(requestFixture(t))
	require.NoError(t, err)
	record, err := seal(request, key)
	require.NoError(t, err)
	record, err = artifact.Prepare(record)
	require.NoError(t, err)
	require.NoError(t, target.artifacts.Put(t.Context(), record))
	stored, err := target.artifacts.Get(t.Context(), record.CapsuleID)
	require.NoError(t, err)
	p.ReadOnly = true
	require.NoError(t, saveProfile(p, false))

	for _, raw := range []bool{false, true} {
		name := "readable"
		if raw {
			name = "raw"
		}
		t.Run(name, func(t *testing.T) {
			args := []string{"get", "--profile", p.Name, "--capsule-id", record.CapsuleID}
			if raw {
				args = append(args, "--raw")
			}
			out, err := invoke(t, "", args...)
			require.NoError(t, err)
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(out), &fields))
			assert.NotContains(t, fields, "result")
			assert.Equal(t, `"capsule-cli-result/v1"`, string(fields["spec_version"]))
			require.Contains(t, fields, "capsule")
			require.Contains(t, fields, "artifacts")
			expected, err := json.Marshal(getRecordOutput(stored, raw))
			require.NoError(t, err)
			delete(fields, "spec_version")
			actual, err := json.Marshal(fields)
			require.NoError(t, err)
			assert.JSONEq(t, string(expected), string(actual))
			if raw {
				var restored artifact.Record
				require.NoError(t, json.Unmarshal([]byte(out), &restored))
				assert.Equal(t, record.CapsuleID, restored.CapsuleID)
				assert.Equal(t, record.Capsule, restored.Capsule)
				assert.Equal(t, record.ProducerEnvelope, restored.ProducerEnvelope)
				assert.ElementsMatch(t, record.Artifacts, restored.Artifacts)
			} else {
				var capsule map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(fields["capsule"], &capsule))
				assert.NotEmpty(t, capsule)
			}
		})
	}
}
