package plugin

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/action-state-group/capsule-cli/internal/cli"
)

// A Statement over a period (capsule-cli#342): `deal list --since/--until`
// picks the deals with a step in the range, and with --json adds the
// range's totals and who was told what. All of it is read from the sealed
// steps: it seals nothing and adds no evidence. Each deal is read as it
// stood at the range's end (its steps before --until), so a deal closed
// after the range is open in it.

// dealListRange is `deal list --json` with a range.
type dealListRange struct {
	// Since and Until are the range's bounds in UTC, Since inclusive and
	// Until exclusive; null when not given.
	Since  *string        `json:"since"`
	Until  *string        `json:"until"`
	Deals  []dealListing  `json:"deals"`
	Totals dealListTotals `json:"totals"`
	Told   []dealListTold `json:"disclosures"`
}

// dealListTotals are the range's totals. Outcomes count the deals listed
// (a sale's buyer threads among them), each by its result at the range's
// end; a sale itself is counted in Sales. Money is per currency, never
// converted.
type dealListTotals struct {
	Deals    int               `json:"deals"`
	Sales    int               `json:"sales"`
	Outcomes dealListOutcomes  `json:"outcomes"`
	Money    []dealListMoneyIn `json:"money"`
}

type dealListOutcomes struct {
	Bought    int `json:"bought"`
	Sold      int `json:"sold"`
	CalledOff int `json:"called_off"`
	NotChosen int `json:"not_chosen"`
	Open      int `json:"open"`
}

// dealListMoneyIn is one currency's money in the range. SpentOutMinor is
// what sealed acts paid out in the range, RefundedInMinor what sealed acts
// returned (a cancel reversing a pay). SoldForMinor is, for each sale
// thread closed as received in the range, the price agreed: its latest
// sealed offer or commit amount, else its opening price. It is the price
// sealed, not money observed arriving. SoldUnpriced counts such threads
// with no price sealed.
type dealListMoneyIn struct {
	Currency        string `json:"currency"`
	SpentOutMinor   int64  `json:"spent_out_minor"`
	RefundedInMinor int64  `json:"refunded_in_minor"`
	SoldForMinor    int64  `json:"sold_for_minor"`
	SoldUnpriced    int    `json:"sold_unpriced,omitempty"`
}

// dealListTold is one disclosure class in the range: how many distinct
// recipients were told it, and of them how many were a seller the user
// bought from, a buyer the user sold to, or someone else. Only classes and
// counts: never a value or who. Every known class is listed, so a zero
// says it was told to no one in the range.
type dealListTold struct {
	Class      string `json:"class"`
	Recipients int    `json:"recipients"`
	ToSellers  int    `json:"to_sellers"`
	ToBuyers   int    `json:"to_buyers"`
	ToOthers   int    `json:"to_others"`
}

// Results of a deal at the range's end, as dealListing.Result states them.
const (
	dealResultBought    = "bought"
	dealResultSold      = "sold"
	dealResultCalledOff = "called_off"
	dealResultNotChosen = "not_chosen"
	dealResultOpen      = "open"
)

// dealResult is a deal's result: open until its final close; then bought
// or sold (by the user's side) when received, not_chosen when the other
// side was not chosen, else called off.
func dealResult(events []sealedEvent) string {
	fc := finalClose(events)
	if fc < 0 {
		return dealResultOpen
	}
	switch events[fc].Event.Close.Status {
	case "received":
		if dealRole(events) == dealRoleSeller {
			return dealResultSold
		}
		return dealResultBought
	case "not_selected":
		return dealResultNotChosen
	}
	return dealResultCalledOff
}

// dealListBounds is a range: from since (inclusive) to until (exclusive),
// either open when zero.
type dealListBounds struct{ since, until time.Time }

func (b dealListBounds) contains(t time.Time) bool {
	return (b.since.IsZero() || !t.Before(b.since)) && (b.until.IsZero() || t.Before(b.until))
}

// parseListBound reads --since or --until: a date (a UTC day's start) or
// an RFC 3339 time with its offset, for a period in the user's own zone.
func parseListBound(flag, v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, cli.InputError("--" + flag + " must be a date (2026-10-01, the start of that UTC day) or a time with its offset (2026-10-01T00:00:00-07:00)")
}

// listBounds reads the range flags; ok is false when neither is given.
func listBounds(since, until string) (b dealListBounds, ok bool, err error) {
	if since == "" && until == "" {
		return b, false, nil
	}
	if since != "" {
		if b.since, err = parseListBound("since", since); err != nil {
			return b, false, err
		}
	}
	if until != "" {
		if b.until, err = parseListBound("until", until); err != nil {
			return b, false, err
		}
	}
	if !b.since.IsZero() && !b.until.IsZero() && !b.since.Before(b.until) {
		return b, false, cli.InputError("--until must be after --since: the range is from --since up to, not including, --until")
	}
	return b, true, nil
}

// stepTime is when a step was sealed.
func stepTime(ev dealEvent) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, ev.At)
	if err != nil {
		return time.Time{}, cli.ErrConflict
	}
	return t, nil
}

// dealListInRange lists the deals with a step in the range, each as it
// stood at the range's end, with the range's totals and disclosures.
func dealListInRange(logs []dealLog, b dealListBounds) (dealListRange, error) {
	r := dealListRange{Deals: []dealListing{}, Totals: dealListTotals{Money: []dealListMoneyIn{}}}
	if !b.since.IsZero() {
		s := b.since.Format(time.RFC3339)
		r.Since = &s
	}
	if !b.until.IsZero() {
		u := b.until.Format(time.RFC3339)
		r.Until = &u
	}
	money := map[string]*dealListMoneyIn{}
	moneyIn := func(cur string) *dealListMoneyIn {
		cur = strings.ToUpper(cur)
		if money[cur] == nil {
			money[cur] = &dealListMoneyIn{Currency: cur}
		}
		return money[cur]
	}
	told := newDealToldTally()
	for _, l := range logs {
		var asOf []sealedEvent
		var at []time.Time
		active := false
		for _, se := range l.events {
			t, err := stepTime(se.Event)
			if err != nil {
				return r, err
			}
			if !b.until.IsZero() && !t.Before(b.until) {
				break
			}
			asOf, at = append(asOf, se), append(at, t)
			active = active || b.contains(t)
		}
		if !active {
			continue
		}
		listing := dealListingOf(l.id, asOf)
		r.Deals = append(r.Deals, listing)
		if listing.Kind == "sale" {
			r.Totals.Sales++
			continue
		}
		r.Totals.Deals++
		switch listing.Result {
		case dealResultBought:
			r.Totals.Outcomes.Bought++
		case dealResultSold:
			r.Totals.Outcomes.Sold++
		case dealResultCalledOff:
			r.Totals.Outcomes.CalledOff++
		case dealResultNotChosen:
			r.Totals.Outcomes.NotChosen++
		default:
			r.Totals.Outcomes.Open++
		}
		var open *dealOpen
		currency := ""
		if open = asOf[0].Event.Open; open != nil {
			currency = open.Terms.Currency
		}
		role := dealRole(asOf)
		for i, se := range asOf {
			if !b.contains(at[i]) {
				continue
			}
			ev := se.Event
			if a := ev.Act; a != nil && a.AmountMinor != nil {
				cur := a.Currency
				if cur == "" {
					cur = currency
				}
				switch {
				case actOut(*a):
					moneyIn(cur).SpentOutMinor += *a.AmountMinor
				case a.Direction == "in":
					moneyIn(cur).RefundedInMinor += *a.AmountMinor
				}
			}
			if d := ev.Disclosure; ev.Kind == "disclosure" && d != nil && open != nil {
				told.add(l.id, ev.N, *d, *open, role)
			}
		}
		if fc := finalClose(asOf); listing.Result == dealResultSold && b.contains(at[fc]) {
			if price, cur, ok := soldPrice(asOf[:fc], currency); ok {
				moneyIn(cur).SoldForMinor += price
			} else {
				moneyIn(cur).SoldUnpriced++
			}
		}
	}
	curs := make([]string, 0, len(money))
	for c := range money {
		curs = append(curs, c)
	}
	sort.Strings(curs)
	for _, c := range curs {
		r.Totals.Money = append(r.Totals.Money, *money[c])
	}
	r.Told = told.rows()
	return r, nil
}

// soldPrice is the price a sale thread sealed before its close: its latest
// offer or commit amount, else its opening price.
func soldPrice(events []sealedEvent, currency string) (int64, string, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		if a := events[i].Event.Act; a != nil && a.AmountMinor != nil && (a.Action == "commit" || a.Action == offerAction) {
			cur := a.Currency
			if cur == "" {
				cur = currency
			}
			return *a.AmountMinor, cur, true
		}
	}
	if open := events[0].Event.Open; open != nil && open.Terms.PriceMinor != nil {
		return *open.Terms.PriceMinor, currency, true
	}
	return 0, currency, false
}

// dealToldTally counts, per class, the distinct recipients told it. A
// recipient is known by the counterparty memory's identities
// (disclosureRecipientKeys, read as one party by sameParty); one with none
// is the deal's own counterparty, or, for someone else, each telling its
// own recipient, so a recipient is never merged with another on a name.
type dealToldTally struct {
	byClass map[string][]dealToldTo
}

type dealToldTo struct {
	keys []string
	role string // seller, buyer or other
}

func newDealToldTally() *dealToldTally {
	t := &dealToldTally{byClass: map[string][]dealToldTo{}}
	for c := range disclosureClasses {
		t.byClass[c] = nil
	}
	return t
}

func (t *dealToldTally) add(dealID string, n int64, d dealDisclosure, open dealOpen, role string) {
	keys := disclosureRecipientKeys(d, open)
	to := dealRoleSeller
	switch {
	case d.To == "other":
		to = "other"
		if len(keys) == 0 {
			keys = []string{"telling:" + dealID + "/" + strconv.FormatInt(n, 10)}
		}
	case role == dealRoleSeller:
		to = dealRoleBuyer
	}
	if len(keys) == 0 {
		keys = []string{"deal:" + dealID}
	}
	for _, f := range d.Fields {
		t.byClass[f.Class] = append(t.byClass[f.Class], dealToldTo{keys: keys, role: to})
	}
}

// distinctParties counts the parties among recipients, each read as the
// first earlier one it is the same party as.
func distinctParties(recipients []dealToldTo, role string) int {
	var seen [][]string
	for _, r := range recipients {
		if role != "" && r.role != role {
			continue
		}
		known := false
		for _, k := range seen {
			if sameParty(k, r.keys) {
				known = true
				break
			}
		}
		if !known {
			seen = append(seen, r.keys)
		}
	}
	return len(seen)
}

func (t *dealToldTally) rows() []dealListTold {
	rows := make([]dealListTold, 0, len(t.byClass))
	for c, rs := range t.byClass {
		rows = append(rows, dealListTold{Class: c, Recipients: distinctParties(rs, ""),
			ToSellers: distinctParties(rs, dealRoleSeller), ToBuyers: distinctParties(rs, dealRoleBuyer), ToOthers: distinctParties(rs, "other")})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Class < rows[j].Class })
	return rows
}
