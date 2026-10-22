package plugin

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strconv"
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
//   - refunded: a sticker paid, then cancelled for a refund and a fee;
//   - sale and its two buyer threads, opened in September (the sale's own
//     log has no step after): the bicycle, sold in October to one buyer at
//     a price below the opening one, and the other buyer not chosen;
//   - late: opened in October, received in November;
//   - november: opened in November.
type statementDeals struct{ spans, bought, calledOff, refunded, sale, thread, notChosen, late, november string }

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
	on(9, 28)
	d.sale, _ = newSale(t)
	d.thread = buyerThread(t, d.sale, "buyer.example")
	d.notChosen = buyerThread(t, d.sale, "other-buyer.example")
	on(10, 3)
	dealRun(t, "close", "--deal", d.spans, "--input", received)
	on(10, 5)
	d.bought = retailDeal(t)
	dealRun(t, "close", "--deal", d.bought, "--input", received)
	on(10, 10)
	d.calledOff, _, _ = openDisclosureDeal(t)
	on(10, 12)
	dealRun(t, "close", "--deal", d.calledOff, "--input", writeJSON(t, `{"status":"not_received","reason":"user_closed"}`))
	on(10, 14)
	d.refunded = retailDeal(t)
	dealRun(t, "note", "--deal", d.refunded, "--kind", "act", "--input", writeJSON(t, `{"action":"cancel","amount_minor":627,"currency":"USD","fee_minor":100}`))
	on(10, 15)
	dealRun(t, "check", "--deal", d.thread, "--input", writeJSON(t, offerInput))
	on(10, 20)
	dealRun(t, "close", "--deal", d.thread, "--input", writeJSON(t, `{"status":"received","delivered":{"item":"example bicycle","quantity":1,"price_minor":185000,"currency":"USD"}}`))
	dealRun(t, "close", "--deal", d.notChosen, "--input", writeJSON(t, `{"status":"not_selected"}`))
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
	assert.Equal(t, "report", r.Voice, "the range is \"Your receipts\" (capsule-cli#345)")
	assert.Equal(t, "Your receipts: "+strconv.Itoa(r.Totals.Deals)+" errands in this period.", r.Say)

	require.NotNil(t, r.Since)
	require.NotNil(t, r.Until)
	assert.Equal(t, "2026-10-01T00:00:00Z", *r.Since)
	assert.Equal(t, "2026-11-01T00:00:00Z", *r.Until)
	got := listedByID(r)
	assert.Len(t, got, 8)
	assert.NotContains(t, got, d.november, "no step in October")
	assert.Contains(t, got, d.sale, "its log ends in September; its threads ran in October")
	for id, want := range map[string]struct{ state, result string }{
		d.spans: {"closed", "bought"}, d.bought: {"closed", "bought"}, d.calledOff: {"closed", "called_off"},
		d.refunded: {"open", "open"}, d.thread: {"closed", "sold"}, d.notChosen: {"closed", "not_chosen"}, d.late: {"open", "open"},
	} {
		assert.Equal(t, want.state, got[id].State, id)
		assert.Equal(t, want.result, got[id].Result, id)
	}
	assert.Equal(t, "sale", got[d.sale].Kind)
	assert.Empty(t, got[d.sale].Result, "a sale's threads carry its result")
	assert.Equal(t, "2026-10-28T10:00:00Z", got[d.late].LastRecordAt, "read as it stood at the range's end: November's close is not in it")
	assert.Equal(t, "open", got[d.late].Summary.Outcome)

	assert.Equal(t, dealListTotals{Deals: 7, Sales: 1,
		Outcomes: dealListOutcomes{Bought: 2, Sold: 1, CalledOff: 1, NotChosen: 1, Open: 2},
		// The September payment is September's. Paid out: two stickers
		// and the cancellation's fee; the refund is its own figure. The
		// bicycle is sold at the price its close sealed.
		Money: []dealListMoneyIn{{Currency: "USD", SpentOutMinor: 627 + 627 + 100, RefundedInMinor: 627, SoldForMinor: 185000}},
		// Checked: both stickers' pays and the bicycle's offer, and the
		// marketplace bike's telling, paused for the user's nod; no rules
		// checker is configured. The cancellation was sealed with no
		// check, and the second telling had no approval left to use.
		Checks: dealListChecks{Verdicts: map[string]int{"pass": 3, "pause": 1, "deny": 0},
			Rules:    map[string]int{"evaluated": 0, "partly_unanswered": 0, "not_evaluated": 0, "not_configured": 4, "not_applicable": 0, "unrecorded": 0},
			Rulesets: []dealListRuleset{}},
		Anomalies: map[string]int{"acted_against_answer": 0, "skipped_check": 1, "unsealed_approval": 0, "differs_from_check": 0, "unapproved_disclosure": 1, "unapproved_representation": 0},
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
	assert.Len(t, history["deals"], 7)
}

// A deal open across the range's start is in both months: September has
// its payment and reads it open, October its close.
func TestDealListRangeSpansTheBoundary(t *testing.T) {
	d := statementStore(t)
	r, _ := listRange(t, "--since", "2026-09-01", "--until", "2026-10-01")
	got := listedByID(r)
	require.Len(t, got, 4)
	assert.Equal(t, "open", got[d.spans].State)
	assert.Equal(t, "open", got[d.spans].Result)
	assert.Equal(t, "sale", got[d.sale].Kind)
	assert.Equal(t, "open", got[d.thread].Result, "sold in October")
	assert.Equal(t, dealListTotals{Deals: 3, Sales: 1, Outcomes: dealListOutcomes{Open: 3},
		Money: []dealListMoneyIn{{Currency: "USD", SpentOutMinor: 627}},
		Checks: dealListChecks{Verdicts: map[string]int{"pass": 1, "pause": 0, "deny": 0},
			Rules:    map[string]int{"evaluated": 0, "partly_unanswered": 0, "not_evaluated": 0, "not_configured": 1, "not_applicable": 0, "unrecorded": 0},
			Rulesets: []dealListRuleset{}},
		Anomalies: newDealListAnomalies()}, r.Totals)

	// A bound is exact: a step at --since is in the range, one at --until
	// is not. The spanning deal's close was sealed at 2026-10-03T10:00:00Z.
	r, _ = listRange(t, "--since", "2026-10-01", "--until", "2026-10-03T10:00:00Z")
	assert.Empty(t, r.Deals, "the close at --until is after the range")
	r, _ = listRange(t, "--since", "2026-10-03T10:00:00Z", "--until", "2026-10-03T10:00:01Z")
	require.Len(t, r.Deals, 1)
	assert.Equal(t, "bought", r.Deals[0].Result)
	// The same instant with an offset reads the same; an hour later does not.
	r, _ = listRange(t, "--since", "2026-10-03T03:00:00-07:00", "--until", "2026-10-03T04:00:00-07:00")
	assert.Len(t, r.Deals, 1)
	r, _ = listRange(t, "--since", "2026-10-03T03:00:00-08:00", "--until", "2026-10-03T04:00:00-08:00")
	assert.Empty(t, r.Deals)
	// A fraction of a second is kept in the bound echoed.
	r, _ = listRange(t, "--since", "2026-10-03T10:00:00.5Z")
	assert.Equal(t, "2026-10-03T10:00:00.5Z", *r.Since)

	// Open-ended: from November on, the late deal's close and the
	// November deal, each as it stands now.
	r, _ = listRange(t, "--since", "2026-11-01")
	assert.Nil(t, r.Until)
	got = listedByID(r)
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
	r, raw := listRange(t, "--since", "2026-01-01", "--until", "2026-02-01")
	assert.Empty(t, r.Deals)
	assert.Equal(t, dealListTotals{Money: []dealListMoneyIn{}, Checks: newDealListChecks(), Anomalies: newDealListAnomalies()}, r.Totals)
	// Present with every key at zero, so an absent member means not counted.
	assert.Contains(t, raw, `"checks":{"verdicts":{"deny":0,"pass":0,"pause":0},"rules":{"evaluated":0,"not_applicable":0,"not_configured":0,"not_evaluated":0,"partly_unanswered":0,"unrecorded":0},"rulesets":[]}`)
	assert.Contains(t, raw, `"anomalies":{"acted_against_answer":0,"differs_from_check":0,"skipped_check":0,"unapproved_disclosure":0,"unapproved_representation":0,"unsealed_approval":0}`)
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
	tally.add("deal-a", dealDisclosure{To: "counterparty", Fields: address}, shop, dealRoleBuyer)
	tally.add("deal-b", dealDisclosure{To: "counterparty", Fields: address}, shop, dealRoleBuyer)
	tally.add("deal-c", dealDisclosure{To: "counterparty", Fields: address}, stranger, dealRoleBuyer)
	tally.add("deal-c", dealDisclosure{To: "counterparty", Fields: address}, stranger, dealRoleBuyer)
	tally.add("deal-d", dealDisclosure{To: "counterparty", Fields: address}, stranger, dealRoleBuyer)
	tally.add("deal-e", dealDisclosure{To: "counterparty", Fields: address}, stranger, dealRoleSeller)
	tally.add("deal-e", dealDisclosure{To: "other", Who: &dealWho{Name: "a courier"}, Fields: address}, stranger, dealRoleSeller)
	tally.add("deal-e", dealDisclosure{To: "other", Who: &dealWho{Name: "a courier"}, Fields: address}, stranger, dealRoleSeller)
	tally.add("deal-f", dealDisclosure{To: "other", Who: &dealWho{Phone: "+1 555 010 7777"}, Fields: address}, shop, dealRoleBuyer)
	tally.add("deal-g", dealDisclosure{To: "other", Who: &dealWho{Phone: "+1 555 010 7777"}, Fields: address}, shop, dealRoleBuyer)
	byClass := map[string]dealListTold{}
	for _, r := range tally.rows() {
		byClass[r.Class] = r
	}
	// lamps.example once; Sam in deal-c and deal-d; the buyer in deal-e;
	// the courier twice; the phone once.
	assert.Equal(t, dealListTold{Class: "address", Recipients: 7, ToSellers: 3, ToBuyers: 1, ToOthers: 3}, byClass["address"])
	assert.Equal(t, dealListTold{Class: "phone"}, byClass["phone"])
}

// Recipients linked through a shared identity are one, in whatever order
// they were told; a deal's counterparty, named only later by an identity,
// is still one.
func TestDealListToldJoinsLinkedIdentities(t *testing.T) {
	phone := []dealDisclosureField{{Class: "phone", Value: "+1 555 010 4477"}}
	byEmail := &dealWho{Email: "a@example.com"}
	byBoth := &dealWho{Email: "a@example.com", Phone: "+1 555 010 1234"}
	byPhone := &dealWho{Phone: "+1 555 010 1234"}
	for _, order := range [][]*dealWho{{byEmail, byPhone, byBoth}, {byBoth, byEmail, byPhone}, {byPhone, byEmail, byBoth}} {
		tally := newDealToldTally()
		for i, w := range order {
			tally.add("deal-"+string(rune('a'+i)), dealDisclosure{To: "other", Who: w, Fields: phone}, dealOpen{}, dealRoleBuyer)
		}
		assert.Equal(t, 1, tally.rows()[slices.IndexFunc(tally.rows(), func(r dealListTold) bool { return r.Class == "phone" })].Recipients)
	}
	tally := newDealToldTally()
	stranger := dealOpen{Channel: "marketplace", Who: dealWho{Name: "Sam"}}
	tally.add("deal-a", dealDisclosure{To: "counterparty", Fields: phone}, stranger, dealRoleBuyer)
	tally.add("deal-a", dealDisclosure{To: "counterparty", Who: &dealWho{Email: "sam@example.com"}, Fields: phone}, stranger, dealRoleBuyer)
	tally.add("deal-b", dealDisclosure{To: "counterparty", Who: &dealWho{Email: "sam@example.com"}, Fields: phone}, stranger, dealRoleBuyer)
	for _, r := range tally.rows() {
		if r.Class == "phone" {
			assert.Equal(t, dealListTold{Class: "phone", Recipients: 1, ToSellers: 1}, r)
		}
	}
}

// A party linked only through a telling in another role is one party in
// each role it had, so a role's count never exceeds the whole; and one
// telling of two values of a class tells its recipient once.
func TestDealListToldCountsLinksAcrossRoles(t *testing.T) {
	phone := []dealDisclosureField{{Class: "phone", Value: "+1 555 010 4477"}}
	tally := newDealToldTally()
	tally.add("deal-a", dealDisclosure{To: "counterparty", Fields: phone}, dealOpen{Who: dealWho{Email: "x@example.com"}}, dealRoleBuyer)
	tally.add("deal-b", dealDisclosure{To: "counterparty", Fields: phone}, dealOpen{Who: dealWho{Phone: "+1 555 010 1234"}}, dealRoleBuyer)
	tally.add("deal-c", dealDisclosure{To: "other", Who: &dealWho{Email: "x@example.com", Phone: "+1 555 010 1234"}, Fields: phone}, dealOpen{}, dealRoleBuyer)
	tally.add("deal-d", dealDisclosure{To: "other", Who: &dealWho{Name: "a courier"},
		Fields: []dealDisclosureField{{Class: "phone", Value: "+1 555 010 4477"}, {Class: "phone", Value: "+1 555 010 4478"}}}, dealOpen{}, dealRoleBuyer)
	for _, r := range tally.rows() {
		if r.Class == "phone" {
			assert.Equal(t, dealListTold{Class: "phone", Recipients: 2, ToSellers: 1, ToOthers: 2}, r)
		}
	}
}

// One errand, two merchants: a class told to each is told to two
// recipients, never merged by the deal they share (capsule-cli#340).
func TestDealListToldKeepsEachLegsMerchant(t *testing.T) {
	email := []dealDisclosureField{{Class: "email", Value: "pat@mail.example"}}
	tally := newDealToldTally()
	tally.addAt("deal-a", dealDisclosure{To: "counterparty", Fields: email}, dealWho{Name: "AliExpress", Email: "orders@aliexpress.example"}, "web", dealRoleBuyer)
	tally.addAt("deal-a#5", dealDisclosure{To: "counterparty", Fields: email}, dealWho{Name: "eBay", Email: "orders@ebay.example"}, "web", dealRoleBuyer)
	for _, r := range tally.rows() {
		if r.Class == "email" {
			assert.Equal(t, dealListTold{Class: "email", Recipients: 2, ToSellers: 2}, r)
		}
	}
}
