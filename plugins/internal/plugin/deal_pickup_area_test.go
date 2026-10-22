package plugin

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pickup_area is the public area a listing shows (capsule-engine#181): a
// contact class like the rest, but not a place that waits for an accepted
// offer, as pickup_location (the exact spot) does.
func TestPickupAreaIsAContactClassAndNotAnAddress(t *testing.T) {
	assert.Equal(t, "share_contact", disclosureClasses["pickup_area"])
	area := dealDisclosure{To: "counterparty", Fields: []dealDisclosureField{{Class: "pickup_area", Value: "Hyde Park"}}}
	assert.False(t, area.addressed(), "a pickup area needs no acceptance first")
	assert.True(t, dealDisclosure{Fields: []dealDisclosureField{{Class: "pickup_location", Value: "the shed"}}}.addressed())
	assert.Equal(t, "share_contact", area.action())
	assert.Equal(t, "pickup area", classWord("pickup_area"))
	assert.True(t, slices.Contains(spokenClassOrder, "pickup_area"))
	action, err := disclosureAction([]string{"pickup_area", "email"})
	require.NoError(t, err)
	assert.Equal(t, "share_contact", action)
	_, err = disclosureAction([]string{"pickup_zone"})
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Contains(t, err.Error(), "pickup_area")
}

// The guard Sherlock2 asked for: the agent picks the class label, so a value
// that reads as a street address is refused as a pickup_area.
func TestAPickupAreaThatReadsAsAStreetAddressIsRefused(t *testing.T) {
	for _, v := range []string{
		"12 Oak St", "12 Oak St.", "1600 Pennsylvania Ave", "4 Elm Dr., Austin", "12B Elm Dr.",
		"12 Oak Street", "9 Sunset Blvd", "9 Sunset Boulevard", "3 Mill Rd", "3 Mill Road", "8 Penny Ln",
		"8 Penny Lane", "100 Ocean Drive", "2 Kings Ct.", "2 Kings Court", "5 Abbey Way", "6 Hart Pl.",
		"6 Grove Place", "Austin, TX 78701, 12 Oak St", "123456 Long Rd", "12 OAK ST", "12 Elm Hwy",
		"12345 Oak St", "11501 Alterra Pkwy, Austin", "10250 Santa Monica Blvd", "１２ Oak St", "١٢ Oak St",
		"１２ Ｏａｋ Ｓｔ", "12 Oak S\u200bt", "7 Old Mill Creek Rd", "3 Ash Cir", "3 Ash Ter.", "3 Ash Trl",
		"3 Ash Sq", "3 Ash Loop", "3 Ash Plaza", "3 Ash Alley", "3 Ash Pike", "Apt 4", "Unit 12",
		"#5, Hyde Park", "Suite 200", "Ste. 3, Austin", "Hyde Park, apt #4",
	} {
		assert.True(t, readsAsStreetAddress(v), v)
		err := dealDisclosure{To: "counterparty", Fields: []dealDisclosureField{{Class: "pickup_area", Value: v}}}.validate()
		require.ErrorIs(t, err, cli.ErrInput, v)
		assert.Contains(t, err.Error(), "pickup_location", v)
		assert.NotContains(t, err.Error(), v, "the value stays out of the message")
	}
	for _, v := range []string{
		"78701", "78701-1234", "Austin", "Austin, TX", "Austin, TX 78701", "Brooklyn", "Brooklyn, NY 11201",
		"Hyde Park", "Downtown Austin", "Capitol Hill, Seattle", "Bay Area", "St. Louis, MO 63101",
		"St. Louis, Ward 3", "Main Ave. 7", "Oak Lane neighborhood", "Park Place area", "Main St corridor",
		"near 5th Ave", "Midway, Utah 84049", "District 9", "Union Square", "Community Unit area",
		"Ward 3, near Main St", "2 blocks from Main St", "Exit 12, Commerce Dr area", "Exit 12 Commerce Dr",
		"Zone 2, Park Place", "10 min on the way to Dallas", "Hartford, CT, 2 miles from downtown",
		// Stated limits: a street type after the number, none at all, or more
		// than three name words between.
		"Hauptstraße 7", "1600 Broadway", "7 Old Mill Creek Run Rd",
	} {
		assert.False(t, readsAsStreetAddress(v), v)
		assert.NoError(t, dealDisclosure{To: "counterparty", Fields: []dealDisclosureField{{Class: "pickup_area", Value: v}}}.validate(), v)
	}
	// The guard is pickup_area's alone: the exact spot is a street address.
	assert.NoError(t, dealDisclosure{To: "counterparty", Fields: []dealDisclosureField{{Class: "pickup_location", Value: "12 Oak St"}}}.validate())
}

const craigslistArea = `{"action":"share_contact","disclosing_to":"platform","disclosing":["pickup_area"],"recipient":{"name":"Craigslist","domain":"craigslist.org"}}`

// A seller gives a listing platform the listing's pickup area before any
// offer: capsulectl's own gates hold no acceptance for it, and the rules
// checker is given the share as a platform's, disclosing [pickup_area].
func TestASellerGivesAPlatformThePickupArea(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	checker := stubChecker(t, "rules", checkerPrints("allow", passFinding))
	pinChecker(t, map[string]any{"command": []string{checker}})
	approved(t, id, dealRun(t, "check", "--deal", id, "--input", writeJSON(t, craigslistArea)))

	raw, err := os.ReadFile(checker + ".input")
	require.NoError(t, err)
	var input map[string]any
	require.NoError(t, json.Unmarshal(raw, &input))
	given := input["record"].(map[string]any)["agent_input"].(map[string]any)["body"].(map[string]any)
	assert.Equal(t, "platform", given["recipient_role"])
	assert.Equal(t, []any{"pickup_area"}, given["disclosing"])

	note := func(value string) (map[string]any, error) {
		out, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", id, "--kind", "disclosure", "--input", writeJSON(t,
			`{"channel":"web","to":"platform","who":{"name":"Craigslist","domain":"craigslist.org"},"fields":[{"class":"pickup_area","value":"`+value+`"}]}`))
		if err != nil {
			return nil, err
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &m), out)
		return m, nil
	}
	// A street address under the area's label is refused, naming the class
	// for an exact spot, and nothing is sealed.
	refused := func(value string) {
		t.Helper()
		before := len(records(t, id))
		_, err := note(value)
		require.ErrorIs(t, err, cli.ErrInput)
		assert.Contains(t, err.Error(), "pickup_location")
		assert.Len(t, records(t, id), before, "a refused note seals nothing")
	}
	refused("12 Oak St.")

	told, err := note("Austin, TX 78701")
	require.NoError(t, err)
	assert.Equal(t, true, told["approved"], "no acceptance holds a pickup area")
	checkerPasses(t, id)

	// A repeat telling to the same platform is guarded the same way.
	refused("4 Elm Dr., Austin")
}

// The exact spot is still an address: a seller's share of pickup_location to
// the same platform, approved, is held for an accepted offer.
func TestAPlatformsPickupLocationStillWaitsForAnAcceptance(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	approved(t, id, dealRun(t, "check", "--deal", id, "--input", writeJSON(t,
		`{"action":"share_contact","disclosing_to":"platform","disclosing":["pickup_location"],"recipient":{"name":"Craigslist","domain":"craigslist.org"}}`)))
	before := len(records(t, id))
	held := heldNote(t, id, `{"channel":"web","to":"platform","who":{"name":"Craigslist","domain":"craigslist.org"},"fields":[{"class":"pickup_location","value":"12 Oak St"}]}`)
	assert.Equal(t, []any{"pickup_location"}, held["held"])
	assert.Len(t, records(t, id), before, "a held note seals nothing")
}

// The guard is on the disclosure note whatever the record set: an x-deal-v0
// deal refuses a street address labelled pickup_area too, sealing nothing.
func TestAnXDealPickupAreaThatReadsAsAStreetAddressIsRefused(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"marketplace",
		"intent":{"verbatim":"buy the bike and arrange pickup","allowed":["pay","share_contact"]},
		"who":{"name":"Pat Seller"},"terms":{"item":"bike","price_minor":12000,"currency":"USD"},"recourse":{"rail":"cash","refundable":false}}`))["deal_id"].(string)
	before := len(records(t, id))
	_, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", id, "--kind", "disclosure", "--input", writeJSON(t,
		`{"channel":"app_chat","fields":[{"class":"pickup_area","value":"12345 Oak St"}]}`))
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Contains(t, err.Error(), "pickup_location")
	assert.Len(t, records(t, id), before, "a refused note seals nothing")
}
