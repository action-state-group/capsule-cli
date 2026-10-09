package cli

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const crossPartyBuyerOpen = `{"type":"purchase","channel":"web",
 "intent":{"verbatim":"buy me the example bicycle for at most 1900","max_total_minor":190000,"allowed":["pay"]},
 "who":{"name":"Example Seller","domain":"seller.example"},
 "terms":{"item":"example bicycle","price_minor":190000,"currency":"USD"},
 "recourse":{"rail":"card","refundable":false}}`

// openingIDs are the counterparty fingerprints a copy's opening sealed.
func openingIDs(t *testing.T, id string) map[string]any {
	t.Helper()
	records, _ := exportRecords(t, id)
	for _, r := range records {
		if b, ok := r["x-deal-v0"].(map[string]any); ok && b["record_type"] == "baseline" {
			return b["counterparty"].(map[string]any)["ids"].(map[string]any)
		}
	}
	t.Fatal("no baseline")
	return nil
}

// party switches to a party's own configuration and store, made by
// dealFixture, so two parties' copies of one deal can be built in one test.
func party(t *testing.T, config string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", config)
}

// recordDealIDs are the deal ids a copy's records carry.
func recordDealIDs(t *testing.T, id string) map[string]bool {
	t.Helper()
	records, _ := exportRecords(t, id)
	ids := map[string]bool{}
	for _, r := range records {
		if b, ok := r["x-deal-v0"].(map[string]any); ok {
			ids[b["deal_id"].(string)] = true
		} else if chain, ok := r["chain_id"].(string); ok {
			ids[chain] = true
		}
	}
	return ids
}

// The other party opens its side of the deal under the deal id the opening
// party minted: one opaque id on both copies, sealed as each log's deal_id,
// so the two copies can be composed and joined on it. Each side keeps its
// own per-deal key: the id is shared, the keys are not.
func TestBothPartiesSealOneDealID(t *testing.T) {
	dealFixture(t)
	buyerConfig := os.Getenv("XDG_CONFIG_HOME")
	buyerDeal := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	buyerBaseline := openingIDs(t, buyerDeal)

	dealFixture(t)
	sellerConfig := os.Getenv("XDG_CONFIG_HOME")
	require.NotEqual(t, buyerConfig, sellerConfig)
	seller := dealRun(t, "open", "--records", "typed", "--deal-id", buyerDeal, "--input", writeJSON(t, sellerTyped))
	assert.Equal(t, buyerDeal, seller["deal_id"], "the seller's side opens under the buyer's deal id")
	dealRun(t, "check", "--deal", buyerDeal, "--input", writeJSON(t,
		`{"action":"offer","amount_minor":190000,"terms":{"item":"example bicycle","quantity":1,"price_minor":190000}}`))

	assert.Equal(t, map[string]bool{buyerDeal: true}, recordDealIDs(t, buyerDeal), "every record of the seller's copy carries the one id")
	party(t, buyerConfig)
	assert.Equal(t, map[string]bool{buyerDeal: true}, recordDealIDs(t, buyerDeal), "and so does every record of the buyer's")
	assert.Equal(t, buyerBaseline, openingIDs(t, buyerDeal))

	// The ids are the same; each store's per-deal key is its own, so the
	// same identifier fingerprints differently on the two copies.
	party(t, sellerConfig)
	buyerKey := storeDealKey(t, buyerDeal)
	party(t, buyerConfig)
	assert.NotEqual(t, buyerKey, storeDealKey(t, buyerDeal))
}

// storeDealKey is the per-deal key this profile's store holds for a deal.
func storeDealKey(t *testing.T, id string) string {
	t.Helper()
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.NoError(t, s.useDeal(t.Context(), id, false))
	return string(s.dkey)
}

func TestACarriedDealIDIsRefusedWhenMalformedOrHeld(t *testing.T) {
	dealFixture(t)
	held := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	for _, bad := range []string{"deal-0B11A7E2A1C0FFEE", "deal-123", "sale-0b11a7e2a1c0ffee", "the bicycle sale", "https://example.com/deal"} {
		_, err := invoke(t, "", "--profile", "deal", "deal", "open", "--deal-id", bad, "--input", writeJSON(t, crossPartyBuyerOpen))
		assert.ErrorContains(t, err, "--deal-id must be a deal id", bad)
	}
	_, err := invoke(t, "", "--profile", "deal", "deal", "open", "--deal-id", held, "--input", writeJSON(t, crossPartyBuyerOpen))
	assert.ErrorContains(t, err, "already holds deal "+held)
}
