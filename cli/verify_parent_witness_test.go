package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// verify --bundle reports a parent-log-witness/v1 chain, the neutral form of
// a log's witnessed checkpoint (Evidence Bundle -03), from agent-action-capsule's
// strict reading: its claim gates the verdict, and the covered extent is
// computed from the verified checkpoints (capsule-cli#608).
func TestVerifyBundleReportsAParentLogWitnessChain(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	raw, err := os.ReadFile(filepath.Join("testdata", "parent-log-witness", "cases.json"))
	require.NoError(t, err)
	var doc struct {
		WitnessDirectory json.RawMessage `json:"witness_directory"`
		Cases            []struct {
			ID            string                 `json:"id"`
			Bundle        json.RawMessage        `json:"bundle"`
			Directory     bool                   `json:"directory"`
			Verdict       string                 `json:"verdict"`
			ParentWitness map[string]interface{} `json:"parent_witness"`
			Witnesses     map[string]interface{} `json:"witnesses"`
		} `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.NotEmpty(t, doc.Cases)
	dir := t.TempDir()
	directory := filepath.Join(dir, "witnesses.json")
	require.NoError(t, os.WriteFile(directory, doc.WitnessDirectory, 0o600))
	for _, c := range doc.Cases {
		t.Run(c.ID, func(t *testing.T) {
			path := filepath.Join(dir, c.ID+".json")
			require.NoError(t, os.WriteFile(path, c.Bundle, 0o600))
			dirArg := ""
			if c.Directory {
				dirArg = directory
			}
			result, err := verifyWithDirectory(t, path, dirArg)
			assert.Equal(t, c.Verdict, result["verdict"])
			assert.Equal(t, c.Verdict == "VALID", err == nil, "exit follows the verdict")
			if c.ParentWitness == nil {
				// The vector has no chain to report: only the checkpoint's own
				// witness claim is in play.
				require.NotNil(t, c.Witnesses)
				assert.Equal(t, c.Witnesses["status"], result["witnesses"].(map[string]interface{})["status"])
				return
			}
			var entry map[string]interface{}
			for _, x := range result["extensions"].([]interface{}) {
				if e := x.(map[string]interface{}); e["kind"] == "parent-log-witness/v1" {
					entry = e
				}
			}
			require.NotNil(t, entry, "the extension is listed")
			got, _ := entry["parent_witness"].(map[string]interface{})
			require.NotNil(t, got)
			assert.Equal(t, c.ParentWitness["status"], got["status"])
			assert.Equal(t, c.ParentWitness["status"], entry["status"])
			assert.ElementsMatch(t, c.ParentWitness["findings"], got["findings"])
			if want, ok := c.ParentWitness["extent"].(map[string]interface{}); ok && want != nil {
				extent := got["extent"].(map[string]interface{})
				assert.EqualValues(t, want["entries"], extent["entries"])
				assert.EqualValues(t, want["of"], extent["of"])
				assert.Equal(t, want["entries"] == want["of"], extent["whole"])
			} else {
				assert.Nil(t, got["extent"])
			}
			wantReceipts := c.ParentWitness["receipts"].([]interface{})
			gotReceipts, _ := got["receipts"].([]interface{})
			require.Len(t, gotReceipts, len(wantReceipts))
			for i, raw := range wantReceipts {
				want := raw.(map[string]interface{})
				receipt := gotReceipts[i].(map[string]interface{})
				for _, field := range []string{"ts_url", "binding", "status", "key_id", "registration_time", "registration_time_reason"} {
					assert.EqualValues(t, want[field], receipt[field], "receipt %d %s", i, field)
				}
			}
			if c.Witnesses != nil {
				// A receipt on the bundle's own checkpoint is the checkpoint's witness claim.
				assert.Equal(t, c.Witnesses["status"], result["witnesses"].(map[string]interface{})["status"])
			}
		})
	}
}
