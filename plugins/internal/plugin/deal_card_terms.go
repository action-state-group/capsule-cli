package plugin

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/action-state-group/capsule-cli/internal/cli"
)

// An approval binds only to what its card showed (capsule-cli#380). A check
// card names every term the check changes from what was agreed, whatever
// the action, from the comparison material_fields_changed counts
// (materialChangedKeys2). An approval adopts a changed term into what was
// agreed only when the card it was given on named that term, with the value
// it named. An approval with no card shown (standing_intent, on a check that
// passed) adopts no changed term, so the next check carrying it still counts
// it. A term stated for the first time, or restated in other words, is no
// change and is adopted as before.
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

// namedOnCard is what the card of a sealed check named as changing the
// agreement: each key with the value it named (nil for a term it dropped).
// Nil for a check with no card that takes a yes (a pass, a deny) and for
// one sealed before cards named terms (snapRuleInputs, its snapshot's
// version).
func (s dealState) namedOnCard(se sealedEvent, snap *dealSnapshot, snapRuleInputs string) map[string]interface{} {
	ck := se.Event.Check
	if ck == nil || snap == nil || ck.Card == "" || ck.Verdict == "deny" || !adoptsNamedTermsOnly(snapRuleInputs) {
		return nil
	}
	keys, _, proposed := s.cardChangedTerms(*snap)
	if len(keys) == 0 {
		return nil
	}
	named := make(map[string]interface{}, len(keys))
	for _, k := range keys {
		named[k] = proposed[k]
	}
	return named
}

// keepUnnamedChanges undoes, in what an approval just adopted, every change
// from before that the card it answered did not name with that value.
func (s *dealState) keepUnnamedChanges(before dealTerms, beforeRecourse dealRecourse, a dealApproval) {
	var named map[string]interface{}
	if a.Approver != "standing_intent" {
		named = s.cardNamed[a.Check]
	}
	wasRecourse, nowRecourse := beforeRecourse, s.agreedRecourse
	was := dealFieldValues(before, &wasRecourse, "")
	now := dealFieldValues(s.agreed, &nowRecourse, "")
	for _, k := range materialChangedKeys2(was, now, beforeRecourse) {
		v, isNamed := named[k]
		got, has := now[k]
		if isNamed && ((v == nil && !has) || (v != nil && has && jcsEqual(v, got))) {
			continue
		}
		switch k {
		case "terms.item":
			s.agreed.Item = before.Item
		case "terms.quantity":
			s.agreed.Quantity = before.Quantity
		case "terms.price_minor":
			s.agreed.PriceMinor = before.PriceMinor
		case "terms.deposit_minor":
			s.agreed.DepositMinor = before.DepositMinor
		case "terms.currency":
			s.agreed.Currency = before.Currency
		case "terms.when":
			s.agreed.When = before.When
		case "terms.place":
			s.agreed.Place = before.Place
		case "terms.conditions":
			s.agreed.Conditions = maps.Clone(before.Conditions)
		case "recourse.rail":
			s.agreedRecourse.Rail = beforeRecourse.Rail
		case "recourse.refundable":
			s.agreedRecourse.Refundable = beforeRecourse.Refundable
		}
	}
}

// cardTermsLine is the card's line naming each term the check changes from
// what was agreed: what a yes agrees to beside the action itself. Empty on
// a deny (it takes no yes) and when nothing changes.
func cardTermsLine(w cli.Wording, state dealState, snap dealSnapshot, r dealCheckResult) string {
	if r.Verdict == "deny" || !adoptsNamedTermsOnly(dealRuleInputsVersion) {
		return ""
	}
	keys, agreed, proposed := state.cardChangedTerms(snap)
	currency, _ := proposed["terms.currency"].(string)
	if currency == "" {
		currency, _ = agreed["terms.currency"].(string)
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
			said = append(said, w.Line("card.terms.dropped", cli.V{"what": termWords(w, k, old, currency)}))
		case had:
			said = append(said, withWas(termWords(w, k, v, currency), termValue(w, k, old, currency)))
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
