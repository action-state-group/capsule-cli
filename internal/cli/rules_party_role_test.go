package cli

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sentPartyRole runs one check on the deal under a pinned stub checker and
// returns the party_role its input carried.
func sentPartyRole(t *testing.T, dealID, check string) any {
	t.Helper()
	checker := stubChecker(t, "rules", checkerPrints("allow", passFinding))
	pinChecker(t, map[string]any{"command": []string{checker}})
	_, _ = invoke(t, "", "--profile", "deal", "deal", "check", "--deal", dealID, "--input", writeJSON(t, check))
	raw, err := os.ReadFile(checker + ".input")
	require.NoError(t, err, "the checker ran")
	var input map[string]any
	require.NoError(t, json.Unmarshal(raw, &input))
	return input["party_role"]
}

// sealedPartyRole is the party_role the deal's opening record seals, "" when
// it names none.
func sealedPartyRole(t *testing.T, dealID string) string {
	t.Helper()
	records, _ := exportRecords(t, dealID)
	for _, r := range records {
		if b, ok := r["x-deal-v0"].(map[string]any); ok && b["record_type"] == "baseline" {
			intent, _ := r["body"].(map[string]any)["intent"].(map[string]any)
			role, _ := intent["party_role"].(string)
			return role
		}
	}
	t.Fatal("no baseline")
	return ""
}

// Every deal check sends the side of the deal the user is on, always
// explicit: the value the deal's opening sealed, or buyer when it names none.
func TestTheCheckerInputCarriesThePartyRole(t *testing.T) {
	t.Run("a buyer deal naming none", func(t *testing.T) {
		id := stickerDeal(t, "card", false)
		assert.Equal(t, "", sealedPartyRole(t, id), "the opening names no side")
		assert.Equal(t, "buyer", sentPartyRole(t, id, payCheck("card", 454, 454)))
	})
	t.Run("a buyer deal naming it", func(t *testing.T) {
		dealFixture(t)
		id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
			"intent":{"verbatim":"buy a sticker","party_role":"buyer","allowed":["pay"]},
			"who":{"name":"Sticker Marketplace","domain":"stickers.example"},
			"terms":{"item":"otter sticker","price_minor":454,"currency":"USD"},"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
		assert.Equal(t, "buyer", sealedPartyRole(t, id))
		assert.Equal(t, "buyer", sentPartyRole(t, id, payCheck("card", 454, 454)))
	})
	t.Run("a seller deal", func(t *testing.T) {
		dealFixture(t)
		id := openTyped(t, sellerTyped)
		assert.Equal(t, "seller", sealedPartyRole(t, id))
		assert.Equal(t, "seller", sentPartyRole(t, id, `{"action":"offer","amount_minor":190000,"terms":{"item":"example bicycle","quantity":1,"price_minor":190000}}`))
	})
	t.Run("a sale thread", func(t *testing.T) {
		dealFixture(t)
		saleID, _ := newSale(t)
		thread := buyerThread(t, saleID, "buyer-a.example")
		assert.Equal(t, "seller", sealedPartyRole(t, thread))
		assert.Equal(t, "seller", sentPartyRole(t, thread, offerInput))
	})
}

// A typed deal names its deal by chain_id, exactly its x-deal-v0 deal_id; no
// record carries both.
func TestATypedDealsChainIDIsItsDealID(t *testing.T) {
	id := stickerDeal(t, "card", true)
	dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 454)))
	records, _ := exportRecords(t, id)
	typed := 0
	for _, r := range records {
		block, hasBlock := r["x-deal-v0"].(map[string]any)
		chain, hasChain := r["chain_id"]
		assert.False(t, hasBlock && hasChain, "a record carries both x-deal-v0.deal_id and chain_id")
		if hasChain {
			typed++
			assert.Equal(t, id, chain)
		} else {
			assert.Equal(t, id, block["deal_id"])
		}
	}
	assert.Positive(t, typed, "the deal has typed records")
}
