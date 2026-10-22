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
	// What the agent says about it: "Your receipts" (capsule-cli#345).
	cli.Narration
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
	// Checks and Anomalies are always present on a range, every key listed
	// with its zero (capsule-cli#385): an absent member means a capsulectl
	// that does not count them, never zero.
	Checks    dealListChecks `json:"checks"`
	Anomalies map[string]int `json:"anomalies"`
}

// dealListChecks counts the checks sealed in the range. Verdicts and Rules
// each partition them: Verdicts by the verdict sealed, Rules by what the
// profile's rules said (dealRulesAnswer). A word outside the known set is
// counted under its own key, never dropped. Rulesets counts the checks whose
// rules were evaluated, by the ruleset the checker reported.
type dealListChecks struct {
	Verdicts map[string]int    `json:"verdicts"`
	Rules    map[string]int    `json:"rules"`
	Rulesets []dealListRuleset `json:"rulesets"`
}

type dealListRuleset struct {
	RulesetID        string `json:"ruleset_id"`
	DefinitionDigest string `json:"definition_digest"`
	Checks           int    `json:"checks"`
}

func newDealListChecks() dealListChecks {
	return dealListChecks{
		Verdicts: map[string]int{"pass": 0, "pause": 0, "deny": 0},
		Rules:    map[string]int{"evaluated": 0, "partly_unanswered": 0, "not_evaluated": 0, "not_configured": 0, "unrecorded": 0},
		Rulesets: []dealListRuleset{},
	}
}

// newDealListAnomalies lists the act- and disclosure-level anomaly kinds of
// `deal report` (with an act against the user's answer not to proceed apart
// from the other unsealed approvals), and a seller's representation nothing
// authorized, each at zero. Counterparty-side kinds and the causes of a check's pause are not
// counted: the verdicts count the pauses.
func newDealListAnomalies() map[string]int {
	return map[string]int{"skipped_check": 0, "unsealed_approval": 0, "differs_from_check": 0, "unapproved_disclosure": 0, "unapproved_representation": 0, "acted_against_answer": 0}
}

// dealRulesAnswer is what a check's sealed rules answered: unrecorded on a
// check sealed before there was a rules member; the status when they were
// not evaluated or not configured; when evaluated, partly_unanswered if the
// overall verdict or a measured rule's finding is not_evaluable (a rule the
// ruleset only declares never counts), else evaluated.
func dealRulesAnswer(r *dealRules) string {
	if r == nil {
		return "unrecorded"
	}
	if r.Status != "evaluated" {
		return r.Status
	}
	if r.Verdict == "not_evaluable" {
		return "partly_unanswered"
	}
	for _, f := range r.Findings {
		if f.Verdict == "not_evaluable" && !isDeclaredRule(f) {
			return "partly_unanswered"
		}
	}
	return "evaluated"
}

// actAnomalyKind is the anomaly an act sealed without a valid check is, as
// `deal report` names it; "" for a checked act. An act whose check asked and
// that went ahead against the user's answer not to proceed is
// acted_against_answer; on no go-ahead in force (none, one already used, or
// the agent's card answer to an ask) unsealed_approval; one with no check in
// force is skipped_check.
func actAnomalyKind(a dealAct) string {
	if !a.Unchecked {
		return ""
	}
	switch a.Rule {
	case "differs_from_check":
		return "differs_from_check"
	case "answer_was_not_proceed":
		return "acted_against_answer"
	case "no_sealed_approval", "approval_already_used", "ask_needs_your_words":
		return "unsealed_approval"
	}
	return "skipped_check"
}

type dealListOutcomes struct {
	Bought    int `json:"bought"`
	Sold      int `json:"sold"`
	CalledOff int `json:"called_off"`
	NotChosen int `json:"not_chosen"`
	Open      int `json:"open"`
}

// dealListMoneyIn is one currency's money in the range. SpentOutMinor is
// what sealed acts paid out in the range, a cancellation's fee among it;
// RefundedInMinor what sealed acts returned (a cancel reversing a pay).
// SoldForMinor is, for each sale thread closed as received in the range,
// the money terms its close commits (closedTerms): the delivered price,
// else the agreed one. It is the price sealed, not money observed
// arriving. SoldUnpriced counts such threads with no price or currency
// sealed.
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
// stood at the range's end, with the range's totals and disclosures. A sale
// is listed when it, or a buyer thread opened under it, has a step in the
// range.
func dealListInRange(logs []dealLog, b dealListBounds) (dealListRange, error) {
	r := dealListRange{Deals: []dealListing{}, Totals: dealListTotals{Money: []dealListMoneyIn{}, Checks: newDealListChecks(), Anomalies: newDealListAnomalies()}}
	rulesets := map[dealListRuleset]int{}
	if !b.since.IsZero() {
		s := b.since.Format(time.RFC3339Nano)
		r.Since = &s
	}
	if !b.until.IsZero() {
		u := b.until.Format(time.RFC3339Nano)
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
	type read struct {
		asOf   []sealedEvent
		at     []time.Time
		active bool
	}
	reads := make([]read, len(logs))
	activeSales := map[string]bool{}
	for i, l := range logs {
		rd := &reads[i]
		for _, se := range l.events {
			t, err := stepTime(se.Event)
			if err != nil {
				return r, err
			}
			if !b.until.IsZero() && !t.Before(b.until) {
				break
			}
			rd.asOf, rd.at = append(rd.asOf, se), append(rd.at, t)
			if b.contains(t) {
				rd.active = true
				r.Totals.count(se.Event, rulesets)
			}
		}
		if open := l.events[0].Event.Open; rd.active && open != nil && open.Sale != "" {
			activeSales[open.Sale] = true
		}
	}
	told := newDealToldTally()
	for i, l := range logs {
		asOf, at := reads[i].asOf, reads[i].at
		if !reads[i].active && !(activeSales[l.id] && len(asOf) > 0) {
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
			if a := ev.Act; a != nil {
				cur := a.Currency
				if cur == "" {
					cur = currency
				}
				if a.AmountMinor != nil {
					switch {
					case actOut(*a):
						moneyIn(cur).SpentOutMinor += *a.AmountMinor
					case a.Direction == "in":
						moneyIn(cur).RefundedInMinor += *a.AmountMinor
					}
				}
				// A cancellation's fee is its own amount, paid out.
				if a.FeeMinor != nil {
					moneyIn(cur).SpentOutMinor += *a.FeeMinor
				}
			}
			if d := ev.Disclosure; ev.Kind == "disclosure" && d != nil && open != nil {
				// Each leg's counterparty is its own: a telling is keyed to
				// the leg it was made in, never merged with another
				// merchant's in the same errand.
				key := l.id
				if from := currentLegStart(asOf[:i+1]); from > 0 {
					key += "#" + strconv.Itoa(from)
				}
				told.addAt(key, *d, legWho(asOf[:i+1]), open.Channel, role)
			}
		}
		if fc := finalClose(asOf); listing.Result == dealResultSold && b.contains(at[fc]) {
			amount, cur, err := closedTerms(asOf[:fc], asOf[fc].Event)
			if err != nil {
				return r, err
			}
			if minor, perr := strconv.ParseInt(amount, 10, 64); amount != "" && perr == nil {
				moneyIn(cur).SoldForMinor += minor
			} else {
				moneyIn(currency).SoldUnpriced++
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
	for rs, n := range rulesets {
		rs.Checks = n
		r.Totals.Checks.Rulesets = append(r.Totals.Checks.Rulesets, rs)
	}
	sort.Slice(r.Totals.Checks.Rulesets, func(i, j int) bool {
		a, b := r.Totals.Checks.Rulesets[i], r.Totals.Checks.Rulesets[j]
		return a.RulesetID < b.RulesetID || a.RulesetID == b.RulesetID && a.DefinitionDigest < b.DefinitionDigest
	})
	r.Told = told.rows()
	return r, nil
}

// count adds one step sealed in the range to the check and anomaly counts:
// a check under its verdict and its rules' answer (and an evaluated one under
// its ruleset, keyed with Checks zero), an act sealed without a valid check
// under its kind, a disclosure no approval covered, and a seller's claim
// nothing authorized.
func (t *dealListTotals) count(ev dealEvent, rulesets map[dealListRuleset]int) {
	switch {
	case ev.Kind == "check" && ev.Check != nil:
		t.Checks.Verdicts[ev.Check.Verdict]++
		t.Checks.Rules[dealRulesAnswer(ev.Check.Rules)]++
		if r := ev.Check.Rules; r != nil && r.Status == "evaluated" {
			rulesets[dealListRuleset{RulesetID: r.RulesetID, DefinitionDigest: r.DefinitionDigest}]++
		}
	case ev.Kind == "act" && ev.Act != nil:
		if kind := actAnomalyKind(*ev.Act); kind != "" {
			t.Anomalies[kind]++
		}
	case ev.Kind == "disclosure" && ev.Disclosure != nil && ev.Disclosure.AuthorizedBy == "":
		t.Anomalies["unapproved_disclosure"]++
	case ev.Kind == "claim" && ev.Claim != nil && ev.Claim.Unchecked:
		// An agent's claim to a buyer that no represent check authorized,
		// sealed anyway (deal_representations.go).
		t.Anomalies["unapproved_representation"]++
	}
}

// dealToldTally counts, per class, the distinct recipients told it. A
// recipient is known by the counterparty memory's identities
// (disclosureRecipientKeys, read as one party by sameParty), and a deal's
// counterparty is one recipient within that deal. Recipients that share an
// identity are one, however they are linked; one with no identity is never
// merged with another on a name.
type dealToldTally struct {
	byClass map[string][]dealToldTo
}

type dealToldTo struct {
	keys []string
	// deal is the deal id of a telling to the deal's counterparty, else "".
	deal string
	role string // seller, buyer or other
}

func newDealToldTally() *dealToldTally {
	t := &dealToldTally{byClass: map[string][]dealToldTo{}}
	for c := range disclosureClasses {
		t.byClass[c] = nil
	}
	return t
}

// add tallies a disclosure made in a deal whose counterparty is the
// opening's.
func (t *dealToldTally) add(dealID string, d dealDisclosure, open dealOpen, role string) {
	t.addAt(dealID, d, open.Who, open.Channel, role)
}

// addAt tallies a disclosure made in the leg with the counterparty leg.
func (t *dealToldTally) addAt(dealID string, d dealDisclosure, leg dealWho, channel, role string) {
	to := dealToldTo{keys: disclosureRecipientKeys(d, leg, channel), deal: dealID, role: dealRoleSeller}
	switch {
	case namedRecipient(d.To):
		to.deal, to.role = "", "other"
	case role == dealRoleSeller:
		to.role = dealRoleBuyer
	}
	// One telling of two values of a class tells that recipient once.
	seen := map[string]bool{}
	for _, f := range d.Fields {
		if !seen[f.Class] {
			seen[f.Class] = true
			t.byClass[f.Class] = append(t.byClass[f.Class], to)
		}
	}
}

// dealToldCount counts the parties among recipients: recipients are joined
// when they are the same party or the same deal's counterparty, and joined
// transitively, so the count does not depend on the order they were told
// in. A party is counted once in each role any of its tellings had, so no
// role's count exceeds the whole.
func dealToldCount(class string, rs []dealToldTo) dealListTold {
	parent := make([]int, len(rs))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	for i := range rs {
		for j := 0; j < i; j++ {
			if rs[i].deal != "" && rs[i].deal == rs[j].deal ||
				len(rs[i].keys) > 0 && len(rs[j].keys) > 0 && sameParty(rs[i].keys, rs[j].keys) {
				parent[find(i)] = find(j)
			}
		}
	}
	roles := map[int]map[string]bool{}
	for i, r := range rs {
		root := find(i)
		if roles[root] == nil {
			roles[root] = map[string]bool{}
		}
		roles[root][r.role] = true
	}
	row := dealListTold{Class: class, Recipients: len(roles)}
	for _, has := range roles {
		if has[dealRoleSeller] {
			row.ToSellers++
		}
		if has[dealRoleBuyer] {
			row.ToBuyers++
		}
		if has["other"] {
			row.ToOthers++
		}
	}
	return row
}

func (t *dealToldTally) rows() []dealListTold {
	rows := make([]dealListTold, 0, len(t.byClass))
	for c, rs := range t.byClass {
		rows = append(rows, dealToldCount(c, rs))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Class < rows[j].Class })
	return rows
}
