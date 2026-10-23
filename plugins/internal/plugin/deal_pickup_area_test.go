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

// The guard Sherlock2 asked for, as an allowlist (capsule-cli#399, PM
// decision 65806c1a): the agent picks the class label, so a pickup_area may
// hold only what a public area does, and anything else is refused.
func TestAPickupAreaHoldsOnlyAPublicArea(t *testing.T) {
	for _, v := range []string{
		"12 Oak St", "12 Oak St.", "1600 Pennsylvania Ave", "4 Elm Dr., Austin", "12B Elm Dr.",
		"12 Oak Street", "9 Sunset Blvd", "100 Ocean Drive", "Austin, TX 78701, 12 Oak St", "123456 Long Rd",
		"12 OAK ST", "12345 Oak St", "11501 Alterra Pkwy, Austin", "\uff11\uff12 Oak St", "\u0661\u0662 Oak St",
		"7 Old Mill Creek Run Rd", "Apt 4", "Unit 12", "#5, Hyde Park", "Suite 200", "Ste. 3, Austin",
		"Hyde Park, apt #4", "pickup is off 12 Oak St", "near 12 Oak St", "the 12 Oak St building",
		// What detection let through: a number after the street, none before
		// a street type, a unit with no unit word.
		"Hauptstra\u00dfe 7", "1600 Broadway", "Main Ave. 7", "Oak St, 12",
		// A ZIP+4 narrows to a building; other countries' postal codes; a ZIP
		// that does not end its part.
		"78701-1234", "Austin, TX 78701-1234", "SW1A 1AA", "K1A 0B1", "78701 Oak St",
		// Numbers outside the allowed contexts.
		"2 bedrooms in Hyde Park", "Building 4, Hyde Park", "Pier 39",
		// Review of the allowlist: a ZIP+4 whatever surrounds it; a letter
		// that draws as a digit is never a ZIP; escapes are refused as
		// written; an ordinal takes its own suffix.
		"Zone 78701-1234 km", "Apt \u0417\u04171", "\u0417123, Oak St", "Oak St \u0417123", "\u01c0234, Oak St",
		"\u13ce12, Main St", "\u0417\u0417\u0417\u0417\u0417", "&#1600; Pennsylvania Ave", "&#4512; Duval St", "Hyde&nbsp;Park",
		"%41ustin", "12st Oak", "Oak 12St", "3nd Ave", "11st Ave", "12-14 Oak",
		"1st-2nd Ave", "Zone 78701--1234 km", "Zone 78701 -- 1234 km", "Zone 78701-/1234 km", "Zone 78701-(1234 km)",
		// A label or distance word goes with its number only right beside it.
		"Zone, 12 Oak St", "Exit (12) Oak St", "12, km from Oak St",
	} {
		assert.False(t, pickupAreaAllowed(v), v)
		err := dealDisclosure{To: "counterparty", Fields: []dealDisclosureField{{Class: "pickup_area", Value: v}}}.validate()
		require.ErrorIs(t, err, cli.ErrInput, v)
		assert.Contains(t, err.Error(), "pickup_location", v)
		assert.NotContains(t, err.Error(), v, "the value stays out of the message")
	}
	for _, v := range []string{
		"78701", "Austin", "Austin, TX", "Austin, TX 78701", "Austin 78701", "Brooklyn", "Brooklyn, NY 11201",
		"Hyde Park", "Downtown Austin", "Capitol Hill, Seattle", "Bay Area", "St. Louis, MO 63101",
		"St. Louis, Ward 3", "Oak Lane neighborhood", "Park Place area", "Main St corridor", "near 5th Ave",
		"Midway, Utah 84049", "District 9", "Union Square", "Community Unit area", "Ward 3, near Main St",
		"2 blocks from Main St", "Exit 12, Commerce Dr area", "Exit 12 Commerce Dr", "Zone 2, Park Place",
		"10 min on the way to Dallas", "Hartford, CT, 2 miles from downtown", "Route 66 corridor",
		"Highway 101 area", "Saint-L\u00f4", "Saint-L\u00f4 (Manche)", "21st and Main area", "11th St area", "2nd Ave", "3rd St", "13th St", "112th St",
		"Austin 78701.", "Austin (78701)", "\uff25\uff58\uff49\uff54 12 area", "\uff15\uff54\uff48 Ave area", "Coeur d'Alene (north side)", "Wilkes-Barre / Scranton",
		// Letters of any one script, written any way that is still letters.
		"Z\u00fcrich Kreis", "Cafe\u0301 district", "Hawai\u02bbi Kai", "K\u00f6ln-Ehrenfeld",
		"\uff28\uff59\uff44\uff45 \uff30\uff41\uff52\uff4b", "\uff17\uff18\uff17\uff10\uff11",
		"\u0421\u0430\u043d\u043a\u0442-\u041f\u0435\u0442\u0435\u0440\u0431\u0443\u0440\u0433, \u041d\u0435\u0432\u0441\u043a\u0438\u0439 \u0440\u0430\u0439\u043e\u043d",
		"\u0dc1\u0dca\u200d\u0dbb\u0dd3 \u0dbd\u0d82\u0d9a\u0dcf", "\u0645\u06cc\u200c\u062f\u0627\u0646", "\u6c5f\ub0a8 area",
		"\u6e0b\u8c37\u306e\u3042\u305f\u308a", "\u30b7\u30d6\u30e4 \u99c5\u524d", "\uac15\ub0a8\uc5ed \uadfc\ucc98",
	} {
		assert.True(t, pickupAreaAllowed(v), "%q", v)
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
func TestAnXDealPickupAreaThatHoldsAnAddressIsRefused(t *testing.T) {
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

// capsule-cli#399: what detection had to read through is refused as
// written: lookalike letters, marks on digits, markup, escapes, symbols,
// controls and fillers, and letters in another script that draw as digits.
func TestAPickupAreaWrittenToHideAnAddressIsRefused(t *testing.T) {
	for name, v := range map[string]string{
		"a Cyrillic S in St":               "Oak \u0405t",
		"a Greek T in St":                  "Oak S\u03a4",
		"Cyrillic and Greek in one word":   "Oak \u0405\u03a4 \u0438 \u03bb",
		"a Latin small capital":            "Oak \u0405\u1d1b",
		"a Cyrillic a in Ave":              "Oak \u0430ve",
		"a Greek A in Apt":                 "\u0391pt",
		"an enclosing mark on a digit":     "1\u20dd2 Oak St",
		"Cyrillic Ze drawn as 33":          "\u0417\u0417 Oak St",
		"a stroke and a Cyrillic O as 100": "\u01c0\u041e\u041e Oak St",
		"a stroke letter as 1":             "\u01c0 Oak St",
		"a fullwidth stroke":               "Oak\uff5cSt",
		"a box stroke":                     "Austin \u2502 Main St",
		"a danda":                          "Oak\u0964St",
		"a pipe":                           "Austin | Main St",
		"a word joiner":                    "Oak S\u2060t",
		"a zero-width space":               "Oak S\u200bt",
		"a Hangul filler":                  "Oak S\u3164t",
		"a control character":              "Oak S\u0001t",
		"a private-use character":          "Oak S\ue000t",
		"an HTML tag":                      "<b>Hyde Park</b>",
		"an HTML comment":                  "Hyde Park<!-- 12 Oak St -->",
		"text that only looks like a tag":  "Pickup <12 Oak St>",
		"Markdown emphasis":                "**Downtown** Austin",
		"a Markdown link":                  "[Hyde Park](https://example.com/map)",
		"an HTML character reference":      "12 Oak &#83;t",
		"a named HTML reference":           "12&nbsp;Oak&nbsp;St",
		"a percent escape":                 "12%20Oak%20St",
		"a JSON escape":                    "12 Oak \x5cu0053t",
		"an ampersand":                     "Bed & Breakfast row",
		"a fullwidth number sign":          "\uff035, Hyde Park",
		"a semicolon":                      "Austin; Hyde Park",
		"a grapheme joiner":                "Oak S\u034ft",
		"a variation selector":             "Oak S\ufe0ft",
		"a joiner between Common letters":  "Hawai\u02bb\u200d\u02bbi",
		"a joiner after a Latin mark":      "Hy\u0301\u200d\u02bcde Park",
		"a joiner in a Cyrillic word":      "\u041c\u043e\u200d\u0441\u043a\u0432\u0430",
		"a tab":                            "Austin\tDowntown",
		"a line break":                     "Austin\nDowntown",
		"a joiner in a Latin word":         "Oak S\u200dt",
		"a non-joiner in Latin":            "Hy\u200cde Park",
		"Han and Latin in one word":        "\u6c5fA area",
	} {
		assert.False(t, pickupAreaAllowed(v), "%s: %q", name, v)
		err := dealDisclosure{To: "counterparty", Fields: []dealDisclosureField{{Class: "pickup_area", Value: v}}}.validate()
		assert.ErrorIs(t, err, cli.ErrInput, name)
	}
	// A single Cyrillic or Greek letter that draws as o is a word, not a 0.
	for _, v := range []string{"\u0412\u0441\u0442\u0440\u0435\u0447\u0430 \u043e \u0440\u0430\u0439\u043e\u043d\u0435", "\u03bf \u03c3\u03c4\u03b1\u03b8\u03bc\u03cc\u03c2"} {
		assert.True(t, pickupAreaAllowed(v), "%q", v)
	}
}
