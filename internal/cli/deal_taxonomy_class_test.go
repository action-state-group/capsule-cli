package cli

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// taxonomyV2 is every action name in version 2 of capsule-engine's
// capsule_engine/guards/action_taxonomy.json
// (github.com/action-state-group/capsule-engine at 2521ee6), copied here so
// a class this CLI seals is checked against the table, not against itself.
var taxonomyV2 = []string{
	"money.purchase", "money.transfer", "money.subscription", "money.refund",
	"booking.create", "booking.modify", "booking.cancel",
	"communication.send", "communication.publish",
	"disclosure.personal", "disclosure.secret",
	"agreement.accept", "marketplace.offer", "marketplace.sale",
	"data.delete", "account.security_change", "background.schedule",
	"external_commitment.other", "info.query",
}

// capsV3Classes are the classes capsule-engine's caps/3.0.0
// (guards/wickets/catalog_defs/caps.v3.yaml at 2521ee6) caps, at
// per_action_minor 2500 each: every COMMIT class where money leaves.
var capsV3Classes = []string{
	"money.purchase", "money.transfer", "money.subscription",
	"booking.create", "booking.modify", "booking.cancel",
	"agreement.accept", "marketplace.offer", "external_commitment.other",
}

// Every action a deal type allows is classed by a taxonomy-v2 name, never the
// one non-consequential class; a pair the table does not name falls back to
// external_commitment.other, never to no class.
func TestDealActionClassTableIsTaxonomyV2(t *testing.T) {
	require.Len(t, taxonomyV2, 19)
	for dealType, actions := range dealPointsOfNoReturn {
		for _, action := range actions {
			for _, direction := range []string{"", "out", "in"} {
				class := dealActionClass(dealType, action, direction)
				assert.Contains(t, taxonomyV2, class, "%s %s %q", dealType, action, direction)
				assert.NotEqual(t, "info.query", class, "%s %s: a deal action is consequential", dealType, action)
			}
		}
	}
	assert.Equal(t, "money.purchase", dealActionClass("purchase", "pay", "out"), "a merchant card payment")
	assert.Equal(t, "booking.create", dealActionClass("booking", "pay", "out"), "paying for a booking")
	assert.Equal(t, "money.refund", dealActionClass("purchase", "cancel", "in"), "a cancel that returns a payment")
	assert.Equal(t, "money.refund", dealActionClass("booking", "cancel", "in"))
	assert.Equal(t, "booking.cancel", dealActionClass("booking", "cancel", ""), "a cancel that returns no money")
	assert.Equal(t, "external_commitment.other", dealActionClass("purchase", "cancel", ""))
	assert.Equal(t, "external_commitment.other", dealActionClass("lease", "pay", "out"), "a deal type the table does not name")
	assert.Equal(t, "external_commitment.other", dealActionClass("purchase", "transfer", ""), "an action the table does not name")
	assert.Equal(t, "disclosure.personal", dealActionClass("rental", "share_contact", ""))
	assert.Equal(t, "disclosure.secret", dealActionClass("service", "share_credentials", ""))
}

// recordsOf is each sealed step's record and its event, in chain order.
func recordsOf(t *testing.T, dealID string) ([]sealedEvent, []map[string]any) {
	t.Helper()
	events := chainSteps(t, dealID)
	byID := chainRecords(t, dealID)
	records := make([]map[string]any, len(events))
	for i, se := range events {
		records[i] = byID[se.CapsuleID]
	}
	return events, records
}

// The acceptance case: a $558.80 card purchase seals action_class
// money.purchase on its check and on its act, under taxonomy version 2, and
// is over caps/3.0.0's per-action limit. The cancel that returns it is a
// refund, which no cap covers, so a spend cap can never deny it.
func TestDealPurchaseAndItsCancelCarryTheirTaxonomyClass(t *testing.T) {
	id := cancelAfterPurchase(t)
	events, records := recordsOf(t, id)
	type step struct{ kind, action, class, direction string }
	var got []step
	for i, se := range events {
		rt := typeOf(records[i])
		if rt != "x-deal-v0:check" && rt != "x-deal-v0:action" {
			continue
		}
		body := bodyOf(records[i])
		assert.Equal(t, "2", body["taxonomy_version"], "step %d", se.Event.N)
		direction, _ := body["direction"].(string)
		got = append(got, step{rt, body["action"].(string), body["action_class"].(string), direction})
		if body["action"] == "pay" {
			assert.EqualValues(t, 55880, body["amount_minor"])
			assert.Contains(t, capsV3Classes, body["action_class"], "a payment is a class caps/3.0.0 caps")
			assert.Greater(t, body["amount_minor"].(float64), float64(2500), "over the per-action limit")
		}
	}
	assert.Equal(t, []step{
		{"x-deal-v0:check", "pay", "money.purchase", ""},
		{"x-deal-v0:action", "pay", "money.purchase", "out"},
		{"x-deal-v0:check", "cancel", "money.refund", ""},
		{"x-deal-v0:action", "cancel", "money.refund", "in"},
	}, got)
	assert.NotContains(t, capsV3Classes, "money.refund", "a refund is money arriving: no spend cap covers it")
}

// A typed deal seals the class inside the proposed-action/v0 the evaluation
// names by digest, and inside the action-record/v0 of the act, so the class
// is part of what the check and the act seal.
func TestTypedProposedActionSealsTheTaxonomyClass(t *testing.T) {
	dealFixture(t)
	id := openTypedSticker(t)
	require.Equal(t, "pass", checkPay(t, id, 600)["verdict"], "within the task authority: a DO")
	payNow(t, id, 600)
	_, records := recordsOf(t, id)
	var proposed, evaluation, action map[string]any
	var proposedDigest string
	events := chainSteps(t, id)
	for i, r := range records {
		switch typeOf(r) {
		case typeProposedAction:
			proposed, proposedDigest = r, events[i].Digest
		case typeActionEvaluation:
			evaluation = r
		case typeActionRecord:
			action = r
		}
	}
	require.NotNil(t, proposed)
	require.NotNil(t, evaluation)
	require.NotNil(t, action)
	assert.Equal(t, "money.purchase", bodyOf(proposed)["action_class"])
	assert.Equal(t, "2", bodyOf(proposed)["taxonomy_version"])
	evaluated, _ := json.Marshal(bodyOf(evaluation))
	assert.Contains(t, string(evaluated), proposedDigest, "the evaluation names the proposed action, class and all, by digest")
	assert.Equal(t, "money.purchase", bodyOf(action)["action_class"])
	assert.Equal(t, "2", bodyOf(action)["taxonomy_version"])
}

// A step sealed before records carried a class re-derives byte for byte
// without one: the class is gated on the step's own taxonomy version.
func TestDealStepSealedBeforeTheClassReDerivesWithoutOne(t *testing.T) {
	id := cancelAfterPurchase(t)
	events := chainSteps(t, id)
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.NoError(t, s.useDeal(t.Context(), id, false))
	for i, se := range events {
		legacy := se.Event
		legacy.TaxonomyVersion = ""
		raw, _, err := encodeDealRecord(legacy, events[:i], s.dkey)
		require.NoError(t, err, "step %d", se.Event.N)
		assert.NotContains(t, string(raw), "action_class", "step %d", se.Event.N)
		assert.NotContains(t, string(raw), "taxonomy_version", "step %d", se.Event.N)
	}
}

// Carrying the class withholds nothing a shared copy disclosed before: every
// step is shared, or withheld, exactly as it is without the class, and every
// class the table can seal is a value a shared copy may carry.
func TestDealShareIsUnchangedByTheClass(t *testing.T) {
	id := cancelAfterPurchase(t)
	events := chainSteps(t, id)
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.NoError(t, s.useDeal(t.Context(), id, false))
	private := dealPrivateValues(events)
	classed := 0
	for i, se := range events {
		shareable := map[string]bool{}
		for _, version := range []string{dealTaxonomyVersion, ""} {
			ev := se.Event
			ev.TaxonomyVersion = version
			raw, _, err := encodeDealRecord(ev, events[:i], s.dkey)
			require.NoError(t, err)
			var record map[string]any
			require.NoError(t, json.Unmarshal(raw, &record))
			shareable[version] = dealRecordShareable(record, "", dealAudienceCounterparty, private)
		}
		assert.Equal(t, shareable[""], shareable[dealTaxonomyVersion], "step %d (%s)", se.Event.N, se.Event.Kind)
		if se.Event.TaxonomyVersion != "" {
			classed++
		}
	}
	assert.Equal(t, len(events), classed, "every step of a new deal carries the taxonomy version")
	for _, class := range taxonomyV2 {
		assert.True(t, private.clean(class), "%s is a value a shared copy may carry", class)
	}
	assert.True(t, private.clean(dealTaxonomyVersion))
	assert.True(t, dealShareKeys["action_class"] && dealShareKeys["taxonomy_version"])
}
