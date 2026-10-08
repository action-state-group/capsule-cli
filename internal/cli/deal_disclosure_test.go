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

// openDisclosureDeal opens a marketplace pickup in which the user approves
// giving the seller their phone and the pickup spot, which the agent does in
// one send; then the agent sends the phone again with no approval left to
// cover it (a repeat, so it is not held, and is flagged).
func openDisclosureDeal(t *testing.T) (dealID string, phone, pickup map[string]any) {
	t.Helper()
	open := `{"type":"purchase","channel":"marketplace",
		"intent":{"verbatim":"buy the bike for $120, you can give them my number","allowed":["pay","share_contact"]},
		"who":{"name":"` + sellerName + `","phone":"` + sellerPhone + `"},
		"terms":{"item":"bike","price_minor":12000,"currency":"USD"},
		"recourse":{"rail":"cash","refundable":false}}`
	dealID = dealRun(t, "open", "--input", writeJSON(t, open))["deal_id"].(string)
	// A first telling to a seller never dealt with: the user's own nod.
	check := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"share_contact","disclosing_to":"counterparty","description":"give the seller my number and where","disclosing":["phone","pickup_location"]}`))
	require.Equal(t, "pause", check["verdict"])
	approval := dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", "yes, give them my number")
	phone = dealRun(t, "note", "--deal", dealID, "--kind", "disclosure", "--input", writeJSON(t,
		`{"channel":"marketplace","fields":[{"class":"phone","value":"`+userPhone+`"},{"class":"pickup_location","value":"`+pickupSpot+`"}]}`))
	pickup = dealRun(t, "note", "--deal", dealID, "--kind", "disclosure", "--input", writeJSON(t,
		`{"channel":"marketplace","fields":[{"class":"phone","value":"`+userPhone+`"}]}`))
	require.Equal(t, approval["capsule_id"], phone["authorized_by"])
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
	assert.NotEmpty(t, phone["authorized_by"], "the user's approval covers the phone")
	assert.Equal(t, []any{"phone", "pickup_location"}, phone["classes"])
	assert.Equal(t, false, pickup["approved"], "that approval already covered the first send")
	assert.Empty(t, pickup["authorized_by"])
	assert.Equal(t, "that approval already covered an earlier step", pickup["reason"])

	report := dealRun(t, "report", "--deal", dealID)
	told := report["told"].([]any)
	require.Len(t, told, 2)
	first, second := told[0].(map[string]any), told[1].(map[string]any)
	assert.Equal(t, sellerName, first["to"], "the recipient is named")
	assert.Equal(t, "2026-09-27T18:00:00Z", first["at"])
	assert.Equal(t, "approval", first["authority"])
	assert.Equal(t, []any{map[string]any{"class": "phone", "value": userPhone}, map[string]any{"class": "pickup_location", "value": pickupSpot}}, first["fields"], "the user's own report keeps what was given")
	assert.Equal(t, []any{phone["authorized_by"], phone["capsule_id"]}, first["steps"], "the covering approval, then the disclosure")
	assert.Equal(t, sellerName, second["to"])
	assert.Equal(t, "none", second["authority"])
	assert.Equal(t, "that approval already covered an earlier step", second["reason"])
	assert.Equal(t, []any{map[string]any{"class": "phone", "value": userPhone}}, second["fields"])
	assert.Contains(t, reportTexts(t, report, "anomalies"),
		"agent/unapproved_disclosure: Told "+sellerName+" your phone without your approval (that approval already covered an earlier step)",
		"a disclosure with no covering approval is an agent-side anomaly")
	for _, line := range reportTexts(t, report, "anomalies") {
		assert.NotContains(t, line, "pickup location without", "the approved disclosure is not an anomaly")
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

	if python := profilePython(t); python != "" {
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
		ext := dealReportOf(embeddedBundle(t, html))
		told := ext["told"].([]any)
		require.Len(t, told, 2, audience)
		for i, want := range [][2]string{{"pickup_location", "approval"}, {"phone", "none"}} {
			item := told[i].(map[string]any)
			fields := item["fields"].([]any)
			field := fields[len(fields)-1].(map[string]any)
			assert.Equal(t, want[0], field["class"], audience)
			for _, f := range fields {
				assert.NotContains(t, f, "value", "%s: the class and the fact, never the value", audience)
			}
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

// A first telling covered by nothing at all (no check was run) is held when
// it is noted, before it is sent: nothing is sealed, and the step pauses.
func TestDealFirstTellingWithNoCheckIsHeld(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	out := heldNote(t, dealID, `{"fields":[{"class":"home_address","value":"`+pickupSpot+`"}]}`)
	assert.Equal(t, []any{"home_address"}, out["held"])
	assert.Equal(t, "First time telling Example Hotel Shinjuku your home address, and no check the user approved named it", out["reason"])
	assert.NotContains(t, out["reason"], pickupSpot)
	report := dealRun(t, "report", "--deal", dealID)
	assert.Empty(t, report["told"], "nothing was told: it was held")
}

// heldNote notes a disclosure that must be held: exit code 7, proceed false.
func heldNote(t *testing.T, dealID, body string) map[string]any {
	t.Helper()
	raw, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", dealID, "--kind", "disclosure", "--input", writeJSON(t, body))
	require.ErrorIs(t, err, ErrPaused, raw)
	assert.Equal(t, 7, ExitCode(err))
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw[:strings.LastIndex(raw, "}")+1]), &out), raw)
	assert.Equal(t, false, out["proceed"])
	assert.Equal(t, "pause", out["verdict"])
	return out
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
