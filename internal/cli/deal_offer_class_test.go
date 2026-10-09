package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A seller's offer is classed marketplace.offer, the class a seller's rules
// select on, so an offer below the floor reaches the floor rule: the checker
// is given the offer, its class and the floor's opening.
func TestASellersOfferIsClassedMarketplaceOffer(t *testing.T) {
	dealFixture(t)
	open := strings.Replace(sellerWithFloor, `"allowed": ["commit"]`, `"allowed": ["offer", "commit"]`, 1)
	id := dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, open))["deal_id"].(string)
	sealed, given, input := ruleInputs(t, id, `{"action":"offer","amount_minor":160000,"terms":{"item":"example bicycle","quantity":1,"price_minor":160000}}`)
	for name, body := range map[string]map[string]any{"sealed": sealed, "given": given} {
		assert.Equal(t, "offer", body["action"], name)
		assert.Equal(t, classMarketplaceOffer, body["action_class"], name)
		assert.Equal(t, "6", body["taxonomy_version"], name, "no taxonomy change: version 6 has the class")
	}
	assert.Equal(t, "seller", input["party_role"])
	require.Contains(t, input, "commercial_bounds_opening", "the floor in force reaches the checker with the offer")
	doc := input["commercial_bounds_opening"].(map[string]any)["document"].(map[string]any)
	assert.Equal(t, float64(170000), doc["min_total_minor"], "the offer (1600.00) is below it")
}

// An offer sealed before the class changed keeps its bytes: without the
// step's marker it re-derives as external_commitment.other.
func TestAnOfferSealedBeforeReDerivesUnchanged(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	offerAt(t, id, "190000")
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	events, err := s.loadOther(t.Context(), id)
	require.NoError(t, err)
	found := false
	for i, se := range events {
		if se.Event.Kind != "snapshot" || se.Event.Snapshot == nil || se.Event.Snapshot.Action != offerAction {
			continue
		}
		found = true
		require.Equal(t, dealOfferClassVersion, se.Event.OfferClass, "a new step carries the marker")
		_, digest, err := encodeDealRecord(se.Event, events[:i], s.dkey)
		require.NoError(t, err)
		assert.Equal(t, se.Digest, digest, "the marked offer re-derives to its sealed bytes")

		before := se.Event
		before.OfferClass = ""
		payload, _, err := encodeDealRecord(before, events[:i], s.dkey)
		require.NoError(t, err)
		var record map[string]any
		require.NoError(t, json.Unmarshal(payload, &record))
		assert.Equal(t, classExternalCommitOther, record["body"].(map[string]any)["action_class"], "an offer sealed before the marker")
	}
	require.True(t, found, "the deal has an offer")
}
