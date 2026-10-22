package plugin

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// r09 (material_fields_changed/1.1.0, max_changed 0) asks when the pay
// check changes what was agreed. Rule inputs "2" count terms.conditions as
// one material field, a key added included, so a condition the agent adds
// on the pay check is a change of terms (capsule-engine#191). These tests
// pin the count capsulectl seals and sends; the checker here is a stub that
// allows, and r09's decision on the count is pinned by the engine's and
// Muse's everyday fixtures.

const freeShippingPay = `{"action":"pay","amount_minor":454,"authorized_max_minor":454,"terms":{"item":"otter sticker","price_minor":454,"conditions":{"shipping":"Free shipping"}},"recourse":{"rail":"card","refundable":true}}`

// The agent adds free shipping on the pay check to an agreement with no
// conditions: a change r09 asks about, though the card treats it as a
// default pick.
func TestFreeShippingTheAgentAddsIsAChangeOfTerms(t *testing.T) {
	id := stickerDeal(t, "card", false)
	sealed, given, _ := ruleInputs(t, id, freeShippingPay)
	for _, b := range []map[string]any{sealed, given} {
		assert.Equal(t, float64(1), b["material_fields_changed"], "terms.conditions added")
		assert.Equal(t, materialFieldsBasis, b["material_fields_basis"])
	}
}

// Free shipping agreed at the opening and the same on the pay check: no
// change.
func TestFreeShippingFromTheOpeningIsNoChange(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"buy me a funny otter sticker for at most 5 dollars","max_total_minor":500,"allowed":["pay"]},
		"who":{"name":"Sticker Marketplace","domain":"stickers.example"},
		"terms":{"item":"otter sticker","price_minor":454,"currency":"USD","conditions":{"shipping":"Free shipping"}},
		"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	sealed, given, _ := ruleInputs(t, id, freeShippingPay)
	for _, b := range []map[string]any{sealed, given} {
		assert.Equal(t, float64(0), b["material_fields_changed"])
	}
}

// Tax in the amount, within the limit: the terms are the same, so nothing
// changed.
func TestTaxWithinTheLimitIsNoChange(t *testing.T) {
	id := stickerDeal(t, "card", false)
	sealed, given, _ := ruleInputs(t, id, `{"action":"pay","amount_minor":490,"authorized_max_minor":490,"terms":{"item":"otter sticker","price_minor":454},"recourse":{"rail":"card","refundable":true}}`)
	for _, b := range []map[string]any{sealed, given} {
		assert.Equal(t, float64(0), b["material_fields_changed"])
	}
}

// A condition first carried on a non-pay check is counted at that check,
// but r09 does not apply there: material_fields_changed/1.1.0 leaves
// disclosure.personal out, and this share pauses only for its first
// disclosure. Approving it adopts the condition into what was agreed, so
// the pay check carrying it again counts no change and nothing asks about
// it. This pins today's behaviour; capsule-cli#380 changes it (an approval
// adopts only the terms its card named), and this test flips with it.
func TestAConditionOnAnApprovedShareIsAdoptedWithoutAnAsk(t *testing.T) {
	id := stickerDeal(t, "card", false)
	share := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["name"],"terms":{"conditions":{"shipping":"Free shipping"}}}`))
	require.Equal(t, "pause", share["verdict"], share["card"])
	// It pauses for the first telling alone; no rule asks about the
	// condition, and the card the user approves doesn't name it.
	assert.Equal(t, []string{"first_disclosure"}, rules(share))
	assert.NotContains(t, strings.ToLower(share["card"].(string)), "shipping")
	_, err := answer(t, id, share["check_id"].(string), "yes", shownCard(t, share["card"].(string)))
	require.NoError(t, err)
	sealed, given, _ := ruleInputs(t, id, freeShippingPay)
	for _, b := range []map[string]any{sealed, given} {
		assert.Equal(t, float64(0), b["material_fields_changed"], "adopted by the share's approval (capsule-cli#380)")
	}
	bodies := checkBodies(t, id)
	require.Len(t, bodies, 2)
	assert.Equal(t, float64(1), bodies[0]["material_fields_changed"], "measured at the share, where r09 does not apply")
}

// Free shipping the seller's change brought in, not stated on the pay
// check, is still in the proposal the check evaluates: counted.
func TestFreeShippingNotOnThePayCheckIsStillCounted(t *testing.T) {
	id := stickerDeal(t, "card", false)
	note(t, id, "change", `{"source":"seller message","terms":{"conditions":{"shipping":"Free shipping"}}}`)
	sealed, _, _ := ruleInputs(t, id, stickerPay)
	assert.Equal(t, float64(1), sealed["material_fields_changed"])
}
