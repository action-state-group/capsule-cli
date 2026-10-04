package cli

import (
	"fmt"
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

// dealCarried is a cancel-by obligation still open when the deal closed,
// carried by the close (`deal close --carry-open-obligations`).
type dealCarried struct {
	Step      int64  `json:"step"`
	CapsuleID string `json:"capsule_id"`
	CancelBy  string `json:"cancel_by"`
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
	switch {
	case o.ExpectCloseBy == "":
		l.Text = "Open: no close is sealed on this deal, and no close date was expected."
	case now.UTC().Format("2006-01-02") > o.ExpectCloseBy:
		l.PastExpected = true
		l.Text = fmt.Sprintf("Open: no close is sealed on this deal. It was expected to close by %s, which has passed (as of %s).", o.ExpectCloseBy, asOf)
	default:
		l.Text = fmt.Sprintf("Open: no close is sealed on this deal yet. It is expected to close by %s.", o.ExpectCloseBy)
	}
	return l
}

// dealLifecycle is what a receipt says about where the deal stands: open
// or closed, what was linked after the close, and that more may be linked
// after this receipt was made. It states what the deal holds, never what
// anyone did or failed to do. Anything that depends on today is computed
// when the receipt is made and says so ("as of").
type dealLifecycle struct {
	State     string           `json:"state"` // "open" or "closed"
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

func buildDealLifecycle(events []sealedEvent, now time.Time) dealLifecycle {
	asOf := now.UTC().Format("2006-01-02T15:04:05Z")
	l := dealLifecycle{AsOf: asOf, Later: []dealLateRecord{}, MayChange: dealMayChange}
	i := finalClose(events)
	if i < 0 {
		l.State = "open"
		l.Open = openDealListing(events, now)
		l.Text = l.Open.Text
		return l
	}
	c := events[i]
	l.State, l.ClosedAt, l.Outcome = "closed", c.Event.At, c.Event.Close.Outcome
	for _, se := range events[i+1:] {
		if se.Event.Confirms == "" {
			continue
		}
		l.Later = append(l.Later, dealLateRecord{Step: se.Event.N, CapsuleID: se.CapsuleID, At: se.Event.At, Relation: "confirms", Text: trailLine(se.Event)})
	}
	for _, d := range dealDeadlines(events, now, 2) {
		if d.Carried {
			l.Carried = append(l.Carried, d)
		}
	}
	l.Text = fmt.Sprintf("Closed at %s (%s).", c.Event.At, c.Event.Close.Outcome)
	switch n := len(l.Later); n {
	case 0:
		l.Text += " No record has been linked to the close yet."
	case 1:
		l.Text += " 1 record sealed after the close is linked to it (it confirms the close)."
	default:
		l.Text += fmt.Sprintf(" %d records sealed after the close are linked to it (each confirms the close).", n)
	}
	if len(l.Carried) > 0 {
		l.Text += fmt.Sprintf(" %d cancel-by date(s) carried at the close; see below.", len(l.Carried))
	}
	return l
}
