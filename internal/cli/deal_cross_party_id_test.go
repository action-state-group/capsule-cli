package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/canonical"
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

// composeCopies puts two parties' copies of a deal, byte for byte, into a
// composed/v1 block on the composer's own container, as capsule-viewer's
// bilateral fixtures do: one agreeing join over the parts' root records.
func composeCopies(t *testing.T, container, a, b map[string]any) map[string]any {
	t.Helper()
	member := func(id, observer string, part map[string]any) map[string]any {
		request, err := canonical.JCS(map[string]any{"subject": map[string]any{"correlation": "deal-copies"}, "nonce": "n-" + id})
		require.NoError(t, err)
		sum := sha256.Sum256(request)
		digest, err := aacbundle.BundleDigest(part)
		require.NoError(t, err)
		return map[string]any{"id": id, "observer": observer, "request_digest": hex.EncodeToString(sum[:]), "outcome": "artifact", "digest": digest, "bundle": part}
	}
	agreed := sha256.Sum256([]byte("deal"))
	block := map[string]any{
		"members": []any{member("party-a", "obs-a", a), member("party-b", "obs-b", b)},
		"observers": []any{
			map[string]any{"id": "obs-a", "role": "buyer", "custody_domain": "buyer.example"},
			map[string]any{"id": "obs-b", "role": "seller", "custody_domain": "seller.example"},
		},
		"joins": []any{map[string]any{"members": []any{"party-a", "party-b"}, "basis": "pre_agreed_identifier", "pointer": "/operator",
			"identifier_digest": hex.EncodeToString(agreed[:]), "compare": []any{"/developer", "/spec_version"}, "state": "agree"}},
	}
	// The digest covers the declarations, not composed_digest itself; the
	// block is parsed whole, so it holds a placeholder until it is computed.
	block["composed_digest"] = strings.Repeat("0", 64)
	digest, err := aacbundle.ComposedDigest(block)
	require.NoError(t, err)
	block["composed_digest"] = digest
	extensions, _ := container["extensions"].(map[string]any)
	if extensions == nil {
		extensions = map[string]any{}
	}
	extensions["composed/v1"] = block
	container["extensions"] = extensions
	return container
}

// copyDealIDs are the deal ids a shared copy names anywhere (its records,
// its log coordinates).
func copyDealIDs(raw string) map[string]bool {
	ids := map[string]bool{}
	for _, id := range regexp.MustCompile(`deal-[0-9a-f]{16}`).FindAllString(raw, -1) {
		ids[id] = true
	}
	return ids
}

// exactBundle decodes a bundle with its numbers exact, as digests need them.
func exactBundle(t *testing.T, raw string) map[string]any {
	t.Helper()
	b, err := decodeBundleJSON([]byte(raw))
	require.NoError(t, err)
	return b
}

// Two real copies of one deal, the buyer's and the seller's opened under the
// buyer's deal id, compose into one composed/v1 bundle that verifies VALID,
// and both parts name the one deal.
func TestTwoRealCopiesOfOneDealComposeAndVerify(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	_, buyerCopy := sharedCopy(t, id, dealAudienceCounterparty, "the seller")

	dealFixture(t)
	dealRun(t, "open", "--records", "typed", "--deal-id", id, "--input", writeJSON(t, sellerTyped))
	dealRun(t, "check", "--deal", id, "--input", writeJSON(t,
		`{"action":"offer","amount_minor":190000,"terms":{"item":"example bicycle","quantity":1,"price_minor":190000}}`))
	_, sellerCopy := sharedCopy(t, id, dealAudienceCounterparty, "the buyer")

	// The composer's own container: an ordinary evidence bundle it sealed.
	dealFixture(t)
	other := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	container := exactBundle(t, dealOwnBundle(t, other))

	assert.Equal(t, map[string]bool{id: true}, copyDealIDs(buyerCopy))
	assert.Equal(t, map[string]bool{id: true}, copyDealIDs(sellerCopy), "both parts name the one deal")

	whole := composeCopies(t, container, exactBundle(t, buyerCopy), exactBundle(t, sellerCopy))
	out, err := invoke(t, "", "verify", "--bundle", writeBundle(t, whole))
	require.NoError(t, err, out)
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &result), out)
	assert.Equal(t, "VALID", result["verdict"], out)

	// The verdict is earned: a member whose declared digest is not its
	// copy's does not verify.
	block := whole["extensions"].(map[string]any)["composed/v1"].(map[string]any)
	block["members"].([]any)[1].(map[string]any)["digest"] = strings.Repeat("e", 64)
	out, _ = invoke(t, "", "verify", "--bundle", writeBundle(t, whole))
	var tampered map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &tampered), out)
	assert.Equal(t, "INVALID", tampered["verdict"], out)
}
