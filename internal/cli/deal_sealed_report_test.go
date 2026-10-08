package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// writeBundleFile writes b as a bundle file and returns its path.
func writeBundleFile(t *testing.T, b map[string]interface{}) string {
	t.Helper()
	data, err := json.Marshal(b)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "bundle.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

// reportIDOf is the sealed report a bundle's x-deal-v0 extension names.
func reportIDOf(t *testing.T, b map[string]interface{}) string {
	t.Helper()
	id, ok := b["extensions"].(map[string]interface{})[dealProfile].(map[string]interface{})[dealReportPointer].(string)
	require.True(t, ok, "the extension names the copy's sealed report")
	return id
}

// A deal receipt's readable text is sealed with it: the copy's own deal
// section is a deal_report record on the deal's log, disclosed in the
// bundle, which the extension names. verify --bundle and the page check it
// like every record, so editing any line, or removing or replacing the
// extension, makes the bundle INVALID.
func TestDealReportTextIsSealed(t *testing.T) {
	id := cancelAfterPurchase(t)
	page, b := dealReceipt(t, id)
	result, err := verifyBundleOutput(t, page)
	require.NoError(t, err)
	assert.Equal(t, "VALID", result["verdict"])
	entry := dealExtensionEntry(t, result)
	assert.Equal(t, "pass", entry["status"])
	reportID := reportIDOf(t, b)
	assert.Equal(t, reportID, entry[dealReportPointer])
	raw, err := os.ReadFile(page)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `dataset.sealed = "x-deal-v0"`)
	report := dealReportOf(b)
	require.NotNil(t, report)
	assert.Equal(t, dealAudienceKeep, report["audience"])
	assert.NotEmpty(t, report["steps"])

	fresh := func() map[string]interface{} {
		var c map[string]interface{}
		data, err := json.Marshal(b)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(data, &c))
		return c
	}
	sealedText := func(c map[string]interface{}) map[string]interface{} { return dealReportOf(c) }
	for name, tamper := range map[string]func(map[string]interface{}){
		"a summary line": func(c map[string]interface{}) { sealedText(c)["did_line"] = "A line nobody sealed." },
		"a step line": func(c map[string]interface{}) {
			sealedText(c)["steps"].([]interface{})[0].(map[string]interface{})["line"] = "A step line nobody sealed."
		},
		"the money": func(c map[string]interface{}) { sealedText(c)["money"] = map[string]interface{}{"paid_minor": 1} },
		"an anomaly": func(c map[string]interface{}) {
			sealedText(c)["anomalies"] = []interface{}{map[string]interface{}{"kind": "x", "text": "Nobody sealed this."}}
		},
		"the extension removed": func(c map[string]interface{}) { delete(c["extensions"].(map[string]interface{}), dealProfile) },
		"the extension replaced by unsealed text": func(c map[string]interface{}) {
			text := sealedText(fresh())
			text["did_line"] = "A line nobody sealed."
			c["extensions"].(map[string]interface{})[dealProfile] = text
		},
		"the pointer moved to a step": func(c map[string]interface{}) {
			c["extensions"].(map[string]interface{})[dealProfile] = map[string]interface{}{dealReportPointer: c["root"]}
		},
	} {
		c := fresh()
		tamper(c)
		result, err := verifyBundleOutput(t, writeBundleFile(t, c))
		assert.ErrorIs(t, err, ErrBundleInvalid, name)
		assert.Equal(t, "INVALID", result["verdict"], name)
	}

	// A bundle written before reports were sealed (simulated: the text in
	// the extension, no sealed report disclosed) is VALID, with its text
	// reported unbound.
	legacy := fresh()
	legacy["extensions"].(map[string]interface{})[dealProfile] = sealedText(fresh())
	delete(legacy["disclosures"].(map[string]interface{}), reportID)
	result, err = verifyBundleOutput(t, writeBundleFile(t, legacy))
	require.NoError(t, err)
	entry = dealExtensionEntry(t, result)
	assert.Equal(t, "uninterpreted", entry["status"])
	assert.Equal(t, []any{"extension_unbound"}, entry["findings"])
}

// Each copy seals its own deal section, and a later copy discloses only its
// own: every earlier report's record is in the bundle (it is on the log the
// bundle covers) with its input withheld. A shared copy seals the section
// already rewritten for its audience, never the user's own.
func TestDealCopiesSealTheirOwnReport(t *testing.T) {
	id := cancelAfterPurchase(t)
	_, own := dealReceipt(t, id)
	ownID := reportIDOf(t, own)

	sharedPage, shared := dealReceipt(t, id, "--share", "counterparty", "--to", "the airline's support desk")
	sharedID := reportIDOf(t, shared)
	assert.NotEqual(t, ownID, sharedID)
	result, err := verifyBundleOutput(t, sharedPage)
	require.NoError(t, err)
	assert.Equal(t, "VALID", result["verdict"])
	assert.Equal(t, "pass", dealExtensionEntry(t, result)["status"])
	text := dealReportOf(shared)
	assert.Equal(t, dealAudienceCounterparty, text["audience"])
	assert.NotContains(t, text, "asked_opening", "the shared copy's sealed section is the rewritten one")
	assertWithheld := func(b map[string]interface{}, id, why string) {
		t.Helper()
		var onRecord bool
		for _, r := range b["records"].([]interface{}) {
			onRecord = onRecord || r.(map[string]interface{})["capsule_id"] == id
		}
		assert.True(t, onRecord, why)
		d, _ := b["disclosures"].(map[string]interface{})[id].(map[string]interface{})
		assert.NotContains(t, d, "agent_input", why)
	}
	assertWithheld(shared, ownID, "the user's own report is withheld from the shared copy")

	// Steps go on after reports, and the next copy withholds both.
	dealRun(t, "note", "--deal", id, "--kind", "message", "--input", writeJSON(t, `{"from":"counterparty","text":"Your refund is on its way."}`))
	_, later := dealReceipt(t, id)
	laterID := reportIDOf(t, later)
	assertWithheld(later, ownID, "an earlier own report is withheld")
	assertWithheld(later, sharedID, "an earlier shared report is withheld")
	assert.NotContains(t, []string{ownID, sharedID}, laterID)
	steps := dealReportOf(later)["steps"].([]interface{})
	assert.Len(t, steps, len(dealReportOf(own)["steps"].([]interface{}))+1, "the new step is in the report; the reports are not steps")
}

// A report row whose capsule never reached the log (a crash between the two
// writes) is set aside: the deal still loads and reports.
func TestDealReportRowWithoutAnEntryIsIgnored(t *testing.T) {
	id := cancelAfterPurchase(t)
	p, err := loadProfile("deal")
	require.NoError(t, err)
	db, _, err := sqliteConnection(p)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO deal_reports (deal_id, capsule_id, audience, cll_sequence) VALUES (?,?,?,0)`, id, strings.Repeat("ab", 32), dealAudienceKeep)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	page, _ := dealReceipt(t, id)
	_, err = verifyBundleOutput(t, page)
	require.NoError(t, err)
}

// disclosedRecords are the deal records a bundle discloses (withheld ones
// carry none), without the copy's sealed report.
func disclosedRecords(b map[string]interface{}) []map[string]interface{} {
	var out []map[string]interface{}
	for _, d := range b["disclosures"].(map[string]interface{}) {
		if r, ok := d.(map[string]interface{})["agent_input"].(map[string]interface{}); ok && r["type"] != "deal_report" {
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
