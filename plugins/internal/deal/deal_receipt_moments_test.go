package deal

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/action-state-group/capsule-cli/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The receipt moments of actionstate-muse#208 that capsulectl words: the
// first receipt, a called-off receipt and "show my receipts".

const fawnPaid = `{"action":"pay","amount_minor":190,"rail":"card","reference":"card ending 4242"}`

// pickedFawn opens the fawn errand (no item yet) and picks eBay's Fawn
// Sticker from the options shown.
func pickedFawn(t *testing.T) string {
	t.Helper()
	id := dealRun(t, "open", "--input", writeJSON(t, fawnErrand))["deal_id"].(string)
	note(t, id, "options", fawnOptions)
	require.Equal(t, "picked", note(t, id, "intent", `{"verbatim":"the eBay one","who":{"name":"eBay","domain":"ebay.com"}}`)["move"])
	return id
}

// The first receipt in the profile adds what it covers next to the
// merchant's and why to keep it; no later receipt says either again.
func TestTheFirstReceiptSaysWhyToKeepItOnce(t *testing.T) {
	dealFixture(t)
	keep := words("say.receipt.first.keep")
	contrast := cli.BuiltinWording().Line("say.receipt.first.contrast", cli.V{"merchant": "eBay"})

	first := pickedFawn(t)
	page := filepath.Join(t.TempDir(), "receipt.html")
	act := dealRun(t, "note", "--deal", first, "--kind", "act", "--html", page, "--input", writeJSON(t, fawnPaid))
	assert.True(t, strings.HasSuffix(act["say"].(string), "\n"+contrast+"\n"+keep+"\n"+words("report.view_details")), act["say"])
	plainSay(t, "first act", act)

	closed := dealRun(t, "close", "--deal", first, "--input", writeJSON(t, `{"status":"received"}`))
	assert.NotContains(t, closed["say"], keep, "the deal's second receipt moment")

	second := pickedFawn(t)
	again := dealRun(t, "note", "--deal", second, "--kind", "act", "--input", writeJSON(t, fawnPaid))
	assert.NotContains(t, again["say"], keep, "a later deal's receipt")
	assert.NotContains(t, again["say"], contrast)

	rep := dealRun(t, "report", "--deal", first)
	assert.NotContains(t, rep["say"], keep, "a report asked for later")
}

// A merchant whose sealed name is an id or a domain is not read out: the
// first receipt says only why to keep it.
func TestTheFirstReceiptNeverReadsOutAnIdAsAMerchant(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
	act := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, fawnPaid))
	assert.NotContains(t, act["say"], "sticker_shop_example")
	assert.True(t, strings.HasSuffix(act["say"].(string), "\n"+words("say.receipt.first.keep")), act["say"])
	for name, like := range map[string]bool{"eBay": false, "Sticker Shop": false, "sticker_shop_example": true, "marketplace.example": true, "shop@example.com": true} {
		assert.Equal(t, like, idLike.MatchString(name), name)
	}
}

// A first receipt for an errand called off says it is a record of what was
// not done, never what a missing item or a refund would need.
func TestAFirstCalledOffReceiptIsARecordOfWhatWasNotDone(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
	closed := dealRun(t, "close", "--deal", id, "--input", writeJSON(t, `{"status":"called_off"}`))
	assert.Equal(t, cli.Lines(words("say.close.called_off"), words("say.close.nothing_done"), words("say.receipt.first.keep.called_off")), closed["say"])
}

// A deal called off after a hold says the receipt shows nothing paid and
// nothing shared, and its receipt is still sent; once details were shared,
// only that nothing was paid.
func TestACalledOffReceiptSaysWhatItShowsWasNotDone(t *testing.T) {
	dealFixture(t)
	// A receipt moment elsewhere, so none below is the first.
	other := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
	dealRun(t, "note", "--deal", other, "--kind", "act", "--input", writeJSON(t, fawnPaid))

	held := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
	page := filepath.Join(t.TempDir(), "receipt.html")
	closed := dealRun(t, "close", "--deal", held, "--html", page, "--input", writeJSON(t, `{"status":"called_off"}`))
	assert.Equal(t, cli.VoiceReport, closed["voice"])
	assert.Equal(t, cli.Lines(words("say.close.called_off"), words("say.close.nothing_done"), words("report.view_details")), closed["say"])
	assert.Equal(t, map[string]any{"path": page, "media_type": "text/html", "label": words("report.private")}, closed["attach"])
	rep := dealRun(t, "report", "--deal", held)
	assert.True(t, strings.HasPrefix(rep["say"].(string), cli.Lines(words("say.close.called_off"), words("say.close.nothing_done"))), rep["say"])

	shared := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
	check := dealRun(t, "check", "--deal", shared, "--input", writeJSON(t, fawnShare))
	_, err := answer(t, shared, check["check_id"].(string), "yes share it", shownCard(t, check["card"].(string)))
	require.NoError(t, err)
	dealRun(t, "note", "--deal", shared, "--kind", "disclosure", "--input", writeJSON(t,
		`{"channel":"web","fields":[{"class":"email","value":"buyer@example.com"}]}`))
	closed = dealRun(t, "close", "--deal", shared, "--input", writeJSON(t, `{"status":"called_off"}`))
	assert.Equal(t, cli.Lines(words("say.close.called_off"), words("say.close.nothing_paid")), closed["say"])

	// A deal not chosen (another seller was) says the same.
	unchosen := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
	closed = dealRun(t, "close", "--deal", unchosen, "--input", writeJSON(t, `{"status":"not_selected"}`))
	assert.Equal(t, cli.Lines(words("say.close.called_off"), words("say.close.nothing_done")), closed["say"])
}

// A seller's thread records payments as evidence notes, not acts: a thread
// that ends unchosen never says nothing was paid.
func TestASellersCalledOffThreadClaimsNothingAboutPayment(t *testing.T) {
	dealFixture(t)
	sale, _ := newSale(t)
	thread := buyerThread(t, sale, "buyer.example")
	closed := dealRun(t, "close", "--deal", thread, "--input", writeJSON(t, `{"status":"not_selected"}`))
	assert.True(t, strings.HasPrefix(closed["say"].(string), words("say.close.called_off")), closed["say"])
	assert.NotContains(t, closed["say"], words("say.close.nothing_done"))
	assert.NotContains(t, closed["say"], words("say.close.nothing_paid"))
}

// listings reads `deal list` with args into each deal's say line by id.
func listings(t *testing.T, args ...string) map[string]string {
	t.Helper()
	raw := dealListOf(t, args...)
	var deals []map[string]any
	if args[len(args)-1] == "--json" {
		require.NoError(t, json.Unmarshal([]byte(raw), &deals), raw)
	} else {
		var r struct {
			Deals []map[string]any `json:"deals"`
		}
		require.NoError(t, json.Unmarshal([]byte(raw), &r), raw)
		deals = r.Deals
	}
	byID := map[string]string{}
	for _, d := range deals {
		say, _ := d["say"].(string)
		byID[d["deal_id"].(string)] = say
		plainSay(t, "listing", map[string]any{"say": say})
	}
	return byID
}

// "Show my receipts" reads `deal list --json`: each deal has its own line,
// with no id, plain or over a range.
func TestShowMyReceiptsSaysOneLinePerDeal(t *testing.T) {
	legacyDeals(t)
	closed, open, sale, thread := stickerStore(t)
	for _, args := range [][]string{{"--json"}, {"--json", "--since", "2000-01-01"}} {
		byID := listings(t, args...)
		assert.Regexp(t, `^[^:]+: bought( for \$[0-9.,]+)?, [A-Z][a-z]{2} \d{1,2}\.$`, byID[closed], args)
		assert.Regexp(t, `^[^:]+: open since [A-Z][a-z]{2} \d{1,2}\.$`, byID[open], args)
		assert.Regexp(t, `^[^:]+: open since `, byID[thread], args)
		assert.Regexp(t, `^[^:]+: listed for sale, [A-Z][a-z]{2} \d{1,2}\.$`, byID[sale], args)
	}
}

// A listing names the item the user picked, not the opening's; a paid deal
// that did not arrive is never "called off"; a demo says it is one.
func TestAListingSaysWhatWasBoughtAndHowItEnded(t *testing.T) {
	dealFixture(t)
	picked := pickedFawn(t)
	dealRun(t, "note", "--deal", picked, "--kind", "act", "--input", writeJSON(t, fawnPaid))
	dealRun(t, "close", "--deal", picked, "--input", writeJSON(t, `{"status":"received"}`))

	lost := pickedFawn(t)
	dealRun(t, "note", "--deal", lost, "--kind", "act", "--input", writeJSON(t, fawnPaid))
	dealRun(t, "close", "--deal", lost, "--input", writeJSON(t, `{"status":"not_received","reason":"not_delivered"}`))

	// Nothing paid: the opening's price is never said as paid.
	unpaid := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
	dealRun(t, "close", "--deal", unpaid, "--input", writeJSON(t, `{"status":"not_received","reason":"not_delivered"}`))

	demo := dealRun(t, "open", "--input", writeJSON(t, strings.Replace(fawnOpen, `"type":"purchase",`, `"type":"purchase","demo":true,`, 1)))["deal_id"].(string)

	byID := listings(t, "--json")
	assert.Regexp(t, `^Fawn Sticker: bought for \$1\.90, `, byID[picked])
	assert.Regexp(t, `^Fawn Sticker: paid \$1\.90, didn't arrive as agreed, `, byID[lost])
	assert.Regexp(t, `^Fawn Sticker: didn't arrive as agreed, [A-Z][a-z]{2} \d{1,2}\.$`, byID[unpaid])
	assert.Regexp(t, `^Fawn Sticker \(demo\): open since `, byID[demo])
}

// A listing's date has the year only when it is not this year's.
func TestAListingDateHasTheYearOnlyWhenItIsAnother(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.Local)
	clock := cli.DealClock
	cli.DealClock = func() time.Time { return now }
	t.Cleanup(func() { cli.DealClock = clock })
	assert.Equal(t, "Oct 20", listingDate(now.AddDate(0, 0, -1).Format(time.RFC3339)))
	assert.Equal(t, "Oct 20, 2025", listingDate(now.AddDate(-1, 0, -1).Format(time.RFC3339)))
}

// A seller's offer and commit are not receipt moments: the first receipt is
// the thread's close, and it says why a seller keeps it. The listing says it
// sold, with no amount: a seller's payments are evidence notes, not acts.
func TestASellersFirstReceiptIsTheThreadsClose(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	check := offerAt(t, id, "190000")
	offer := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"offer","amount_minor":190000}`))
	assert.NotContains(t, offer["say"], words("say.receipt.first.keep.sold"))
	_, err := acceptOffer(t, id, check)
	require.NoError(t, err)
	commit := commitNow(t, id, "190000")
	assert.NotContains(t, commit["say"], words("say.receipt.first.keep.sold"))
	closed := dealRun(t, "close", "--deal", id, "--input", writeJSON(t, `{"status":"received"}`))
	assert.True(t, strings.HasSuffix(closed["say"].(string), "\n"+words("say.receipt.first.keep.sold")), closed["say"])
	assert.NotContains(t, closed["say"], words("say.receipt.first.keep")+"\n")

	assert.Regexp(t, `^example bicycle \(demo\): sold, [A-Z][a-z]{2} \d{1,2}\.$`, listings(t, "--json")[id])
}

// A sale whose listing ended says so, dated when it ended.
func TestAnEndedSaleListsAsEnded(t *testing.T) {
	dealFixture(t)
	sale, _ := newSale(t)
	dealRun(t, "sale", "close", "--sale", sale)
	assert.Regexp(t, `^[^:]+: listing ended, [A-Z][a-z]{2} \d{1,2}\.$`, listings(t, "--json")[sale])
}

// A seller's open offer in one deal does not take the first receipt from a
// purchase in another, and the purchase's then takes it from the sale.
func TestASellersOfferLeavesTheFirstReceiptToAPurchase(t *testing.T) {
	dealFixture(t)
	sale := openTyped(t, sellerTyped)
	check := offerAt(t, sale, "190000")
	dealRun(t, "note", "--deal", sale, "--kind", "act", "--input", writeJSON(t, `{"action":"offer","amount_minor":190000}`))

	bought := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
	act := dealRun(t, "note", "--deal", bought, "--kind", "act", "--input", writeJSON(t, fawnPaid))
	assert.True(t, strings.HasSuffix(act["say"].(string), "\n"+words("say.receipt.first.keep")), act["say"])

	_, err := acceptOffer(t, sale, check)
	require.NoError(t, err)
	commitNow(t, sale, "190000")
	closed := dealRun(t, "close", "--deal", sale, "--input", writeJSON(t, `{"status":"received"}`))
	assert.NotContains(t, closed["say"], words("say.receipt.first.keep.sold"))
}
