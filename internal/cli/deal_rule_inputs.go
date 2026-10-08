package cli

import (
	"bytes"
	"strings"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
)

// A check carries, beside what is about to happen, the scalars a rules
// checker reads: each a number, a member of a small closed set, or an opaque
// reference, never a name, a contact detail, a diff or free text. Each is
// absent when it is not known, never a fabricated zero or empty string.
//
//   - recipient_role: who receives a share, by role (fulfilling_merchant or
//     third_party; self is in the set and never derived here).
//   - channel, first_contact_channel: the channel kind in use at this check
//     and the one the relationship started on (x-deal-v0's channel set).
//   - upfront_amount_minor: the deposit the check's own terms state.
//   - material_fields_changed, material_fields_basis: how many of the
//     material fields the proposal changes from what was agreed, and the
//     digest of that field list.
//   - offer_fields_changed, offer_fields_basis: the same over the offer
//     fields, against what the user's own words stated.
//   - task_authority_ref (typed records): the task authority in force.
//
// A step sealed before records carried them has no RuleInputs and
// re-derives without any.
const dealRuleInputsVersion = "1"

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
	a, errA := jcsOf(x)
	b, errB := jcsOf(y)
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
func sealRuleInputs(body map[string]interface{}, events []sealedEvent, sn *dealSnapshot) {
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
			body["recipient_role"] = "fulfilling_merchant"
		case "other":
			body["recipient_role"] = "third_party"
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
	// The proposal as the check evaluates it: the deal as it stands, with
	// what the check states on top.
	who, terms, recourse := state.who, state.terms, state.recourse
	if sn.Who != nil {
		who = overlayWho(who, *sn.Who)
	}
	if sn.Terms != nil {
		terms = overlayTerms(terms, *sn.Terms)
	}
	if sn.Recourse != nil {
		recourse = overlayRecourse(recourse, *sn.Recourse)
	}
	proposed := dealFieldValues(terms, &recourse, payeeOf(who))
	agreedRecourse := state.agreedRecourse
	agreed := dealFieldValues(state.agreed, &agreedRecourse, payeeOf(open.Who))
	body["material_fields_changed"] = changedFields(materialFields, agreed, proposed)
	body["material_fields_basis"] = materialFieldsBasis
	// The user's own words state terms only (intent.asked); a key they do not
	// state counts as changed if the proposal states it, as the rule reads.
	asked := dealFieldValues(state.intent.Asked, nil, "")
	body["offer_fields_changed"] = changedFields(offerFields, asked, proposed)
	body["offer_fields_basis"] = offerFieldsBasis
}
