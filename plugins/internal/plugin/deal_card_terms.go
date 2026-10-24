package plugin

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/action-state-group/capsule-cli/cli"
)

// An approval binds only to what its card showed (capsule-cli#380). A check
// card names every term the check changes from what was agreed, whatever
// the action, from the comparison material_fields_changed counts
// (materialChangedKeys2). An approval adopts a changed term into what was
// agreed only when the card it was given on named that term, with the value
// it named, a named drop included. An approval with no card shown
// (standing_intent, on a check that passed) adopts no changed term, so the
// next check carrying it still counts it. A term stated for the first time,
// or restated in other words, is no change and is adopted as before.
//
// Which terms a card named is recomputed from the check and the agreement in
// force when it was sealed, as the card was rendered from them: the card
// text is sealed with the check and committed to by the answer
// (card_commitment), so the recomputation never needs the text.
//
// The rule applies to checks sealed under rule inputs "3" on, which say so
// in their record (rule_inputs_version); an approval adopts by the rule of
// the check it answers, whenever it was sealed. A check sealed before
// rendered no list of terms and its approval adopts the checked terms
// whole, as it did: re-derived, a deal that mixes versions keeps every
// sealed record's material counts.

// adoptsNamedTermsOnly is whether a check sealed under rule inputs version
// follows the rule above: its card names the changed terms, and an
// approval of it adopts only those.
func adoptsNamedTermsOnly(version string) bool {
	switch version {
	case "", "1", "2":
		return false
	}
	return true
}

// cardChangedTerms are the material keys the check changes from what was
// agreed, in list order, and the two sides compared. who.payee is left out:
// an approval never adopts who (the question names who it goes to, and a
// changed payee has its own reason). A represent check judges no terms and
// names none.
func (s dealState) cardChangedTerms(snap dealSnapshot) (keys []string, agreed, proposed map[string]interface{}) {
	if snap.Action == representAction {
		return nil, nil, nil
	}
	agreed, proposed = s.materialSides(&snap)
	for _, k := range materialChangedKeys2(agreed, proposed, s.agreedRecourse) {
		if k != "who.payee" {
			keys = append(keys, k)
		}
	}
	return keys, agreed, proposed
}

// checkedTerms are what a check sealed under rule inputs "3" on evaluated:
// the proposal (the deal as it stood with the check on top), and the keys
// its card named as changing what was agreed.
type checkedTerms struct {
	terms    dealTerms
	recourse dealRecourse
	named    map[string]bool
}

// checkedOn is what the sealed check se evaluated, from its snapshot and
// the agreement in force when it was sealed, which is what its card was
// rendered from. Its card names terms only when it takes a yes (not a pass,
// which has none, nor a deny). ok is false for a check sealed before the
// rule (snapRuleInputs, its snapshot's version).
func (s dealState) checkedOn(se sealedEvent, snap *dealSnapshot, snapRuleInputs string) (checkedTerms, bool) {
	ck := se.Event.Check
	if ck == nil || snap == nil || !adoptsNamedTermsOnly(snapRuleInputs) {
		return checkedTerms{}, false
	}
	_, terms, recourse := s.proposal(snap)
	c := checkedTerms{terms: terms, recourse: recourse}
	if ck.Card != "" && ck.Verdict != "deny" {
		keys, _, _ := s.cardChangedTerms(*snap)
		c.named = make(map[string]bool, len(keys))
		for _, k := range keys {
			c.named[k] = true
		}
	}
	return c, true
}

// adoptChecked is what an approved proceed of a check sealed under rule
// inputs "3" on makes agreed. From the agreement before it, each key the
// check's proposal changes (materialChangedKeys2) takes the proposal's value
// only when the card the approval was given on named it, a named drop
// included; a standing_intent approval was given on no card. A key the
// proposal states that is no change (stated for the first time, or the same
// place refined) takes the proposal's value, as before.
func (s *dealState) adoptChecked(a dealApproval, snap *dealSnapshot) {
	c, ok := s.checked[a.Check]
	if !ok {
		// An approval of a check this fold did not see: no card named anything.
		_, c.terms, c.recourse = s.proposal(snap)
	}
	named := c.named
	if a.Approver == "standing_intent" {
		named = nil
	}
	before, beforeRecourse := s.agreed, s.agreedRecourse
	proposedRecourse := c.recourse
	was := dealFieldValues(before, &beforeRecourse, "")
	now := dealFieldValues(c.terms, &proposedRecourse, "")
	changed := materialChangedKeys2(was, now, beforeRecourse)
	out, outRecourse := before, beforeRecourse
	out.Conditions = maps.Clone(before.Conditions)
	for _, k := range materialFields {
		_, states := now[k]
		if slices.Contains(changed, k) {
			if !named[k] {
				continue
			}
		} else if !states {
			continue
		}
		switch k {
		case "terms.item":
			out.Item = c.terms.Item
		case "terms.quantity":
			out.Quantity = c.terms.Quantity
		case "terms.price_minor":
			out.PriceMinor = c.terms.PriceMinor
		case "terms.deposit_minor":
			out.DepositMinor = c.terms.DepositMinor
		case "terms.currency":
			out.Currency = c.terms.Currency
		case "terms.when":
			out.When = c.terms.When
		case "terms.place":
			out.Place = c.terms.Place
		case "terms.conditions":
			out.Conditions = maps.Clone(c.terms.Conditions)
		case "recourse.rail":
			outRecourse.Rail = c.recourse.Rail
		case "recourse.refundable":
			outRecourse.Refundable = c.recourse.Refundable
		}
	}
	s.agreed, s.agreedRecourse = out, outRecourse
}

// cardTermsLine is the card's line naming each term the check changes from
// what was agreed: what a yes agrees to beside the action itself. Empty on
// a deny (it takes no yes) and when nothing changes.
func cardTermsLine(w cli.Wording, state dealState, snap dealSnapshot, r dealCheckResult) string {
	if r.Verdict == "deny" || !adoptsNamedTermsOnly(dealRuleInputsVersion) {
		return ""
	}
	keys, agreed, proposed := state.cardChangedTerms(snap)
	// A new value is said in the proposal's currency, an agreed one (what
	// it was, or what is dropped) in the agreed currency.
	currency, _ := proposed["terms.currency"].(string)
	agreedCurrency, _ := agreed["terms.currency"].(string)
	if currency == "" {
		currency = agreedCurrency
	}
	if agreedCurrency == "" {
		agreedCurrency = currency
	}
	// A term that had an agreed value says it; one added or dropped needs
	// no "was".
	withWas := func(phrase, was string) string {
		return phrase + " " + w.Line("card.terms.was", cli.V{"value": was})
	}
	var said []string
	for _, k := range keys {
		if k == "terms.conditions" {
			was, _ := agreed[k].(map[string]string)
			now, _ := proposed[k].(map[string]string)
			names := slices.Sorted(maps.Keys(now))
			for c := range was {
				if _, ok := now[c]; !ok {
					names = append(names, c)
				}
			}
			slices.Sort(names)
			for _, c := range names {
				name := strings.ReplaceAll(c, "_", " ")
				v, ok := now[c]
				old, had := was[c]
				switch {
				case !ok:
					said = append(said, w.Line("card.terms.dropped", cli.V{"what": cardConditionWords(w, name, old)}))
				case had && v != old:
					said = append(said, withWas(cardConditionWords(w, name, v), old))
				case !had:
					said = append(said, cardConditionWords(w, name, v))
				}
			}
			continue
		}
		v, ok := proposed[k]
		old, had := agreed[k]
		switch {
		case !ok:
			said = append(said, w.Line("card.terms.dropped", cli.V{"what": termWords(w, k, old, agreedCurrency)}))
		case had:
			said = append(said, withWas(termWords(w, k, v, currency), termValue(w, k, old, agreedCurrency)))
		default:
			said = append(said, termWords(w, k, v, currency))
		}
	}
	if len(said) == 0 {
		return ""
	}
	return w.Line("card.terms.label", cli.V{"terms": listWords(said, "and")})
}

// cardConditionWords is one condition, by its name (its key, spaced) and value.
func cardConditionWords(w cli.Wording, name, value string) string {
	return w.Line("card.terms.condition", cli.V{"name": name, "value": value})
}

// termValue is one term's value alone, as the card says it.
func termValue(w cli.Wording, key string, v interface{}, currency string) string {
	switch key {
	case "terms.price_minor", "terms.deposit_minor":
		n, _ := v.(int64)
		return spokenMoney(n, currency)
	case "recourse.rail":
		s, _ := v.(string)
		return railName(s)
	case "recourse.refundable":
		if b, _ := v.(bool); !b {
			return w.Line("card.terms.not_refundable", nil)
		}
		return w.Line("card.terms.refundable", nil)
	}
	return fmt.Sprint(v)
}

// termWords is one term, as the card's terms line says it.
func termWords(w cli.Wording, key string, v interface{}, currency string) string {
	value := termValue(w, key, v, currency)
	switch key {
	case "recourse.refundable":
		return value
	case "terms.price_minor", "terms.deposit_minor":
		key = strings.TrimSuffix(key, "_minor")
	case "recourse.rail":
		key = "terms.rail"
	}
	return w.Line("card."+key, cli.V{"value": value})
}
