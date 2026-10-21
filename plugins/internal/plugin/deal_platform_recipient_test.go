package plugin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A platform (a marketplace, booking site or mail relay between the user and
// the counterparty) is a recipient in its own right: capsule-cli#348.

const ebayEmail = `{"action":"share_contact","disclosing_to":"platform","disclosing":["email"],"recipient":{"name":"eBay","domain":"ebay.com"}}`

func platformEmailNote(domain string) string {
	return `{"to":"platform","who":{"name":"the platform","domain":"` + domain + `"},"fields":[{"class":"email","value":"` + homeEmail + `"}]}`
}

// The first email to a platform asks; once told, a repeat to the same
// platform passes, in this deal and the next; a different platform asks
// again. The check seals recipient_role platform, never third_party.
func TestDealFirstEmailToAPlatformAsksThenPasses(t *testing.T) {
	dealFixture(t)
	dealID := openSeller(t, `{"name":"Garage Sale Gary","profile_id":"mkt:seller:4411"}`)

	first := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, ebayEmail))
	require.Equal(t, "pause", first["verdict"])
	firstTelling(t, first, "eBay", "email")
	records, export := exportRecords(t, dealID)
	checkProfile(t, export)
	var sealed map[string]any
	for _, r := range records {
		if b, ok := r["x-deal-v0"].(map[string]any); ok && b["record_type"] == "check" {
			sealed = r["body"].(map[string]any)
		}
	}
	require.NotNil(t, sealed)
	assert.Equal(t, "platform", sealed["recipient_role"])
	assert.Equal(t, "platform", sealed["disclosing_to"])

	// Unapproved, the email is held; approved, it is sealed as approved.
	held := heldNote(t, dealID, platformEmailNote("ebay.com"))
	assert.Equal(t, []any{"email"}, held["held"])
	dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", first["check_id"].(string), "--choice", "proceed", "--said", "yes, give eBay my email")
	sent := dealRun(t, "note", "--deal", dealID, "--kind", "disclosure", "--input", writeJSON(t, platformEmailNote("www.ebay.com")))
	assert.Equal(t, true, sent["approved"], "the platform by its own site")

	// The same platform again, in another deal: a repeat passes.
	other := openSeller(t, `{"name":"Another Seller","profile_id":"mkt:seller:9000"}`)
	again := dealRun(t, "check", "--deal", other, "--input", writeJSON(t, ebayEmail))
	assert.Equal(t, "pass", again["verdict"], again["card"])
	assert.Equal(t, []any{"email"}, again["recipient"].(map[string]any)["repeat_disclosures"])

	// Another platform asks again.
	craigslist := dealRun(t, "check", "--deal", other, "--input", writeJSON(t,
		`{"action":"share_contact","disclosing_to":"platform","disclosing":["email"],"recipient":{"name":"Craigslist","domain":"craigslist.org"}}`))
	require.Equal(t, "pause", craigslist["verdict"])
	firstTelling(t, craigslist, "Craigslist", "email")
}

// An approval for a platform covers that platform only: not the deal's
// counterparty, not another platform, and not the same site told as
// someone other than a platform.
func TestDealPlatformApprovalCoversThatPlatformOnly(t *testing.T) {
	dealFixture(t)
	dealID := openSeller(t, `{"name":"Garage Sale Gary","profile_id":"mkt:seller:4411"}`)
	check := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, ebayEmail))
	dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", "ok, eBay")

	held := heldNote(t, dealID, `{"fields":[{"class":"email","value":"`+homeEmail+`"}]}`)
	assert.Equal(t, []any{"email"}, held["held"], "the counterparty is not the platform")
	held = heldNote(t, dealID, platformEmailNote("craigslist.org"))
	assert.Equal(t, []any{"email"}, held["held"], "another platform")

	// The same site told as someone other than a platform: held.
	held = heldNote(t, dealID, `{"to":"other","who":{"name":"eBay","domain":"ebay.com","email":"help@ebay.com"},"fields":[{"class":"email","value":"`+homeEmail+`"}]}`)
	assert.Equal(t, []any{"email"}, held["held"], "a platform's approval is for a platform")
}

// A platform share names who it is, by an identity; a disclosure to one
// says who.
func TestDealPlatformShareMustNameThePlatform(t *testing.T) {
	dealFixture(t)
	dealID := openSeller(t, `{"name":"Garage Sale Gary","profile_id":"mkt:seller:4411"}`)
	for body, want := range map[string]string{
		`{"action":"share_contact","disclosing_to":"platform","disclosing":["email"]}`:                                 `"disclosing_to": "platform" needs a recipient with`,
		`{"action":"share_contact","disclosing_to":"platform","disclosing":["email"],"recipient":{"name":"eBay"}}`:     `"disclosing_to": "platform" needs a recipient with`,
		`{"action":"share_contact","disclosing_to":"counterparty","disclosing":["email"],"recipient":{"name":"eBay"}}`: `recipient goes with "disclosing_to": "platform" or "other"`,
		`{"action":"share_contact","disclosing_to":"marketplace","disclosing":["email"]}`:                              "disclosing_to must be counterparty, platform or other",
	} {
		out, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", dealID, "--input", writeJSON(t, body))
		require.ErrorIs(t, err, cli.ErrInput, body)
		assert.Contains(t, out+err.Error(), want, body)
	}
	out, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", dealID, "--kind", "disclosure", "--input", writeJSON(t,
		`{"to":"platform","fields":[{"class":"email","value":"`+homeEmail+`"}]}`))
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Contains(t, out+err.Error(), "a disclosure to a platform or to someone other than the counterparty needs who")
}

// With no counterparty memory to say it was told before, a platform email
// asks: the packs allow it and count nothing, so this pause is the only
// ask. A share to the counterparty with no memory is judged as before.
func TestDealPlatformEmailWithNoMemoryAsks(t *testing.T) {
	state := dealState{open: dealOpen{Channel: "marketplace", Who: dealWho{Name: "Garage Sale Gary", ProfileID: "mkt:seller:4411"}},
		who: dealWho{Name: "Garage Sale Gary", ProfileID: "mkt:seller:4411"}, intent: dealIntent{Verbatim: "sell the bike"}}
	platform := dealSnapshot{Action: "share_contact", Disclosing: []string{"email"}, DisclosingTo: "platform", Recipient: &dealWho{Name: "eBay", Domain: "ebay.com"}}
	r := evaluateDeal(state, platform)
	assert.Equal(t, "pause", r.Verdict)
	require.Len(t, r.Differences, 1)
	assert.Equal(t, "first_disclosure", r.Differences[0].Rule)
	assert.Equal(t, "First time telling eBay your email", r.Differences[0].Text)
	require.NotNil(t, r.Recipient)
	assert.Equal(t, []string{"email"}, r.Recipient.First)

	counterparty := dealSnapshot{Action: "share_contact", Disclosing: []string{"email"}, DisclosingTo: "counterparty"}
	assert.Equal(t, "pass", evaluateDeal(state, counterparty).Verdict, "unchanged for the counterparty")
}

// A platform is known by its registrable domain, even a marketplace host's,
// which never identifies a seller on it.
func TestPlatformKeysKeepTheMarketplaceDomain(t *testing.T) {
	assert.Equal(t, []string{"domain:ebay.com"}, platformKeys(dealWho{Name: "eBay", Domain: "https://www.ebay.com/sell", RelayAddress: "abc@members.ebay.com"}))
	assert.Empty(t, counterpartyKeys(dealWho{Domain: "ebay.com"}, ""), "a seller is never keyed on the marketplace")
	assert.Equal(t, counterpartyKeys(dealWho{RelayAddress: "reply-1@relay.example"}, ""), platformKeys(dealWho{RelayAddress: "reply-1@relay.example"}))
	assert.Equal(t, platformKeys(dealWho{Domain: "ebay.com"}), recipientKeys("platform", dealWho{Domain: "ebay.com"}, "marketplace"))
	assert.Empty(t, recipientKeys("other", dealWho{Domain: "ebay.com"}, "marketplace"))
}

// In typed records a share that passes is covered by its own evaluation,
// not by an approval: a repeat email to the same platform stays covered,
// and an approval of the counterparty never covers a platform.
func TestDealTypedRepeatToAPlatformIsCovered(t *testing.T) {
	dealFixture(t)
	open := func() string {
		return dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, `{"type":"purchase","channel":"marketplace",
		"intent":{"verbatim":"buy the bike","allowed":["pay","share_contact"]},
		"who":{"name":"Garage Sale Gary","profile_id":"mkt:seller:4411"},
		"terms":{"item":"bike","price_minor":12000,"currency":"USD"},
		"recourse":{"rail":"cash","refundable":false}}`))["deal_id"].(string)
	}
	dealID := open()
	first := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, ebayEmail))
	require.Equal(t, "pause", first["verdict"])
	card := filepath.Join(t.TempDir(), "card.txt")
	require.NoError(t, os.WriteFile(card, []byte(first["card"].(string)), 0o600))
	dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", first["check_id"].(string), "--choice", "proceed", "--said", "ok, eBay", "--shown-card", card)
	require.Equal(t, true, dealRun(t, "note", "--deal", dealID, "--kind", "disclosure", "--input", writeJSON(t, platformEmailNote("ebay.com")))["approved"])

	again := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, ebayEmail))
	require.Equal(t, "pass", again["verdict"], again["card"])
	repeat := dealRun(t, "note", "--deal", dealID, "--kind", "disclosure", "--input", writeJSON(t, platformEmailNote("ebay.com")))
	assert.Equal(t, true, repeat["approved"], repeat["reason"])

	// The counterparty's approval does not cover a platform.
	other := open()
	check := dealRun(t, "check", "--deal", other, "--input", writeJSON(t, `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["phone"]}`))
	require.NoError(t, os.WriteFile(card, []byte(check["card"].(string)), 0o600))
	dealRun(t, "note", "--deal", other, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", "ok", "--shown-card", card)
	held := heldNote(t, other, `{"to":"platform","who":{"domain":"craigslist.org"},"fields":[{"class":"phone","value":"+1 555 010 4477"}]}`)
	assert.Equal(t, []any{"phone"}, held["held"])
}

// A shared copy says a platform was told, never who or what value; deal
// list counts a platform among "others", once per platform, by its domain.
func TestDealPlatformTellingInTheSharedCopyAndTheList(t *testing.T) {
	email := []dealDisclosureField{{Class: "email", Value: homeEmail}}
	items := toldItems([]dealToldItem{{To: "eBay", ToKind: "platform", Fields: email, Authority: "approval"}}, false)
	text := items[0].(map[string]interface{})["text"].(string)
	assert.Equal(t, "Told a platform: email", text)

	tally := newDealToldTally()
	tally.add("deal-a", dealDisclosure{To: "platform", Who: &dealWho{Domain: "ebay.com"}, Fields: email}, dealOpen{Channel: "marketplace"}, dealRoleSeller)
	tally.add("deal-b", dealDisclosure{To: "platform", Who: &dealWho{Domain: "www.ebay.com"}, Fields: email}, dealOpen{Channel: "marketplace"}, dealRoleBuyer)
	for _, r := range tally.rows() {
		if r.Class == "email" {
			assert.Equal(t, dealListTold{Class: "email", Recipients: 1, ToOthers: 1}, r)
		}
	}
}

// In typed records a passing check covers a repeat telling to the deal's
// counterparty too, and only the classes it named.
func TestDealTypedPassingShareCoversTheCounterpartyAndItsClassesOnly(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, `{"type":"purchase","channel":"marketplace",
		"intent":{"verbatim":"buy the bike","allowed":["pay","share_contact"]},
		"who":{"name":"Garage Sale Gary","profile_id":"mkt:seller:4411"},
		"terms":{"item":"bike","price_minor":12000,"currency":"USD"},
		"recourse":{"rail":"cash","refundable":false}}`))["deal_id"].(string)
	tell := func(classes, fields string) {
		check := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"share_contact","disclosing_to":"counterparty","disclosing":[`+classes+`]}`))
		if check["verdict"] == "pause" {
			card := filepath.Join(t.TempDir(), "card.txt")
			require.NoError(t, os.WriteFile(card, []byte(check["card"].(string)), 0o600))
			dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", "ok", "--shown-card", card)
		}
		require.Equal(t, true, dealRun(t, "note", "--deal", dealID, "--kind", "disclosure", "--input", writeJSON(t, `{"fields":[`+fields+`]}`))["approved"])
	}
	tell(`"email","phone"`, `{"class":"email","value":"`+homeEmail+`"},{"class":"phone","value":"+1 555 010 4477"}`)
	// A repeat passes, and its own evaluation covers it.
	tell(`"email"`, `{"class":"email","value":"`+homeEmail+`"}`)
	// A class the passing check did not name is not covered by it.
	check := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["email"]}`))
	require.Equal(t, "pass", check["verdict"])
	sent := dealRun(t, "note", "--deal", dealID, "--kind", "disclosure", "--input", writeJSON(t, `{"fields":[{"class":"email","value":"`+homeEmail+`"},{"class":"phone","value":"+1 555 010 4477"}]}`))
	assert.Equal(t, false, sent["approved"])
	assert.Contains(t, sent["reason"], "the check did not name your phone")
}
