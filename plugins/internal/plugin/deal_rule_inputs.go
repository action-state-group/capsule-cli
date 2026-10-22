package plugin

import (
	"bytes"
	"maps"
	"strings"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/capsule-cli/internal/cli"
)

// A check carries, beside what is about to happen, the scalars a rules
// checker reads: each a number, a member of a small closed set, or an opaque
// reference, never a name, a contact detail, a diff or free text. Each is
// absent when it is not known, never a fabricated zero or empty string.
//
//   - recipient_role: who receives a share, by role (the counterparty:
//     fulfilling_merchant for a buyer, buyer for a seller; platform, for a
//     marketplace, booking site or mail relay; or third_party; self is in
//     the set and never derived here).
//   - repeat_disclosures, recipient_told_before: on a share, from the
//     telling memory (dealTelling): the classes of disclosing already told
//     to this recipient, and whether this recipient was told any class
//     before, in this deal or another. Both absent with no memory.
//   - channel, first_contact_channel: the channel kind in use at this check
//     and the one the relationship started on (x-deal-v0's channel set).
//   - upfront_amount_minor: the deposit the check's own terms state.
//   - material_fields_changed, material_fields_basis: how many of the
//     material fields the proposal changes from what was agreed, and the
//     digest of that field list.
//   - offer_fields_changed, offer_fields_basis: how many of the offer
//     fields the user's own words state the proposal does not match; absent
//     when their words state none.
//   - task_authority_ref (typed records): the task authority in force.
//
// A step sealed before records carried them has no RuleInputs and
// re-derives without any. Version "2" (capsule-cli#340) counts
// material_fields_changed against the leg in force (materialChanges2);
// a step sealed under "1" re-derives with the count it had. Version "3"
// (capsule-cli#380) counts the same way against an agreement that an
// approval changes only by the terms its card named (adoptsNamedTermsOnly);
// a step sealed under "2" or before re-derives with the agreement it had.
// A check sealed under "3" or later says so in its record
// (rule_inputs_version), so a verifier knows which rule its approval
// adopts by. A var only so that a test can seal a deal across versions.
var dealRuleInputsVersion = "3"

// The field lists the two counts read, in this order, keyed as the check
// body names them. terms.conditions counts as one key, so a count is never
// more than the list is long. Each list's basis is the hex SHA-256 of its
// JCS bytes: a rules checker pins the same list, and a basis that differs
// means the count is not over its list.
var (
	materialFields = []string{"terms.item", "terms.quantity", "terms.price_minor", "terms.deposit_minor", "terms.currency",
		"terms.when", "terms.place", "terms.conditions", "recourse.rail", "recourse.refundable", "who.payee"}
	offerFields = []string{"terms.item", "terms.quantity", "terms.price_minor", "terms.deposit_minor", "terms.conditions",
		"recourse.refundable"}
)

// dealOutcomeID is a typed task authority's plan outcome, by deal type
// (purchase, rental, booking or service): fixed once, never changed, since
// it is part of the plan's own digest. A kind of task, never a description
// of this one.
func dealOutcomeID(dealType string) string {
	return "capsulectl.deal." + dealType + "/1.0.0"
}

func fieldListBasis(list []string) string {
	items := make([]interface{}, len(list))
	for i, k := range list {
		items[i] = k
	}
	digest, err := canonical.JSONDigest(items)
	if err != nil {
		panic("a fixed list of strings always canonicalizes: " + err.Error())
	}
	return digest
}

var (
	materialFieldsBasis = fieldListBasis(materialFields)
	offerFieldsBasis    = fieldListBasis(offerFields)
)

// dealFieldValues is one side of a count: each listed key's value, absent
// when that side does not state it.
func dealFieldValues(t dealTerms, r *dealRecourse, payee string) map[string]interface{} {
	v := map[string]interface{}{}
	if t.Item != "" {
		v["terms.item"] = t.Item
	}
	if t.Quantity != 0 {
		v["terms.quantity"] = t.Quantity
	}
	if t.PriceMinor != nil {
		v["terms.price_minor"] = *t.PriceMinor
	}
	if t.DepositMinor != nil {
		v["terms.deposit_minor"] = *t.DepositMinor
	}
	if t.Currency != "" {
		v["terms.currency"] = t.Currency
	}
	if t.When != "" {
		v["terms.when"] = t.When
	}
	if t.Place != "" {
		v["terms.place"] = t.Place
	}
	if len(t.Conditions) > 0 {
		v["terms.conditions"] = t.Conditions
	}
	if r != nil {
		if r.Rail != "" {
			v["recourse.rail"] = r.Rail
		}
		if r.Refundable != nil {
			v["recourse.refundable"] = *r.Refundable
		}
	}
	if payee != "" {
		v["who.payee"] = payee
	}
	return v
}

// changedFields counts the keys of list that differ between a and b: present
// on one side and absent on the other, or present on both with unequal JCS
// values.
func changedFields(list []string, a, b map[string]interface{}) int {
	n := 0
	for _, k := range list {
		x, inA := a[k]
		y, inB := b[k]
		switch {
		case inA != inB:
			n++
		case inA && !jcsEqual(x, y):
			n++
		}
	}
	return n
}

func jcsEqual(x, y interface{}) bool {
	a, errA := cli.JCSOf(x)
	b, errB := cli.JCSOf(y)
	// A value that cannot be canonicalized never equals another.
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

// payeeOf is who a payment goes to, as the deal names them: the payee, else
// the counterparty's name (as the payee-change rule reads first contact).
func payeeOf(w dealWho) string {
	if w.Payee != "" {
		return w.Payee
	}
	return w.Name
}

// sealRuleInputs adds the rule inputs to a check body, from the steps sealed
// before it and the check's own snapshot.
func sealRuleInputs(body map[string]interface{}, events []sealedEvent, sn *dealSnapshot, version string) {
	if len(events) == 0 || events[0].Event.Open == nil {
		return
	}
	open := events[0].Event.Open
	if sn.Terms != nil && sn.Terms.DepositMinor != nil {
		body["upfront_amount_minor"] = *sn.Terms.DepositMinor
	}
	if strings.HasPrefix(sn.Action, "share_") {
		switch sn.DisclosingTo {
		case "counterparty":
			// The deal's counterparty, by the user's side of it: the merchant
			// for a buyer (and every deal that states no role), the buyer for
			// a seller.
			body["recipient_role"] = "fulfilling_merchant"
			if dealRole(events) == dealRoleSeller {
				body["recipient_role"] = "buyer"
			}
		case "platform":
			body["recipient_role"] = "platform"
		case "other":
			body["recipient_role"] = "third_party"
		}
		// What the telling memory said about this recipient: class names
		// and a flag about the party the check names, never who they are.
		if t := sn.Telling; t != nil {
			repeat := make([]interface{}, len(t.Repeat))
			for i, c := range t.Repeat {
				repeat[i] = c
			}
			body["repeat_disclosures"] = repeat
			body["recipient_told_before"] = t.ToldBefore
		}
	}
	if open.Channel != "" {
		body["first_contact_channel"] = open.Channel
	}
	channel := open.Channel
	for i := len(events) - 1; i >= 0; i-- {
		if m := events[i].Event.Message; m != nil && m.Channel != "" {
			channel = m.Channel
			break
		}
	}
	if channel != "" {
		body["channel"] = channel
	}

	state, err := foldDeal(events)
	if err != nil {
		return
	}
	agreed, proposed := state.materialSides(sn)
	if version == "1" {
		agreedRecourse := state.agreedRecourse
		agreed = dealFieldValues(state.agreed, &agreedRecourse, payeeOf(open.Who))
		body["material_fields_changed"] = changedFields(materialFields, agreed, proposed)
	} else {
		body["material_fields_changed"] = materialChanges2(agreed, proposed, state.agreedRecourse)
	}
	body["material_fields_basis"] = materialFieldsBasis
	if adoptsNamedTermsOnly(version) {
		// Which rule an approval of this check adopts by (foldDeal).
		body["rule_inputs_version"] = 3
	}
	// Against the user's own words (intent.asked), only over the offer keys
	// those words state: a key they never state (refundability, which they
	// cannot) is not a change. With none stated there is nothing to compare,
	// so the count is absent, never a fabricated one.
	asked := dealFieldValues(state.intent.Asked, nil, "")
	if n, ok := statedChanges(offerFields, asked, proposed); ok {
		body["offer_fields_changed"] = n
		body["offer_fields_basis"] = offerFieldsBasis
	}
}

// materialSides are the two sides material_fields_changed compares from
// rule inputs "2" on: what was agreed, with the payee of the leg in force,
// and the proposal as the check evaluates it (the deal as it stands, with
// what the check states on top). The check card names the terms the same
// comparison finds changed (cardChangedTerms).
func (s dealState) materialSides(sn *dealSnapshot) (agreed, proposed map[string]interface{}) {
	who, terms, recourse := s.proposal(sn)
	agreedRecourse := s.agreedRecourse
	return dealFieldValues(s.agreed, &agreedRecourse, payeeOf(s.legFirst)), dealFieldValues(terms, &recourse, payeeOf(who))
}

// proposal is the deal as it stands with what the check states on top.
func (s dealState) proposal(sn *dealSnapshot) (dealWho, dealTerms, dealRecourse) {
	who, terms, recourse := s.who, s.terms, s.wayToPay()
	if sn.Who != nil {
		who = overlayWho(who, *sn.Who)
	}
	if sn.Terms != nil {
		terms = overlayTerms(terms, *sn.Terms)
	}
	if sn.Recourse != nil {
		recourse = overlayRecourse(recourse, *sn.Recourse)
	}
	return who, terms, recourse
}

// materialChanges2 is material_fields_changed under rule inputs "2", over
// the same list, against the leg in force: a quantity either side leaves
// out is 1; a place counts only when both state it, and a refinement of the
// same place (a postal code or region added) is no change; the way to pay
// counts only once something was agreed about it with this leg. Every other
// key counts as changedFields does, a key added included.
func materialChanges2(agreedIn, proposedIn map[string]interface{}, agreedRecourse dealRecourse) int {
	return len(materialChangedKeys2(agreedIn, proposedIn, agreedRecourse))
}

// materialChangedKeys2 are the keys materialChanges2 counts, in list order.
func materialChangedKeys2(agreedIn, proposedIn map[string]interface{}, agreedRecourse dealRecourse) []string {
	// Copies: the caller's maps are read again (offer_fields_changed).
	agreed, proposed := maps.Clone(agreedIn), maps.Clone(proposedIn)
	for _, side := range []map[string]interface{}{agreed, proposed} {
		if _, ok := side["terms.quantity"]; !ok {
			side["terms.quantity"] = int64(1)
		}
	}
	var changed []string
	for _, k := range materialFields {
		x, inA := agreed[k]
		y, inB := proposed[k]
		switch k {
		case "terms.place":
			if inA && inB && !samePlace(x.(string), y.(string)) {
				changed = append(changed, k)
			}
			continue
		case "recourse.rail":
			if agreedRecourse.Rail == "" {
				continue
			}
		case "recourse.refundable":
			if agreedRecourse.Refundable == nil {
				continue
			}
		}
		if inA != inB || (inA && !jcsEqual(x, y)) {
			changed = append(changed, k)
		}
	}
	return changed
}

// statedChanges counts the keys of list that stated states and proposed
// does not match (absent, or present with an unequal JCS value); ok is false
// when stated states none of them.
func statedChanges(list []string, stated, proposed map[string]interface{}) (n int, ok bool) {
	for _, k := range list {
		x, has := stated[k]
		if !has {
			continue
		}
		ok = true
		if y, in := proposed[k]; !in || !jcsEqual(x, y) {
			n++
		}
	}
	return n, ok
}
