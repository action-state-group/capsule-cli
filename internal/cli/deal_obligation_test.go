package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setDealClock(t *testing.T, at string) {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, at)
	require.NoError(t, err)
	dealClock = func() time.Time { return ts }
}

// A trial that becomes paid unless cancelled: the cancel-by date is sealed
// with the merchant's own email as its source, listed on every check, emitted
// for the host's calendar, and then answered by a sealed cancel and the
// merchant's own cancellation email. The report says what that proves.
func TestDealCancelByDateAndCancellationProof(t *testing.T) {
	dealFixture(t)
	offline := stubDNS(t, map[string]string{merchantSelector + "._domainkey.shop.example": merchantKeyTXT(t), dmarcName: merchantDMARC(t)})
	setDealClock(t, "2026-10-03T21:00:00Z")
	dealID := openMerchantDeal(t)

	// The trial email: proposed, then sealed with the obligation.
	setDealClock(t, "2026-10-03T22:00:00Z")
	trial := filepath.Join(merchantFixture, "trial.eml")
	raw, err := os.ReadFile(trial)
	require.NoError(t, err)
	m, err := captureEmail(raw, nil)
	require.NoError(t, err)
	hint := obligationHint(m.Parsed)
	require.NotNil(t, hint, "a trial email with a cancel-by date proposes an obligation")
	assert.Equal(t, "2026-10-16", hint.CancelBy)
	assert.Equal(t, int64(2400), *hint.AmountMinor)
	assert.Equal(t, "month", hint.Period)
	assert.Equal(t, "confirmation", m.Parsed.Kind)

	sealed := dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--email", trial, "--input", writeJSON(t, `{
		"about": "Shop Example Plus trial", "source": "merchant_email",
		"obligation": {"kind": "trial_conversion", "cancel_by": "2026-10-16", "takes_effect": "2026-10-17", "amount_minor": 2400, "currency": "USD", "period": "month",
			"terms": "After the trial, Plus is $24/month. Cancel by October 16, 2026 to avoid being charged."}
	}`))
	assert.Equal(t, "pass", sealed["dkim"])
	deadline := sealed["deadline"].(map[string]any)
	assert.Equal(t, "the trial becomes paid ($24.00/month) on 2026-10-17 unless cancelled by 2026-10-16", deadline["text"])
	assert.Equal(t, deadlineNotEnforced, deadline["note"])

	// Every check lists the open date.
	check := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"share_contact","description":"give the shop my phone for delivery","disclosing":["phone"]}`))
	open := check["open_deadlines"].([]any)
	require.Len(t, open, 1)
	assert.Equal(t, "2026-10-16", open[0].(map[string]any)["cancel_by"])
	assert.Equal(t, float64(13), open[0].(map[string]any)["days_left"])

	// The host's scheduler gets the date: JSON, and a calendar file.
	ics := filepath.Join(t.TempDir(), "deadlines.ics")
	list := dealRun(t, "deadlines", "--ics", ics, "--remind-days", "3")
	assert.Equal(t, false, list["enforced"])
	ds := list["deadlines"].([]any)
	require.Len(t, ds, 1)
	assert.Equal(t, "2026-10-13", ds[0].(map[string]any)["remind_on"])
	cal, err := os.ReadFile(ics)
	require.NoError(t, err)
	for _, want := range []string{"BEGIN:VCALENDAR", "DTSTART;VALUE=DATE:20261016", "TRIGGER;RELATED=START:-P3D", "SUMMARY:Last day to cancel (" + dealID + "): the trial becomes paid ($24.00/month) on 2026-10-17 unless cancelled by 2026-10-16", "we do not enforce it"} {
		assert.Contains(t, string(cal), want)
	}

	// Closing would end the record while the date is open.
	_, err = invoke(t, "", "--profile", "deal", "deal", "close", "--deal", dealID, "--input", writeJSON(t, `{"status":"received"}`))
	require.ErrorIs(t, err, ErrInput)

	// "I cancelled on the 4th": a checked, sealed cancel...
	setDealClock(t, "2026-10-04T15:30:00Z")
	dealRun(t, "note", "--deal", dealID, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"cancel the Plus trial before it charges me","allowed":["pay","cancel"]}`))
	cancel := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"cancel","description":"cancel Shop Example Plus"}`))
	require.Equal(t, "pass", cancel["verdict"], cancel["card"])
	act := dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"cancel","description":"cancelled Plus in the account settings"}`))
	require.Equal(t, false, act["unchecked"])
	// ...and the merchant's own cancellation email.
	setDealClock(t, "2026-10-04T16:10:00Z")
	conf := dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--email", filepath.Join(merchantFixture, "cancelled.eml"))
	assert.Equal(t, "cancellation", conf["parsed"].(map[string]any)["kind"])
	assert.Equal(t, true, conf["merchant_signed"])
	offline()

	page := filepath.Join(t.TempDir(), "receipt.html")
	receipt := filepath.Join(t.TempDir(), "receipt.eml")
	report := dealRun(t, "report", "--deal", dealID, "--html", page, "--email", receipt)
	cs := report["cancellations"].([]any)
	require.Len(t, cs, 1)
	c := cs[0].(map[string]any)
	assert.Equal(t, "confirmed", c["merchant"])
	assert.Equal(t, true, c["before_cancel_by"])
	assert.Equal(t, "2026-10-04T16:02:11Z", c["merchant_confirmed_at"])
	proven := strings.Join(toStrings(c["proven"]), "\n")
	notProven := strings.Join(toStrings(c["not_proven"]), "\n")
	assert.Contains(t, proven, "Your agent recorded a cancel at 2026-10-04T15:30:00Z")
	assert.Contains(t, proven, "dated on or before the cancel-by date (2026-10-16)")
	assert.Contains(t, proven, "The merchant's own signed email, dated 2026-10-04T16:02:11Z, says the cancellation went through (merchant-confirmed: ")
	assert.Contains(t, notProven, "That you will not be charged again")
	d := report["deadlines"].([]any)[0].(map[string]any)
	assert.Equal(t, "cancelled", d["status"])

	html, err := os.ReadFile(page)
	require.NoError(t, err)
	b := embeddedBundle(t, string(html))
	ext := b["extensions"].(map[string]interface{})["x-deal-v0"].(map[string]interface{})
	assert.Len(t, ext["cancellations"], 1)
	assert.Len(t, ext["deadlines"], 1)
	mail := report["email"].(map[string]any)
	for _, body := range []string{mail["text"].(string), strings.ReplaceAll(mail["html"].(string), "&#39;", "'")} {
		for _, want := range []string{"Cancel-by dates", "Your cancellation", "What this shows", "What this does not show", "That you will not be charged again", dealScopeLine} {
			assert.Contains(t, body, want)
		}
	}

	// Nothing is open now; --all still shows the cancelled date. The deal can close.
	assert.Empty(t, dealRun(t, "deadlines")["deadlines"])
	assert.Len(t, dealRun(t, "deadlines", "--all")["deadlines"], 1)
	dealRun(t, "close", "--deal", dealID, "--input", writeJSON(t, `{"status":"received"}`))

	// The records fit the profile and carry no raw terms or addresses.
	export := filepath.Join(t.TempDir(), "deal.json")
	dealRun(t, "export", "--deal", dealID, "--output", export)
	records, err := os.ReadFile(export)
	require.NoError(t, err)
	assert.Contains(t, string(records), `"obligation":{"amount_minor":2400,"cancel_by":"2026-10-16","currency":"USD","kind":"trial_conversion","period":"month","takes_effect":"2026-10-17","terms_commitment":`)
	assert.Contains(t, string(records), `"kind":"cancellation"`)
	for _, private := range []string{"Cancel by October 16", "sam.customer@mail.example", "cancelled Plus in the account settings"} {
		assert.NotContains(t, string(records), private)
	}
	if python, err := exec.LookPath("python3"); err == nil {
		result, err := exec.Command(python, dealProfileDir+"/check_profile.py", export).CombinedOutput()
		require.NoError(t, err, string(result))
		assert.Contains(t, string(result), "ALL OK")
	}
}

// The honest variants: a cancel after the date, with no merchant email; and a
// cancellation email that does not check out.
func TestDealCancellationWhatIsNotProven(t *testing.T) {
	dealFixture(t)
	stubDNS(t, map[string]string{merchantSelector + "._domainkey.shop.example": merchantKeyTXT(t), dmarcName: merchantDMARC(t)})
	setDealClock(t, "2026-10-03T21:00:00Z")
	dealID := openMerchantDeal(t)
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--input", writeJSON(t, `{
		"about": "Plus trial offer", "source": "page_snapshot",
		"obligation": {"kind": "trial_conversion", "cancel_by": "2026-10-16", "amount_minor": 2400, "period": "month"}
	}`))
	setDealClock(t, "2026-10-18T09:00:00Z")
	list := dealRun(t, "deadlines", "--all")["deadlines"].([]any)
	assert.Equal(t, "passed", list[0].(map[string]any)["status"])
	assert.Equal(t, false, list[0].(map[string]any)["source_merchant_confirmed"], "a page snapshot is our own record")
	dealRun(t, "note", "--deal", dealID, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"cancel it now","allowed":["cancel"]}`))
	dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"cancel","description":"cancel Plus"}`))
	dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"cancel"}`))

	raw, err := os.ReadFile(filepath.Join(merchantFixture, "cancelled.eml"))
	require.NoError(t, err)
	altered := filepath.Join(t.TempDir(), "altered.eml")
	require.NoError(t, os.WriteFile(altered, bytes.Replace(raw, []byte("You won't"), []byte("You will"), 1), 0o600))

	report := dealRun(t, "report", "--deal", dealID)
	c := report["cancellations"].([]any)[0].(map[string]any)
	assert.Equal(t, "none", c["merchant"])
	assert.Equal(t, false, c["before_cancel_by"])
	assert.Contains(t, strings.Join(toStrings(c["proven"]), "\n"), "dated AFTER the cancel-by date (2026-10-16)")
	assert.Contains(t, strings.Join(toStrings(c["not_proven"]), "\n"), "No cancellation email from the merchant has been sealed")

	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--email", altered)
	report = dealRun(t, "report", "--deal", dealID)
	c = report["cancellations"].([]any)[0].(map[string]any)
	assert.Equal(t, "not_confirmed", c["merchant"])
	assert.Contains(t, strings.Join(toStrings(c["not_proven"]), "\n"), "not confirmed: fails DMARC alignment (domain policy p=reject)")
	assert.NotContains(t, strings.Join(toStrings(c["proven"]), "\n"), "merchant's own signed email")
}

func TestObligationInputIsChecked(t *testing.T) {
	dealFixture(t)
	setDealClock(t, "2026-10-03T21:00:00Z")
	stubDNS(t, nil)
	dealID := openMerchantDeal(t)
	for _, bad := range []string{
		`{"about":"x","source":"page_snapshot","obligation":{"kind":"forever","cancel_by":"2026-10-16"}}`,
		`{"about":"x","source":"page_snapshot","obligation":{"kind":"renewal","cancel_by":"Oct 16"}}`,
		`{"about":"x","source":"page_snapshot","obligation":{"kind":"renewal","cancel_by":"2026-10-16","period":"fortnight"}}`,
	} {
		_, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", dealID, "--kind", "evidence", "--input", writeJSON(t, bad))
		require.ErrorIs(t, err, ErrInput, bad)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}
