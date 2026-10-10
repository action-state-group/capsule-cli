package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const conditionWords = "Ridden twice, kept indoors; the rear tyre has a small scuff"

const sellerOffer = `{"action":"offer","amount_minor":190000,"terms":{"item":"example bicycle","quantity":1,"price_minor":190000}}`

// rawCheckerInput is what the pinned stub checker was last given, as bytes.
func rawCheckerInput(t *testing.T, checker string) string {
	t.Helper()
	raw, err := os.ReadFile(checker + ".input")
	require.NoError(t, err)
	return string(raw)
}

func inputFitsSchema(t *testing.T, raw string) {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join(dealProfileDir, "external-check-input-v0.schema.json"))
	require.NoError(t, err)
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(schema)))
	require.NoError(t, err)
	c := jsonschema.NewCompiler()
	require.NoError(t, c.AddResource("input.json", doc))
	compiled, err := c.Compile("input.json")
	require.NoError(t, err)
	given, err := jsonschema.UnmarshalJSON(strings.NewReader(raw))
	require.NoError(t, err)
	assert.NoError(t, compiled.Validate(given))
}

func noteClaim(t *testing.T, id, body string) map[string]any {
	t.Helper()
	return dealRun(t, "note", "--deal", id, "--kind", "claim", "--input", writeJSON(t, body))
}

// Where the user sells, a statement the agent made earlier in the deal
// reaches the rules checker on a later check, as its sealed claim record
// states it and never in words, so a rule on a required disclosure (s07) can
// find it.
func TestASellersEarlierAgentClaimReachesTheChecker(t *testing.T) {
	dealFixture(t)
	checker := pinStubChecker(t)
	id := openTyped(t, sellerTyped)
	claim := noteClaim(t, id, `{"text":"`+conditionWords+`","source_kind":"agent","source":"listing_message","class":"condition"}`)
	noteClaim(t, id, `{"text":"The buyer asked about the frame size","source_kind":"merchant","source":"buyer_message"}`)
	noteClaim(t, id, `{"text":"The agent's own note with no class","source_kind":"agent","source":"listing_message"}`)
	dealRun(t, "check", "--deal", id, "--input", writeJSON(t, sellerOffer))

	raw := rawCheckerInput(t, checker)
	inputFitsSchema(t, raw)
	assert.NotContains(t, raw, conditionWords, "the words stay a commitment")
	var input map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &input))
	claims := input["record"].(map[string]any)["deal_claims"].([]any)
	require.Len(t, claims, 1, "only the agent's classed claim: not the buyer's, not one with no class")

	records, _ := exportRecords(t, id)
	var sealed map[string]any
	for _, r := range records {
		if b, ok := r["x-deal-v0"].(map[string]any); ok && b["record_type"] == "claim" && r["body"].(map[string]any)["source_kind"] == "agent" && r["body"].(map[string]any)["class"] == "condition" {
			sealed = r
		}
	}
	require.NotNil(t, sealed)
	encoded, err := json.Marshal(sealed)
	require.NoError(t, err)
	exact, err := decodeBundleJSON(encoded)
	require.NoError(t, err)
	digest, err := canonical.JSONDigest(exact)
	require.NoError(t, err)
	body := sealed["body"].(map[string]any)
	assert.Equal(t, map[string]any{
		"capsule_id": claim["capsule_id"], "record_digest": digest, "class": "condition", "source_kind": "agent",
		"at": sealed["x-deal-v0"].(map[string]any)["at"], "text_commitment": body["text_commitment"],
	}, claims[0], "exactly the sealed claim record's values")
	for _, h := range input["history"].([]any) {
		assert.NotContains(t, h, "deal_claims", "never on history entries")
	}
}

// A seller's check with no agent claim before it carries none, and a buyer's
// deal never carries any, even with an agent's claim: its input is unchanged.
func TestNoDealClaimsWithoutASellersClaim(t *testing.T) {
	dealFixture(t)
	checker := pinStubChecker(t)
	id := openTyped(t, sellerTyped)
	dealRun(t, "check", "--deal", id, "--input", writeJSON(t, sellerOffer))
	var input map[string]any
	require.NoError(t, json.Unmarshal([]byte(rawCheckerInput(t, checker)), &input))
	assert.NotContains(t, input["record"], "deal_claims", "no statement: nothing, never an empty list")

	buyer := openHotel(t, true, "Example Hotel Shinjuku", "hotel.example")
	noteClaim(t, buyer, `{"text":"`+conditionWords+`","source_kind":"agent","source":"listing_message","class":"condition"}`)
	dealRun(t, "check", "--deal", buyer, "--input", writeJSON(t, hotelCommit))
	require.NoError(t, json.Unmarshal([]byte(rawCheckerInput(t, checker)), &input))
	assert.NotContains(t, input["record"], "deal_claims", "a buyer's deal is unchanged")
}
