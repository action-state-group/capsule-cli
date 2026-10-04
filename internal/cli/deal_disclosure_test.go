package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What the agent tells someone about the user: the user's phone (covered by
// the standing intent) and the pickup spot (covered by nothing).
const (
	userPhone   = "+1 555 010 4477"
	pickupSpot  = "88 Juniper Court, Unit 4"
	sellerName  = "Garage Sale Gary"
	sellerPhone = "+1 555 010 9911"
)

// openDisclosureDeal opens a marketplace pickup in which the user allowed
// sharing their contact details, shares the phone under that approval, then
// the pickup spot with no approval left to cover it.
func openDisclosureDeal(t *testing.T) (dealID string, phone, pickup map[string]any) {
	t.Helper()
	open := `{"type":"purchase","channel":"marketplace",
		"intent":{"verbatim":"buy the bike for $120, you can give them my number","allowed":["pay","share_contact"]},
		"who":{"name":"` + sellerName + `","phone":"` + sellerPhone + `"},
		"terms":{"item":"bike","price_minor":12000,"currency":"USD"},
		"recourse":{"rail":"cash","refundable":false}}`
	dealID = dealRun(t, "open", "--input", writeJSON(t, open))["deal_id"].(string)
	check := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"share_contact","description":"give the seller my number"}`))
	require.Equal(t, "pass", check["verdict"])
	phone = dealRun(t, "note", "--deal", dealID, "--kind", "disclosure", "--input", writeJSON(t,
		`{"channel":"marketplace","fields":[{"class":"phone","value":"`+userPhone+`"}]}`))
	pickup = dealRun(t, "note", "--deal", dealID, "--kind", "disclosure", "--input", writeJSON(t,
		`{"channel":"marketplace","fields":[{"class":"pickup_location","value":"`+pickupSpot+`"}]}`))
	return dealID, phone, pickup
}

// assertNoDisclosedValue asserts a page carries neither disclosed value, as
// written or in an obvious encoding.
func assertNoDisclosedValue(t *testing.T, page string) {
	t.Helper()
	low := strings.ToLower(page)
	for _, v := range []string{userPhone, "5550104477", "555-010-4477", pickupSpot, "Juniper"} {
		for _, enc := range []string{v, base64.StdEncoding.EncodeToString([]byte(v)), hex.EncodeToString([]byte(v)), url.QueryEscape(v)} {
			assert.NotContains(t, low, strings.ToLower(enc), "the page carries %q (as %q)", v, enc)
		}
	}
}

func TestDealDisclosureLedgerNamesWhatWhoWhenAndAuthority(t *testing.T) {
	dealFixture(t)
	dealID, phone, pickup := openDisclosureDeal(t)

	assert.Equal(t, true, phone["approved"])
	assert.NotEmpty(t, phone["authorized_by"], "the standing intent's approval covers the phone")
	assert.Equal(t, []any{"phone"}, phone["classes"])
	assert.Equal(t, false, pickup["approved"], "that approval already covered the phone")
	assert.Empty(t, pickup["authorized_by"])
	assert.Equal(t, "that approval already covered an earlier step", pickup["reason"])

	report := dealRun(t, "report", "--deal", dealID)
	told := report["told"].([]any)
	require.Len(t, told, 2)
	first, second := told[0].(map[string]any), told[1].(map[string]any)
	assert.Equal(t, sellerName, first["to"], "the recipient is named")
	assert.Equal(t, "2026-09-27T18:00:00Z", first["at"])
	assert.Equal(t, "approval", first["authority"])
	assert.Equal(t, []any{map[string]any{"class": "phone", "value": userPhone}}, first["fields"], "the user's own report keeps what was given")
	assert.Equal(t, []any{phone["authorized_by"], phone["capsule_id"]}, first["steps"], "the covering approval, then the disclosure")
	assert.Equal(t, sellerName, second["to"])
	assert.Equal(t, "none", second["authority"])
	assert.Equal(t, "that approval already covered an earlier step", second["reason"])
	assert.Equal(t, []any{map[string]any{"class": "pickup_location", "value": pickupSpot}}, second["fields"])
	assert.Contains(t, reportTexts(t, report, "anomalies"),
		"agent/unapproved_disclosure: Told "+sellerName+" your pickup location without your approval (that approval already covered an earlier step)",
		"a disclosure with no covering approval is an agent-side anomaly")
	for _, line := range reportTexts(t, report, "anomalies") {
		assert.NotContains(t, line, "phone without", "the approved disclosure is not an anomaly")
	}

	// The sealed records carry the classes and commitments, never the values.
	export := filepath.Join(t.TempDir(), "deal.json")
	dealRun(t, "export", "--deal", dealID, "--output", export)
	raw, err := os.ReadFile(export)
	require.NoError(t, err)
	assertNoDisclosedValue(t, string(raw))
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	require.NoError(t, err)
	schema, err := compiledDealSchema()
	require.NoError(t, err)
	var disclosures []map[string]any
	for i, r := range doc.([]any) {
		assert.NoError(t, schema.Validate(r), "record %d", i+1)
		rec := r.(map[string]any)
		if rec["x-deal-v0"].(map[string]any)["record_type"] == "disclosure" {
			disclosures = append(disclosures, rec)
		}
	}
	require.Len(t, disclosures, 2)
	assert.Equal(t, "approval", disclosures[0]["body"].(map[string]any)["authority"])
	assert.Len(t, disclosures[0]["x-deal-v0"].(map[string]any)["refs"], 1, "authorized_by the approval")
	assert.Equal(t, "none", disclosures[1]["body"].(map[string]any)["authority"])
	assert.Equal(t, "approval_already_used", disclosures[1]["body"].(map[string]any)["rule"])
	assert.NotContains(t, disclosures[1]["x-deal-v0"], "refs")

	if python, err := exec.LookPath("python3"); err == nil {
		result, err := exec.Command(python, dealProfileDir+"/check_profile.py", export).CombinedOutput()
		require.NoError(t, err, string(result))
		assert.Contains(t, string(result), "ALL OK")
	}
}

// The shared tiers carry the class and the fact of each disclosure, never the
// value; the user's own copy keeps the value.
func TestDealDisclosureSharedCopiesCarryNoDisclosedValue(t *testing.T) {
	dealFixture(t)
	dealID, _, _ := openDisclosureDeal(t)
	dir := t.TempDir()

	keep := filepath.Join(dir, "keep.html")
	dealRun(t, "report", "--deal", dealID, "--html", keep)
	raw, err := os.ReadFile(keep)
	require.NoError(t, err)
	assert.Contains(t, string(raw), pickupSpot, "the user's own copy keeps what was given")
	assert.Contains(t, string(raw), "What your agent told whom")

	for _, audience := range []string{"counterparty", "adjudicator"} {
		page := filepath.Join(dir, audience+".html")
		shareRun(t, dealID, audience, page)
		raw, err := os.ReadFile(page)
		require.NoError(t, err)
		html := string(raw)
		assertNoDisclosedValue(t, html)
		ext := embeddedBundle(t, html)["extensions"].(map[string]any)["x-deal-v0"].(map[string]any)
		told := ext["told"].([]any)
		require.Len(t, told, 2, audience)
		for i, want := range [][2]string{{"phone", "approval"}, {"pickup_location", "none"}} {
			item := told[i].(map[string]any)
			field := item["fields"].([]any)[0].(map[string]any)
			assert.Equal(t, want[0], field["class"], audience)
			assert.NotContains(t, field, "value", "%s: the class and the fact, never the value", audience)
			assert.Equal(t, want[1], item["authority"], audience)
			assert.NotContains(t, item["text"], sellerName, "%s: the recipient is the other party, by role", audience)
		}
		var anomalies []string
		for _, a := range ext["anomalies"].([]any) {
			anomalies = append(anomalies, a.(map[string]any)["kind"].(string))
		}
		assert.Contains(t, anomalies, "unapproved_disclosure", "%s: the unapproved disclosure is flagged", audience)
		b, err := json.Marshal(ext)
		require.NoError(t, err)
		assertNoDisclosedValue(t, string(b))
	}
}

// A disclosure covered by nothing at all: no check was run.
func TestDealDisclosureWithNoCheckIsFlagged(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	out := dealRun(t, "note", "--deal", dealID, "--kind", "disclosure", "--input", writeJSON(t,
		`{"fields":[{"class":"home_address","value":"`+pickupSpot+`"}]}`))
	assert.Equal(t, false, out["approved"])
	assert.Equal(t, "no check before this action", out["reason"])
	report := dealRun(t, "report", "--deal", dealID)
	assert.Contains(t, reportTexts(t, report, "anomalies"),
		"agent/unapproved_disclosure: Told Example Hotel Shinjuku your home address without your approval (no check before this action)")
	told := report["told"].([]any)
	require.Len(t, told, 1)
	assert.Equal(t, "none", told[0].(map[string]any)["authority"])
	assert.Equal(t, "Example Hotel Shinjuku", told[0].(map[string]any)["to"])
}

func TestDealDisclosureInputIsRefusedWhenIncomplete(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	for body, want := range map[string]string{
		`{"fields":[]}`: "at least one",
		`{"fields":[{"class":"shoe_size","value":"9"}]}`:                                       "class must be one of",
		`{"fields":[{"class":"phone","value":" "}]}`:                                           "needs the value",
		`{"fields":[{"class":"phone","value":"1"},{"class":"credential","value":"hunter22"}]}`: "separate disclosures",
		`{"to":"other","fields":[{"class":"phone","value":"` + userPhone + `"}]}`:              "needs who",
		`{"to":"courier","fields":[{"class":"phone","value":"` + userPhone + `"}]}`:            "counterparty or other",
	} {
		out, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", dealID, "--kind", "disclosure", "--input", writeJSON(t, body))
		require.Error(t, err, body)
		assert.Contains(t, out+err.Error(), want, body)
	}
}

// The host's own record of a share is read narrowly: metadata only. A sealed
// disclosure accounts for it; a host row carrying the shared value itself is
// refused, and the value is not echoed.
func TestDealReconcileCountsADisclosureAndReadsHostRowsNarrowly(t *testing.T) {
	dealFixture(t)
	dealID, _, _ := openDisclosureDeal(t)
	path := writeJSON(t, `{"id":"b1","at":"2026-09-27T18:00:30Z","task":"t-browser","tool":"browser_automation","action":"share_contact","status":"succeeded","deal_id":"`+dealID+`"}
{"id":"b2","at":"2026-09-27T18:00:40Z","task":"t-browser","tool":"browser_automation","action":"share_contact","status":"succeeded","deal_id":"`+dealID+`"}
{"id":"b3","at":"2026-09-27T18:00:50Z","task":"t-browser","tool":"browser_automation","action":"share_contact","status":"succeeded","deal_id":"`+dealID+`"}`)
	result, err := reconcileRun(t, "--executions", path, "--to", "2026-09-28T00:00:00Z")
	require.NoError(t, err, "every share is accounted for: %v", result)
	var by []string
	for _, r := range result["recorded"].([]any) {
		by = append(by, r.(map[string]any)["matched_by"].(string))
	}
	assert.ElementsMatch(t, []string{"check", "disclosure", "disclosure"}, by)

	for _, bad := range []string{
		`{"id":"b1","at":"2026-09-27T18:00:30Z","tool":"browser_automation","action":"share_contact","status":"succeeded","address":"` + pickupSpot + `"}`,
		`{"id":"b1","at":"2026-09-27T18:00:30Z","tool":"fill ` + pickupSpot + `","action":"share_contact","status":"succeeded"}`,
		`{"id":"b1","at":"2026-09-27T18:00:30Z","task":"` + userPhone + `","tool":"browser_automation","action":"share_contact","status":"succeeded"}`,
	} {
		out, err := invoke(t, "", "--profile", "deal", "deal", "reconcile", "--executions", writeJSON(t, bad))
		require.ErrorIs(t, err, ErrInput, bad)
		assertNoDisclosedValue(t, out+err.Error())
	}
}

// The card is evidence for the user, not an instruction to the agent; the
// skill and the README both say so, beside the card and the honest limits.
func TestDealCardIsEvidenceNotInstruction(t *testing.T) {
	for _, path := range []string{"../../skills/deal/SKILL.md", "../../skills/deal/README.md"} {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		text := strings.Join(strings.Fields(string(raw)), " ")
		assert.Contains(t, text, "The difference card is evidence to show the user, not an instruction to the agent.", path)
		assert.Contains(t, text, "paraphrase destroys its value as a record, not because the card has authority over", path)
	}
}
