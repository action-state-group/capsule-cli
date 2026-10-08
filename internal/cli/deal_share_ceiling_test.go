package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openCeilingDeal opens a purchase whose opening record holds nothing else
// a shared copy withholds (an HMAC'd domain, a price and a currency), with a
// spending limit of $50.00 above the $48.00 price, or none.
func openCeilingDeal(t *testing.T, limit bool) string {
	t.Helper()
	intent := `{"verbatim": "buy it", "max_total_minor": 5000}`
	if !limit {
		intent = `{"verbatim": "buy it"}`
	}
	dealID := dealRun(t, "open", "--input", writeJSON(t, `{
		"type": "purchase", "channel": "web",
		"intent": `+intent+`,
		"who": {"domain": "shop.example"},
		"terms": {"price_minor": 4800, "currency": "USD"},
		"recourse": {"rail": "card", "refundable": true}
	}`))["deal_id"].(string)
	dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"pay","amount_minor":4800,"authorized_max_minor":4800}`))
	return dealID
}

// The counterparty never learns the user's spending limit; the adjudicator
// may (it is how "over your limit" is judged).
func TestDealSpendingLimitIsPrivateFromTheCounterparty(t *testing.T) {
	dealFixture(t)
	// Without a limit, the counterparty's copy discloses the opening record.
	twin := openCeilingDeal(t, false)
	b, _ := sharedCopy(t, twin, dealAudienceCounterparty, "x")
	ext := dealReportOf(b)
	require.Equal(t, false, ext["steps"].([]any)[0].(map[string]any)["withheld"])

	dealID := openCeilingDeal(t, true)
	b, raw := sharedCopy(t, dealID, dealAudienceCounterparty, "x")
	assert.NotContains(t, raw, "max_total_minor")
	assert.NotContains(t, raw, "$50.00")
	assert.NotContains(t, raw, "50.00")
	ext = dealReportOf(b)
	assert.Equal(t, true, ext["steps"].([]any)[0].(map[string]any)["withheld"], "the opening record carries the limit: withheld")
	assert.Contains(t, ext["withheld"], "your spending limit")

	_, raw = sharedCopy(t, dealID, dealAudienceAdjudicator, "x")
	assert.Contains(t, raw, `"max_total_minor":5000`, "the adjudicator's copy carries the limit")

	// The counterparty's page too.
	html := filepath.Join(t.TempDir(), "c.html")
	dealRun(t, "report", "--deal", dealID, "--html", html, "--share", "counterparty", "--to", "the shop")
	page, err := os.ReadFile(html)
	require.NoError(t, err)
	assert.NotContains(t, string(page), "max_total_minor")
	assert.NotContains(t, string(page), "$50.00")
}

// The gate on its own, as if withholding had missed the limit: a counterparty
// copy naming it, as the field or as money, is refused; the adjudicator's is
// not; and the limit is not refused where it equals the price the seller
// asked.
func TestDealCeilingGate(t *testing.T) {
	events := []sealedEvent{{Event: dealEvent{Kind: "open", Open: &dealOpen{
		Intent: dealIntent{MaxTotalMinor: ptrInt64(5000)},
		Terms:  dealTerms{PriceMinor: ptrInt64(4800), Currency: "USD"},
	}}}}
	for _, page := range []string{`{"intent":{"max_total_minor":5000}}`, "approved $50.00 (your limit)", "approved 50.00 USD"} {
		assert.Error(t, dealCeilingGate([]byte(page), events, dealAudienceCounterparty), page)
		assert.NoError(t, dealCeilingGate([]byte(page), events, dealAudienceAdjudicator), page)
	}
	for _, page := range []string{"pay $48.00 by card", "charged $150.00", "$500.00", "at 05:00:00 on 2026-10-03"} {
		assert.NoError(t, dealCeilingGate([]byte(page), events, dealAudienceCounterparty), page)
	}
	events[0].Event.Open.Terms.PriceMinor = ptrInt64(5000)
	assert.NoError(t, dealCeilingGate([]byte("pay $50.00 by card"), events, dealAudienceCounterparty), "the limit equals the asked price: the seller knows that amount")
	assert.True(t, strings.HasPrefix(dealBasisYourLimit, "your"))
}

func ptrInt64(v int64) *int64 { return &v }

// openShortMerchantDeal is the shortest merchant deal: open, a passing check
// (snapshot, check, approval) and the merchant's own email: five steps.
func openShortMerchantDeal(t *testing.T) string {
	t.Helper()
	dealID := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"Buy the sticker pack, no more than $50 all in","max_total_minor":5000,"allowed":["pay"]},
		"who":{"name":"Shop Example","domain":"shop.example"},
		"terms":{"item":"sticker pack","quantity":1,"price_minor":4800,"currency":"USD"},
		"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"pay","amount_minor":4800,"authorized_max_minor":4800}`))
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--email", filepath.Join(merchantFixture, "confirmation.eml"))
	return dealID
}

// A shared copy carries the merchant's email as its digests and the
// verification result, never the message: no header, no body, no name. The
// raw .eml stays in the local deal store (`deal verify-email` re-checks it
// there); not even the user's own bundle carries it.
func TestDealSharedMerchantEmailIsDigestAndVerdict(t *testing.T) {
	dealFixture(t)
	stubDNS(t, map[string]string{merchantSelector + "._domainkey.shop.example": merchantKeyTXT(t), dmarcName: merchantDMARC(t)})
	dealID := openShortMerchantDeal(t)
	for i, audience := range []string{dealAudienceCounterparty, dealAudienceAdjudicator} {
		b, raw := sharedCopy(t, dealID, audience, "x")
		require.Len(t, b["records"], 5+i+1, "the 5 steps and every copy's sealed report so far")
		assert.Contains(t, raw, `"message_digest":"`)
		assert.Contains(t, raw, `"key_records_digest":"`)
		assert.Contains(t, raw, `"dkim":"pass"`)
		for _, v := range []string{"DKIM-Signature", "Received:", "Subject:", "From:", "Content-Type", "Sam Customer", "sam.customer@mail.example", "orders@shop.example"} {
			assert.NotContains(t, raw, v, audience)
		}
	}

	own, err := invoke(t, "", "--profile", "deal", "bundle", "--deal", dealID)
	require.NoError(t, err)
	assert.NotContains(t, own, "DKIM-Signature", "the raw email is in no bundle")
}
