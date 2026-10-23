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
		"pickup is off 12 Oak St", "near 12 Oak St", "from 12 Oak St", "12 The Grove Rd", "12 Road to Nowhere Ln",
		"near 4512 Duval St", "close to 12 Oak St", "corner of 12 Oak St", "5 mi from 12 Oak St", "the 12 Oak St building",
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
		// Text in another script is read as written: "ст." is a station.
		"\u0432 5 \u043c\u0438\u043d\u0443\u0442\u0430\u0445 \u043e\u0442 \u0441\u0442. \u043c. \u0421\u043e\u043a\u043e\u043b",
		"\u0417\u0430 \u0441\u0442. \u043c\u0435\u0442\u0440\u043e \u0421\u043e\u043a\u043e\u043b",
		"5 \u043c\u0438\u043d \u043e\u0442 \u0441\u0442. \u043c\u0435\u0442\u0440\u043e \u0421\u043e\u043a\u043e\u043b",
		// Stated limits: beside text in its own script, a word wholly in that
		// script is read as that script, even when it draws like a street type;
		// a street type after the number, none at all, or more than three name
		// words between.
		"12 Oak \u0405\u0442 \u0438",
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

// capsule-cli#399: a street address written with lookalike letters, marks,
// separators, escapes or markup is still read as one, as the listing guard
// reads its words (#398): marks dropped, letters read as their TR39
// skeleton, any separator a space, and markup read as a gap, as drawn and
// as nothing.
func TestAPickupAreaWrittenToHideAStreetAddressIsRefused(t *testing.T) {
	for name, v := range map[string]string{
		"Cyrillic Ѕ":                          "12 Oak Ѕt",
		"Cyrillic Ѕ and т":                    "12 Oak Ѕт",
		"Greek Τ":                             "12 Oak SΤ",
		"Greek Ι in Drive":                    "12 Oak DRΙVE",
		"Cyrillic а in Ave":                   "12 Oak аve",
		"Cyrillic о in Road":                  "12 Mill Rоad",
		"Greek ο in Road":                     "12 Mill Rοad",
		"Armenian ս in Suite":                 "Sսite 200",
		"Greek Α in Apt":                      "Αpt 4",
		"Cyrillic р in Apt":                   "Aрt 4",
		"Cyrillic і in Unit":                  "Unіt 4",
		"enclosing mark U+20DD":               "12 Oak S⃝t",
		"enclosing mark U+0488":               "12 Oak S҈t",
		"spacing mark U+0903":                 "12 Oak Sःt",
		"nonspacing mark U+0301":              "12 Oak Śt",
		"combining mark in a number":          "1⃝2 Oak St",
		"fullwidth letters":                   "12 Oak Ｓｔ",
		"fullwidth m in the number":           "12ｍ Oak St",
		"math letters":                        "12 Oak \U0001d412\U0001d42d",
		"no-break space":                      "12 Oak St",
		"em space":                            "12 Oak St",
		"ideographic space":                   "12　Oak　St",
		"line separator":                      "12 Oak St",
		"paragraph separator":                 "12 Oak St",
		"word joiner":                         "12 Oak S⁠t",
		"Hangul filler":                       "12 Oak Sㅤt",
		"a control character":                 "12 Oak S\u0001t",
		"HTML character reference":            "12 Oak &#83;t",
		"named HTML reference":                "12&nbsp;Oak&nbsp;St",
		"percent escape":                      "12%20Oak%20St",
		"JSON escape":                         "12 Oak \x5cu0053t",
		"an escaped control":                  `12 Oak S\u0001t`,
		"HTML tags around words":              `12 <span class="x">Oak</span> <i>St</i>`,
		"HTML tag inside a word":              "12 Oak S<b></b>t",
		"HTML tag as the only gap":            "12<br>Oak<br>St",
		"a break and an inline tag":           "12<br>Oak S<b>t</b>",
		"a block and an inline tag":           "<p>12</p><p>Oak S<i>t</i></p>",
		"a > in a quoted attribute":           "12 Oak S<b title='>'></b>t",
		"a > in a comment":                    "12 Oak S<!-- > -->t",
		"an unclosed comment after the words": "12 Oak St<!-- the rest",
		"Markdown emphasis in a word":         "12 Oak S**t**",
		"Markdown code in a word":             "12 Oak S`t`",
		"an empty Markdown link":              "12 Oak S[t]()",
		"a Markdown link target":              "12 Oak S[](x)t",
		"a Markdown image":                    "12 Oak S![](x.png)t",
		"Devanagari digits":                   "१२ Oak St",
		"Arabic-Indic one":                    "١ Oak St",
		"a stroke for a 1":                    "ǀ2 Oak St",
		"Greek Ι for a 1":                     "Ι2 Oak St",
		"a stroke inside the number":          "1ǀ0 Oak St",
		"Cyrillic О for a 0":                  "1ОО Oak St",
		"lookalike stroke as l":               "8 Penny ǀn",
		"unit with fullwidth #":               "＃5, Hyde Park",
		// Review round 2 of #420.
		"text that only looks like a tag": "Pickup <12 Oak St>",
		"a unit in angle brackets":        "Near <Apt 4>",
		"a < inside a tag":                "12 Oak S<b x=<>t",
		"a tag opened twice":              "12 Oak S<b<>t",
		"a link target with parentheses":  "12 Oak S[](x(y))t",
		"a stroke for the number":         "\u01c0 Oak St",
		"strokes for the number":          "\u01c0\u01c0 Oak St",
		"Greek \u0399 for the number":     "\u0399 Oak St",
		"a stroke in a unit number":       "Apt \u01c02",
		"strokes and zeros in a unit":     "Suite \u01c0\u041e\u041e",
		"a stroke after #":                "#\u01c02",
		"Greek \u0399 in a unit number":   "Apt \u03992",
		// Review round 3 of #420.
		"a Hangul filler after a lookalike word":  "12 Oak \u0405\u0442\u3164",
		"a control splitting a mixed word":        "12 Oak S\u0001\u0442 \u0438",
		"a modifier apostrophe after a lookalike": "12 Oak \u0405\u0422\u02bc",
		"a Latin \u00f0 beside a lookalike word":  "12 Oak \u0405\u0422 \u00f0",
		"Han beside a Cyrillic lookalike word":    "12 Oak \u0405\u0422 \u4e2d",
		"Cyrillic beside a Greek lookalike unit":  "\u0391\u03a1\u03a4 4 \u0438",
		"a fullwidth stroke between words":        "12 Oak\uff5cSt",
		"a box stroke between words":              "12 Oak \u2502St",
		"a danda between words":                   "12 Oak\u0964St",
		// Review round 4 of #420: a word in two scripts is mixed.
		"Cyrillic and Greek in one word":           "12 Oak \u0405\u03a4 \u0438 \u03bb",
		"Greek and Cyrillic in a unit":             "\u0391\u0420\u03a4 4 \u0438 \u03bb",
		"a lookalike beside a Latin small capital": "12 Oak \u0405\u1d1b \u0438",
	} {
		assert.True(t, readsAsStreetAddress(v), "%s: %q", name, v)
		err := dealDisclosure{To: "counterparty", Fields: []dealDisclosureField{{Class: "pickup_area", Value: v}}}.validate()
		assert.ErrorIs(t, err, cli.ErrInput, name)
	}
	// A public area still passes, however it is written: a plain i is never
	// an l ("in" is not "ln"), nor is a compatibility form of one (fullwidth
	// ｉ), a fullwidth m is an m, and a lookalike label is the label.
	for _, v := range []string{
		"2 bedrooms in Hyde Park", "Pickup 5 in Austin", "Lot 4 in the park", "12 pi Hyde Park",
		"Pickup 5 ｉｎ Austin", "2 rooms Ｉn Austin", "2 ｍiles from Main St",
		"Austin \uff5c Main St", "Austin \u2223 Main St", "Hyde Park \u2502 Oak St corridor",
		"Near Rajiv Chowk \u0964 Janpath Rd", "Amager \u00d8 near Main St", "Kbh \u00d8 n\u00e6r \u00d8sterport St.",
		"Austin | Main St", "I5 near Main St", "Hyde Park", "St. Louis, MO 63101",
		"<b>Hyde</b> <i>Park</i>", "**Downtown** Austin", "[Hyde Park](https://example.com/map)",
		"Austin TX 78701", "Sаint Louis", "Zone 2, Park Place", "2 blocks from Main St",
		"Community Unit area", "Εxit 12 Commerce Dr", "Ｈｙｄｅ Ｐａｒｋ",
		"Hauptstraße 7", "Main_St corridor", "District 9",
	} {
		assert.False(t, readsAsStreetAddress(v), "%q", v)
		assert.NoError(t, dealDisclosure{To: "counterparty", Fields: []dealDisclosureField{{Class: "pickup_area", Value: v}}}.validate(), v)
	}
}
