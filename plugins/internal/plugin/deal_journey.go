package plugin

import (
	"slices"
	"strconv"
	"strings"

	"github.com/action-state-group/capsule-cli/internal/cli"
)

// One deal per errand (capsule-cli#340): the deal opens at the user's first
// ask, and records the errand's journey in a fixed vocabulary whose labels
// the tool derives, never the agent.
//
//   - User moves are intent notes. Each one's move (asked, narrowed,
//     switched, raised, picked) is derived from what it changes against the
//     intent in force (intentMove); the user calling the errand off is a
//     close, status called_off.
//   - Agent moves are agent_move records, one note kind per label
//     (searched, options, recommended, chose, couldnt): the command fixes
//     the label. Asking the seller, checking a claim, pausing, telling and
//     acting are the existing messages, evidence, checks, disclosures and
//     acts.
//
// A leg is the stretch of the errand with one counterparty. It starts at
// the opening's counterparty, at a user's intent that names another one
// (switched, or picked from the options), or at the agent's choice (chose)
// while no leg exists. Each leg's first contact is what a payee or contact
// change is compared with, so the user's own switch never reads as the
// counterparty changing its details, while a seller's change within the leg
// still does.

// dealJourneyVersion marks a step sealed with the journey records, kept
// with the step like Producer: an intent sealed before it carries no move,
// and folds its asked terms whole, as it always did.
const dealJourneyVersion = "1"

// Agent moves, one note kind each.
const (
	moveSearched    = "searched"
	moveOptions     = "options"
	moveRecommended = "recommended"
	moveChose       = "chose"
	moveCouldnt     = "couldnt"
)

var agentMoveKinds = []string{moveSearched, moveOptions, moveRecommended, moveChose, moveCouldnt}

// User moves, derived for each intent note.
const (
	moveAsked    = "asked"
	moveNarrowed = "narrowed"
	moveSwitched = "switched"
	moveRaised   = "raised"
	movePicked   = "picked"
)

// couldntReasons is why the agent could not go on, from a closed set.
var couldntReasons = []string{"sign_in_required", "out_of_stock", "cannot_ship_here", "blocked", "payment_declined", "site_error", "other"}

// delegable is what the user's own words may hand the agent to choose.
var delegable = []string{"item"}

// maxOptionsShown bounds one shortlist.
const maxOptionsShown = 20

// dealOptionShown is one option of a shortlist the agent showed the user.
type dealOptionShown struct {
	Item       string   `json:"item"`
	Who        *dealWho `json:"who,omitempty"`
	PriceMinor *int64   `json:"price_minor,omitempty"`
}

// dealAgentMove is one agent move. Which fields it carries depends on Move,
// which is set from the note kind and never from the input file.
type dealAgentMove struct {
	Move string `json:"move"`
	// Where and Results: searched.
	Where   string `json:"where,omitempty"`
	Results *int64 `json:"results,omitempty"`
	// Options and Found: options (Found is how many were found, at least
	// as many as were shown).
	Options []dealOptionShown `json:"options,omitempty"`
	Found   *int64            `json:"found,omitempty"`
	// Option is, on recommended, the recommended option's place (from 1)
	// in the latest options; Item and Who are then set from it.
	Option int64 `json:"option,omitempty"`
	// Reason is, on recommended, the agent's stated reason; on couldnt, why
	// it could not go on (couldntReasons), with Detail when other.
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Item, PriceMinor and Who: chose (the item the agent chose under the
	// user's delegation, and its merchant while no leg exists), and as
	// resolved on recommended. Who on couldnt is the merchant it could not
	// go on with.
	Item       string   `json:"item,omitempty"`
	PriceMinor *int64   `json:"price_minor,omitempty"`
	Who        *dealWho `json:"who,omitempty"`
}

// validate checks an agent move's input against its kind.
func (m *dealAgentMove) validate() error {
	has := func(set bool, field string) error {
		if set {
			return cli.InputError(field + " does not go with --kind " + m.Move)
		}
		return nil
	}
	notFor := func(fields map[string]bool) error {
		for f, set := range fields {
			if err := has(set, f); err != nil {
				return err
			}
		}
		return nil
	}
	switch m.Move {
	case moveSearched:
		if strings.TrimSpace(m.Where) == "" || m.Results == nil || *m.Results < 0 {
			return cli.InputError(`searched needs "where" (the site or app searched) and "results" (how many it found)`)
		}
		return notFor(map[string]bool{"options": m.Options != nil, "found": m.Found != nil, "option": m.Option != 0, "reason": m.Reason != "", "detail": m.Detail != "", "item": m.Item != "", "price_minor": m.PriceMinor != nil, "who": m.Who != nil})
	case moveOptions:
		if len(m.Options) == 0 || len(m.Options) > maxOptionsShown {
			return cli.InputError("options needs the options shown to the user, 1 to " + strconv.Itoa(maxOptionsShown) + ", each with its item")
		}
		for _, o := range m.Options {
			if strings.TrimSpace(o.Item) == "" || (o.PriceMinor != nil && *o.PriceMinor < 0) {
				return cli.InputError("every option needs its item, and a price_minor that is not negative")
			}
		}
		if m.Found == nil {
			n := int64(len(m.Options))
			m.Found = &n
		}
		if *m.Found < int64(len(m.Options)) {
			return cli.InputError("found is how many were found: at least as many as were shown")
		}
		return notFor(map[string]bool{"where": m.Where != "", "results": m.Results != nil, "option": m.Option != 0, "reason": m.Reason != "", "detail": m.Detail != "", "item": m.Item != "", "price_minor": m.PriceMinor != nil, "who": m.Who != nil})
	case moveRecommended:
		if m.Option < 1 || strings.TrimSpace(m.Reason) == "" {
			return cli.InputError(`recommended needs "option" (its place, from 1, in the options shown) and "reason" (why, as you told the user)`)
		}
		return notFor(map[string]bool{"where": m.Where != "", "results": m.Results != nil, "options": m.Options != nil, "found": m.Found != nil, "detail": m.Detail != "", "item": m.Item != "", "price_minor": m.PriceMinor != nil, "who": m.Who != nil})
	case moveChose:
		if strings.TrimSpace(m.Item) == "" || (m.PriceMinor != nil && *m.PriceMinor < 0) {
			return cli.InputError(`chose needs "item" (what you chose for the user), and a price_minor that is not negative`)
		}
		if m.Who != nil && *m.Who == (dealWho{}) {
			return cli.InputError("chose's who needs at least one identifying field")
		}
		return notFor(map[string]bool{"where": m.Where != "", "results": m.Results != nil, "options": m.Options != nil, "found": m.Found != nil, "option": m.Option != 0, "reason": m.Reason != "", "detail": m.Detail != ""})
	case moveCouldnt:
		if !slices.Contains(couldntReasons, m.Reason) {
			return cli.InputError("couldnt needs reason: one of " + strings.Join(couldntReasons, ", "))
		}
		if m.Reason == "other" && strings.TrimSpace(m.Detail) == "" {
			return cli.InputError(`couldnt with reason other needs "detail": what stopped you, in a few words`)
		}
		if m.Reason != "other" && m.Detail != "" {
			return cli.InputError(`couldnt's "detail" goes with reason other only: the reason ` + m.Reason + ` says why`)
		}
		if m.Who != nil && *m.Who == (dealWho{}) {
			return cli.InputError("couldnt's who needs at least one identifying field")
		}
		return notFor(map[string]bool{"where": m.Where != "", "results": m.Results != nil, "options": m.Options != nil, "found": m.Found != nil, "option": m.Option != 0, "item": m.Item != "", "price_minor": m.PriceMinor != nil})
	}
	return cli.InputError("--kind must be one of " + strings.Join(agentMoveKinds, ", ") + " for an agent move")
}

// sameWho reads two counterparties as one: by their mechanical identities
// when both have any (sameParty), else by name.
func sameWho(a, b dealWho) bool {
	ka, kb := counterpartyKeys(a, ""), counterpartyKeys(b, "")
	if len(ka) > 0 && len(kb) > 0 {
		return sameParty(ka, kb)
	}
	na, nb := strings.TrimSpace(a.Name), strings.TrimSpace(b.Name)
	if na == "" {
		na = strings.TrimSpace(a.Payee)
	}
	if nb == "" {
		nb = strings.TrimSpace(b.Payee)
	}
	return na != "" && strings.EqualFold(na, nb)
}

// shortlisted is the option of the latest shortlist that the item or who
// names, if any.
func (s dealState) shortlisted(item string, who *dealWho) (dealOptionShown, bool) {
	for _, o := range s.options {
		switch {
		case who != nil && o.Who != nil && sameWho(*who, *o.Who):
			if item == "" || strings.EqualFold(strings.TrimSpace(item), strings.TrimSpace(o.Item)) {
				return o, true
			}
		case who == nil && item != "" && strings.EqualFold(strings.TrimSpace(item), strings.TrimSpace(o.Item)):
			return o, true
		}
	}
	return dealOptionShown{}, false
}

// dealIntentMove is what an intent note is and does, derived from the
// state before it: its label, whether it starts a leg, and whether it
// proposes more than the limits in force (applied only on the user's
// confirm_limits, whatever its label).
type dealIntentMove struct {
	Move     string
	LegStart bool
	Proposed bool
}

// effectiveIntent is what of an intent a note can change: the ask, the
// limits, the delegation.
func sameEffectiveIntent(a, b dealIntent) bool {
	eq := func(x, y *int64) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return termsEqual(a.Asked, b.Asked) && eq(a.MaxTotalMinor, b.MaxTotalMinor) && eq(a.MinTotalMinor, b.MinTotalMinor) &&
		(a.Allowed == nil) == (b.Allowed == nil) && slices.Equal(a.Allowed, b.Allowed) && slices.Equal(a.Delegated, b.Delegated)
}

func termsEqual(a, b dealTerms) bool {
	ja, errA := cli.JCSOf(termsBody(a))
	jb, errB := cli.JCSOf(termsBody(b))
	return errA == nil && errB == nil && string(ja) == string(jb)
}

// intentMove derives an intent note's move from the state before it. A
// note that changes nothing and proposes nothing is refused: the user's
// words that change nothing (an answer, a route choice) are a message.
func intentMove(s dealState, note dealIntent) (dealIntentMove, error) {
	var m dealIntentMove
	_, m.Proposed = proposedLimits(s.intent.limits(), note)
	if note.Who != nil {
		// The merchant in play, as it first appeared or as it has changed
		// its details since: a change of its own is its record (a change
		// note), never the user's switch.
		if s.leg && (sameWho(*note.Who, s.legFirst) || sameWho(*note.Who, s.who) || sharesKey(perParty(counterpartyKeys(*note.Who, "")), perParty(counterpartyKeys(s.who, "")))) {
			return m, cli.InputError("intent.who names the merchant already in play (as it first appeared, or with details it changed since): leave who out, or name the one the user switched to")
		}
		m.LegStart = true
	}
	next := journeyIntent(s.intent, note)
	if !m.LegStart && !m.Proposed && sameEffectiveIntent(s.intent, next) {
		return m, cli.InputError("this intent note changes nothing the user asked: the user's words that change nothing (an answer, a choice of route) go in a message (deal note --kind message, from user)")
	}
	_, onList := s.shortlisted(note.Asked.Item, note.Who)
	switch {
	case note.Who != nil && s.leg:
		m.Move = moveSwitched
	case note.Who != nil && s.routeSet && s.routeCouldnt && !sameWho(*note.Who, s.route):
		m.Move = moveSwitched
	case note.Who != nil && s.routeSet && !onList && !sameWho(*note.Who, s.route):
		m.Move = moveSwitched
	case note.Who != nil && onList:
		m.Move = movePicked
	case note.Who != nil:
		m.Move = moveNarrowed
	case note.Asked.Item != "" && onList:
		m.Move = movePicked
	case note.Asked.Item != "" && s.intent.Asked.Item != "" && !strings.EqualFold(strings.TrimSpace(note.Asked.Item), strings.TrimSpace(s.intent.Asked.Item)):
		m.Move = moveSwitched
	case !sameEffectiveIntent(s.intent, next):
		m.Move = moveNarrowed
	default:
		m.Move = moveRaised
	}
	return m, nil
}

// journeyIntent folds a journey-era intent note over the intent in force:
// as laterIntent, but the note's asked terms overlay the ask field by field
// (the errand's original ask survives a switch of merchant), and naming an
// item takes the item back from the agent's delegation. A note never widens
// the delegation: adding to it is a proposal, applied on confirm_limits.
func journeyIntent(cur, next dealIntent) dealIntent {
	out := laterIntent(cur, next)
	out.Asked = overlayTerms(cur.Asked, next.Asked)
	out.Delegated = slices.Clone(cur.Delegated)
	out.DelegatedSaid = cur.DelegatedSaid
	if next.Asked.Item != "" {
		out.Delegated = slices.DeleteFunc(out.Delegated, func(d string) bool { return d == "item" })
	}
	out.Who, out.Why = nil, ""
	return out
}

// validDelegation checks an intent's delegation: from the closed set, with
// the user's own words, and never beside an item the user named.
func (i dealIntent) validDelegation() error {
	for _, d := range i.Delegated {
		if !slices.Contains(delegable, d) {
			return cli.InputError("delegated may name only " + strings.Join(delegable, ", "))
		}
	}
	switch {
	case len(i.Delegated) > 0 && strings.TrimSpace(i.DelegatedSaid) == "":
		return cli.InputError(`delegated needs "delegated_said": the user's own words handing you the choice`)
	case len(i.Delegated) == 0 && i.DelegatedSaid != "":
		return cli.InputError("delegated_said goes with delegated")
	case slices.Contains(i.Delegated, "item") && i.Asked.Item != "":
		return cli.InputError("the user named the item (asked.item), so it is not delegated: leave one of them out")
	}
	return nil
}

// startLeg begins a new leg with who: its first contact, and nothing agreed
// with it yet but what was picked or chosen (item and price, the deal's
// currency). Claims, messages and the way back were the earlier
// counterparty's.
func (s *dealState) startLeg(who dealWho, item string, price *int64) {
	s.leg, s.legFirst, s.who = true, who, who
	s.whoSource = map[string]string{}
	currency := s.open.Terms.Currency
	s.agreed = dealTerms{Currency: currency, Item: item, PriceMinor: price}
	s.terms = s.agreed
	s.recourse, s.agreedRecourse = dealRecourse{}, dealRecourse{}
	s.claims, s.messages = nil, nil
	s.route, s.routeSet, s.routeCouldnt = who, true, false
	s.chose, s.legStarted = "", true
}

// adoptPick records an item picked or chosen within the leg in force: it is
// what the user's yes is about from now on.
func (s *dealState) adoptPick(item string, price *int64) {
	if item == "" {
		return
	}
	s.agreed.Item, s.terms.Item = item, item
	if price != nil {
		p := *price
		s.agreed.PriceMinor, s.terms.PriceMinor = &p, &p
	}
}

// foldIntentJourney folds a journey-era intent note into the state.
func (s *dealState) foldIntentJourney(note dealIntent) {
	m, err := intentMove(*s, note)
	opt, onList := s.shortlisted(note.Asked.Item, note.Who)
	item, price := note.Asked.Item, note.Asked.PriceMinor
	if onList && m.Move == movePicked {
		// The user picked an option: its item and price are what they
		// picked, and the item is theirs, no longer the agent's to choose.
		if item == "" {
			item = opt.Item
		}
		if price == nil {
			price = opt.PriceMinor
		}
	}
	s.intent = journeyIntent(s.intent, note)
	// A pick named by its merchant is the user's pick of that option's
	// item: theirs in the ask, not the agent's.
	if item != "" && note.Asked.Item == "" && m.Move == movePicked {
		s.intent.Asked.Item = item
	}
	if item != "" {
		s.intent.Delegated = slices.DeleteFunc(s.intent.Delegated, func(d string) bool { return d == "item" })
		s.chose = ""
	}
	if err == nil && m.LegStart {
		s.startLeg(*note.Who, item, price)
		return
	}
	if err == nil && (m.Move == movePicked || m.Move == moveSwitched) {
		s.adoptPick(item, price)
	}
}

// foldAgentMove folds an agent move into the state.
func (s *dealState) foldAgentMove(m dealAgentMove) {
	switch m.Move {
	case moveOptions:
		s.options = slices.Clone(m.Options)
	case moveRecommended:
		if m.Who != nil {
			s.route, s.routeSet, s.routeCouldnt = *m.Who, true, false
		}
	case moveCouldnt:
		if m.Who != nil {
			s.route, s.routeSet = *m.Who, true
		}
		if s.routeSet {
			s.routeCouldnt = true
		}
	case moveChose:
		if m.Who != nil && !s.leg {
			s.startLeg(*m.Who, m.Item, m.PriceMinor)
		} else {
			s.adoptPick(m.Item, m.PriceMinor)
		}
		s.chose = m.Item
	}
}

// legWho is the counterparty of the leg in force after events: the
// opening's, or the latest leg start's.
func legWho(events []sealedEvent) dealWho {
	var w dealWho
	for _, se := range events {
		switch e := se.Event; {
		case e.Open != nil:
			w = e.Open.Who
		case e.Kind == "intent" && e.Intent != nil && e.Intent.Who != nil:
			w = *e.Intent.Who
		case e.AgentMove != nil && e.AgentMove.Move == moveChose && e.AgentMove.Who != nil:
			w = *e.AgentMove.Who
		}
	}
	return w
}

// legStartOf reports whether a step starts a leg: an intent naming a who
// (refused unless it names another one), or the agent's choice naming one
// (refused while a leg exists).
func legStartOf(e dealEvent) bool {
	return (e.Kind == "intent" && e.Intent != nil && e.Intent.Who != nil) ||
		(e.AgentMove != nil && e.AgentMove.Move == moveChose && e.AgentMove.Who != nil)
}

// dealJourneyStep is one move of the errand, as the report lists it: who
// moved, the move's token, its label from the wording pack, and the values
// the label was filled with (who, item, reason, classes, amount, limit,
// count), so another renderer can word it from its own pack.
type dealJourneyStep struct {
	By     string `json:"by"`
	Move   string `json:"move"`
	Count  int    `json:"count,omitempty"`
	Found  int64  `json:"found,omitempty"`
	Label  string `json:"label"`
	Values cli.V  `json:"values,omitempty"`
	Step   string `json:"step"`
}

// dealJourney is the errand's moves in order: the user's (asked, narrowed,
// switched, raised, picked, called_off), the agent's (searched, options,
// recommended, chose, asked_seller, checked, couldnt, paused, told, and
// what it did) and the other side's (said, changed). An intent sealed
// before journey records is "changed". The report is the user's own copy:
// a label may name the merchant, the item and the classes told.
func dealJourney(events []sealedEvent, w cli.Wording) []dealJourneyStep {
	out := []dealJourneyStep{}
	add := func(by, move, key string, se sealedEvent, vals cli.V) {
		if key == "" {
			key = "journey." + by + "." + move
		}
		out = append(out, dealJourneyStep{By: by, Move: move, Label: w.Line(key, vals), Values: vals, Step: se.CapsuleID})
	}
	currency := ""
	if len(events) > 0 && events[0].Event.Open != nil {
		currency = events[0].Event.Open.Terms.Currency
	}
	for i, se := range events {
		e := se.Event
		switch {
		case e.Kind == "open" && e.Open != nil:
			add("user", moveAsked, "", se, nil)
		case e.Kind == "intent" && e.Intent != nil:
			if e.Journey == "" {
				add("user", "changed", "", se, nil)
				continue
			}
			state, err := foldDeal(events[:i])
			if err != nil {
				continue
			}
			m, err := intentMove(state, *e.Intent)
			if err != nil {
				continue
			}
			switch {
			case m.Move == moveSwitched && e.Intent.Who != nil:
				add("user", m.Move, "", se, cli.V{"who": recipientName(*e.Intent.Who)})
			case m.Move == moveSwitched:
				add("user", m.Move, "journey.user.switched.item", se, cli.V{"item": e.Intent.Asked.Item})
			default:
				add("user", m.Move, "", se, nil)
			}
			if m.Proposed && m.Move != moveRaised {
				add("user", moveRaised, "", se, nil)
			}
		case e.AgentMove != nil:
			am := e.AgentMove
			switch am.Move {
			case moveOptions:
				n := len(am.Options)
				vals := cli.V{"count": strconv.Itoa(n)}
				out = append(out, dealJourneyStep{By: "agent", Move: moveOptions, Count: n, Found: *am.Found,
					Label: w.Line("journey.agent.options", vals), Values: vals, Step: se.CapsuleID})
			case moveCouldnt:
				reason := am.Detail
				if am.Reason != "other" {
					reason = w.Line("journey.reason."+am.Reason, nil)
				}
				if am.Who != nil {
					add("agent", am.Move, "journey.agent.couldnt.who", se, cli.V{"who": recipientName(*am.Who), "reason": reason})
				} else {
					add("agent", am.Move, "", se, cli.V{"reason": reason})
				}
			default:
				add("agent", am.Move, "", se, nil)
			}
		case e.Kind == "message" && e.Message != nil:
			switch e.Message.From {
			case "agent":
				add("agent", "asked_seller", "", se, nil)
			case "counterparty":
				add("other", "said", "", se, nil)
			}
		case e.Kind == "change":
			add("other", "changed", "", se, nil)
		case e.Kind == "evidence" && e.Evidence != nil && e.Evidence.Verified:
			add("agent", "checked", "", se, nil)
		case e.Kind == "check" && e.Check != nil && e.Check.Verdict != "pass":
			add("agent", "paused", "", se, nil)
		case e.Kind == "disclosure" && e.Disclosure != nil:
			d := e.Disclosure
			classes := make([]string, len(d.Fields))
			for j, f := range d.Fields {
				classes[j] = f.Class
			}
			who := recipientName(legWho(events[:i]))
			if d.Who != nil && namedRecipient(d.To) {
				who = recipientName(*d.Who)
			}
			add("agent", "told", "", se, cli.V{"who": who, "classes": disclosureWords(w, classes)})
		case e.Kind == "act" && e.Act != nil:
			a := e.Act
			move := "act." + a.Action
			if !slices.Contains([]string{"pay", "commit", "sign", "cancel", "offer"}, a.Action) {
				move = "act.other"
			}
			if a.Action == "pay" && a.AmountMinor != nil {
				cur := a.Currency
				if cur == "" {
					cur = currency
				}
				vals := cli.V{"amount": spokenMoney(*a.AmountMinor, cur)}
				key := "journey.agent.act.pay"
				if state, err := foldDeal(events[:i]); err == nil && state.intent.MaxTotalMinor != nil &&
					strings.EqualFold(cur, currency) && *a.AmountMinor <= *state.intent.MaxTotalMinor {
					key, vals["limit"] = "journey.agent.act.pay.within", spokenMoney(*state.intent.MaxTotalMinor, cur)
				}
				add("agent", move, key, se, vals)
				continue
			}
			if a.Action == "pay" {
				add("agent", move, "journey.agent.act.pay.no_amount", se, nil)
				continue
			}
			add("agent", move, "", se, nil)
		case e.Kind == "close" && e.Close != nil && e.Close.Status == "called_off":
			add("user", "called_off", "", se, nil)
		}
	}
	return out
}

// proposingIntent is the intent note a confirm_limits approval answers.
func proposingIntent(events []sealedEvent, check string) *dealIntent {
	for _, se := range events {
		if se.CapsuleID == check && se.Event.Kind == "intent" {
			return se.Event.Intent
		}
	}
	return nil
}

// checkAgentMove checks an agent move against the deal as it stands, and
// resolves a recommendation to the option it names.
func checkAgentMove(events []sealedEvent, m *dealAgentMove) error {
	state, err := foldDeal(events)
	if err != nil {
		return err
	}
	switch m.Move {
	case moveRecommended:
		if len(state.options) == 0 {
			return cli.InputError("recommended names one of the options shown: seal them first (deal note --kind options)")
		}
		if m.Option > int64(len(state.options)) {
			return cli.InputError("option is the recommended option's place, from 1, in the " + strconv.Itoa(len(state.options)) + " options shown")
		}
		o := state.options[m.Option-1]
		m.Item, m.PriceMinor = o.Item, o.PriceMinor
		if o.Who != nil {
			w := *o.Who
			m.Who = &w
		}
	case moveChose:
		if !slices.Contains(state.intent.Delegated, "item") {
			return cli.InputError(`chose is your choice of what the user handed you to choose: their intent must delegate the item ("delegated": ["item"], with their words in "delegated_said"). An item they picked is their intent note`)
		}
		switch {
		case m.Who != nil && state.leg:
			return cli.InputError("chose names a merchant only while none is in play: leave who out. A change of merchant is the user's move (deal note --kind intent with who)")
		case m.Who == nil && !state.leg:
			return cli.InputError("no merchant is in play yet: chose names the one you chose (who)")
		}
	}
	return nil
}

// intentTexts adds the user's words an intent commits to beside its
// verbatim: the words that delegate a choice, and the why of a switch or a
// raise.
func intentTexts(t map[string]string, i dealIntent) {
	if i.DelegatedSaid != "" {
		t["delegated"] = i.DelegatedSaid
	}
	if i.Why != "" {
		t["why"] = i.Why
	}
}

// agentMoveTexts are the texts an agent move commits to: never in a record
// in the clear.
func agentMoveTexts(t map[string]string, m dealAgentMove) {
	switch m.Move {
	case moveSearched:
		t["where"] = m.Where
	case moveOptions:
		for i, o := range m.Options {
			t["option_"+strconv.Itoa(i)] = o.Item
		}
	case moveRecommended:
		t["reason"] = m.Reason
	case moveChose:
		t["item"] = m.Item
	case moveCouldnt:
		if m.Detail != "" {
			t["detail"] = m.Detail
		}
	}
}

// journeyIntentBody adds an intent note's derived move to its record body:
// the label, whether it proposes more than the limits in force, and the
// commitment to the user's why. Derived from the steps before it, so a
// record whose move disagrees with them does not re-derive.
func journeyIntentBody(body map[string]interface{}, events []sealedEvent, i dealIntent, commit func(string) (string, error)) error {
	state, err := foldDeal(events)
	if err != nil {
		return err
	}
	m, err := intentMove(state, i)
	if err != nil {
		return err
	}
	body["move"] = m.Move
	if m.Proposed {
		body["proposed"] = true
	}
	if i.Why != "" {
		if body["why_commitment"], err = commit("why"); err != nil {
			return err
		}
	}
	return nil
}

// agentMoveBody is an agent move's record body: the move, counts and closed
// tokens in the clear, every text as a salted commitment.
func agentMoveBody(m dealAgentMove, commit func(string) (string, error)) (map[string]interface{}, error) {
	body := map[string]interface{}{"move": m.Move}
	var err error
	switch m.Move {
	case moveSearched:
		if body["where_commitment"], err = commit("where"); err != nil {
			return nil, err
		}
		body["results"] = *m.Results
	case moveOptions:
		options := make([]interface{}, len(m.Options))
		for i, o := range m.Options {
			c, err := commit("option_" + strconv.Itoa(i))
			if err != nil {
				return nil, err
			}
			opt := map[string]interface{}{"item_commitment": c}
			if o.PriceMinor != nil {
				opt["price_minor"] = *o.PriceMinor
			}
			options[i] = opt
		}
		body["options"] = options
		body["found"] = *m.Found
	case moveRecommended:
		body["option"] = m.Option
		if body["reason_commitment"], err = commit("reason"); err != nil {
			return nil, err
		}
	case moveChose:
		if body["item_commitment"], err = commit("item"); err != nil {
			return nil, err
		}
		if m.PriceMinor != nil {
			body["price_minor"] = *m.PriceMinor
		}
	case moveCouldnt:
		body["reason"] = m.Reason
		if m.Detail != "" {
			if body["detail_commitment"], err = commit("detail"); err != nil {
				return nil, err
			}
		}
	}
	return body, nil
}

// currentLegStart is the index of the step that started the leg in force:
// 0, the opening, when no later step started one.
func currentLegStart(events []sealedEvent) int {
	from := 0
	for i, se := range events {
		if legStartOf(se.Event) {
			from = i
		}
	}
	return from
}

// agentMoveLine is an agent move as the report's trail says it: the move
// and its counts, never the texts it commits to.
func agentMoveLine(m dealAgentMove) string {
	switch m.Move {
	case moveSearched:
		return "the agent searched (" + strconv.FormatInt(*m.Results, 10) + " results)"
	case moveOptions:
		return "the agent showed you " + strconv.Itoa(len(m.Options)) + " options (of " + strconv.FormatInt(*m.Found, 10) + " found)"
	case moveRecommended:
		return "the agent recommended option " + strconv.FormatInt(m.Option, 10)
	case moveChose:
		return "the agent chose for you, as you asked it to"
	case moveCouldnt:
		return "the agent couldn't go on: " + strings.ReplaceAll(m.Reason, "_", " ")
	}
	return "agent move"
}

// changesPick reports whether a step changes what the errand is buying
// within a leg: the agent's choice, or a journey-era intent naming an item.
func changesPick(e dealEvent) bool {
	return (e.AgentMove != nil && e.AgentMove.Move == moveChose) ||
		(e.Kind == "intent" && e.Journey != "" && e.Intent != nil && e.Intent.Asked.Item != "")
}

// legFirstContact is a leg's first contact as its record names it: as the
// baseline names the opening's, its payee is its name when none is given.
func legFirstContact(w dealWho) dealWho {
	if w.Payee == "" {
		w.Payee = w.Name
	}
	return w
}

// wayToPay is the way to pay a check is about: the one in force, or, for a
// leg that started with none (opened at the first ask, or after a switch),
// the one the user approved with it, under any change since. It is read,
// never folded into the recourse in force, so a check's scope is unchanged
// by the approval that answered it.
func (s dealState) wayToPay() dealRecourse {
	if s.legStarted || s.open.Recourse.Rail == "" {
		return overlayRecourse(s.agreedRecourse, s.recourse)
	}
	return s.recourse
}
