package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sellerOpen = `{"type": "purchase", "demo": true, "channel": "web",
 "intent": {"verbatim": "Sell my example bicycle; ask 1900", "party_role": "seller",
            "asked": {"item": "example bicycle", "quantity": 1}, "allowed": ["commit"]},
 "who": {"name": "Example Buyer", "domain": "buyer.example"},
 "terms": {"item": "example bicycle", "quantity": 1, "price_minor": 190000, "currency": "USD"},
 "recourse": {"rail": "card", "refundable": false}}`

// dealOwnBundle is the deal's own copy, as deal report writes it.
func dealOwnBundle(t *testing.T, dealID string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "own.json")
	dealRun(t, "report", "--deal", dealID, "--bundle", path)
	return string(mustRead(t, path))
}

func TestASellerDealSealsItsRole(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, sellerOpen))["deal_id"].(string)
	assert.Contains(t, dealOwnBundle(t, id), `"party_role":"seller"`)

	// A later intent note keeps the deal's role, with or without naming it.
	dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim": "Ask 1850 now", "allowed": ["commit"]}`))
	dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim": "Still selling", "party_role": "seller", "allowed": ["commit"]}`))

	// A note cannot turn the deal around.
	out, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", id, "--kind", "intent",
		"--input", writeJSON(t, `{"verbatim": "Buy it back", "party_role": "buyer"}`))
	require.Error(t, err)
	assert.Contains(t, out+err.Error(), "party_role is seller")
}

func TestADealWithNoRoleSealsNone(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", filepath.Join("../../skills/deal/demo/retail-checkout", "open.json"))["deal_id"].(string)
	assert.False(t, strings.Contains(dealOwnBundle(t, id), "party_role"), "no role is written in for a deal that states none")
}

func TestAPartyRoleIsBuyerOrSeller(t *testing.T) {
	dealFixture(t)
	out, err := invoke(t, "", "--profile", "deal", "deal", "open",
		"--input", writeJSON(t, strings.Replace(sellerOpen, `"party_role": "seller"`, `"party_role": "broker"`, 1)))
	require.Error(t, err)
	assert.Contains(t, out+err.Error(), "party_role must be buyer or seller")
}
