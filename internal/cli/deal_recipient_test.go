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
	return dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"share_contact","disclosing":[`+classes+`]}`))
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
	pay := dealRun(t, "check", "--deal", again, "--input", writeJSON(t, `{"action":"pay","amount_minor":1200,"terms":{"price_minor":1200}}`))
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

	// The user holds; the agent sends it anyway: an unapproved disclosure.
	dealRun(t, "note", "--deal", stranger, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "hold", "--said", "not yet")
	sent := dealRun(t, "note", "--deal", stranger, "--kind", "disclosure", "--input", writeJSON(t, `{"fields":[{"class":"address","value":"`+homeStreet+`"}]}`))
	assert.Equal(t, false, sent["approved"])
	report := dealRun(t, "report", "--deal", stranger)
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
	out := dealRun(t, "note", "--deal", dealID, "--kind", "disclosure", "--input", writeJSON(t, `{"fields":[{"class":"email","value":"`+homeEmail+`"},{"class":"phone","value":"+1 555 010 3030"}]}`))
	assert.Equal(t, false, out["approved"])
	assert.Equal(t, "the check did not name your phone", out["reason"])
}

func TestDealCheckDisclosingIsChecked(t *testing.T) {
	dealFixture(t)
	dealID := openMerchant(t, "redbubble.com")
	for body, want := range map[string]string{
		`{"action":"share_contact","disclosing":["shoe_size"]}`:          "not one of",
		`{"action":"share_contact","disclosing":["email","credential"]}`: "mixes contact details and credentials",
		`{"action":"share_credentials","disclosing":["email"]}`:          "goes with action share_contact",
	} {
		out, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", dealID, "--input", writeJSON(t, body))
		require.Error(t, err, body)
		assert.Contains(t, out+err.Error(), want, body)
	}
}
