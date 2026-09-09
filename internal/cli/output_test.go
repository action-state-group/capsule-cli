package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOutputFlatObject(t *testing.T) {
	for _, value := range []any{
		map[string]any{"sequence": uint64(9007199254740993)},
		struct {
			Sequence uint64 `json:"sequence"`
		}{9007199254740993},
	} {
		var buf bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&buf)
		require.NoError(t, output(cmd, value))
		assert.JSONEq(t, `{"spec_version":"capsule-cli-result/v1","sequence":9007199254740993}`, buf.String())
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(buf.Bytes(), &fields))
		assert.NotContains(t, fields, "result")
		assert.Equal(t, "9007199254740993", string(fields["sequence"]))
	}
}

func TestOutputRejectsInvalidObjects(t *testing.T) {
	for _, value := range []any{nil, []string{"entry"}, "text", make(chan int), map[string]string{"spec_version": "collision"}} {
		var buf bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&buf)
		require.Error(t, output(cmd, value))
		assert.Empty(t, buf.String())
	}
}
