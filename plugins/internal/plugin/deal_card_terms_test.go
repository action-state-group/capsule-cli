package plugin

import (
	"testing"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An approval binds only to what its card showed (capsule-cli#380): a card
// names every term its check changes from what was agreed, and an approval
// adopts a changed term only when its card named it. The pay check that
// follows counts what is still not agreed (material_fields_changed, r09's
// input).

const freeShippingShare = `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["name"],"terms":{"conditions":{"shipping":"Free shipping"}}}`

// payCount is material_fields_changed on a pay check, sealed and as the
// rules checker is given it (they agree).
func payCount(t *testing.T, id, pay string) float64 {
	t.Helper()
	sealed, given, _ := ruleInputs(t, id, pay)
	require.Equal(t, sealed["material_fields_changed"], given["material_fields_changed"])
	return sealed["material_fields_changed"].(float64)
}

func TestAShareCardNamesTheConditionItAddsAndItsApprovalAgreesToIt(t *testing.T) {
	id := stickerDeal(t, "card", false)
	share := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, freeShippingShare))
	require.Equal(t, "pause", share["verdict"], share["card"])
	assert.Contains(t, share["card"], "\nSaying yes also agrees to shipping: Free shipping.\n")
	_, err := answer(t, id, share["check_id"].(string), "yes", shownCard(t, share["card"].(string)))
	require.NoError(t, err)
	assert.Equal(t, float64(0), payCount(t, id, freeShippingPay), "the card named it: agreed")
}

func TestAShareCardNamesAWayBackItChangesAndItsApprovalAgreesToIt(t *testing.T) {
	id := stickerDeal(t, "card", false)
	share := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["name"],"recourse":{"refundable":false}}`))
	require.Equal(t, "pause", share["verdict"], share["card"])
	assert.Contains(t, share["card"], "\nSaying yes also agrees to no refunds (was refunds allowed).\n")
	_, err := answer(t, id, share["check_id"].(string), "yes", shownCard(t, share["card"].(string)))
	require.NoError(t, err)
	notRefundable := `{"action":"pay","amount_minor":454,"authorized_max_minor":454,"terms":{"item":"otter sticker","price_minor":454},"recourse":{"rail":"card","refundable":false}}`
	assert.Equal(t, float64(0), payCount(t, id, notRefundable), "the card named it: agreed")
}

// knownShare is a deal whose seller already has the user's name: a first
// share of it, approved, so the next share of it is a repeat that passes.
func knownShare(t *testing.T, recourse string) string {
	t.Helper()
	// The user allows the share, so a repeat of it passes on their standing
	// intent. recourse is the way to pay agreed at the opening.
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"buy me a funny otter sticker for at most 5 dollars","max_total_minor":500,"allowed":["pay","share_contact"]},
		"who":{"name":"Sticker Marketplace","domain":"stickers.example"},
		"terms":{"item":"otter sticker","price_minor":454,"currency":"USD"},"recourse":`+recourse+`}`))["deal_id"].(string)
	first := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["name"]}`))
	require.Equal(t, "pause", first["verdict"], first["card"])
	_, err := answer(t, id, first["check_id"].(string), "yes", shownCard(t, first["card"].(string)))
	require.NoError(t, err)
	require.Equal(t, true, note(t, id, "disclosure", `{"fields":[{"class":"name","value":"`+homeName+`"}]}`)["approved"])
	return id
}

// A repeat share passes with no card: the standing intent approves it, and
// approves no change of terms it carries. The pay check still counts it.
func TestAPassingShareAgreesToNoConditionItCarries(t *testing.T) {
	id := knownShare(t, `{"rail":"card","refundable":true}`)
	share := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, freeShippingShare))
	require.Equal(t, "pass", share["verdict"], share["card"])
	assert.NotContains(t, share["approval_text"], "agrees to", "a pass asks nothing and agrees to nothing")
	assert.Equal(t, float64(1), payCount(t, id, freeShippingPay), "no card named it: still a change at the pay")
	bodies := checkBodies(t, id)
	require.Len(t, bodies, 3)
	for _, b := range bodies {
		assert.Equal(t, float64(3), b["rule_inputs_version"], "each check states the rule its approval adopts by")
	}
}

// A deal that mixes versions: the share was checked and approved under rule
// inputs "2", which adopted the checked terms whole. A pay checked under
// "3" counts against that agreement, and each check states its version
// (none under "2"), through an exported profile check.
func TestAnOldApprovalThenANewCheck(t *testing.T) {
	dealRuleInputsVersion = "2"
	t.Cleanup(func() { dealRuleInputsVersion = "3" })
	id := knownShare(t, `{"rail":"card","refundable":true}`)
	share := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, freeShippingShare))
	require.Equal(t, "pass", share["verdict"], share["card"])
	dealRuleInputsVersion = "3"
	assert.Equal(t, float64(0), payCount(t, id, freeShippingPay), "the old approval adopted the condition, as it did then")
	bodies := checkBodies(t, id)
	require.Len(t, bodies, 3)
	assert.NotContains(t, bodies[0], "rule_inputs_version")
	assert.NotContains(t, bodies[1], "rule_inputs_version")
	assert.Equal(t, float64(3), bodies[2]["rule_inputs_version"])
	assert.Equal(t, float64(1), bodies[1]["material_fields_changed"], "measured at the share")
}

// A first-share card checked under "2" names no terms.
func TestACardCheckedBeforeNamesNoTerms(t *testing.T) {
	dealRuleInputsVersion = "2"
	t.Cleanup(func() { dealRuleInputsVersion = "3" })
	id := stickerDeal(t, "card", false)
	share := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, freeShippingShare))
	require.Equal(t, "pause", share["verdict"], share["card"])
	assert.NotContains(t, share["card"], "agrees to")
	_, err := answer(t, id, share["check_id"].(string), "yes", shownCard(t, share["card"].(string)))
	require.NoError(t, err)
	dealRuleInputsVersion = "3"
	assert.Equal(t, float64(0), payCount(t, id, freeShippingPay), "an approval of an old check adopts as before")
}

// A way to pay stated for the first time is no change: a passing check's
// standing intent still adopts it, so a later change of it counts.
func TestAPassingShareStillAdoptsAWayToPayStatedFirst(t *testing.T) {
	id := knownShare(t, `{}`)
	share := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["name"],"recourse":{"rail":"card"}}`))
	require.Equal(t, "pass", share["verdict"], share["card"])
	byBank := `{"action":"pay","amount_minor":454,"authorized_max_minor":454,"terms":{"item":"otter sticker","price_minor":454},"recourse":{"rail":"bank_transfer","refundable":true}}`
	assert.Equal(t, float64(1), payCount(t, id, byBank), "card agreed at the share; bank transfer is a change")
}

// A record sealed before this rule re-derives as it was sealed: approvals
// sealed under rule inputs "2" adopted the checked terms whole.
func TestAnApprovalSealedBeforeReDerivesAsItAdopted(t *testing.T) {
	id := knownShare(t, `{"rail":"card","refundable":true}`)
	share := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, freeShippingShare))
	require.Equal(t, "pass", share["verdict"], share["card"])
	dealRun(t, "check", "--deal", id, "--input", writeJSON(t, freeShippingPay))
	events, s := dealEvents(t, id)
	last := -1
	for i, se := range events {
		if se.Event.Kind == "snapshot" {
			last = i
		}
	}
	require.Positive(t, last)
	count := func(events []sealedEvent) interface{} {
		rec, err := buildDealRecord(events[last].Event, events[:last], s.dkey)
		require.NoError(t, err)
		return rec["body"].(map[string]interface{})["material_fields_changed"]
	}
	assert.EqualValues(t, 1, count(events), "sealed under 3: the standing intent agreed to nothing")
	old := make([]sealedEvent, len(events))
	copy(old, events)
	for i := range old[:last] {
		old[i].Event.RuleInputs = "2"
	}
	old[last].Event.RuleInputs = "2"
	assert.EqualValues(t, 0, count(old), "sealed under 2: the standing intent adopted the condition, as it did then")
}

// dealEvents are a deal's sealed steps, as foldDeal reads them.
func dealEvents(t *testing.T, id string) ([]sealedEvent, *dealSession) {
	t.Helper()
	p, err := cli.LoadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	require.NoError(t, s.useDeal(t.Context(), id, false))
	events, err := s.load(t.Context(), id)
	require.NoError(t, err)
	return events, s
}

// Approving a represent check adopts nothing, as before: it judges no
// terms, and its card names none.
func TestARepresentApprovalAdoptsNothing(t *testing.T) {
	id := stickerDeal(t, "card", false)
	share := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, freeShippingShare))
	require.Equal(t, "pause", share["verdict"], share["card"])
	_, err := answer(t, id, share["check_id"].(string), "yes", shownCard(t, share["card"].(string)))
	require.NoError(t, err)
	events, _ := dealEvents(t, id)
	agreed := func(events []sealedEvent) map[string]string {
		s, err := foldDeal(events)
		require.NoError(t, err)
		return s.agreed.Conditions
	}
	require.Equal(t, map[string]string{"shipping": "Free shipping"}, agreed(events), "as sealed: the card named it")
	for name, edit := range map[string]func(*dealEvent){
		"a represent check": func(e *dealEvent) {
			if e.Kind == "snapshot" {
				sn := *e.Snapshot
				sn.Action = representAction
				e.Snapshot = &sn
			}
		},
	} {
		changed := make([]sealedEvent, len(events))
		copy(changed, events)
		for i := range changed {
			edit(&changed[i].Event)
		}
		assert.Empty(t, agreed(changed), name)
	}
}

// In typed records the proposed action states the version too, and the
// rules checker is given it.
func TestATypedCheckStatesItsRuleInputsVersion(t *testing.T) {
	id := stickerDeal(t, "card", true)
	sealed, given, _ := ruleInputs(t, id, stickerPay)
	assert.Equal(t, float64(3), sealed["rule_inputs_version"])
	assert.Equal(t, float64(3), given["rule_inputs_version"])
}

// Three terms read as one list, each changed one with what it was.
func TestATermsLineListsThreeTerms(t *testing.T) {
	id := stickerDeal(t, "card", false)
	pay := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":480,"authorized_max_minor":480,"terms":{"item":"otter sticker","price_minor":480,"conditions":{"shipping":"Free shipping"}},"recourse":{"rail":"paypal","refundable":true}}`))
	require.Equal(t, "pause", pay["verdict"], pay["card"])
	assert.Contains(t, pay["card"], "\nSaying yes also agrees to a price of $4.80 (was $4.54), shipping: Free shipping and paying by PayPal (was card).\n")
}
