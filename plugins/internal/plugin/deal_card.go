package plugin

import (
	"slices"
	"strconv"
	"strings"

	"github.com/action-state-group/capsule-cli/internal/cli"
)

// The check card (capsule-cli#344): one tag, one question, the facts, at
// most two reasons, at most one thing good to know, what comes next, and the
// buttons. Its text (cardText) is what the user answers and what an answer
// is sealed against (card_commitment). Rule ids, selector names, the ruleset,
// unmeasured rules, defaults and ids never reach it: they stay in the sealed
// check (differences, rules, picked_by_agent) and the receipt's proof detail.

// stopRules are the scam-shaped signals that tag a card STOP: CHECK THIS.
var stopRules = map[string]bool{
	"payee_or_contact_changed":  true,
	"credentials_requested":     true,
	"verification_code_request": true,
	"pay_before_seeing":         true,
}

// cardReason is one row of the collapse table: the differences (by rule)
// that say the same thing to the user, and the one reason that says it.
type cardReason struct {
	key   string
	rules []string
	// questions are difference questions the row covers whatever their
	// rule (a remote reader's own rules).
	questions []string
	// when, if set, also decides whether the row applies.
	when func(dealCheckResult, dealSnapshot, cardFacts) bool
}

// cardFacts are the values the reasons are said with.
type cardFacts struct {
	limit, floor, picked string
	// told are the details given for the first time, as a sentence lists
	// them.
	told string
	// askedOver is an over-limit purchase the user named themselves, item
	// and price: the rule still asks, as an exception.
	askedOver bool
	// acceptanceOld is a seller's share to the buyer of a thread whose
	// acceptance is older than the history the rules checker is given: the
	// checker sees none, though the thread holds one.
	acceptanceOld bool
}

// cardReasons is the collapse table, in priority order: a card shows the
// first two rows that apply. Rules that say the same thing collapse to one
// reason (r02 and r06 to "first payment to this seller", a first dealing
// and every first telling to "first time this seller gets your details"),
// and a rules escalation is not said again when a row above already says
// why. Tested row by row (TestTheCardCollapsesReasons).
var cardReasons = []cardReason{
	{key: "card.why.payee_changed", rules: []string{"payee_or_contact_changed"}},
	{key: "card.why.credentials_requested", rules: []string{"credentials_requested", "verification_code_request"}},
	{key: "card.why.pay_before_seeing", rules: []string{"pay_before_seeing"}},
	{key: "card.why.acceptance_called_off", rules: []string{"acceptance_called_off"}},
	// Why the user's rules found no acceptance: capsulectl holds the whole
	// thread, the checker only its window. Under a deny it is the deny's
	// own reason; where the rules could not be fully checked it stands
	// alone. An escalation is the rules asking for something else.
	{key: "card.why.rules_deny.acceptance_too_old", when: func(r dealCheckResult, _ dealSnapshot, f cardFacts) bool {
		return f.acceptanceOld && hasRule(r, "rules_deny")
	}},
	{key: "card.why.rules_deny", when: func(r dealCheckResult, _ dealSnapshot, f cardFacts) bool {
		return hasRule(r, "rules_deny") && !f.acceptanceOld
	}},
	{key: "card.why.acceptance_too_old", when: func(r dealCheckResult, _ dealSnapshot, f cardFacts) bool {
		return f.acceptanceOld && !hasRule(r, "rules_deny") && hasRule(r, "rules_not_evaluable")
	}},
	{key: "card.why.over_limit_asked", when: func(r dealCheckResult, _ dealSnapshot, f cardFacts) bool {
		return f.askedOver && hasRule(r, "over_limit")
	}},
	{key: "card.why.over_limit", when: func(r dealCheckResult, _ dealSnapshot, f cardFacts) bool {
		return !f.askedOver && hasRule(r, "over_limit")
	}},
	{key: "card.why.under_floor", rules: []string{"under_floor"}},
	{key: "card.why.representation_missing", rules: []string{ruleRepresentationMissing}},
	{key: "card.why.representation_outside", rules: []string{ruleRepresentationOutside}},
	// To someone dealt with before, only what is new to them.
	{key: "card.why.first_disclosure", when: func(r dealCheckResult, _ dealSnapshot, f cardFacts) bool {
		return hasRule(r, "first_disclosure") && (r.Recipient == nil || r.Recipient.FirstTime || f.told == "")
	}},
	{key: "card.why.first_disclosure.some", when: func(r dealCheckResult, _ dealSnapshot, f cardFacts) bool {
		return hasRule(r, "first_disclosure") && r.Recipient != nil && !r.Recipient.FirstTime && f.told != ""
	}},
	{key: "card.why.not_asked", rules: []string{"not_asked"}},
	{key: "card.why.agent_picked", rules: []string{"agent_picked"}},
	{key: "card.why.terms_changed", rules: []string{"terms_changed"}},
	{key: "card.why.no_way_back", rules: []string{"recourse_changed"}},
	{key: "card.why.no_protection", rules: []string{"irreversible_rail"}},
	{key: "card.why.off_platform", rules: []string{"off_platform_early"}},
	{key: "card.why.new_site", rules: []string{"domain_recent"}},
	{key: "card.why.materiality_changed", rules: []string{"materiality_changed"}},
	{key: "card.why.remote", rules: []string{"remote_pause"}, questions: []string{"remote"}},
	{key: "card.why.rules_not_checked", rules: []string{"rules_not_checked", "rules_not_evaluable"}},
	{key: "card.why.rules_ask", rules: []string{"rules_escalate"}},
	// A first dealing is said beside a rules escalation on a pay, as a fact
	// next to the cause: capsulectl reads no ruleset's rule ids, so it never
	// says which rule asked (capsule-cli#344 review).
	{key: "card.why.first_dealing", when: func(r dealCheckResult, snap dealSnapshot, _ cardFacts) bool {
		return snap.Action == "pay" && r.Recipient != nil && r.Recipient.FirstTime && hasRule(r, "rules_escalate")
	}},
	{key: "card.why.promised", rules: []string{"promised_ask"}},
}

// cardWhy are the card's reasons: the first two rows of the collapse table
// that apply, each said once. A rules escalation only speaks when nothing
// above it does.
func cardWhy(w cli.Wording, r dealCheckResult, snap dealSnapshot, f cardFacts) []string {
	rules, questions := map[string]bool{}, map[string]bool{}
	for _, d := range r.Differences {
		rules[d.Rule], questions[d.Question] = true, true
	}
	var why []string
	for _, row := range cardReasons {
		applies := row.when != nil && row.when(r, snap, f)
		for _, rule := range row.rules {
			applies = applies || rules[rule]
		}
		for _, q := range row.questions {
			applies = applies || questions[q]
		}
		if !applies || (row.key == "card.why.rules_ask" && len(why) > 0) {
			continue
		}
		// A first dealing is said only beside the rules' own reason, never
		// in its place.
		if row.key == "card.why.first_dealing" && !slices.Contains(why, w.Line("card.why.rules_ask", nil)) {
			continue
		}
		what := f.picked
		if row.key == "card.why.first_disclosure.some" {
			what = f.told
		}
		text := w.Line(row.key, cli.V{"limit": f.limit, "floor": f.floor, "what": what, "days": strconv.Itoa(cli.RulesHistoryDays)})
		if text != "" && !slices.Contains(why, text) {
			why = append(why, text)
		}
		// A refusal for an acceptance past the window ends on what to do:
		// nothing is said after it.
		if row.key == "card.why.rules_deny.acceptance_too_old" && text != "" {
			break
		}
		if len(why) == 2 {
			break
		}
	}
	return why
}

// cardTag is the card's tag: not allowed on a deny, stop on a scam-shaped
// signal, paused otherwise.
func cardTag(w cli.Wording, r dealCheckResult, demo bool) string {
	key := "card.tag.ask"
	switch {
	case r.Verdict == "deny":
		key = "card.tag.deny"
	case slices.ContainsFunc(r.Differences, func(d dealDifference) bool { return stopRules[d.Rule] }):
		key = "card.tag.stop"
	}
	tag := w.Line(key, nil)
	if demo {
		tag = w.Line("card.tag.demo", cli.V{"tag": tag})
	}
	return tag
}

// cardWho is who the action goes to: the payee (or name) the check saw,
// and the site in brackets.
func cardWho(state dealState, snap dealSnapshot) (payee, where string) {
	who := state.who
	if snap.Who != nil {
		if snap.Who.Name != "" {
			who.Name = snap.Who.Name
		}
		if snap.Who.Domain != "" {
			who.Domain = snap.Who.Domain
		}
		if snap.Who.Payee != "" {
			who.Payee = snap.Who.Payee
		}
	}
	if namedRecipient(snap.DisclosingTo) && snap.Recipient != nil {
		who = *snap.Recipient
	}
	payee = who.Payee
	if payee == "" {
		payee = who.Name
	}
	if payee == "" {
		payee = recipientName(who)
	}
	if who.Domain != "" {
		where = " (" + who.Domain + ")"
	}
	return payee, where
}

// checkCard builds the card a paused or denied check shows.
func checkCard(w cli.Wording, state dealState, snap dealSnapshot, r dealCheckResult, events []sealedEvent) cli.Ask {
	terms := state.terms
	if snap.Terms != nil {
		terms = overlayTerms(terms, *snap.Terms)
	}
	currency := terms.Currency
	money := func(v int64) string { return spokenMoney(v, currency) }
	payee, where := cardWho(state, snap)
	item := shortTitle(terms.Item)
	amount := ""
	if snap.AmountMinor != nil {
		amount = money(*snap.AmountMinor)
	}
	limit, floor := "", ""
	if m := state.intent.MaxTotalMinor; m != nil {
		limit = money(*m)
	}
	if m := state.intent.MinTotalMinor; m != nil {
		floor = money(*m)
	}

	ask := cli.Ask{Tag: cardTag(w, r, state.open.Demo)}
	vals := cli.V{"amount": amount, "payee": payee, "where": where, "item": item,
		"what": disclosureWords(w, snap.Disclosing), "description": snap.Description}
	ask.Question = cardQuestion(w, snap, vals)
	// A seller's cancel on a sale's thread with a live acceptance records
	// that the buyer backed out: the user confirms that, not a cancellation
	// with a merchant.
	if snap.Action == "cancel" && state.saleThread() && state.buyerAccepted && !state.calledOff {
		ask.Question = w.Line("card.question.cancel.acceptance", vals)
	}
	if r.Verdict == "deny" {
		// A deny takes no yes: it says what will not happen, and why it
		// stopped (u245: "will not do it and will not ask").
		ask.Question, ask.WhyKey = cardDenial(w, snap, vals), "card.why.label_stopped"
	}
	ask.Facts = cardFactsLine(w, state, snap, r, terms, money)
	ask.Terms = cardTermsLine(w, state, snap, r)
	askedOver := false
	if total, field := limitBound(terms, snap); total != nil {
		askedOver = askedException(state.intent.Asked, terms, *total, field)
	}
	var told []string
	if r.Recipient != nil {
		told = r.Recipient.First
	}
	// A share to the thread's buyer of something the acceptance releases
	// (a platform's or anyone else's is not its to release, and the public
	// pickup area needs none).
	acceptanceOld := isShare(snap.Action) && snap.DisclosingTo == "counterparty" && slices.ContainsFunc(snap.Disclosing, func(c string) bool { return c != "pickup_area" }) &&
		state.saleThread() && state.acceptanceOutsideHistory(state.historySince)
	ask.Why = cardWhy(w, r, snap, cardFacts{limit: limit, floor: floor, picked: pickedWords(r), told: disclosureWords(w, told), askedOver: askedOver, acceptanceOld: acceptanceOld})
	ask.GoodToKnow = cardGoodToKnow(w, r)
	// A share before a payment is the first of two stops; the charge is not
	// known yet, so the line names none.
	if r.Verdict != "deny" && isShare(snap.Action) && slices.Contains(state.intent.Allowed, "pay") && !paid(events) {
		ask.Next = w.Line("card.next.pay", nil)
	}
	for _, o := range r.Options {
		ask.Options = append(ask.Options, cli.AskOption{Label: o.Label, Choice: o.ID})
	}
	return ask
}

func isShare(action string) bool { return action == "share_contact" || action == "share_credentials" }

func paid(events []sealedEvent) bool {
	return slices.ContainsFunc(events, func(se sealedEvent) bool {
		return se.Event.Kind == "act" && se.Event.Act != nil && se.Event.Act.Action == "pay"
	})
}

// cardQuestion is the headline as a question: verb, what, to whom, why.
func cardQuestion(w cli.Wording, snap dealSnapshot, v cli.V) string {
	if rp := snap.Representing; snap.Action == representAction && rp != nil {
		return w.Line("card.question.represent", cli.V{"buyer": strings.TrimSpace(v["payee"]), "class": classWord(rp.Class), "text": rp.Text})
	}
	base := "card.question." + snap.Action
	var keys []string
	switch {
	case snap.Action == "pay" && v["amount"] == "":
		keys = append(keys, base+".no_amount")
	case snap.Action == "share_contact" && v["what"] == "":
		keys = append(keys, base+".details")
	}
	if v["item"] == "" {
		keys = append(keys, base+".no_item")
	}
	for _, key := range append(keys, base) {
		if _, ok := w.Entries[key]; ok {
			return w.Line(key, v)
		}
	}
	return w.Line("card.question.other", v)
}

// disclosureWords are the classes a share gives, as a sentence lists them.
func disclosureWords(w cli.Wording, classes []string) string {
	words := make([]string, 0, len(classes))
	sorted := slices.Clone(classes)
	slices.SortStableFunc(sorted, func(a, b string) int { return classRank(a) - classRank(b) })
	for _, c := range slices.Compact(sorted) {
		if word := w.Line("class."+c, nil); word != "" {
			words = append(words, word)
		} else {
			words = append(words, classWord(c))
		}
	}
	return listWords(words, "and")
}

// cardFactsLine is the money line: the amount (with what it adds to the price),
// the rail, whether it comes back, and whether it is within the limit.
func cardFactsLine(w cli.Wording, state dealState, snap dealSnapshot, r dealCheckResult, terms dealTerms, money func(int64) string) string {
	amount := snap.AmountMinor
	if amount == nil && (snap.Action == "commit" || snap.Action == "pay") {
		// A commitment says its price.
		amount = terms.PriceMinor
	}
	if amount == nil {
		return ""
	}
	var parts []string
	if p := terms.PriceMinor; p != nil && *amount > *p {
		parts = append(parts, w.Line("card.facts.breakdown", cli.V{"base": money(*p), "extra": money(*amount - *p)}))
	} else {
		parts = append(parts, money(*amount))
	}
	// What may be taken, when more than the charge: it is what a limit binds.
	if m := snap.AuthorizedMaxMinor; m != nil && *m > *amount && (state.intent.MaxTotalMinor == nil || *m != *state.intent.MaxTotalMinor) {
		// Said only when it is not the limit itself, said next.
		parts[0] = w.Line("card.facts.up_to", cli.V{"amount": parts[0], "max": money(*m)})
	}
	recourse := state.wayToPay()
	if snap.Recourse != nil {
		recourse = overlayRecourse(recourse, *snap.Recourse)
	}
	if recourse.Rail != "" {
		parts = append(parts, w.Line("card.facts.by", cli.V{"rail": railName(recourse.Rail)}))
	}
	if recourse.Refundable != nil {
		key := "card.facts.not_refundable"
		if *recourse.Refundable {
			key = "card.facts.refundable"
		}
		parts = append(parts, w.Line(key, nil))
	}
	if m := state.intent.MaxTotalMinor; m != nil {
		// Over exactly when the check found it over (its over_limit
		// difference), so the facts and the reason never disagree.
		key := "card.facts.within"
		if hasRule(r, "over_limit") {
			key = "card.facts.over"
		}
		parts = append(parts, w.Line(key, cli.V{"limit": money(*m)}))
	}
	return strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), " · ")
}

// pickedWords are the agent's paused picks, as one phrase.
// A default (quantity 1, free shipping) is named only when it is the one
// pick the check paused on.
func pickedWords(r dealCheckResult) string {
	var said, defaults []string
	for _, a := range r.Picked {
		if !slices.ContainsFunc(r.Differences, func(d dealDifference) bool { return d.Rule == "agent_picked" && d.Field == a.Field }) {
			continue
		}
		if isDefaultPick(a) {
			defaults = append(defaults, a.Label+" "+a.Value)
		} else {
			said = append(said, a.Label+" "+a.Value)
		}
	}
	if len(said) == 0 {
		said = defaults
	}
	return listWords(said, "and")
}

// cardGoodToKnow is at most one line: a choice the agent made that did not
// pause (never a default: quantity 1, free shipping), or else a claim the
// decision rests on that nobody checked.
func cardGoodToKnow(w cli.Wording, r dealCheckResult) string {
	for _, a := range r.Picked {
		if isDefaultPick(a) || slices.ContainsFunc(r.Differences, func(d dealDifference) bool { return d.Rule == "agent_picked" && d.Field == a.Field }) {
			continue
		}
		return w.Line("card.good_to_know.picked", cli.V{"what": a.Label + " " + a.Value})
	}
	if len(r.Unverified) > 0 {
		return w.Line("card.good_to_know.unverified", cli.V{"claim": r.Unverified[0]})
	}
	return ""
}

// isDefaultPick is a pick nobody would call a choice: one of the item, or a
// free shipping or delivery option. Only those fields: "free shipping" in
// any other value (a size, an item) is still a pick.
func isDefaultPick(a cli.DealAttribute) bool {
	v := strings.ToLower(strings.TrimSpace(a.Value))
	switch a.Field {
	case "quantity":
		return v == "1"
	case "shipping", "conditions.shipping", "conditions.delivery":
		return strings.HasPrefix(v, "free")
	}
	return false
}

// labelOptions gives the check's options their button labels from the
// pack, before the check is sealed: verb and amount, "anyway" only when a
// limit or rule the user set is broken, and Hold.
func labelOptions(w cli.Wording, r *dealCheckResult, snap dealSnapshot, state dealState) {
	amount := cardAmount(snap, state)
	violation := slices.ContainsFunc(r.Differences, func(d dealDifference) bool { return violationRules[d.Rule] })
	for i, o := range r.Options {
		var label string
		switch o.ID {
		case "hold":
			label = w.Line("card.button.hold", nil)
			if r.Verdict == "deny" {
				// The one button of a deny acknowledges it.
				label = w.Line("card.button.ok", nil)
			}
		case "verify_contact":
			// The number from the leg's first contact, never the changed one.
			label = w.Line("card.button.verify_contact", cli.V{"phone": state.legFirst.Phone})
		case "proceed":
			key := "card.button." + snap.Action
			if snap.Action == "pay" && amount == "" {
				key += ".no_amount"
			}
			if violation {
				key += ".anyway"
			}
			label = w.Line(key, cli.V{"amount": amount})
		}
		if label == "" {
			label = o.ID
		}
		r.Options[i].Label = label
	}
}

// promisedPay is, on a pay check, whether an earlier card said the user
// would be asked again before paying (a share that was step 1 of 2) and no
// pay has happened since: the pay is then asked even when its check passes.
func promisedPay(events []sealedEvent, state dealState, snap dealSnapshot) bool {
	if snap.Action != "pay" || paid(events) || !slices.Contains(state.intent.Allowed, "pay") {
		return false
	}
	// A promise is kept once the user answered its pause with a yes; a Hold
	// leaves it standing.
	promisedChecks := map[string]bool{}
	for _, se := range events {
		if c := se.Event.Check; se.Event.Kind == "check" && c != nil && hasRule(*c, "promised_ask") {
			promisedChecks[se.CapsuleID] = true
		}
		if a := se.Event.Approval; a != nil && promisedChecks[a.Check] && a.Proceed && a.Reason == "" {
			return false
		}
	}
	// The promise is the one a share card made: paused (a deny makes none),
	// in a deal that then allowed paying.
	for i, se := range events {
		c := se.Event.Check
		if se.Event.Kind != "check" || c == nil || !isShare(c.Action) || c.Verdict != "pause" {
			continue
		}
		if then, err := foldDeal(events[:i+1]); err == nil && slices.Contains(then.intent.Allowed, "pay") {
			return true
		}
	}
	return false
}

// cardAmount is the amount a check is about, in the deal's currency.
func cardAmount(snap dealSnapshot, state dealState) string {
	if snap.AmountMinor == nil {
		return ""
	}
	currency := state.terms.Currency
	if snap.Terms != nil && snap.Terms.Currency != "" {
		currency = snap.Terms.Currency
	}
	return spokenMoney(*snap.AmountMinor, currency)
}

// listWords joins words as a sentence does: "a", "a and b", "a, b and c".
func listWords(words []string, conj string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " " + conj + " " + words[len(words)-1]
}

// spokenClassOrder is the order a sentence lists what is shared in: who you
// are, how to reach you, where you are, then the rest.
var spokenClassOrder = []string{"name", "email", "phone", "home_address", "address", "pickup_location", "pickup_area", "other_contact", "credential", "verification_code", "payment_card", "id_document"}

func classRank(c string) int {
	if i := slices.Index(spokenClassOrder, c); i >= 0 {
		return i
	}
	return len(spokenClassOrder)
}

func hasRule(r dealCheckResult, rule string) bool {
	return slices.ContainsFunc(r.Differences, func(d dealDifference) bool { return d.Rule == rule })
}

// cardDenial is what a deny says will not happen.
func cardDenial(w cli.Wording, snap dealSnapshot, v cli.V) string {
	if rp := snap.Representing; snap.Action == representAction && rp != nil {
		return w.Line("card.deny.represent", cli.V{"buyer": strings.TrimSpace(v["payee"])})
	}
	base := "card.deny." + snap.Action
	var keys []string
	switch {
	case snap.Action == "pay" && v["amount"] == "":
		keys = append(keys, base+".no_amount")
	case snap.Action == "share_contact" && v["what"] == "":
		keys = append(keys, base+".details")
	}
	for _, key := range append(keys, base) {
		if _, ok := w.Entries[key]; ok {
			return w.Line(key, v)
		}
	}
	return w.Line("card.deny.other", v)
}
