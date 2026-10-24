package deal

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// summaryStore is the sticker store and a jet-ski deal paid without
// approval: five deals, one of them a sale.
func summaryStore(t *testing.T) (closed, open, sale, thread, jetSki string) {
	t.Helper()
	closed, open, sale, thread = stickerStore(t)
	jetSki = openJetSki(t)
	check := dealRun(t, "check", "--deal", jetSki, "--input", filepath.Join(jetSkiDemo, "06-check-pay.json"))
	dealRun(t, "note", "--deal", jetSki, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "hold", "--said", "hold off for now")
	dealRun(t, "note", "--deal", jetSki, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":20000,"payee":"M. Torres","rail":"zelle"}`))
	return closed, open, sale, thread, jetSki
}

func listedSummaries(t *testing.T) map[string]dealSummary {
	t.Helper()
	raw := dealListOf(t, "--json")
	var list []dealListing
	require.NoError(t, json.Unmarshal([]byte(raw), &list), raw)
	out := map[string]dealSummary{}
	for _, l := range list {
		out[l.DealID] = l.Summary
	}
	return out
}

// `deal list --json` carries each deal's plain summary and changes no byte
// of the store (dealListOf compares the sha256 of every store file).
func TestDealListSummary(t *testing.T) {
	closed, open, sale, thread, jetSki := summaryStore(t)
	got := listedSummaries(t)
	require.Len(t, got, 5)
	usd := func(minor int64, basis string) *dealSummaryAmount {
		return &dealSummaryAmount{AmountMinor: minor, Currency: "USD", Basis: basis}
	}
	for id, want := range map[string]dealSummary{
		closed: {What: "Order the cat sticker in my cart at Example Stickers", Counterparty: "Example Stickers", Amount: usd(627, "paid"), Outcome: "completed",
			Text: "Order the cat sticker in my cart at Example Stickers · with Example Stickers · net $6.27 paid · completed"},
		open: {What: "Order the cat sticker in my cart at Example Stickers", Counterparty: "Example Stickers", Amount: usd(627, "opening_terms"), Outcome: "open",
			Text: "Order the cat sticker in my cart at Example Stickers · with Example Stickers · $6.27 in the opening terms · open"},
		sale: {What: "Sell my example bicycle; ask 1900, not under 1700", Counterparty: "not recorded", Amount: usd(190000, "opening_terms"), Outcome: "open",
			Text: "Sell my example bicycle; ask 1900, not under 1700 · counterparty not recorded · $1900.00 in the opening terms · open"},
		jetSki: {What: "rent me 2 jet skis Saturday", Counterparty: "Coastal Jet Rentals LLC", Amount: usd(20000, "paid"), Outcome: "open",
			Text: "rent me 2 jet skis Saturday · with Coastal Jet Rentals LLC · net $200.00 paid · open"},
		thread: {What: "Sell my example bicycle; ask 1900, not under 1700", Counterparty: "Example Buyer", Amount: usd(190000, "opening_terms"), Outcome: "open",
			Text: "Sell my example bicycle; ask 1900, not under 1700 · with Example Buyer · $1900.00 in the opening terms · open"},
	} {
		assert.Equal(t, want, got[id], id)
	}

	// The same words `deal report` gives (run after the list: it writes).
	// A sale has no report of its own (`deal report` takes a deal id).
	for _, id := range []string{closed, open, thread, jetSki} {
		report := dealRun(t, "report", "--deal", id)
		assert.Equal(t, report["asked"], got[id].What, id)
		assert.Equal(t, report["outcome"], got[id].Outcome, id)
		if money, ok := report["money"].(map[string]any); ok {
			assert.Equal(t, "paid", got[id].Amount.Basis, id)
			assert.EqualValues(t, money["net_minor"], got[id].Amount.AmountMinor, id)
		} else if got[id].Amount != nil {
			assert.NotEqual(t, "paid", got[id].Amount.Basis, id)
		}
	}
}

// A counterparty named only in free text (a message, the asked words) is
// "not recorded": the summary never reads a name out of prose.
func TestDealListSummaryNameOnlyInFreeText(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, `{"type": "purchase", "intent": {"verbatim": "Buy the lamp from Jo at Example Lamps"},
 "who": {"domain": "lamps.example"}, "terms": {"item": "lamp", "currency": "EUR"}, "recourse": {"rail": "card", "refundable": true}}`))["deal_id"].(string)
	dealRun(t, "note", "--deal", id, "--kind", "message", "--input", writeJSON(t, `{"from": "counterparty", "text": "Hi, this is Jo from Example Lamps"}`))
	got := listedSummaries(t)[id]
	assert.Equal(t, dealSummary{What: "Buy the lamp from Jo at Example Lamps", Counterparty: "not recorded", Outcome: "open",
		Text: "Buy the lamp from Jo at Example Lamps · counterparty not recorded · amount not recorded · open"}, got)
}

// Acts in a currency other than the first are counted, never summed, as
// `deal report` counts them; the summary's net is the report's.
func TestDealListSummaryOtherCurrencyActs(t *testing.T) {
	dealFixture(t)
	id := retailDeal(t)
	dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":300,"currency":"EUR","payee":"Example Stickers","rail":"card"}`))
	got := listedSummaries(t)[id]
	require.NotNil(t, got.Amount)
	assert.Equal(t, dealSummaryAmount{AmountMinor: 627, Currency: "USD", Basis: "paid", OtherCurrencyActs: 1}, *got.Amount)
	assert.Contains(t, got.Text, "net $6.27 paid, and 1 act(s) in another currency")
	money := dealRun(t, "report", "--deal", id)["money"].(map[string]any)
	assert.EqualValues(t, money["net_minor"], got.Amount.AmountMinor)
	assert.EqualValues(t, money["other_currency_acts"], got.Amount.OtherCurrencyActs)
}

// The asked words are the report's own bytes, spaces and all.
func TestDealListSummaryAskedVerbatim(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, `{"type": "purchase", "intent": {"verbatim": "  Buy the lamp  "},
 "who": {"name": "Example Lamps"}, "terms": {"item": "lamp"}, "recourse": {"rail": "card", "refundable": true}}`))["deal_id"].(string)
	got := listedSummaries(t)[id]
	assert.Equal(t, "  Buy the lamp  ", got.What)
	assert.Equal(t, dealRun(t, "report", "--deal", id)["asked"], got.What)
}
