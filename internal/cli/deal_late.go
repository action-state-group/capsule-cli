package cli

import (
	"fmt"
	"strings"
	"time"
)

// A deal is closed when its point of resolution is reached, and evidence
// that arrives later is linked to it rather than holding the deal open:
// the merchant's confirmation an hour later, a shipping notice days later,
// a refund weeks later. Each is a NEW record whose Capsule chains to the
// close with the registered chain.relation `confirms` (non-terminal: it
// records an outcome of the parent, whose state stands) and whose x-deal-v0
// record commits to the close record's digest, so it cannot be reattached
// to another deal. It proves that whoever sealed it held that deal; it does
// not prove the deal expected it. `follows`, used between ordinary steps, is
// ordering only and is never used for a late record. `supersedes`, the
// registered terminal transition, is not emitted: an expiry is computed when
// a receipt is made, never sealed.
//
// When to close, by deal type (SKILL.md says the same):
//   - purchase: at delivery (a parcel), or at payment for something
//     delivered at once;
//   - booking: at confirmation or ticketing (a flight), or after the stay;
//   - rental: when the item is returned;
//   - service: when the work is done.

// dealCarried is an obligation still open when the deal closed, carried by
// the close (`deal close --carry-open-obligations`): a cancel-by date, or a
// due date.
type dealCarried struct {
	Step      int64  `json:"step"`
	CapsuleID string `json:"capsule_id"`
	CancelBy  string `json:"cancel_by,omitempty"`
	DueBy     string `json:"due_by,omitempty"`
}

// finalClose is the index of the deal's final close (a close whose outcome
// is not open), or -1 while the deal is open.
func finalClose(events []sealedEvent) int {
	for i := len(events) - 1; i >= 0; i-- {
		if c := events[i].Event.Close; events[i].Event.Kind == "close" && c != nil && c.Outcome != "open" {
			return i
		}
	}
	return -1
}

func dealFinallyClosed(events []sealedEvent) bool { return finalClose(events) >= 0 }

// expectCloseDays is how long after opening a deal is expected to close,
// when the opening did not say: a parcel arrives, a ticket is issued, a
// rental is returned, a job is done.
var expectCloseDays = map[string]int{"purchase": 14, "booking": 1, "rental": 7, "service": 30}

func defaultExpectClose(dealType string, opened time.Time) string {
	return opened.UTC().AddDate(0, 0, expectCloseDays[dealType]).Format("2006-01-02")
}

// dealOpenListing is an open deal, as `deal deadlines` lists it: closing is
// proposed by the system, so nobody has to remember an open deal.
type dealOpenListing struct {
	DealID        string `json:"deal_id"`
	State         string `json:"state"` // "open"
	OpenedAt      string `json:"opened_at"`
	ExpectCloseBy string `json:"expect_close_by,omitempty"`
	PastExpected  bool   `json:"past_expected"`
	AsOf          string `json:"as_of"`
	Text          string `json:"text"`
}

func openDealListing(events []sealedEvent, now time.Time) *dealOpenListing {
	if dealFinallyClosed(events) {
		return nil
	}
	o := events[0].Event.Open
	asOf := now.UTC().Format("2006-01-02T15:04:05Z")
	l := &dealOpenListing{DealID: events[0].Event.DealID, State: "open", OpenedAt: events[0].Event.At, ExpectCloseBy: o.ExpectCloseBy, AsOf: asOf}
	// An open deal with no merchant email sealed yet is waiting for the
	// merchant's receipt; one that has it is waiting only to be closed.
	l.Text = "Open: waiting for the merchant's receipt."
	if hasMerchantEmail(events) {
		l.Text = "Open: the merchant's email is sealed; no close is sealed on this deal yet."
	}
	switch {
	case o.ExpectCloseBy == "":
		l.Text += " No close date was expected."
	case now.UTC().Format("2006-01-02") > o.ExpectCloseBy:
		l.PastExpected = true
		l.Text += fmt.Sprintf(" It was expected to close by %s, which has passed (as of %s).", o.ExpectCloseBy, asOf)
	default:
		l.Text += fmt.Sprintf(" It is expected to close by %s.", o.ExpectCloseBy)
	}
	return l
}

func hasMerchantEmail(events []sealedEvent) bool {
	for _, se := range events {
		if e := se.Event.Evidence; e != nil && e.Email != nil {
			return true
		}
	}
	return false
}

// lateReceiptLine is what a receipt says about a merchant email sealed after
// the close, set beside what the user approved: it matches, or it differs
// with both amounts. "" when there is no charge in it, or nothing approved
// to set it beside (only a limit is not an approval of an amount).
func lateReceiptLine(events []sealedEvent, i int) string {
	m := events[i].Event.Evidence.Email
	if m.Parsed.TotalMinor == nil {
		return ""
	}
	state, err := foldDeal(events[:i])
	if err != nil {
		return ""
	}
	approved, currency, _, _, limit := approvedAmount(events[:i], state)
	if approved == nil || limit {
		return ""
	}
	line := "The merchant's receipt arrived. It matches what you approved."
	sameCurrency := m.Parsed.Currency == "" || currency == "" || strings.EqualFold(m.Parsed.Currency, currency)
	if !sameCurrency || *m.Parsed.TotalMinor != *approved {
		line = fmt.Sprintf("The merchant's receipt arrived. It differs: you approved %s; their receipt says %s.", formatMoney(*approved, currency), formatMoney(*m.Parsed.TotalMinor, m.Parsed.Currency))
	}
	if !(m.DKIM.Result == "pass" && m.DKIM.Merchant) {
		line += " (This copy is not confirmed by the merchant's signature.)"
	}
	return line
}

// dealLifecycle is what a receipt says about where the deal stands: open
// or closed, what was linked after the close, and that more may be linked
// after this receipt was made. It states what the deal holds, never what
// anyone did or failed to do. Anything that depends on today is computed
// when the receipt is made and says so ("as of").
type dealLifecycle struct {
	State     string           `json:"state"` // "open", "cancelled" (an authorized cancel, no close yet) or "closed"
	AsOf      string           `json:"as_of"`
	Text      string           `json:"text"`
	ClosedAt  string           `json:"closed_at,omitempty"`
	Outcome   string           `json:"outcome,omitempty"`
	Later     []dealLateRecord `json:"later"`
	Carried   []dealDeadline   `json:"carried,omitempty"`
	MayChange string           `json:"may_change"`
	Open      *dealOpenListing `json:"open,omitempty"`
}

// dealLateRecord is one record sealed after the close, linked to it.
type dealLateRecord struct {
	Step      int64  `json:"step"`
	CapsuleID string `json:"capsule_id"`
	At        string `json:"at"`
	Relation  string `json:"relation"` // "confirms"
	Text      string `json:"text"`
}

const dealMayChange = "Records can be linked to this deal after this receipt was made (a merchant's confirmation, a shipping notice, a refund). This receipt shows those sealed when it was made; to see later ones, make a new report with `capsulectl deal report`."

// authorizedCancelAt is when the latest act was an authorized cancel, or ""
// when the latest act is not one (a later pay reopens the business).
func authorizedCancelAt(events []sealedEvent) string {
	for i := len(events) - 1; i >= 0; i-- {
		if a := events[i].Event.Act; a != nil {
			if a.Action == "cancel" && !a.Unchecked {
				return events[i].Event.At
			}
			return ""
		}
	}
	return ""
}

func buildDealLifecycle(events []sealedEvent, now time.Time) dealLifecycle {
	asOf := now.UTC().Format("2006-01-02T15:04:05Z")
	l := dealLifecycle{AsOf: asOf, Later: []dealLateRecord{}, MayChange: dealMayChange}
	i := finalClose(events)
	if i < 0 {
		l.Open = openDealListing(events, now)
		l.State, l.Text = "open", l.Open.Text
		// An authorized cancel ends the deal's business before any close is
		// sealed: the deal is cancelled, not open.
		if at := authorizedCancelAt(events); at != "" {
			l.State = "cancelled"
			l.Text = "Cancelled: an authorized cancel was sealed at " + at + ". No close is sealed on this deal yet."
		}
		return l
	}
	c := events[i]
	l.State, l.ClosedAt, l.Outcome = "closed", c.Event.At, c.Event.Close.Outcome
	for j := i + 1; j < len(events); j++ {
		se := events[j]
		if se.Event.Confirms == "" {
			continue
		}
		text := trailLine(se.Event)
		if e := se.Event.Evidence; e != nil && e.Email != nil {
			if line := lateReceiptLine(events, j); line != "" {
				text = line
			}
		}
		l.Later = append(l.Later, dealLateRecord{Step: se.Event.N, CapsuleID: se.CapsuleID, At: se.Event.At, Relation: "confirms", Text: text})
	}
	for _, d := range dealDeadlines(events, now, 2) {
		if d.Carried {
			l.Carried = append(l.Carried, d)
		}
	}
	l.Text = fmt.Sprintf("Closed at %s (%s).", c.Event.At, c.Event.Close.Outcome)
	switch c.Event.Close.Outcome {
	case "completed":
		// What was delivered matched what was agreed, with nothing open.
		l.Text = fmt.Sprintf("Closed at %s: matched, nothing left to match.", c.Event.At)
	case "not_selected":
		l.Text = fmt.Sprintf("Closed at %s: not selected; nothing was taken or delivered.", c.Event.At)
	}
	switch n := len(l.Later); n {
	case 0:
		l.Text += " No record has been linked to the close yet."
	case 1:
		l.Text += " 1 record sealed after the close is linked to it (it confirms the close)."
	default:
		l.Text += fmt.Sprintf(" %d records sealed after the close are linked to it (each confirms the close).", n)
	}
	if len(l.Carried) > 0 {
		what := "cancel-by date(s)"
		for _, c := range l.Carried {
			if c.DueBy != "" {
				what = "date(s)"
			}
		}
		l.Text += fmt.Sprintf(" %d %s carried at the close; see below.", len(l.Carried), what)
	}
	return l
}
