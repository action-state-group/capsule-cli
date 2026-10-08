package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bundleChain returns, by capsule id, the chain block of each Capsule the
// deal's bundle carries.
func bundleChain(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var b map[string]any
	require.NoError(t, json.Unmarshal(raw, &b))
	out := map[string]map[string]any{}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if id, ok := x["capsule_id"].(string); ok {
				if chain, ok := x["chain"].(map[string]any); ok {
					out[id] = chain
				}
			}
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(b["records"])
	return out
}

func exportRecords(t *testing.T, dealID string) ([]map[string]any, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "deal.json")
	dealRun(t, "export", "--deal", dealID, "--output", path)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var records []map[string]any
	require.NoError(t, json.Unmarshal(raw, &records))
	return records, path
}

func recordDigest(t *testing.T, r map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(r)
	require.NoError(t, err)
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	require.NoError(t, err)
	d, err := canonical.JSONDigest(v)
	require.NoError(t, err)
	return d
}

// profilePython is the python3 that runs the profile checker. In CI
// (CI set) a missing python3 fails the test: the checker must run there.
// Elsewhere it returns "" and the caller skips only the checker step.
func profilePython(t *testing.T) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("python3 is required in CI: the profile checker must run")
		}
		t.Log("python3 not available; the profile checker did not run")
		return ""
	}
	return python
}

func checkProfile(t *testing.T, export string) {
	t.Helper()
	python := profilePython(t)
	if python == "" {
		return
	}
	result, err := exec.Command(python, dealProfileDir+"/check_profile.py", export).CombinedOutput()
	require.NoError(t, err, string(result))
	assert.Contains(t, string(result), "ALL OK")
}

// A record sealed after the close is linked to it with the registered
// relation `confirms` (never `follows`, which is ordering only) and commits
// to the close record's digest. A closed deal takes nothing else.
func TestDealLateEvidenceConfirmsTheClose(t *testing.T) {
	dealFixture(t)
	setDealClock(t, "2026-10-03T21:00:00Z")
	stubDNS(t, map[string]string{merchantSelector + "._domainkey.shop.example": merchantKeyTXT(t), dmarcName: merchantDMARC(t)})
	dealID := openMerchantDeal(t)
	closed := dealRun(t, "close", "--deal", dealID, "--input", writeJSON(t, `{"status":"received"}`))
	closeID := closed["capsule_id"].(string)

	setDealClock(t, "2026-10-03T22:15:00Z")
	late := dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--email", filepath.Join(merchantFixture, "confirmation.eml"))
	lateID := late["capsule_id"].(string)

	// Its Capsule chains to the close with `confirms`. This would fail if a
	// late record were chained with `follows`.
	bundle := filepath.Join(t.TempDir(), "bundle.json")
	report := dealRun(t, "report", "--deal", dealID, "--bundle", bundle)
	chains := bundleChain(t, bundle)
	require.Contains(t, chains, lateID)
	assert.Equal(t, "confirms", chains[lateID]["relation"], "a late record never follows: follows is ordering only")
	assert.Equal(t, closeID, chains[lateID]["parent_capsule_id"])
	assert.Equal(t, "follows", chains[closeID]["relation"], "ordinary steps keep follows")

	// Its record commits to the close record's digest, not only its id.
	records, export := exportRecords(t, dealID)
	var closeRecord, lateRecord map[string]any
	for _, r := range records {
		switch r["x-deal-v0"].(map[string]any)["record_type"] {
		case "close":
			closeRecord = r
		case "evidence":
			lateRecord = r
		}
	}
	require.NotNil(t, closeRecord)
	refs := lateRecord["x-deal-v0"].(map[string]any)["refs"].([]any)
	var confirms map[string]any
	for _, r := range refs {
		if r.(map[string]any)["rel"] == "confirms" {
			confirms = r.(map[string]any)
		}
	}
	require.NotNil(t, confirms, "the late record carries a confirms ref")
	assert.Equal(t, recordDigest(t, closeRecord), confirms["digest"])
	checkProfile(t, export)

	// The receipt renders the chain and says more may be linked later.
	life := report["lifecycle"].(map[string]any)
	assert.Equal(t, "closed", life["state"])
	later := life["later"].([]any)
	require.Len(t, later, 1)
	assert.Equal(t, "confirms", later[0].(map[string]any)["relation"])
	assert.Equal(t, lateID, later[0].(map[string]any)["capsule_id"])
	assert.Contains(t, life["text"], "1 record sealed after the close is linked to it")
	assert.Contains(t, life["may_change"], "make a new report")

	// A shared copy says where the deal stands too, in fixed words.
	shared, raw := sharedCopy(t, dealID, dealAudienceCounterparty, "the shop")
	sl := dealReportOf(shared)["lifecycle"].(map[string]any)
	assert.Equal(t, "closed", sl["state"])
	require.Len(t, sl["later"], 1)
	assert.Equal(t, "a record sealed after the close", sl["later"].([]any)[0].(map[string]any)["text"])
	assert.NotContains(t, raw, "shop.example sent this email", "a linked record's own line stays in your copy")

	// Nothing but later evidence; no new cancel-by date.
	for _, args := range [][]string{
		{"--kind", "message", "--input", writeJSON(t, `{"from":"counterparty","text":"thanks"}`)},
		{"--kind", "evidence", "--input", writeJSON(t, `{"about":"x","source":"page_snapshot","obligation":{"kind":"renewal","cancel_by":"2026-11-01"}}`)},
	} {
		_, err := invoke(t, "", append([]string{"--profile", "deal", "deal", "note", "--deal", dealID}, args...)...)
		require.ErrorIs(t, err, ErrInput, args[1])
	}
	_, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"pay","amount_minor":100,"authorized_max_minor":100}`))
	require.ErrorIs(t, err, ErrInput)
}

// The acceptance run: the confirmation arrives an hour later, the shipping
// notice two days later, and delivery is never confirmed. At every stage
// the receipt reads honestly, and the open deal surfaces in `deal deadlines`
// without anyone remembering it.
func TestDealPurchaseLifecycleReadsHonestly(t *testing.T) {
	dealFixture(t)
	setDealClock(t, "2026-10-03T21:00:00Z")
	stubDNS(t, map[string]string{merchantSelector + "._domainkey.shop.example": merchantKeyTXT(t), dmarcName: merchantDMARC(t)})
	dealID := openMerchantDeal(t)
	lifecycle := func() map[string]any { return dealRun(t, "report", "--deal", dealID)["lifecycle"].(map[string]any) }
	openDeals := func(args ...string) []any {
		return dealRun(t, append([]string{"deadlines"}, args...)...)["open_deals"].([]any)
	}

	at0 := lifecycle()
	assert.Equal(t, "open", at0["state"])
	assert.Equal(t, "Open: waiting for the merchant's receipt. It is expected to close by 2026-10-17.", at0["text"])
	assert.Len(t, openDeals(), 1)

	setDealClock(t, "2026-10-03T22:00:00Z")
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--email", filepath.Join(merchantFixture, "confirmation.eml"))
	setDealClock(t, "2026-10-05T21:00:00Z")
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--input", writeJSON(t, `{"about":"the order shipped","source":"merchant_shipping_notice","detail":"tracking number on the notice"}`))
	at2 := lifecycle()
	assert.Equal(t, "open", at2["state"], "evidence on an open deal keeps it open")

	// Weeks later, nothing closed it: the listing proposes closing it, and
	// the receipt says what it holds.
	setDealClock(t, "2026-10-24T09:00:00Z")
	ics := filepath.Join(t.TempDir(), "deadlines.ics")
	listed := dealRun(t, "deadlines", "--ics", ics)["open_deals"].([]any)
	require.Len(t, listed, 1)
	l := listed[0].(map[string]any)
	assert.Equal(t, dealID, l["deal_id"])
	assert.Equal(t, true, l["past_expected"])
	cal, err := os.ReadFile(ics)
	require.NoError(t, err)
	assert.Contains(t, string(cal), "SUMMARY:Close deal "+dealID+"? (open\\, expected to close by 2026-10-17)")
	late := lifecycle()
	assert.Equal(t, "Open: the merchant's email is sealed; no close is sealed on this deal yet. It was expected to close by 2026-10-17, which has passed (as of 2026-10-24T09:00:00Z).", late["text"])
	assertStatesHoldingsOnly(t, late["text"].(string))

	// Closed at delivery, the deal leaves the listing.
	dealRun(t, "close", "--deal", dealID, "--input", writeJSON(t, `{"status":"received"}`))
	assert.Empty(t, openDeals())
}

// assertStatesHoldingsOnly: a receipt states what the deal holds, never what
// the user or anyone did or did not do.
func assertStatesHoldingsOnly(t *testing.T, text string) {
	t.Helper()
	conduct := regexp.MustCompile(`(?i)\b(did not|didn't|failed to|forgot|not cancell?ed|never cancell?ed|neglected|abandon(ed)?|missed)\b`)
	assert.Empty(t, conduct.FindAllString(text, -1), "a claim about conduct in: %s", text)
}

// The trial deal for the carried-obligation tests: a cancel-by date sealed
// from the merchant's own email, on a purchase that was paid.
func trialDeal(t *testing.T) string {
	t.Helper()
	setDealClock(t, "2026-10-03T21:00:00Z")
	dealID := openMerchantDeal(t)
	setDealClock(t, "2026-10-03T22:00:00Z")
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--email", filepath.Join(merchantFixture, "trial.eml"), "--input", writeJSON(t, `{
		"about": "Shop Example Plus trial", "source": "merchant_email",
		"obligation": {"kind": "trial_conversion", "cancel_by": "2026-10-16", "takes_effect": "2026-10-17", "amount_minor": 2400, "currency": "USD", "period": "month"}
	}`))
	return dealID
}

// Steven's ruling: (1) close refuses by default while a cancel-by date is
// open; (2) --carry-open-obligations closes, seals the open dates, and keeps
// them open after the close; (3) a carried date is resolved by a later
// record that confirms the close, or reads "date passed" as of when the
// listing or receipt is made, never sealed.
func TestDealCloseCarriesOpenObligations(t *testing.T) {
	dealFixture(t)
	stubDNS(t, map[string]string{merchantSelector + "._domainkey.shop.example": merchantKeyTXT(t), dmarcName: merchantDMARC(t)})
	dealID := trialDeal(t)

	// (1) The default is unchanged.
	_, err := invoke(t, "", "--profile", "deal", "deal", "close", "--deal", dealID, "--input", writeJSON(t, `{"status":"received"}`))
	require.ErrorIs(t, err, ErrInput)

	// (2) Opt-in: closed, the date sealed in the close and still open.
	setDealClock(t, "2026-10-04T10:00:00Z")
	closed := dealRun(t, "close", "--deal", dealID, "--carry-open-obligations", "--input", writeJSON(t, `{"status":"received"}`))
	require.Len(t, closed["carried_open_obligations"], 1)
	records, export := exportRecords(t, dealID)
	closeBody := records[len(records)-1]["body"].(map[string]any)
	carried := closeBody["carried_obligations"].([]any)
	require.Len(t, carried, 1)
	assert.Equal(t, "2026-10-16", carried[0].(map[string]any)["cancel_by"])
	checkProfile(t, export)

	ics := filepath.Join(t.TempDir(), "carried.ics")
	listing := dealRun(t, "deadlines", "--ics", ics)
	ds := listing["deadlines"].([]any)
	require.Len(t, ds, 1, "a carried date stays in the default listing")
	d := ds[0].(map[string]any)
	assert.Equal(t, "carried_at_close", d["status"])
	assert.Equal(t, "CARRIED AT CLOSE", d["marking"])
	assert.Equal(t, "closed", d["deal_state"])
	assert.Equal(t, dealID, d["deal_id"])
	assert.Empty(t, listing["open_deals"], "the deal itself is closed")
	cal, err := os.ReadFile(ics)
	require.NoError(t, err)
	summary := regexp.MustCompile(`(?m)^SUMMARY:(.*)\r$`).FindAllStringSubmatch(string(cal), -1)
	require.Len(t, summary, 1)
	assert.True(t, strings.HasPrefix(summary[0][1], "CARRIED AT CLOSE · "+dealID+" · last day to cancel: "), "the title alone says what it is: %s", summary[0][1])
	life := dealRun(t, "report", "--deal", dealID)["lifecycle"].(map[string]any)
	require.Len(t, life["carried"], 1)
	assert.Contains(t, life["text"], "1 cancel-by date(s) carried at the close")

	// (3a) A later record that confirms the close resolves it.
	setDealClock(t, "2026-10-04T16:10:00Z")
	resolved := dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--email", filepath.Join(merchantFixture, "cancelled.eml"),
		"--input", writeJSON(t, `{"about":"Plus cancelled","source":"merchant_email","resolves_step":6}`))
	assert.NotEmpty(t, resolved["capsule_id"])
	assert.Empty(t, dealRun(t, "deadlines")["deadlines"], "a resolved date leaves the open list")
	all := dealRun(t, "deadlines", "--all")["deadlines"].([]any)
	require.Len(t, all, 1)
	assert.Equal(t, "resolved", all[0].(map[string]any)["status"])
	assert.Equal(t, "RESOLVED", all[0].(map[string]any)["marking"])
	assert.Contains(t, all[0].(map[string]any)["holds"], "the merchant's own email, merchant-confirmed")
	_, export = exportRecords(t, dealID)
	checkProfile(t, export)
}

func TestDealCarriedObligationDatePassedIsComputedWhenRead(t *testing.T) {
	dealFixture(t)
	stubDNS(t, map[string]string{merchantSelector + "._domainkey.shop.example": merchantKeyTXT(t), dmarcName: merchantDMARC(t)})
	dealID := trialDeal(t)
	setDealClock(t, "2026-10-04T10:00:00Z")
	dealRun(t, "close", "--deal", dealID, "--carry-open-obligations", "--input", writeJSON(t, `{"status":"received"}`))
	before, _ := exportRecords(t, dealID)

	// (3b) The date passes with nothing resolving it sealed.
	setDealClock(t, "2026-10-20T08:30:00Z")
	all := dealRun(t, "deadlines", "--all")["deadlines"].([]any)
	d := all[0].(map[string]any)
	assert.Equal(t, "passed", d["status"])
	assert.Equal(t, "DATE PASSED", d["marking"])
	holds := d["holds"].(string)
	assert.Equal(t, "The date passed (as of 2026-10-20T08:30:00Z). No cancellation confirmation is sealed on this deal.", holds)
	assertStatesHoldingsOnly(t, holds)
	assert.Empty(t, dealRun(t, "deadlines")["deadlines"], "a passed date leaves the open list")

	// The rendered receipt says the same, in every form, and the wording
	// makes no claim about anyone's conduct.
	page := filepath.Join(t.TempDir(), "receipt.html")
	receipt := filepath.Join(t.TempDir(), "receipt.eml")
	report := dealRun(t, "report", "--deal", dealID, "--html", page, "--email", receipt)
	mail := report["email"].(map[string]any)
	for _, body := range []string{mail["text"].(string), mail["html"].(string)} {
		assert.Contains(t, body, "DATE PASSED")
		assert.Contains(t, body, "No cancellation confirmation is sealed on this deal.")
		section := body[strings.Index(body, "Cancel-by dates"):]
		assertStatesHoldingsOnly(t, section[:min(len(section), 600)])
	}

	// "Passed" was computed when read, never sealed: the records are the
	// same, and an earlier reading still says carried.
	after, _ := exportRecords(t, dealID)
	assert.Equal(t, before, after)
	setDealClock(t, "2026-10-10T08:30:00Z")
	again := dealRun(t, "deadlines")["deadlines"].([]any)
	require.Len(t, again, 1)
	assert.Equal(t, "carried_at_close", again[0].(map[string]any)["status"])
}

// We emit; we never place. The sealed date is ours; a reminder is the
// host's, best-effort. So no receipt ever says the user was reminded, in
// any of its forms.
func TestDealReceiptNeverSaysTheUserWasReminded(t *testing.T) {
	dealFixture(t)
	stubDNS(t, map[string]string{merchantSelector + "._domainkey.shop.example": merchantKeyTXT(t), dmarcName: merchantDMARC(t)})
	dealID := trialDeal(t)
	setDealClock(t, "2026-10-04T10:00:00Z")
	dealRun(t, "close", "--deal", dealID, "--carry-open-obligations", "--input", writeJSON(t, `{"status":"received"}`))
	reminded := regexp.MustCompile(`(?i)\breminded\b|\bwill be reminded\b|\bwe(?:'ll| will) remind\b`)
	for _, at := range []string{"2026-10-10T08:00:00Z", "2026-10-20T08:00:00Z"} {
		setDealClock(t, at)
		page := filepath.Join(t.TempDir(), "receipt.html")
		report := dealRun(t, "report", "--deal", dealID, "--html", page, "--email", filepath.Join(t.TempDir(), "r.eml"))
		raw, err := json.Marshal(report)
		require.NoError(t, err)
		html, err := os.ReadFile(page)
		require.NoError(t, err)
		ext, err := json.Marshal(embeddedBundle(t, string(html))["extensions"])
		require.NoError(t, err)
		for name, text := range map[string]string{"json": string(raw), "page data": string(ext), "page script": dealViewJS} {
			assert.Empty(t, reminded.FindAllString(text, -1), "%s at %s", name, at)
		}
		assert.Contains(t, string(raw), "carried")
	}
}
