package plugin

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// statementStore is a store spread over three months, with its clock set
// per step:
//   - spans: a sticker paid in September and received in October;
//   - bought: a sticker paid and received in October;
//   - calledOff: the marketplace bike, where the agent told the seller the
//     user's phone (twice) and the pickup spot, then the user called it off;
//   - sale and its thread: the bicycle, sold to a buyer in October;
//   - late: opened in October, received in November;
//   - november: opened in November.
type statementDeals struct{ spans, bought, calledOff, sale, thread, late, november string }

func statementStore(t *testing.T) statementDeals {
	t.Helper()
	dealFixture(t)
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	old := cli.DealClock
	cli.DealClock = func() time.Time { return now }
	t.Cleanup(func() { cli.DealClock = old })
	on := func(month time.Month, day int) { now = time.Date(2026, month, day, 10, 0, 0, 0, time.UTC) }
	received := writeJSON(t, `{"status":"received","delivered":{"item":"cat sticker"}}`)
	var d statementDeals

	d.spans = retailDeal(t)
	on(10, 3)
	dealRun(t, "close", "--deal", d.spans, "--input", received)
	on(10, 5)
	d.bought = retailDeal(t)
	dealRun(t, "close", "--deal", d.bought, "--input", received)
	on(10, 10)
	d.calledOff, _, _ = openDisclosureDeal(t)
	on(10, 12)
	dealRun(t, "close", "--deal", d.calledOff, "--input", writeJSON(t, `{"status":"not_received","reason":"user_closed"}`))
	on(10, 15)
	d.sale, _ = newSale(t)
	d.thread = buyerThread(t, d.sale, "buyer.example")
	dealRun(t, "check", "--deal", d.thread, "--input", writeJSON(t, offerInput))
	on(10, 20)
	dealRun(t, "close", "--deal", d.thread, "--input", writeJSON(t, `{"status":"received","delivered":{"item":"example bicycle","quantity":1}}`))
	on(10, 28)
	d.late = dealRun(t, "open", "--input", filepath.Join(retailDemo, "open.json"))["deal_id"].(string)
	on(11, 3)
	dealRun(t, "close", "--deal", d.late, "--input", received)
	on(11, 4)
	d.november = dealRun(t, "open", "--input", filepath.Join(retailDemo, "open.json"))["deal_id"].(string)
	return d
}

func listRange(t *testing.T, args ...string) (dealListRange, string) {
	t.Helper()
	raw := dealListOf(t, append([]string{"--json"}, args...)...)
	var r dealListRange
	require.NoError(t, json.Unmarshal([]byte(raw), &r), raw)
	return r, raw
}

func listedByID(r dealListRange) map[string]dealListing {
	out := map[string]dealListing{}
	for _, l := range r.Deals {
		out[l.DealID] = l
	}
	return out
}

func toldByClass(r dealListRange) map[string]dealListTold {
	out := map[string]dealListTold{}
	for _, row := range r.Told {
		out[row.Class] = row
	}
	return out
}

// October: the deals with a step in it, each as it stood at its end, with
// the month's totals and who was told what.
func TestDealListRangeStatement(t *testing.T) {
	d := statementStore(t)
	r, raw := listRange(t, "--since", "2026-10-01", "--until", "2026-11-01")

	require.NotNil(t, r.Since)
	require.NotNil(t, r.Until)
	assert.Equal(t, "2026-10-01T00:00:00Z", *r.Since)
	assert.Equal(t, "2026-11-01T00:00:00Z", *r.Until)
	got := listedByID(r)
	assert.Len(t, got, 6)
	assert.NotContains(t, got, d.november, "no step in October")
	for id, want := range map[string]struct{ state, result string }{
		d.spans: {"closed", "bought"}, d.bought: {"closed", "bought"}, d.calledOff: {"closed", "called_off"},
		d.thread: {"closed", "sold"}, d.late: {"open", "open"},
	} {
		assert.Equal(t, want.state, got[id].State, id)
		assert.Equal(t, want.result, got[id].Result, id)
	}
	assert.Equal(t, "sale", got[d.sale].Kind)
	assert.Empty(t, got[d.sale].Result, "a sale's threads carry its result")
	assert.Equal(t, "2026-10-28T10:00:00Z", got[d.late].LastRecordAt, "read as it stood at the range's end: November's close is not in it")
	assert.Equal(t, "open", got[d.late].Summary.Outcome)

	assert.Equal(t, dealListTotals{Deals: 5, Sales: 1,
		Outcomes: dealListOutcomes{Bought: 2, Sold: 1, CalledOff: 1, Open: 1},
		// The September payment is September's; the bicycle is sold at
		// its sealed offer.
		Money: []dealListMoneyIn{{Currency: "USD", SpentOutMinor: 627, SoldForMinor: 190000}},
	}, r.Totals)

	told := toldByClass(r)
	assert.Len(t, told, len(disclosureClasses), "every known class, told or not")
	assert.Equal(t, dealListTold{Class: "phone", Recipients: 1, ToSellers: 1}, told["phone"], "told twice to the one seller")
	assert.Equal(t, dealListTold{Class: "pickup_location", Recipients: 1, ToSellers: 1}, told["pickup_location"])
	assert.Equal(t, dealListTold{Class: "credential"}, told["credential"], "told to no one")
	assertNoDisclosedValue(t, raw)
	assert.NotContains(t, raw, sellerPhone)

	// The text lines list the same deals.
	lines := strings.Split(strings.TrimSpace(dealListOf(t, "--since", "2026-10-01", "--until", "2026-11-01")), "\n")
	assert.Len(t, lines, len(r.Deals))
	for i, line := range lines {
		assert.True(t, strings.HasPrefix(line, r.Deals[i].DealID+"\t"), line)
	}

	// The owner's Statement copy is `deal history --audience keep` over
	// the deals picked (a sale has its own copy).
	args := []string{"history", "--audience", "keep", "--out", filepath.Join(t.TempDir(), "statement.json")}
	for _, l := range r.Deals {
		if l.Kind == "deal" {
			args = append(args, "--deal", l.DealID)
		}
	}
	history := dealRun(t, args...)
	assert.Len(t, history["deals"], 5)
}

// A deal open across the range's start is in both months: September has
// its payment and reads it open, October its close.
func TestDealListRangeSpansTheBoundary(t *testing.T) {
	d := statementStore(t)
	r, _ := listRange(t, "--since", "2026-09-01", "--until", "2026-10-01")
	require.Len(t, r.Deals, 1)
	assert.Equal(t, d.spans, r.Deals[0].DealID)
	assert.Equal(t, "open", r.Deals[0].State)
	assert.Equal(t, "open", r.Deals[0].Result)
	assert.Equal(t, dealListTotals{Deals: 1, Outcomes: dealListOutcomes{Open: 1},
		Money: []dealListMoneyIn{{Currency: "USD", SpentOutMinor: 627}}}, r.Totals)

	// Open-ended: from November on, the late deal's close and the
	// November deal, each as it stands now.
	r, _ = listRange(t, "--since", "2026-11-01")
	assert.Nil(t, r.Until)
	got := listedByID(r)
	assert.Len(t, got, 2)
	assert.Equal(t, "bought", got[d.late].Result)
	assert.Equal(t, "open", got[d.november].Result)

	// A time with its offset: October 1st in UTC-7 starts at 07:00 UTC.
	r, _ = listRange(t, "--since", "2026-10-01T00:00:00-07:00", "--until", "2026-10-04T00:00:00-07:00")
	assert.Equal(t, "2026-10-01T07:00:00Z", *r.Since)
	require.Len(t, r.Deals, 1)
	assert.Equal(t, d.spans, r.Deals[0].DealID)
	assert.Equal(t, "bought", r.Deals[0].Result)
}

// A range with nothing in it lists nothing, with zero totals and every
// class told to no one.
func TestDealListRangeEmpty(t *testing.T) {
	statementStore(t)
	r, _ := listRange(t, "--since", "2026-01-01", "--until", "2026-02-01")
	assert.Empty(t, r.Deals)
	assert.Equal(t, dealListTotals{Money: []dealListMoneyIn{}}, r.Totals)
	require.Len(t, r.Told, len(disclosureClasses))
	for _, row := range r.Told {
		assert.Equal(t, dealListTold{Class: row.Class}, row)
	}
	assert.Empty(t, strings.TrimSpace(dealListOf(t, "--since", "2026-01-01", "--until", "2026-02-01")))
}

// A store with no deal yet has an empty range too.
func TestDealListRangeNoStore(t *testing.T) {
	dealFixture(t)
	r, _ := listRange(t, "--until", "2026-11-01")
	assert.Empty(t, r.Deals)
	assert.NotNil(t, r.Deals)
}

func TestDealListRangeRefusesABadRange(t *testing.T) {
	dealFixture(t)
	for args, want := range map[[2]string]string{
		{"--since", "2026-13-01"}:          "--since must be a date",
		{"--until", "October"}:             "--until must be a date",
		{"--since", "2026-10-01T00:00:00"}: "--since must be a date",
		{"--since", "10/01/2026"}:          "--since must be a date",
	} {
		_, err := invoke(t, "", "--profile", "deal", "deal", "list", "--json", args[0], args[1])
		require.ErrorIs(t, err, cli.ErrInput, args)
		assert.Contains(t, err.Error(), want, args)
	}
	for _, r := range [][2]string{{"2026-10-01", "2026-10-01"}, {"2026-11-01", "2026-10-01"}} {
		_, err := invoke(t, "", "--profile", "deal", "deal", "list", "--since", r[0], "--until", r[1])
		require.ErrorIs(t, err, cli.ErrInput, r)
		assert.Contains(t, err.Error(), "--until must be after --since", r)
	}
}

// Recipients are counted as the counterparty memory reads parties: one
// identity across deals is one recipient; a marketplace stranger known by
// name alone is one per deal; someone else with no identity is one per
// telling; and a recipient is counted once in each role it had.
func TestDealListToldCountsDistinctParties(t *testing.T) {
	tally := newDealToldTally()
	shop := dealOpen{Who: dealWho{Name: "Example Lamps", Domain: "lamps.example"}}
	stranger := dealOpen{Channel: "marketplace", Who: dealWho{Name: "Sam"}}
	address := []dealDisclosureField{{Class: "address", Value: "1 Example St"}}
	tally.add("deal-a", 3, dealDisclosure{To: "counterparty", Fields: address}, shop, dealRoleBuyer)
	tally.add("deal-b", 4, dealDisclosure{To: "counterparty", Fields: address}, shop, dealRoleBuyer)
	tally.add("deal-c", 5, dealDisclosure{To: "counterparty", Fields: address}, stranger, dealRoleBuyer)
	tally.add("deal-c", 6, dealDisclosure{To: "counterparty", Fields: address}, stranger, dealRoleBuyer)
	tally.add("deal-d", 7, dealDisclosure{To: "counterparty", Fields: address}, stranger, dealRoleBuyer)
	tally.add("deal-e", 8, dealDisclosure{To: "counterparty", Fields: address}, stranger, dealRoleSeller)
	tally.add("deal-e", 9, dealDisclosure{To: "other", Who: &dealWho{Name: "a courier"}, Fields: address}, stranger, dealRoleSeller)
	tally.add("deal-e", 10, dealDisclosure{To: "other", Who: &dealWho{Name: "a courier"}, Fields: address}, stranger, dealRoleSeller)
	tally.add("deal-f", 11, dealDisclosure{To: "other", Who: &dealWho{Phone: "+1 555 010 7777"}, Fields: address}, shop, dealRoleBuyer)
	tally.add("deal-g", 12, dealDisclosure{To: "other", Who: &dealWho{Phone: "+1 555 010 7777"}, Fields: address}, shop, dealRoleBuyer)
	rows := tally.rows()
	byClass := map[string]dealListTold{}
	for _, r := range rows {
		byClass[r.Class] = r
	}
	// lamps.example once; Sam in deal-c and deal-d; the buyer in deal-e;
	// the courier twice; the phone once.
	assert.Equal(t, dealListTold{Class: "address", Recipients: 7, ToSellers: 3, ToBuyers: 1, ToOthers: 3}, byClass["address"])
	assert.Equal(t, dealListTold{Class: "phone"}, byClass["phone"])
}

// A sale thread is sold at its latest sealed offer or commit, else at its
// opening price; one with neither is counted unpriced.
func TestDealListSoldPrice(t *testing.T) {
	price := func(v int64) *int64 { return &v }
	opening := sealedEvent{Event: dealEvent{Kind: "open", Open: &dealOpen{Terms: dealTerms{PriceMinor: price(190000)}}}}
	act := func(action string, amount int64, currency string) sealedEvent {
		return sealedEvent{Event: dealEvent{Kind: "act", Act: &dealAct{Action: action, AmountMinor: price(amount), Currency: currency}}}
	}
	for name, c := range map[string]struct {
		events []sealedEvent
		minor  int64
		cur    string
		ok     bool
	}{
		"opening price":     {[]sealedEvent{opening}, 190000, "USD", true},
		"latest offer":      {[]sealedEvent{opening, act("offer", 185000, ""), act("offer", 180000, "")}, 180000, "USD", true},
		"commit after":      {[]sealedEvent{opening, act("offer", 185000, ""), act("commit", 175000, "eur")}, 175000, "eur", true},
		"a pay is no price": {[]sealedEvent{opening, act("pay", 100, "")}, 190000, "USD", true},
		"no price":          {[]sealedEvent{{Event: dealEvent{Kind: "open", Open: &dealOpen{}}}}, 0, "USD", false},
	} {
		minor, cur, ok := soldPrice(c.events, "USD")
		assert.Equal(t, c.minor, minor, name)
		assert.Equal(t, c.cur, cur, name)
		assert.Equal(t, c.ok, ok, name)
	}
}
