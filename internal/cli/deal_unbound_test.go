package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dealReceipt writes a deal's receipt page (args add --share and the like)
// and returns the page's path and the bundle it carries.
func dealReceipt(t *testing.T, id string, args ...string) (string, map[string]interface{}) {
	t.Helper()
	page := filepath.Join(t.TempDir(), "receipt.html")
	dealRun(t, append([]string{"report", "--deal", id, "--html", page}, args...)...)
	raw, err := os.ReadFile(page)
	require.NoError(t, err)
	return page, embeddedBundle(t, string(raw))
}

// dealExtensionEntry is verify --bundle's entry for the x-deal-v0 extension.
func dealExtensionEntry(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	for _, x := range result["extensions"].([]any) {
		if entry := x.(map[string]any); entry["kind"] == dealProfile {
			return entry
		}
	}
	t.Fatalf("verify --bundle lists no %s extension: %v", dealProfile, result["extensions"])
	return nil
}

// A deal receipt's readable text rides in the x-deal-v0 extension, which no
// record seals. verify --bundle says so on the extension's entry and the page
// says so first; the verdict is unchanged. Editing that text still verifies
// VALID: this pins the gap the label states, until the extension is sealed.
func TestDealExtensionIsReportedUnbound(t *testing.T) {
	id := cancelAfterPurchase(t)
	page, b := dealReceipt(t, id)
	result, err := verifyBundleOutput(t, page)
	require.NoError(t, err)
	assert.Equal(t, "VALID", result["verdict"])
	entry := dealExtensionEntry(t, result)
	assert.Equal(t, "uninterpreted", entry["status"])
	assert.Equal(t, []any{"extension_unbound"}, entry["findings"])
	assert.Contains(t, entry["note"], "not verified")

	raw, err := os.ReadFile(page)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "this page does not check it")
	assert.Contains(t, string(raw), `dataset.unchecked = "x-deal-v0"`)

	ext := b["extensions"].(map[string]interface{})[dealProfile].(map[string]interface{})
	ext["did_line"] = "A line nobody sealed."
	steps := ext["steps"].([]interface{})
	require.NotEmpty(t, steps)
	steps[0].(map[string]interface{})["line"] = "A step line nobody sealed."
	edited := filepath.Join(t.TempDir(), "edited.json")
	data, err := json.Marshal(b)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(edited, data, 0o600))
	result, err = verifyBundleOutput(t, edited)
	require.NoError(t, err, "an edited extension still verifies: the text is unbound")
	assert.Equal(t, "VALID", result["verdict"])
	assert.Equal(t, []any{"extension_unbound"}, dealExtensionEntry(t, result)["findings"])

	// A shared copy carries the same finding.
	shared, _ := dealReceipt(t, id, "--share", "counterparty", "--to", "the airline's support desk")
	result, err = verifyBundleOutput(t, shared)
	require.NoError(t, err)
	assert.Equal(t, []any{"extension_unbound"}, dealExtensionEntry(t, result)["findings"])
}

// disclosedRecords are the record payloads a bundle discloses (withheld ones
// carry none).
func disclosedRecords(b map[string]interface{}) []map[string]interface{} {
	var out []map[string]interface{}
	for _, d := range b["disclosures"].(map[string]interface{}) {
		if r, ok := d.(map[string]interface{})["agent_input"].(map[string]interface{}); ok {
			out = append(out, r)
		}
	}
	return out
}

// Every record a step seals now declares its commitments' construction:
// x-deal-v0 records in their block, typed records in their header. A shared
// copy still discloses records that carry it (commit_alg is a vocabulary
// token, on the share list). A step sealed before it was declared re-derives
// without it (the rc6 records, TestDealSchemaStillAcceptsRecordsThatNameAPack
// and the re-derivation on every read).
func TestDealRecordsDeclareCommitAlg(t *testing.T) {
	id := cancelAfterPurchase(t)
	_, b := dealReceipt(t, id)
	records := disclosedRecords(b)
	require.NotEmpty(t, records)
	for _, r := range records {
		block, ok := r[dealProfile].(map[string]interface{})
		require.True(t, ok, "an x-deal-v0 deal seals x-deal-v0 records")
		assert.Equal(t, dealCommitAlg, block["commit_alg"], "record %v", block["record_type"])
	}

	_, shared := dealReceipt(t, id, "--share", "counterparty", "--to", "the airline's support desk")
	sharedRecords := disclosedRecords(shared)
	require.NotEmpty(t, sharedRecords, "a shared copy still discloses records that declare commit_alg")
	for _, r := range sharedRecords {
		assert.Equal(t, dealCommitAlg, r[dealProfile].(map[string]interface{})["commit_alg"])
	}

	typed := openTypedFlight(t)
	_, b = dealReceipt(t, typed)
	var typedSeen int
	for _, r := range disclosedRecords(b) {
		if block, ok := r[dealProfile].(map[string]interface{}); ok {
			assert.Equal(t, dealCommitAlg, block["commit_alg"])
			continue
		}
		typedSeen++
		assert.Equal(t, dealCommitAlg, r["commit_alg"], "typed record %v", r["type"])
	}
	assert.Positive(t, typedSeen, "a typed deal seals typed records")
}
