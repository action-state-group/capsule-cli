package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Where the user sells, what their agent told the buyer (its own claims) is
// opened in the buyer's copy, labelled by class, so the buyer can hold the
// seller to it. No other claim is opened there, and none whose words carry
// the user's private details.
func TestASellersRepresentationsAreOpenedForTheBuyer(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	claim := func(body string) {
		dealRun(t, "note", "--deal", id, "--kind", "claim", "--input", writeJSON(t, body))
	}
	claim(`{"text": "The frame has one small scratch by the seat", "source_kind": "agent", "class": "condition"}`)
	claim(`{"text": "Returns accepted within 7 days if unridden", "source_kind": "agent", "class": "refund_terms"}`)
	claim(`{"text": "Call me on 415-555-0123 to arrange it", "source_kind": "agent", "class": "other"}`)
	claim(`{"text": "I paid 2400 for it new", "source_kind": "user"}`)

	own, _ := ownReportAndChain(t, id)
	assert.Len(t, own["representations"], 3, "the user's own copy opens every one")

	for _, audience := range []string{dealAudienceCounterparty, dealAudienceAdjudicator} {
		b, shared := sharedCopy(t, id, audience, "the buyer")
		ext := dealReportOf(b)
		reps := ext["representations"].([]any)
		require.Len(t, reps, 2, audience)
		first := reps[0].(map[string]any)
		assert.Equal(t, "condition", first["class"])
		assert.Equal(t, "The frame has one small scratch by the seat", first["text"])
		assert.Equal(t, "refund_terms", reps[1].(map[string]any)["class"])
		assert.NotContains(t, shared, "415-555-0123")
		assert.NotContains(t, shared, "I paid 2400", "the user's own claims are not representations")
		// The sealed claim the page checks the words against is in the copy,
		// with its class.
		assert.Contains(t, shared, `"class":"condition"`)
		assert.Contains(t, shared, `"source_kind":"agent"`)
	}
	b, _ := sharedCopy(t, id, dealAudienceCounterparty, "the buyer")
	for _, o := range dealReportOf(b)["claim_openings"].([]any) {
		assert.NotContains(t, o.(map[string]any)["text"].(map[string]any)["text"], "I paid", "the buyer's copy opens only the agent's claims")
	}
	checkerPasses(t, id)
}

// A buyer's deal has no representations: there, the agent's claims are its
// own notes, and the merchant's copy opens none of them.
func TestABuyersAgentClaimsStayClosedForTheMerchant(t *testing.T) {
	dealFixture(t)
	id := openTypedSticker(t)
	dealRun(t, "note", "--deal", id, "--kind", "claim", "--input", writeJSON(t, `{"text": "The user needs it by Friday", "source_kind": "agent", "class": "other"}`))
	own, _ := ownReportAndChain(t, id)
	assert.Empty(t, own["representations"])
	b, shared := sharedCopy(t, id, dealAudienceCounterparty, "the merchant")
	assert.Nil(t, dealReportOf(b)["representations"])
	assert.NotContains(t, shared, "needs it by Friday")
}

func TestAClaimClassIsFromTheSet(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	_, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", id, "--kind", "claim", "--input",
		writeJSON(t, `{"text": "Mint", "source_kind": "agent", "class": "guarantee"}`))
	require.ErrorIs(t, err, ErrInput)
}
