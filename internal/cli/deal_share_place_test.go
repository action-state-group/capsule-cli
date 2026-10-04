package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Realistic pickup spots. The first three used to block every shared copy:
// a word of the place matched a key or a fixed label of the page ("side" the
// anomaly key, "main" inside "domain", "station" inside "attestation", "back"
// and "porch" likewise in the page's own words).
var pickupSpots = []string{
	"88 Juniper Court, side gate",
	"Main St station, north exit",
	"back porch",
	"41 Larkspur Lane, Apt 3",
	"Lakeside marina, slip 14",
	"Pier 9 north lot",
	"Union Square parking garage, level 2",
	"Elm Park front entrance",
	"Corner of 5th and Pine",
	"Riverside library steps",
	"Walgreens on Oak Avenue",
	"the blue mailbox on Cedar Road",
}

// openPickupDeal opens a marketplace pickup at spot, with the place in the
// terms, a message naming it, and a disclosure of it.
func openPickupDeal(t *testing.T, spot string) string {
	t.Helper()
	place, err := json.Marshal(spot)
	require.NoError(t, err)
	dealID := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"marketplace",
		"intent":{"verbatim":"buy the bike for $120","allowed":["pay","share_contact"]},
		"who":{"name":"Garage Sale Gary","phone":"+1 555 010 9911"},
		"terms":{"item":"bike","price_minor":12000,"currency":"USD","place":`+string(place)+`},
		"recourse":{"rail":"cash","refundable":false}}`))["deal_id"].(string)
	text, err := json.Marshal("Pick it up at " + spot + " after six.")
	require.NoError(t, err)
	dealRun(t, "note", "--deal", dealID, "--kind", "message", "--input", writeJSON(t, `{"from":"counterparty","text":`+string(text)+`}`))
	check := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"share_contact","disclosing_to":"counterparty","description":"tell the seller where","disclosing":["pickup_location"]}`))
	dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", "ok")
	dealRun(t, "note", "--deal", dealID, "--kind", "disclosure", "--input", writeJSON(t,
		`{"fields":[{"class":"pickup_location","value":`+string(place)+`}]}`))
	return dealID
}

func TestDealSharePickupSpotsDoNotBlockAShare(t *testing.T) {
	for _, spot := range pickupSpots {
		t.Run(spot, func(t *testing.T) {
			dealFixture(t)
			dealID := openPickupDeal(t, spot)
			dir := t.TempDir()
			for _, audience := range []string{"counterparty", "adjudicator"} {
				page := filepath.Join(dir, audience+".html")
				shareRun(t, dealID, audience, page)
				raw, err := os.ReadFile(page)
				require.NoError(t, err)
				assert.NotContains(t, strings.ToLower(string(raw)), strings.ToLower(spot), "%s: the place itself is withheld", audience)
			}
		})
	}
}

// A page that really carries a word of the place as a value is still refused;
// the same word as a key, or inside a longer word, is not a leak.
func TestDealGatePlaceWordsAreWholeWordsInValues(t *testing.T) {
	for _, tc := range []struct{ place, leak, kept string }{
		{"88 Juniper Court, side gate", "meet me at the side gate", "a one-sided deal, sidewalk sale"},
		{"Main St station, north exit", "use the north exit", "the domain and its attestation"},
		{"back porch", "it is on the porch", "porches and backpacks"},
	} {
		events := []sealedEvent{{Event: dealEvent{Kind: "open", Open: &dealOpen{Terms: dealTerms{Place: tc.place}}}}}
		assert.Error(t, dealPageGate(gatePage(t, tc.leak), events), "%q carries a word of %q", tc.leak, tc.place)
		assert.Error(t, dealPageGate(gatePage(t, "at "+tc.place), events), "the whole place")
		assert.NoError(t, dealPageGate(gatePage(t, tc.kept), events), "%q is not %q", tc.kept, tc.place)
	}
	events := []sealedEvent{{Event: dealEvent{Kind: "open", Open: &dealOpen{Terms: dealTerms{Place: "88 Juniper Court, side gate"}}}}}
	// As a key, never a leak; spelled out or reversed in a value, still one.
	keyed := []byte(`<!doctype html><script>window.__BUNDLE__ = {"side":"agent","juniperless":1};</script>`)
	assert.NoError(t, dealPageGate(keyed, events))
	assert.Error(t, dealPageGate(gatePage(t, "the S-I-D-E gate"), events))
	assert.Error(t, dealPageGate(gatePage(t, "repinuj"), events), "backwards")
	// A page with no parseable bundle is read whole: fail closed.
	assert.Error(t, dealPageGate([]byte(`<p>side</p>`), events))
}
