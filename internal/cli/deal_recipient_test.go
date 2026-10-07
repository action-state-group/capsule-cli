package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The user's own details, as a checkout's delivery form takes them.
const (
	homeStreet = "17 Wren Street, Flat 2, Bristol"
	homeEmail  = "pat@mail.example"
	homeName   = "Pat Quinlan"
)

func openMerchant(t *testing.T, domain string) string {
	t.Helper()
	return dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"buy the otter sticker, under $10","allowed":["pay"],"max_total_minor":1000},
		"who":{"name":"Redbubble","domain":"`+domain+`"},
		"terms":{"item":"otter sticker","price_minor":627,"currency":"USD"},
		"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
}

func shareCheck(t *testing.T, dealID, classes string) map[string]any {
	t.Helper()
	return dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"share_contact","disclosing_to":"counterparty","disclosing":[`+classes+`]}`))
}

func noteDelivery(t *testing.T, dealID string) map[string]any {
	t.Helper()
	return dealRun(t, "note", "--deal", dealID, "--kind", "disclosure", "--input", writeJSON(t, `{"channel":"web","fields":[
		{"class":"name","value":"`+homeName+`"},{"class":"email","value":"`+homeEmail+`"},{"class":"address","value":"`+homeStreet+`"}]}`))
}

// The address entered at a merchant already told it: a note, no pause. The
// same address to a counterparty never dealt with: a pause naming them and
// "street address", with the first-time line on the card.
func TestDealPausesOnTheRecipientNotTheField(t *testing.T) {
	dealFixture(t)

	// The first checkout at the merchant: everything is a first.
	first := openMerchant(t, "redbubble.com")
	check := shareCheck(t, first, `"name","email","address"`)
	require.Equal(t, "pause", check["verdict"])
	card := check["card"].(string)
	assert.Contains(t, card, "First time dealing with Redbubble")
	assert.Contains(t, card, "First time telling Redbubble your street address")
	assert.NotContains(t, card, "You didn't ask for this", "a share that names what it gives is judged by its recipient")
	dealRun(t, "note", "--deal", first, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", "yes, ship it to me")
	assert.Equal(t, true, noteDelivery(t, first)["approved"])

	// The tenth checkout at the same merchant (another page of its site): a
	// note, no pause, and nothing on the card about a first dealing.
	again := openMerchant(t, "https://www.redbubble.com/shop/otters")
	check = shareCheck(t, again, `"address","email","name"`)
	assert.Equal(t, "pass", check["verdict"], check["card"])
	assert.Equal(t, true, check["proceed"])
	assert.Empty(t, check["card"])
	assert.Equal(t, "Told Redbubble your street address, email, name before: noted, no pause for that.", check["note"])
	recipient := check["recipient"].(map[string]any)
	assert.Equal(t, false, recipient["first_time"])
	assert.Equal(t, []any{"address", "email", "name"}, recipient["repeat_disclosures"])
	assert.Equal(t, true, noteDelivery(t, again)["approved"], "the note is covered by the passing check")
	// A known merchant's card, when something else pauses, has no first-time line.
	pay := dealRun(t, "check", "--deal", again, "--input", writeJSON(t, `{"action":"pay","amount_minor":1200,"authorized_max_minor":1200,"terms":{"price_minor":1200}}`))
	require.Equal(t, "pause", pay["verdict"])
	assert.NotContains(t, pay["card"], "First time dealing")

	// A stranger on a marketplace: the same address pauses, naming them.
	stranger := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"marketplace",
		"intent":{"verbatim":"buy the bike, you can arrange pickup","allowed":["pay","share_contact"]},
		"who":{"name":"Garage Sale Gary","profile_id":"mkt:seller:4411"},
		"terms":{"item":"bike","price_minor":12000,"currency":"USD"},
		"recourse":{"rail":"cash","refundable":false}}`))["deal_id"].(string)
	check = shareCheck(t, stranger, `"address"`)
	require.Equal(t, "pause", check["verdict"], "allowed share_contact does not stand in for the first telling")
	card = check["card"].(string)
	assert.Contains(t, card, "First time dealing with Garage Sale Gary")
	assert.Contains(t, card, "First time telling Garage Sale Gary your street address")
	assert.Contains(t, check["differences"], map[string]any{"question": "who", "rule": "first_disclosure", "field": "address", "text": "First time telling Garage Sale Gary your street address"})
	assert.NotContains(t, card, homeStreet, "the card names the class, never the value")

	// The user holds; the agent tries to note it anyway: held, nothing sealed.
	dealRun(t, "note", "--deal", stranger, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "hold", "--said", "not yet")
	held := heldNote(t, stranger, `{"fields":[{"class":"address","value":"`+homeStreet+`"}]}`)
	assert.Equal(t, []any{"address"}, held["held"])
	report := dealRun(t, "report", "--deal", stranger)
	assert.Empty(t, report["told"])
	assert.Contains(t, reportTexts(t, report, "anomalies"), "agent/first_disclosure: First time telling Garage Sale Gary your street address")

	// Neither the note nor the pause puts a raw value in a sealed record or a
	// shared copy.
	for _, dealID := range []string{first, again, stranger} {
		export := filepath.Join(t.TempDir(), "deal.json")
		dealRun(t, "export", "--deal", dealID, "--output", export)
		raw, err := os.ReadFile(export)
		require.NoError(t, err)
		for _, v := range []string{homeStreet, "Wren", homeEmail, homeName, "Redbubble", "Garage Sale Gary"} {
			assert.NotContains(t, string(raw), v, "%s: the sealed records carry no %q", dealID, v)
		}
		dir := t.TempDir()
		for _, audience := range []string{"counterparty", "adjudicator"} {
			page := filepath.Join(dir, audience+".html")
			shareRun(t, dealID, audience, page)
			raw, err := os.ReadFile(page)
			require.NoError(t, err)
			low := strings.ToLower(string(raw))
			for _, v := range []string{homeStreet, "wren street", homeEmail, homeName} {
				assert.NotContains(t, low, strings.ToLower(v), "%s %s: the shared copy carries no %q", dealID, audience, v)
			}
		}
	}
}

// A disclosure gives only what its check named.
func TestDealDisclosureBeyondTheCheckIsNotCovered(t *testing.T) {
	dealFixture(t)
	dealID := openMerchant(t, "redbubble.com")
	check := shareCheck(t, dealID, `"email"`)
	dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", "ok")
	out := heldNote(t, dealID, `{"fields":[{"class":"email","value":"`+homeEmail+`"},{"class":"phone","value":"+1 555 010 3030"}]}`)
	assert.Equal(t, []any{"phone"}, out["held"], "a first telling the check did not name is held")
}

func TestDealCheckDisclosingIsChecked(t *testing.T) {
	dealFixture(t)
	dealID := openMerchant(t, "redbubble.com")
	for body, want := range map[string]string{
		`{"action":"share_contact","disclosing_to":"counterparty","disclosing":["shoe_size"]}`:          "not one of",
		`{"action":"share_contact","disclosing_to":"counterparty","disclosing":["email","credential"]}`: "mixes contact details and credentials",
		`{"action":"share_credentials","disclosing_to":"counterparty","disclosing":["email"]}`:          "goes with action share_contact",
	} {
		out, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", dealID, "--input", writeJSON(t, body))
		require.Error(t, err, body)
		assert.Contains(t, out+err.Error(), want, body)
	}
}

func openSeller(t *testing.T, who string) string {
	t.Helper()
	return dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"marketplace",
		"intent":{"verbatim":"buy the bike","allowed":["pay","share_contact"]},
		"who":`+who+`,
		"terms":{"item":"bike","price_minor":12000,"currency":"USD"},
		"recourse":{"rail":"cash","refundable":false}}`))["deal_id"].(string)
}

// tellAddress checks, approves and seals the address going to the deal's
// counterparty; it returns the check.
func tellAddress(t *testing.T, dealID string) map[string]any {
	t.Helper()
	check := shareCheck(t, dealID, `"address"`)
	if check["verdict"] == "pause" {
		dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", "ok")
	}
	require.Equal(t, true, dealRun(t, "note", "--deal", dealID, "--kind", "disclosure", "--input", writeJSON(t, `{"fields":[{"class":"address","value":"`+homeStreet+`"}]}`))["approved"])
	return check
}

// A platform's domain is shared by every seller on it: a stranger on the
// same marketplace is a stranger. Per-party identities decide when either
// side has one; the registrable domain decides only when it is all both
// sides have.
func TestDealSharedPlatformIsNotOneCounterparty(t *testing.T) {
	dealFixture(t)
	first := tellAddress(t, openSeller(t, `{"name":"Seller A","profile_id":"fb:111","domain":"facebook.com"}`))
	assert.Equal(t, "pause", first["verdict"])

	for _, who := range []string{
		`{"name":"Seller B","profile_id":"fb:222","domain":"https://www.facebook.com/marketplace"}`,
		`{"name":"Seller C","domain":"evil.facebook.com"}`,
		`{"name":"Seller A","phone":"+1 555 010 0001","domain":"facebook.com"}`,
	} {
		check := shareCheck(t, openSeller(t, who), `"address"`)
		assert.Equal(t, "pause", check["verdict"], "%s is not seller A", who)
		assert.Contains(t, check["card"], "First time telling", who)
		assert.Contains(t, check["card"], "First time dealing with", who)
	}

	// Seller A again, by profile id: a note.
	check := shareCheck(t, openSeller(t, `{"name":"A. Seller","profile_id":"fb:111","domain":"m.facebook.com"}`), `"address"`)
	assert.Equal(t, "pass", check["verdict"], check["card"])
	assert.Contains(t, check["note"], "street address before")

	// A merchant known only by its website matches by registrable domain;
	// a lookalike does not.
	tellAddress(t, openMerchant(t, "shop.example.com"))
	assert.Equal(t, "pass", shareCheck(t, openMerchant(t, "https://www.example.com/checkout"), `"address"`)["verdict"])
	for _, lookalike := range []string{"example-shop.com", "examp1e.com", "example.com.evil.net"} {
		assert.Equal(t, "pause", shareCheck(t, openMerchant(t, lookalike), `"address"`)["verdict"], lookalike)
	}
}

// Leaving out what is given is no way around the first-time pause.
func TestDealOmittingWhatIsGivenDoesNotBypassTheFirstTelling(t *testing.T) {
	dealFixture(t)
	dealID := openSeller(t, `{"name":"Garage Sale Gary","profile_id":"mkt:seller:4411"}`)

	// (a) A share_contact check must name what it gives.
	out, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"share_contact"}`))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, out+err.Error(), `needs "disclosing"`)

	// (b) A disclosure whose covering check named nothing (here a
	// share_credentials check, where naming is optional) is a first telling
	// of each class it gives: held when noted, before it is sent.
	check := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"share_credentials","disclosing_to":"counterparty"}`))
	dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", "ok")
	held := heldNote(t, dealID, `{"fields":[{"class":"verification_code","value":"481516"}]}`)
	assert.Equal(t, []any{"verification_code"}, held["held"])

	// (c) Declaring less than is given: the check names only the name, and
	// the address noted at fill time to a first-time counterparty pauses.
	other := openSeller(t, `{"name":"Porch Pickup Pam","profile_id":"mkt:seller:9090"}`)
	check = shareCheck(t, other, `"name"`)
	dealRun(t, "note", "--deal", other, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", "ok")
	held = heldNote(t, other, `{"fields":[{"class":"name","value":"`+homeName+`"},{"class":"address","value":"`+homeStreet+`"}]}`)
	assert.Equal(t, []any{"address"}, held["held"])
	assert.Contains(t, held["reason"], "First time telling Porch Pickup Pam your street address")
}

// A marketplace's domain is never a party's identity, even when it is all a
// seller record carries.
func TestDealMarketplaceDomainIsNotAnIdentity(t *testing.T) {
	dealFixture(t)
	tellAddress(t, openSeller(t, `{"name":"Seller One","domain":"facebook.com"}`))
	check := shareCheck(t, openSeller(t, `{"name":"Seller Two","domain":"https://www.facebook.com/marketplace/item/1"}`), `"address"`)
	assert.Equal(t, "pause", check["verdict"], "a second seller on the same marketplace pauses")
	// A marketplace channel drops the domain too, whatever site it is.
	tellAddress(t, openSeller(t, `{"name":"Swap One","domain":"swapmeet.example"}`))
	check = shareCheck(t, openSeller(t, `{"name":"Swap Two","domain":"swapmeet.example"}`), `"address"`)
	assert.Equal(t, "pause", check["verdict"], "on a marketplace channel the domain is not an identity")
}

// A profile with no counterparty history (a first install, or a reinstall)
// makes everyone first-time: its first pause says so once, and keeps the
// pause.
func TestDealNewProfileSaysSoOnItsFirstPause(t *testing.T) {
	dealFixture(t)
	first := shareCheck(t, openMerchant(t, "redbubble.com"), `"address"`)
	require.Equal(t, "pause", first["verdict"])
	assert.Contains(t, first["card"], newProfileLine)
	second := shareCheck(t, openMerchant(t, "society6.com"), `"address"`)
	require.Equal(t, "pause", second["verdict"])
	assert.NotContains(t, second["card"], newProfileLine, "said once")

	// A reinstall: a fresh profile says it again.
	dealFixture(t)
	again := shareCheck(t, openMerchant(t, "redbubble.com"), `"address"`)
	assert.Contains(t, again["card"], newProfileLine)
}

// An approval covers a telling only to the party its check was about.
func TestDealApprovalForOnePartyDoesNotCoverAnother(t *testing.T) {
	dealFixture(t)
	dealID := openSeller(t, `{"name":"Garage Sale Gary","profile_id":"mkt:seller:4411"}`)
	check := shareCheck(t, dealID, `"address"`)
	dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", "yes, give Gary the address")

	// The address approved for Gary goes to a courier instead: held.
	held := heldNote(t, dealID, `{"to":"other","who":{"phone":"+1 555 010 7777"},"fields":[{"class":"address","value":"`+homeStreet+`"}]}`)
	assert.Equal(t, []any{"address"}, held["held"])

	// A check about the courier, approved, covers the courier and no one else.
	courier := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t,
		`{"action":"share_contact","disclosing":["address"],"disclosing_to":"other","recipient":{"name":"Quick Couriers","phone":"+1 555 010 7777"}}`))
	require.Equal(t, "pause", courier["verdict"])
	assert.Contains(t, courier["card"], "First time telling Quick Couriers your street address")
	dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", courier["check_id"].(string), "--choice", "proceed", "--said", "ok, the courier")
	held = heldNote(t, dealID, `{"to":"other","who":{"phone":"+1 555 010 8888"},"fields":[{"class":"address","value":"`+homeStreet+`"}]}`)
	assert.Equal(t, []any{"address"}, held["held"], "another courier is another party")
	sent := dealRun(t, "note", "--deal", dealID, "--kind", "disclosure", "--input", writeJSON(t,
		`{"to":"other","who":{"phone":"+1 555 010 7777"},"fields":[{"class":"address","value":"`+homeStreet+`"}]}`))
	assert.Equal(t, true, sent["approved"])

	// A check about someone else must say who, by an identity, not a name.
	for body, want := range map[string]string{
		`{"action":"share_contact","disclosing":["address"],"disclosing_to":"other","recipient":{"name":"Quick Couriers"}}`:          "needs a recipient with",
		`{"action":"share_contact","disclosing_to":"counterparty","disclosing":["address"],"recipient":{"phone":"+1 555 010 7777"}}`: `recipient goes with "disclosing_to": "other"`,
		`{"action":"share_contact","disclosing":["address"],"recipient":{"phone":"+1 555 010 7777"}}`:                                `needs "disclosing_to"`,
		`{"action":"share_contact","disclosing":["address"],"disclosing_to":"courier"}`:                                              "disclosing_to must be counterparty or other",
	} {
		out, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", dealID, "--input", writeJSON(t, body))
		require.ErrorIs(t, err, ErrInput, body)
		assert.Contains(t, out+err.Error(), want, body)
	}
}

// Every share check says who receives it: nothing defaults to the
// counterparty, for contact details and credentials alike.
func TestDealEveryShareCheckNamesItsRecipient(t *testing.T) {
	dealFixture(t)
	dealID := openSeller(t, `{"name":"Garage Sale Gary","profile_id":"mkt:seller:4411"}`)
	for _, body := range []string{
		`{"action":"share_contact","disclosing":["address"]}`,
		`{"action":"share_credentials"}`,
		`{"action":"share_credentials","disclosing":["verification_code"]}`,
	} {
		out, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", dealID, "--input", writeJSON(t, body))
		require.ErrorIs(t, err, ErrInput, body)
		assert.Contains(t, out+err.Error(), `a share check needs "disclosing_to"`, body)
	}
	// Not on any other action.
	out, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"pay","amount_minor":12000,"authorized_max_minor":12000,"disclosing_to":"counterparty"}`))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, out+err.Error(), "disclosing_to and recipient go with a share check")
	// Named, it is sealed in the check record, with or without classes.
	check := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"share_credentials","disclosing_to":"counterparty"}`))
	export := filepath.Join(t.TempDir(), "deal.json")
	dealRun(t, "export", "--deal", dealID, "--output", export)
	raw, err := os.ReadFile(export)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"disclosing_to":"counterparty"`)
	assert.NotEmpty(t, check["check_id"])
}
