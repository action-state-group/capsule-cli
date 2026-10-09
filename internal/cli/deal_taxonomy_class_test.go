package cli

import (
	"bytes"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// taxonomyV5 is every action name in version 5 of capsule-engine's
// capsule_engine/guards/action_taxonomy.json
// (github.com/action-state-group/capsule-engine at
// e5ec2c1ba84f9d9269cbb164eb6a92400e4d5d49, file sha256
// 7d31e892cdea7352d0485c89430dba028bf02ce91fb0e932f4752c1f4066022f, JCS sha256
// af3a054cae880e13ce5d8d8bdaa64383e6f3af14fa2bf2ddb88aab226865b13a), copied
// here so a class this CLI seals is checked against the table, not against
// itself. Version 2 (at 2521ee6), version 3 (at a5e9e30) and version 4 (at
// 5300e0b) name the same 19.
var taxonomyV5 = []string{
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

// Every action a deal type allows is classed by a taxonomy-v5 name, never the
// one non-consequential class; a pair the table does not name falls back to
// external_commitment.other, never to no class.
func TestDealActionClassTableIsTaxonomyV5(t *testing.T) {
	require.Equal(t, "5", dealTaxonomyVersion)
	require.Len(t, taxonomyV5, 19)
	for dealType, actions := range dealPointsOfNoReturn {
		for _, action := range actions {
			for _, direction := range []string{"", "out", "in"} {
				class := dealActionClass(dealType, action, direction)
				assert.Contains(t, taxonomyV5, class, "%s %s %q", dealType, action, direction)
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
// money.purchase on its check and on its act, under taxonomy version 4, and
// is over caps/3.0.0's per-action limit. The cancel that returns it is a
// refund, which no cap covers, so a spend cap can never deny it.
func TestDealPurchaseAndItsCancelCarryTheirTaxonomyClass(t *testing.T) {
	id := cancelAfterPurchase(t)
	events, records := recordsOf(t, id)
	type step struct {
		kind, action, class, direction string
		spend                          float64
	}
	var got []step
	for i, se := range events {
		rt := typeOf(records[i])
		if rt != "x-deal-v0:check" && rt != "x-deal-v0:action" {
			continue
		}
		body := bodyOf(records[i])
		assert.Equal(t, "5", body["taxonomy_version"], "step %d", se.Event.N)
		direction, _ := body["direction"].(string)
		got = append(got, step{rt, body["action"].(string), body["action_class"].(string), direction, body["spend_minor"].(float64)})
		if body["action"] == "pay" {
			assert.EqualValues(t, 55880, body["amount_minor"])
			assert.Contains(t, capsV3Classes, body["action_class"], "a payment is a class caps/3.0.0 caps")
			assert.Greater(t, body["amount_minor"].(float64), float64(2500), "over the per-action limit")
		}
	}
	assert.Equal(t, []step{
		{"x-deal-v0:check", "pay", "money.purchase", "", 55880},
		{"x-deal-v0:action", "pay", "money.purchase", "out", 55880},
		{"x-deal-v0:check", "cancel", "money.refund", "in", 0},
		{"x-deal-v0:action", "cancel", "money.refund", "in", 0},
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
	assert.Equal(t, "5", bodyOf(proposed)["taxonomy_version"])
	evaluated, _ := json.Marshal(bodyOf(evaluation))
	assert.Contains(t, string(evaluated), proposedDigest, "the evaluation names the proposed action, class and all, by digest")
	assert.Equal(t, "money.purchase", bodyOf(action)["action_class"])
	assert.Equal(t, "5", bodyOf(action)["taxonomy_version"])
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

// A step sealed under an earlier taxonomy version (2, 3 or 4) re-derives with
// that version and the same class: the version is the step's own, never this
// release's. Its record is the version-5 record with only the version
// changed, since every version names the same classes.
func TestDealStepSealedUnderAnEarlierTaxonomyReDerivesWithIt(t *testing.T) {
	id := cancelAfterPurchase(t)
	events := chainSteps(t, id)
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.NoError(t, s.useDeal(t.Context(), id, false))
	current := []byte(`"taxonomy_version":"5"`)
	for _, version := range []string{"2", "3", "4"} {
		t.Run("version "+version, func(t *testing.T) {
			stored := []byte(`"taxonomy_version":"` + version + `"`)
			classed := 0
			for i, se := range events {
				require.Equal(t, "5", se.Event.TaxonomyVersion, "step %d", se.Event.N)
				now, _, err := encodeDealRecord(se.Event, events[:i], s.dkey)
				require.NoError(t, err, "step %d", se.Event.N)
				old := se.Event
				old.TaxonomyVersion = version
				raw, _, err := encodeDealRecord(old, events[:i], s.dkey)
				require.NoError(t, err, "step %d", se.Event.N)
				if !bytes.Contains(now, current) {
					assert.Equal(t, string(now), string(raw), "step %d carries no class", se.Event.N)
					continue
				}
				classed++
				assert.NotContains(t, string(raw), string(current), "step %d", se.Event.N)
				assert.Equal(t, string(bytes.ReplaceAll(now, current, stored)), string(raw), "step %d", se.Event.N)
			}
			assert.Equal(t, 4, classed, "the pay and the cancel, each checked and done")
		})
	}
}

// The class never discloses what a shared copy withheld without it, and every
// class the table can seal is a value a shared copy may carry. The one record
// it newly withholds is a cancel's check with an amount, which now carries
// direction in (a key shared copies do not disclose): more withheld, never
// less.
func TestDealShareNeverDisclosesMoreForTheClass(t *testing.T) {
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
		if shareable[dealTaxonomyVersion] {
			assert.True(t, shareable[""], "step %d (%s): the class discloses nothing withheld without it", se.Event.N, se.Event.Kind)
		}
		if shareable[""] && !shareable[dealTaxonomyVersion] {
			assert.Equal(t, "snapshot", se.Event.Kind, "step %d: only a cancel's check is newly withheld", se.Event.N)
			assert.Equal(t, "cancel", se.Event.Snapshot.Action, "step %d", se.Event.N)
		}
		if se.Event.TaxonomyVersion != "" {
			classed++
		}
	}
	assert.Equal(t, len(events), classed, "every step of a new deal carries the taxonomy version")
	for _, class := range taxonomyV5 {
		assert.True(t, private.clean(class), "%s is a value a shared copy may carry", class)
	}
	assert.True(t, private.clean(dealTaxonomyVersion))
	assert.True(t, dealShareKeys["action_class"] && dealShareKeys["taxonomy_version"])
}

// purchaseThenCancel pays for a $558.80 ticket, then checks and takes the
// cancel the user asked for, as checkInput and actInput state it, and returns
// the deal.
func purchaseThenCancel(t *testing.T, dealType, checkInput, actInput string) string {
	t.Helper()
	dealFixture(t)
	const item = "Southwest WN 1234 HOU-SJC, Oct 21 to Oct 24, Basic"
	id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"`+dealType+`","channel":"web",
		"intent":{"verbatim":"book me Southwest HOU to SJC Oct 21 to Oct 24, Basic fare","asked":{"item":"`+item+`"},"allowed":["pay"]},
		"who":{"name":"Southwest Airlines","domain":"southwest.example"},
		"terms":{"item":"`+item+`","price_minor":55880,"currency":"USD"},
		"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	answer := func(check map[string]any, said string) {
		if check["verdict"] != "pass" {
			dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", said)
		}
	}
	answer(dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":55880,"authorized_max_minor":55880,"terms":{"item":"`+item+`","price_minor":55880},"recourse":{"rail":"card","refundable":true}}`)), "yes, book it")
	dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":55880,"currency":"USD","rail":"card"}`))
	dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"cancel this ticket for me please","allowed":["pay","cancel"]}`))
	answer(dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"cancel","description":"cancel the ticket",`+checkInput+`}`)), "yes, cancel it")
	act := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"cancel",`+actInput+`}`))
	require.Equal(t, false, act["unchecked"], "the cancel is authorized by the user's answer")
	return id
}

// cancelBodies is the sealed check and action bodies of a deal's cancel.
func cancelBodies(t *testing.T, dealID string) (check, action map[string]any) {
	t.Helper()
	_, records := recordsOf(t, dealID)
	for _, r := range records {
		rt := typeOf(r)
		if (rt == "x-deal-v0:check" || rt == "x-deal-v0:action") && bodyOf(r)["action"] == "cancel" {
			if rt == "x-deal-v0:check" {
				check = bodyOf(r)
			} else {
				action = bodyOf(r)
			}
		}
	}
	require.NotNil(t, check)
	require.NotNil(t, action)
	return check, action
}

// Stopping a commitment is never spend, and no amount on a cancel reads as
// money paid out. Its spend_minor is 0 however it is stated. A cancel with an
// amount carries direction in: one that returns a sealed payment keeps it as
// amount_minor (the refund); any other moves it to cancelled_amount_minor,
// neither money moved nor spend. A fee is its own fee_minor; a cap evaluates
// none of them.
func TestDealCancelIsNeverSpend(t *testing.T) {
	for _, tc := range []struct {
		name, dealType, check, act, class string
		amount, cancelled, fee, direction any
	}{
		{"a partial refund, matching no payment", "purchase", `"amount_minor":30000`, `"amount_minor":30000,"currency":"USD","rail":"card"`, "external_commitment.other", nil, float64(30000), nil, "in"},
		{"a booking cancel with an amount", "booking", `"amount_minor":30000`, `"amount_minor":30000,"currency":"USD","rail":"card"`, "booking.cancel", nil, float64(30000), nil, "in"},
		{"a full refund less a fee", "purchase", `"amount_minor":55880,"fee_minor":5000`, `"amount_minor":55880,"currency":"USD","rail":"card","fee_minor":5000`, "money.refund", float64(55880), nil, float64(5000), "in"},
		{"a fee and nothing returned", "purchase", `"fee_minor":5000`, `"fee_minor":5000`, "external_commitment.other", nil, nil, float64(5000), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := purchaseThenCancel(t, tc.dealType, tc.check, tc.act)
			checkerPasses(t, id)
			check, action := cancelBodies(t, id)
			for _, body := range []map[string]any{check, action} {
				assert.Equal(t, tc.class, body["action_class"])
				assert.Equal(t, float64(0), body["spend_minor"], "a cancel is never spend")
				assert.Equal(t, tc.amount, body["amount_minor"], "amount_minor only for a payment returned")
				assert.Equal(t, tc.cancelled, body["cancelled_amount_minor"], "any other amount is what the cancel was about")
				assert.Equal(t, tc.fee, body["fee_minor"], "a fee is its own recorded amount")
				assert.Equal(t, tc.direction, body["direction"], "no amount on a cancel reads as money out")
			}
		})
	}
}

// A fee is a cancellation's: no other action carries one, and none is
// negative.
func TestDealFeeIsACancelsOnly(t *testing.T) {
	dealFixture(t)
	id := openSticker(t, true)
	_, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":600,"authorized_max_minor":600,"fee_minor":100}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only a cancel carries one")
	_, err = invoke(t, "", "--profile", "deal", "deal", "check", "--deal", id, "--input", writeJSON(t, `{"action":"cancel","fee_minor":-1}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not be negative")
}

// The class is inside what the step seals: the sealed digest is the record's
// digest, and the same record with another class has another digest.
func TestDealActionClassIsInTheSealedDigest(t *testing.T) {
	id := cancelAfterPurchase(t)
	events, records := recordsOf(t, id)
	checked := 0
	for i, se := range events {
		body := bodyOf(records[i])
		if _, ok := body["action_class"]; !ok {
			continue
		}
		digest, err := canonical.JSONDigest(numbered(t, records[i]))
		require.NoError(t, err)
		assert.Equal(t, se.Digest, digest, "step %d: the sealed digest is the record's", se.Event.N)
		changed := numbered(t, records[i])
		changed["body"].(map[string]any)["action_class"] = "external_commitment.other"
		if body["action_class"] == "external_commitment.other" {
			changed["body"].(map[string]any)["action_class"] = "money.purchase"
		}
		other, err := canonical.JSONDigest(changed)
		require.NoError(t, err)
		assert.NotEqual(t, se.Digest, other, "step %d: another class is another sealed record", se.Event.N)
		checked++
	}
	assert.Equal(t, 4, checked, "both checks and both acts")
}

// numbered is v decoded again with its numbers kept as json.Number, the form
// the canonical digest takes.
func numbered(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out map[string]any
	require.NoError(t, dec.Decode(&out))
	return out
}

// An unchecked cancel is sealed as an outcome, never as an action, and its
// amount still never reads as money paid out: direction in, the amount that
// returns a sealed payment as amount_minor, any other as
// cancelled_amount_minor. An outcome carries no class and no spend.
func TestDealUncheckedCancelAmountNeverReadsAsMoneyOut(t *testing.T) {
	for _, tc := range []struct {
		name              string
		pay               bool
		amount, cancelled any
	}{
		{"matching no payment", false, nil, float64(30000)},
		{"returning a sealed payment", true, float64(55880), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dealFixture(t)
			const item = "Southwest WN 1234 HOU-SJC, Oct 21 to Oct 24, Basic"
			id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
				"intent":{"verbatim":"book me Southwest HOU to SJC Oct 21 to Oct 24, Basic fare","asked":{"item":"`+item+`"},"allowed":["pay"]},
				"who":{"name":"Southwest Airlines","domain":"southwest.example"},
				"terms":{"item":"`+item+`","price_minor":55880,"currency":"USD"},
				"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
			amount := 30000
			if tc.pay {
				amount = 55880
				check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":55880,"authorized_max_minor":55880,"terms":{"item":"`+item+`","price_minor":55880},"recourse":{"rail":"card","refundable":true}}`))
				if check["verdict"] != "pass" {
					dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", "yes, book it")
				}
				dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":55880,"currency":"USD","rail":"card"}`))
			}
			cancel := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t,
				`{"action":"cancel","amount_minor":`+strconv.Itoa(amount)+`,"currency":"USD","rail":"card"}`))
			require.Equal(t, true, cancel["unchecked"], "no check before this cancel")
			checkerPasses(t, id)

			_, records := recordsOf(t, id)
			var attempted map[string]any
			for _, r := range records {
				if typeOf(r) == "x-deal-v0:outcome" && bodyOf(r)["status"] == "unchecked_action" {
					attempted = bodyOf(r)["unchecked"].(map[string]any)
				}
			}
			require.NotNil(t, attempted)
			assert.Equal(t, "cancel", attempted["action"])
			assert.Equal(t, "in", attempted["direction"])
			assert.Equal(t, tc.amount, attempted["amount_minor"])
			assert.Equal(t, tc.cancelled, attempted["cancelled_amount_minor"])
			assert.NotContains(t, attempted, "action_class", "an outcome is not an action")
			assert.NotContains(t, attempted, "spend_minor")
		})
	}
}
