package plugin

import (
	"strings"
	"testing"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// One deal per errand, with journey records (capsule-cli#340).

// fawnErrand is the 2026-10-20 errand's opening: the user asked to buy, set
// a $2 limit, and handed the agent the choice of item, in their own words.
// No merchant is in play yet, and no way to pay.
const fawnErrand = `{"type":"purchase","channel":"web",
	"intent":{"verbatim":"get me a fawn sticker, any of the options is fine, cheapest with shipping, under $2",
		"max_total_minor":200,"allowed":["pay","share_contact"],
		"delegated":["item"],"delegated_said":"any of the options is fine, cheapest with shipping"},
	"who":{},"terms":{"currency":"USD"},"recourse":{}}`

const fawnOptions = `{"options":[
	{"item":"Fawn sticker 3in","who":{"name":"AliExpress","domain":"aliexpress.com"},"price_minor":120},
	{"item":"Fawn Sticker","who":{"name":"eBay","domain":"ebay.com"},"price_minor":175},
	{"item":"Woodland fawn sticker","who":{"name":"Etsy","domain":"etsy.com"},"price_minor":250},
	{"item":"Fawn vinyl decal","who":{"name":"Redbubble","domain":"redbubble.com"},"price_minor":300}],"found":37}`

func note(t *testing.T, id, kind, body string) map[string]any {
	t.Helper()
	return dealRun(t, "note", "--deal", id, "--kind", kind, "--input", writeJSON(t, body))
}

func noteRefused(t *testing.T, id, kind, body, want string) {
	t.Helper()
	out, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", id, "--kind", kind, "--input", writeJSON(t, body))
	require.ErrorIs(t, err, cli.ErrInput, body)
	assert.Contains(t, out+err.Error(), want, body)
}

func journeyMoves(t *testing.T, id string) []string {
	t.Helper()
	var moves []string
	for _, s := range dealRun(t, "report", "--deal", id)["journey"].([]any) {
		m := s.(map[string]any)
		moves = append(moves, m["by"].(string)+":"+m["move"].(string))
	}
	return moves
}

// checkBodies are the deal's sealed check record bodies, in order.
func checkBodies(t *testing.T, id string) []map[string]any {
	t.Helper()
	records, export := exportRecords(t, id)
	checkProfile(t, export)
	var out []map[string]any
	for _, r := range records {
		if b, ok := r["x-deal-v0"].(map[string]any); ok && b["record_type"] == "check" {
			out = append(out, r["body"].(map[string]any))
		}
	}
	return out
}

func rules(check map[string]any) []string {
	var out []string
	for _, d := range check["differences"].([]any) {
		out = append(out, d.(map[string]any)["rule"].(string))
	}
	return out
}

// fawnDeal seals the 2026-10-20 errand as one deal, up to its pay, and
// returns the deal and its two checks.
func fawnDeal(t *testing.T) (id string, share, pay map[string]any) {
	t.Helper()
	id = dealRun(t, "open", "--input", writeJSON(t, fawnErrand))["deal_id"].(string)
	note(t, id, "searched", `{"where":"aliexpress, ebay, etsy, redbubble","results":37}`)
	note(t, id, "options", fawnOptions)
	note(t, id, "recommended", `{"option":1,"reason":"cheapest delivered"}`)
	note(t, id, "message", `{"from":"user","text":"I'll sign in to AliExpress myself"}`)
	note(t, id, "couldnt", `{"reason":"sign_in_required","who":{"name":"AliExpress","domain":"aliexpress.com"}}`)

	// No merchant is in play yet: nothing to check.
	out, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", id, "--input", writeJSON(t, `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["name","address"]}`))
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Contains(t, out+err.Error(), "no merchant is in play")

	switched := note(t, id, "intent", `{"verbatim":"lets change to ebay","who":{"name":"eBay","domain":"ebay.com"}}`)
	assert.Equal(t, "switched", switched["move"])
	assert.Equal(t, true, switched["starts_leg"])
	note(t, id, "chose", `{"item":"Fawn Sticker","price_minor":175}`)

	share = dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["name","address"]}`))
	require.Equal(t, "pause", share["verdict"], share["card"])
	assert.Equal(t, []string{"first_disclosure", "first_disclosure"}, rules(share))
	_, err = answer(t, id, share["check_id"].(string), "yes, send it", shownCard(t, share["card"].(string)))
	require.NoError(t, err)
	assert.Equal(t, true, note(t, id, "disclosure", `{"fields":[{"class":"name","value":"`+homeName+`"},{"class":"address","value":"`+homeStreet+`"}]}`)["approved"])

	pay = dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":190,"authorized_max_minor":190,"terms":{"item":"Fawn Sticker","price_minor":175},"recourse":{"rail":"card","refundable":true}}`))
	require.Equal(t, "pause", pay["verdict"], pay["card"])
	assert.Equal(t, []string{"promised_ask"}, rules(pay), "the share's card said it would ask before paying")
	assert.Contains(t, pay["card"], "within your $2 limit")
	assert.Contains(t, pay["card"], "Fawn Sticker", "the pay card names the chosen item")
	_, err = answer(t, id, pay["check_id"].(string), "ok", shownCard(t, pay["card"].(string)))
	require.NoError(t, err)
	act := note(t, id, "act", `{"action":"pay","amount_minor":190,"payee":"eBay","rail":"card","reference":"EB-1"}`)
	assert.Equal(t, false, act["unchecked"])

	return id, share, pay
}

// The golden: the 2026-10-20 errand as one deal. Asked → Searched →
// Showed you 4 options → Recommended → Couldn't (sign-in) → Switched to
// eBay → Chose for you → two pauses approved → Paid $1.90 within $2. No
// payee change, no "Not what you asked", no pause on the chosen item, and
// at both checks no material field changed.
func TestTheFawnErrandIsOneDeal(t *testing.T) {
	dealFixture(t)
	id, share, pay := fawnDeal(t)
	assert.Equal(t, []string{"user:asked", "agent:searched", "agent:options", "agent:recommended", "agent:couldnt",
		"user:switched", "agent:chose", "agent:paused", "user:approved", "agent:told", "agent:paused", "user:approved", "agent:act.pay"}, journeyMoves(t, id))
	for _, c := range []map[string]any{share, pay} {
		for _, r := range rules(c) {
			assert.NotContains(t, []string{"payee_or_contact_changed", "not_asked", "agent_picked", "over_limit", "terms_changed"}, r)
		}
	}
	bodies := checkBodies(t, id)
	require.Len(t, bodies, 2)
	for _, b := range bodies {
		assert.Equal(t, float64(0), b["material_fields_changed"], b)
		assert.NotContains(t, b, "offer_fields_changed", "the user's words state no item: nothing to compare")
	}
	report := dealRun(t, "report", "--deal", id)
	labels := []string{}
	for _, s := range report["journey"].([]any) {
		labels = append(labels, s.(map[string]any)["label"].(string))
	}
	assert.Equal(t, []string{"Asked", "Searched", "Showed you 4 options", "Recommended", "Couldn't (AliExpress sign-in)",
		"Switched to eBay", "Chose Fawn Sticker for you", "Waited for your OK", "OK'd sharing your name and address",
		"Told eBay your name and address", "Waited for your OK", "OK'd paying $1.90", "Paid $1.90 within your $2 limit"}, labels)
}

// The Raised path: an ask for options only allows nothing; picking one and
// asking to buy it is a pick with a proposal, shown as both, and pay waits
// for the user's confirmation of it.
func TestAPickThatAsksToBuyIsPickedAndRaised(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"show me fawn stickers under $2","max_total_minor":200,"allowed":[]},
		"who":{},"terms":{"currency":"USD"},"recourse":{}}`))["deal_id"].(string)
	note(t, id, "options", fawnOptions)
	pick := note(t, id, "intent", `{"verbatim":"get the eBay one","asked":{"item":"Fawn Sticker"},"who":{"name":"eBay","domain":"ebay.com"},"allowed":["pay"]}`)
	assert.Equal(t, "picked", pick["move"])
	assert.Contains(t, pick, "proposed")
	payBody := `{"action":"pay","amount_minor":190,"authorized_max_minor":190,"terms":{"item":"Fawn Sticker","price_minor":175},"recourse":{"rail":"card","refundable":true}}`
	before := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payBody))
	assert.Contains(t, rules(before), "not_asked")
	dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", pick["capsule_id"].(string), "--choice", "confirm_limits", "--said", "yes, buy it")
	after := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payBody))
	assert.NotContains(t, rules(after), "not_asked")
	assert.Equal(t, []string{"user:asked", "agent:options", "user:picked", "user:raised", "agent:paused"}, journeyMoves(t, id)[:5])
	checkBodies(t, id)
}

// Each user move is derived by capsulectl from what the note changes.
func TestEachUserMoveIsDerived(t *testing.T) {
	open := func(t *testing.T) string {
		dealFixture(t)
		return dealRun(t, "open", "--input", writeJSON(t, fawnErrand))["deal_id"].(string)
	}
	ebay := `{"name":"eBay","domain":"ebay.com"}`
	t.Run("picked: an option shown, over the recommended one", func(t *testing.T) {
		id := open(t)
		note(t, id, "options", fawnOptions)
		note(t, id, "recommended", `{"option":1,"reason":"cheapest"}`)
		assert.Equal(t, "picked", note(t, id, "intent", `{"verbatim":"the eBay one","who":`+ebay+`}`)["move"])
	})
	t.Run("switched: a merchant off the shortlist after a recommendation", func(t *testing.T) {
		id := open(t)
		note(t, id, "options", fawnOptions)
		note(t, id, "recommended", `{"option":1,"reason":"cheapest"}`)
		assert.Equal(t, "switched", note(t, id, "intent", `{"verbatim":"get it from Amazon instead","who":{"name":"Amazon","domain":"amazon.com"}}`)["move"])
	})
	t.Run("switched: another merchant once a leg exists, with the user's why", func(t *testing.T) {
		id := open(t)
		note(t, id, "intent", `{"verbatim":"use eBay","who":`+ebay+`}`)
		m := note(t, id, "intent", `{"verbatim":"Etsy instead","who":{"name":"Etsy","domain":"etsy.com"},"why":"I trust Etsy more"}`)
		assert.Equal(t, "switched", m["move"])
		assert.Equal(t, true, m["starts_leg"])
	})
	t.Run("narrowed: a lower limit", func(t *testing.T) {
		id := open(t)
		assert.Equal(t, "narrowed", note(t, id, "intent", `{"verbatim":"actually under $1.80","max_total_minor":180}`)["move"])
	})
	t.Run("raised: only a higher limit, confirmed as today", func(t *testing.T) {
		id := open(t)
		m := note(t, id, "intent", `{"verbatim":"up to $3 is fine","max_total_minor":300,"why":"it is a gift"}`)
		assert.Equal(t, "raised", m["move"])
		assert.Contains(t, m, "proposed")
	})
	t.Run("raised: handing over a choice later", func(t *testing.T) {
		dealFixture(t)
		id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
			"intent":{"verbatim":"find me a fawn sticker under $2","max_total_minor":200,"allowed":["pay"]},
			"who":{},"terms":{"currency":"USD"},"recourse":{}}`))["deal_id"].(string)
		m := note(t, id, "intent", `{"verbatim":"just pick one","delegated":["item"],"delegated_said":"just pick one"}`)
		assert.Equal(t, "raised", m["move"])
		noteRefused(t, id, "chose", `{"item":"Fawn Sticker","who":`+ebay+`}`, "must delegate the item")
		dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", m["capsule_id"].(string), "--choice", "confirm_limits", "--said", "yes, pick one")
		note(t, id, "chose", `{"item":"Fawn Sticker","who":`+ebay+`}`)
		checkBodies(t, id)
	})
	t.Run("refused: a note that changes nothing", func(t *testing.T) {
		id := open(t)
		noteRefused(t, id, "intent", `{"verbatim":"I'll sign in myself"}`, "changes nothing the user asked")
	})
	t.Run("refused: why on a narrowing, and the merchant already in play", func(t *testing.T) {
		id := open(t)
		noteRefused(t, id, "intent", `{"verbatim":"under $1.50","max_total_minor":150,"why":"budget"}`, "why is kept only on a switch or a raise")
		note(t, id, "intent", `{"verbatim":"use eBay","who":`+ebay+`}`)
		noteRefused(t, id, "intent", `{"verbatim":"eBay again","who":`+ebay+`}`, "names the merchant already in play")
	})
	t.Run("picking the item takes it back from the delegation", func(t *testing.T) {
		id := open(t)
		note(t, id, "options", fawnOptions)
		assert.Equal(t, "picked", note(t, id, "intent", `{"verbatim":"option 2","asked":{"item":"Fawn Sticker"},"who":`+ebay+`}`)["move"])
		noteRefused(t, id, "chose", `{"item":"Fawn vinyl decal"}`, "must delegate the item")
	})
}

// A user's switch starts a leg: it is no payee change, and nothing checked
// with the earlier merchant covers a step with the new one. A seller's own
// change within the leg is still a payee change.
func TestASwitchIsNoPayeeChangeButASellerChangeStillIs(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"buy the fawn sticker under $2","asked":{"item":"Fawn Sticker"},"max_total_minor":200,"allowed":["pay","share_contact"]},
		"who":{"name":"AliExpress","domain":"aliexpress.com","phone":"+1 555 010 3000"},"terms":{"item":"Fawn Sticker","price_minor":120,"currency":"USD"},
		"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	share := `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["address"]}`
	first := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, share))
	_, err := answer(t, id, first["check_id"].(string), "yes", shownCard(t, first["card"].(string)))
	require.NoError(t, err)

	note(t, id, "intent", `{"verbatim":"lets change to ebay","who":{"name":"eBay","domain":"ebay.com","phone":"+1 555 010 4000"}}`)
	// The address approved for AliExpress does not cover eBay.
	held := heldNote(t, id, `{"fields":[{"class":"address","value":"`+homeStreet+`"}]}`)
	assert.Equal(t, []any{"address"}, held["held"])

	pay := `{"action":"pay","amount_minor":180,"authorized_max_minor":180,"terms":{"item":"Fawn Sticker","price_minor":180},"recourse":{"rail":"card","refundable":true}}`
	c := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, pay))
	assert.NotContains(t, rules(c), "payee_or_contact_changed", "the user's switch is not the seller changing its details")
	assert.NotContains(t, rules(c), "not_asked", "the ask survives the switch")

	note(t, id, "change", `{"source":"seller message","who":{"phone":"+1 555 010 9999"}}`)
	c = dealRun(t, "check", "--deal", id, "--input", writeJSON(t, pay))
	assert.Contains(t, rules(c), "payee_or_contact_changed")
	assert.Contains(t, differenceTexts(c), "+1 555 010 4000 → +1 555 010 9999", "against the leg's first contact")
	assert.Contains(t, optionLabels(c), "Call +1 555 010 4000 first", "the leg's own number")
	checkBodies(t, id)
}

// The agent's choice names a merchant only while none is in play; it needs
// the user's delegation; a recommendation names an option shown.
func TestAgentMovesAreChecked(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, fawnErrand))["deal_id"].(string)
	noteRefused(t, id, "recommended", `{"option":1,"reason":"cheapest"}`, "seal them first")
	noteRefused(t, id, "chose", `{"item":"Fawn Sticker"}`, "chose names the one you chose")
	noteRefused(t, id, "searched", `{"where":"ebay"}`, `searched needs "where"`)
	noteRefused(t, id, "searched", `{"move":"options","where":"ebay","results":3}`, "leave move out")
	noteRefused(t, id, "options", `{"options":[{"item":"a"},{"item":"b"}],"found":1}`, "at least as many as were shown")
	noteRefused(t, id, "couldnt", `{"reason":"tired"}`, "couldnt needs reason")
	noteRefused(t, id, "couldnt", `{"reason":"other"}`, `needs "detail"`)
	noteRefused(t, id, "couldnt", `{"reason":"blocked","detail":"captcha"}`, "goes with reason other only")
	note(t, id, "options", fawnOptions)
	noteRefused(t, id, "recommended", `{"option":5,"reason":"cheapest"}`, "in the 4 options shown")
	note(t, id, "chose", `{"item":"Fawn Sticker","price_minor":175,"who":{"name":"eBay","domain":"ebay.com"}}`)
	noteRefused(t, id, "chose", `{"item":"Fawn Sticker","who":{"name":"Etsy","domain":"etsy.com"}}`, "leave who out")
	note(t, id, "couldnt", `{"reason":"other","detail":"the listing vanished"}`)
	checkBodies(t, id)
}

// An opening's delegation needs the user's words, and never sits beside an
// item they named; a who or why is a later note's.
func TestTheOpeningsDelegationIsChecked(t *testing.T) {
	dealFixture(t)
	for body, want := range map[string]string{
		`"delegated":["item"]`:                                                `delegated needs "delegated_said"`,
		`"delegated":["size"],"delegated_said":"any size"`:                    "delegated may name only item",
		`"delegated":["item"],"delegated_said":"any","asked":{"item":"fawn"}`: "it is not delegated",
		`"delegated_said":"any"`:                                              "delegated_said goes with delegated",
		`"why":"cheap"`:                                                       "intent.who and intent.why go on a later intent note",
	} {
		out, err := invoke(t, "", "--profile", "deal", "deal", "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
			"intent":{"verbatim":"a fawn sticker",`+body+`},"who":{},"terms":{"currency":"USD"},"recourse":{}}`))
		require.ErrorIs(t, err, cli.ErrInput, body)
		assert.Contains(t, out+err.Error(), want, body)
	}
	out, err := invoke(t, "", "--profile", "deal", "deal", "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"a fawn sticker"},"who":{},"terms":{"currency":"USD"},"recourse":{"rail":"card"}}`))
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Contains(t, out+err.Error(), "or, at the first ask, neither")
}

// A pay at a leg with no known way to pay is refused.
func TestAPayNeedsAWayToPay(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, fawnErrand))["deal_id"].(string)
	note(t, id, "intent", `{"verbatim":"use eBay","who":{"name":"eBay","domain":"ebay.com"}}`)
	out, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":190}`))
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Contains(t, out+err.Error(), "a pay check needs the way it is paid")
}

// Calling the errand off is the user's own close, never "nothing
// delivered", and only while nothing was done.
func TestCalledOffIsItsOwnClose(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, fawnErrand))["deal_id"].(string)
	note(t, id, "options", fawnOptions)
	closed := dealRun(t, "close", "--deal", id, "--input", writeJSON(t, `{"status":"called_off"}`))
	assert.Equal(t, "called_off", closed["outcome"])
	assert.Equal(t, "user", closed["actor"])
	assert.Equal(t, "called_off", closed["reason"])
	// The profile's first receipt: it says why to keep it.
	assert.Equal(t, cli.Lines(words("say.close.called_off"), words("say.close.nothing_done"), words("say.receipt.first.keep.called_off")), closed["say"])
	moves := journeyMoves(t, id)
	assert.Equal(t, "user:called_off", moves[len(moves)-1])
	assert.Contains(t, dealRun(t, "report", "--deal", id)["trail"], "the agent showed you 4 options (of 37 found)")
	raw, err := invoke(t, "", "--profile", "deal", "deal", "list", "--json")
	require.NoError(t, err)
	assert.Contains(t, raw, `"called_off"`)
	checkBodies(t, id)

	paid := stickerDeal(t, "card", false)
	dealRun(t, "check", "--deal", paid, "--input", writeJSON(t, stickerPay))
	note(t, paid, "act", `{"action":"pay","amount_minor":454}`)
	out, err := invoke(t, "", "--profile", "deal", "deal", "close", "--deal", paid, "--input", writeJSON(t, `{"status":"called_off"}`))
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Contains(t, out+err.Error(), "called_off closes a deal where nothing was done")
	out, err = invoke(t, "", "--profile", "deal", "deal", "close", "--deal", paid, "--input", writeJSON(t, `{"status":"called_off","actor":"agent"}`))
	require.ErrorIs(t, err, cli.ErrInput)
}

// A refinement of the same place (a postal code or a region added) is the
// same place; another place is not.
func TestSamePlace(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"San Francisco", "San Francisco, CA 94131", true},
		{"San Francisco, CA 94131", "san francisco", true},
		{"San Francisco", "San Francisco, CA", true},
		{"Berlin", "Berlin 10115", true},
		{"San Francisco", "San Jose, CA", false},
		{"San Francisco", "San Francisco Airport", false},
		{"Portland, OR", "Portland, ME", false},
		{"", "San Francisco", false},
	} {
		assert.Equal(t, c.same, samePlace(c.a, c.b), "%q vs %q", c.a, c.b)
	}
}

// A ZIP added to the city the user asked for is not "Not what you asked",
// and under rule inputs 2 it is no material change.
func TestAZipAddedIsNotADifference(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"service","channel":"web",
		"intent":{"verbatim":"a cleaner in San Francisco","asked":{"place":"San Francisco"},"allowed":["commit"]},
		"who":{"name":"Clean Co","domain":"clean.example"},"terms":{"item":"cleaning","place":"San Francisco","currency":"USD"},
		"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	c := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"commit","terms":{"place":"San Francisco, CA 94131"}}`))
	assert.Equal(t, "pass", c["verdict"], c["card"])
	assert.Equal(t, float64(0), checkBodies(t, id)[0]["material_fields_changed"])
	other := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"commit","terms":{"place":"Oakland, CA"}}`))
	assert.Contains(t, rules(other), "not_asked")
}

// Tax is in the amount: the limit binds on what is charged, and a purchase
// the user named at its price stays theirs with tax on top.
func TestTheLimitBindsOnTheAmountCharged(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"the fawn sticker for $1.75, under $1.80","asked":{"item":"Fawn Sticker","price_minor":175},"max_total_minor":180,"allowed":["pay"]},
		"who":{"name":"eBay","domain":"ebay.com"},"terms":{"item":"Fawn Sticker","price_minor":175,"currency":"USD"},
		"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	c := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":190,"authorized_max_minor":190,"terms":{"item":"Fawn Sticker","price_minor":175},"recourse":{"rail":"card","refundable":true}}`))
	assert.Contains(t, rules(c), "over_limit")
	assert.Contains(t, differenceTexts(c), "You asked for this $1.90 purchase specifically", "tax on top of the price the user named")
}

// The item the agent chose under the user's delegation never pauses as its
// pick, whatever the materiality predicate (none: every pick is material);
// another item it put in the cart still does.
func TestADelegatedChoiceIsNoPauseUnderAnyPredicate(t *testing.T) {
	materialityFixture(t) // no predicate: every pick pauses
	id := dealRun(t, "open", "--input", writeJSON(t, fawnErrand))["deal_id"].(string)
	note(t, id, "intent", `{"verbatim":"use eBay","who":{"name":"eBay","domain":"ebay.com"}}`)
	note(t, id, "chose", `{"item":"Fawn Sticker","price_minor":175}`)
	pay := func(item string) map[string]any {
		return dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":190,"authorized_max_minor":190,"terms":{"item":"`+item+`","price_minor":175},"recourse":{"rail":"card","refundable":true}}`))
	}
	chosen := pay("Fawn Sticker")
	assert.NotContains(t, rules(chosen), "agent_picked")
	picked := chosen["picked_by_agent"].([]any)
	require.Len(t, picked, 1)
	assert.Equal(t, true, picked[0].(map[string]any)["delegated"])
	other := pay("Fawn vinyl decal")
	assert.Contains(t, rules(other), "agent_picked", "an item the agent did not choose is its pick")
}

// A new choice is a new item: an approval given before it covers nothing
// after it.
func TestANewChoiceVoidsAnEarlierApproval(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, fawnErrand))["deal_id"].(string)
	note(t, id, "intent", `{"verbatim":"use eBay","who":{"name":"eBay","domain":"ebay.com"}}`)
	note(t, id, "chose", `{"item":"Fawn Sticker","price_minor":175}`)
	pay := `{"action":"pay","amount_minor":190,"authorized_max_minor":190,"terms":{"item":"Fawn Sticker","price_minor":175},"recourse":{"rail":"card","refundable":true}}`
	c := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, pay))
	if c["verdict"] == "pause" {
		_, err := answer(t, id, c["check_id"].(string), "ok", shownCard(t, c["card"].(string)))
		require.NoError(t, err)
	}
	note(t, id, "chose", `{"item":"Fawn vinyl decal","price_minor":175}`)
	act := note(t, id, "act", `{"action":"pay","amount_minor":190,"payee":"eBay","rail":"card"}`)
	assert.Equal(t, true, act["unchecked"])
	assert.Equal(t, "changed_after_check", act["rule"])
}

// A pick named by its merchant alone is the option's item and price: no
// material change at the check, and the item is the user's now.
func TestAPickByMerchantIsTheOptionsItem(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, fawnErrand))["deal_id"].(string)
	note(t, id, "options", fawnOptions)
	assert.Equal(t, "picked", note(t, id, "intent", `{"verbatim":"the eBay one","who":{"name":"eBay","domain":"ebay.com"}}`)["move"])
	noteRefused(t, id, "chose", `{"item":"Fawn vinyl decal"}`, "must delegate the item")
	dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":190,"authorized_max_minor":190,"terms":{"item":"Fawn Sticker","price_minor":175},"recourse":{"rail":"card","refundable":true}}`))
	assert.Equal(t, float64(0), checkBodies(t, id)[0]["material_fields_changed"])
}

// The seller's own change of details is never relabelled the user's switch.
func TestASellerChangeCannotBecomeASwitch(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, fawnErrand))["deal_id"].(string)
	note(t, id, "intent", `{"verbatim":"use eBay","who":{"name":"eBay","domain":"ebay.com","phone":"+1 555 010 4000"}}`)
	note(t, id, "change", `{"source":"seller message","who":{"phone":"+1 555 010 9999"}}`)
	noteRefused(t, id, "intent", `{"verbatim":"pay the new number","who":{"name":"Fawn Shop","phone":"+1 555 010 9999"}}`, "names the merchant already in play")
}

// A leg that started with no way to pay takes the one the user approved.
func TestALegTakesTheApprovedWayToPay(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, fawnErrand))["deal_id"].(string)
	note(t, id, "intent", `{"verbatim":"use eBay","who":{"name":"eBay","domain":"ebay.com"}}`)
	note(t, id, "chose", `{"item":"Fawn Sticker","price_minor":175}`)
	c := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["name"],"recourse":{"rail":"card","refundable":true}}`))
	_, err := answer(t, id, c["check_id"].(string), "yes", shownCard(t, c["card"].(string)))
	require.NoError(t, err)
	pay := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":190,"authorized_max_minor":190}`))
	assert.NotEmpty(t, pay["check_id"], "the approved rail is the leg's")
}

// A typed deal carries no delegation yet.
func TestATypedDealRefusesADelegation(t *testing.T) {
	dealFixture(t)
	out, err := invoke(t, "", "--profile", "deal", "deal", "open", "--records", "typed", "--input", writeJSON(t, fawnErrand))
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Contains(t, out+err.Error(), "delegated goes on a deal in the x-deal-v0 records")
}

// "free shipping" is a default only as a shipping or delivery option: in a
// size it is still the agent's pick.
func TestOnlyAShippingOptionIsADefault(t *testing.T) {
	for a, want := range map[cli.DealAttribute]bool{
		{Field: "quantity", Value: "1"}:                        true,
		{Field: "quantity", Value: "2"}:                        false,
		{Field: "conditions.shipping", Value: "Free shipping"}: true,
		{Field: "conditions.size", Value: "XL, free shipping"}: false,
		{Field: "item", Value: "sticker with free shipping"}:   false,
	} {
		assert.Equal(t, want, isDefaultPick(a), "%+v", a)
	}
}

// The profile checker holds an approval to its leg: a disclosure after a
// switch that claims the earlier leg's approval is rejected.
func TestTheProfileCheckerVoidsApprovalsAcrossLegs(t *testing.T) {
	dealFixture(t)
	// eBay was told the address in an earlier deal: a later telling to it
	// is a repeat, sealed even with no approval covering it.
	earlier := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web","intent":{"verbatim":"a sticker","allowed":["pay","share_contact"]},
		"who":{"name":"eBay","domain":"ebay.com","email":"orders@ebay.com"},"terms":{"currency":"USD"},"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	tellAddress(t, earlier)
	id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web","intent":{"verbatim":"a fawn sticker","allowed":["pay","share_contact"]},
		"who":{"name":"AliExpress","domain":"aliexpress.com"},"terms":{"currency":"USD"},"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	share := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["address"]}`))
	_, err := answer(t, id, share["check_id"].(string), "yes", shownCard(t, share["card"].(string)))
	require.NoError(t, err)
	note(t, id, "intent", `{"verbatim":"lets change to ebay","who":{"name":"eBay","domain":"ebay.com","email":"orders@ebay.com"}}`)
	told := note(t, id, "disclosure", `{"fields":[{"class":"address","value":"`+homeStreet+`"}]}`)
	assert.Equal(t, false, told["approved"])
	assert.Equal(t, "details changed after the last check", told["reason"])
	records, export := exportRecords(t, id)
	checkProfile(t, export)
	var approval string
	for _, r := range records {
		if b := r["x-deal-v0"].(map[string]any); b["record_type"] == "approval" {
			approval = recordDigest(t, r)
		}
	}
	last := records[len(records)-1]
	body := last["body"].(map[string]any)
	body["authority"] = "approval"
	delete(body, "rule")
	last["x-deal-v0"].(map[string]any)["refs"] = []any{map[string]any{"rel": "authorized_by", "type": "deal-record", "digest_alg": "SHA-256", "digest": approval}}
	checkProfileRejects(t, records, "details changed after the check")
}

// A step sealed before journey records or rule inputs "2" re-derives as it
// was: an intent with no move and its asked terms whole, a check with the
// version 1 count (a ZIP added to the place counted, as it was).
func TestStepsSealedBeforeJourneyReDeriveAsTheyWere(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"service","channel":"web",
		"intent":{"verbatim":"a cleaner in San Francisco","asked":{"place":"San Francisco"},"max_total_minor":9000,"allowed":["commit"]},
		"who":{"name":"Clean Co","domain":"clean.example"},"terms":{"item":"cleaning","place":"San Francisco","currency":"USD"},
		"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	note(t, id, "intent", `{"verbatim":"under $80","max_total_minor":8000}`)
	dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"commit","terms":{"place":"San Francisco, CA 94131"}}`))
	p, err := cli.LoadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	require.NoError(t, s.useDeal(t.Context(), id, false))
	events, err := s.load(t.Context(), id)
	require.NoError(t, err)
	for i, se := range events {
		ev := se.Event
		switch ev.Kind {
		case "intent":
			rec, err := buildDealRecord(ev, events[:i], s.dkey)
			require.NoError(t, err)
			assert.Equal(t, "narrowed", rec["body"].(map[string]interface{})["move"])
			ev.Journey = ""
			rec, err = buildDealRecord(ev, events[:i], s.dkey)
			require.NoError(t, err)
			assert.NotContains(t, rec["body"], "move", "an intent sealed before journey records has none")
		case "snapshot":
			rec, err := buildDealRecord(ev, events[:i], s.dkey)
			require.NoError(t, err)
			assert.EqualValues(t, 0, rec["body"].(map[string]interface{})["material_fields_changed"])
			ev.RuleInputs = "1"
			rec, err = buildDealRecord(ev, events[:i], s.dkey)
			require.NoError(t, err)
			assert.EqualValues(t, 1, rec["body"].(map[string]interface{})["material_fields_changed"], "version 1 counted the refined place")
		}
	}
}

// A pick named by its merchant is the user's pick of that option's item:
// under a predicate that holds the item material, it is not the agent's.
func TestAPickByMerchantIsTheUsersItem(t *testing.T) {
	materialityFixture(t)
	pin(t, writePredicate(t, `{"type":"materiality-predicate/v0","name":"items","version":"1","material":[{"field":"item"}]}`))
	id := dealRun(t, "open", "--input", writeJSON(t, fawnErrand))["deal_id"].(string)
	note(t, id, "options", fawnOptions)
	note(t, id, "intent", `{"verbatim":"the eBay one","who":{"name":"eBay","domain":"ebay.com"}}`)
	c := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":190,"authorized_max_minor":190,"terms":{"item":"Fawn Sticker","price_minor":175},"recourse":{"rail":"card","refundable":true}}`))
	assert.NotContains(t, rules(c), "agent_picked")
	other := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":190,"authorized_max_minor":190,"terms":{"item":"Fawn vinyl decal","price_minor":175},"recourse":{"rail":"card","refundable":true}}`))
	assert.Contains(t, rules(other), "not_asked", "another item than the one picked")
}

// In typed records, a pay approved with the way to pay a leg started
// without is covered by that approval: the approval does not move the
// check's own scope.
func TestATypedApprovalOfALegsWayToPayCoversThePay(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"get me a fawn sticker under $2","max_total_minor":200,"allowed":["pay"]},
		"who":{},"terms":{"currency":"USD"},"recourse":{}}`))["deal_id"].(string)
	note(t, id, "intent", `{"verbatim":"the eBay one","asked":{"item":"Fawn Sticker"},"who":{"name":"eBay","domain":"ebay.com"}}`)
	// Over the limit, so it pauses and the user's approval is folded.
	c := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":250,"authorized_max_minor":250,"terms":{"item":"Fawn Sticker","price_minor":235},"recourse":{"rail":"card","refundable":true}}`))
	require.Equal(t, "pause", c["verdict"], c["card"])
	_, err := answer(t, id, c["check_id"].(string), "yes, this once", shownCard(t, c["card"].(string)))
	require.NoError(t, err)
	act := note(t, id, "act", `{"action":"pay","amount_minor":250,"payee":"eBay","rail":"card"}`)
	assert.Equal(t, false, act["unchecked"], act["reason"])
}

// An intent naming another item after a check voids its approval, as the
// agent's new choice does; the profile checker holds the same.
func TestANewPickVoidsAnEarlierApproval(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, fawnErrand))["deal_id"].(string)
	note(t, id, "intent", `{"verbatim":"use eBay","who":{"name":"eBay","domain":"ebay.com"}}`)
	note(t, id, "chose", `{"item":"Fawn Sticker","price_minor":175}`)
	pay := `{"action":"pay","amount_minor":190,"authorized_max_minor":190,"terms":{"item":"Fawn Sticker","price_minor":175},"recourse":{"rail":"card","refundable":true}}`
	c := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, pay))
	require.Equal(t, "pass", c["verdict"], c["card"])
	note(t, id, "intent", `{"verbatim":"actually the decal","asked":{"item":"Fawn vinyl decal"}}`)
	act := note(t, id, "act", `{"action":"pay","amount_minor":190,"payee":"eBay","rail":"card"}`)
	assert.Equal(t, true, act["unchecked"])
	assert.Equal(t, "changed_after_check", act["rule"])

	// The checker: a chose after the approval, with the act claiming it.
	dealFixture(t)
	id = dealRun(t, "open", "--input", writeJSON(t, fawnErrand))["deal_id"].(string)
	note(t, id, "intent", `{"verbatim":"use eBay","who":{"name":"eBay","domain":"ebay.com","email":"orders@ebay.com"}}`)
	note(t, id, "chose", `{"item":"Fawn Sticker","price_minor":175}`)
	share := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["name"]}`))
	_, err := answer(t, id, share["check_id"].(string), "yes", shownCard(t, share["card"].(string)))
	require.NoError(t, err)
	note(t, id, "chose", `{"item":"Fawn vinyl decal","price_minor":175}`)
	held := heldNote(t, id, `{"fields":[{"class":"name","value":"`+homeName+`"}]}`)
	assert.Equal(t, []any{"name"}, held["held"], "the approval no longer names the class")
}

// The profile checker holds an approval to the pick it was given on: a
// disclosure after the agent's new choice that claims it is rejected.
func TestTheProfileCheckerVoidsApprovalsAcrossPicks(t *testing.T) {
	dealFixture(t)
	earlier := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web","intent":{"verbatim":"a sticker","allowed":["pay","share_contact"]},
		"who":{"name":"eBay","domain":"ebay.com","email":"orders@ebay.com"},"terms":{"currency":"USD"},"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	tellAddress(t, earlier)
	id := dealRun(t, "open", "--input", writeJSON(t, fawnErrand))["deal_id"].(string)
	note(t, id, "chose", `{"item":"Fawn Sticker","price_minor":175,"who":{"name":"eBay","domain":"ebay.com","email":"orders@ebay.com"}}`)
	share := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["address"]}`))
	require.Equal(t, "pass", share["verdict"], "told before")
	note(t, id, "chose", `{"item":"Fawn vinyl decal","price_minor":175}`)
	told := note(t, id, "disclosure", `{"fields":[{"class":"address","value":"`+homeStreet+`"}]}`)
	assert.Equal(t, false, told["approved"])
	records, export := exportRecords(t, id)
	checkProfile(t, export)
	var approval string
	for _, r := range records {
		if b := r["x-deal-v0"].(map[string]any); b["record_type"] == "approval" {
			approval = recordDigest(t, r)
		}
	}
	require.NotEmpty(t, approval, "a pass is answered by standing intent")
	last := records[len(records)-1]
	body := last["body"].(map[string]any)
	body["authority"] = "approval"
	delete(body, "rule")
	last["x-deal-v0"].(map[string]any)["refs"] = []any{map[string]any{"rel": "authorized_by", "type": "deal-record", "digest_alg": "SHA-256", "digest": approval}}
	checkProfileRejects(t, records, "details changed after the check")
}

// deal list counts a class told to two merchants of one errand as told to
// two recipients.
func TestDealListCountsEachLegsMerchant(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, fawnErrand))["deal_id"].(string)
	tellEmail := func() {
		c := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["email"]}`))
		_, err := answer(t, id, c["check_id"].(string), "yes", shownCard(t, c["card"].(string)))
		require.NoError(t, err)
		require.Equal(t, true, note(t, id, "disclosure", `{"fields":[{"class":"email","value":"`+homeEmail+`"}]}`)["approved"])
	}
	note(t, id, "intent", `{"verbatim":"try AliExpress","who":{"name":"AliExpress","email":"orders@aliexpress.example"}}`)
	tellEmail()
	note(t, id, "intent", `{"verbatim":"lets change to ebay","who":{"name":"eBay","email":"orders@ebay.example"}}`)
	tellEmail()
	r, _ := listRange(t, "--since", "2000-01-01")
	for _, row := range r.Told {
		if row.Class == "email" {
			assert.Equal(t, 2, row.Recipients)
			assert.Equal(t, 2, row.ToSellers)
		}
	}
}

// A pay noted with no amount still reads Paid; a telling names its
// recipient as the other labels do, by name.
func TestJourneyLabelsWithoutAnAmountAndByName(t *testing.T) {
	events := []sealedEvent{
		{CapsuleID: "c1", Event: dealEvent{Kind: "open", Open: &dealOpen{Who: dealWho{Name: "AliExpress", Payee: "PAYPAL *ALIEXPRESS"}, Terms: dealTerms{Currency: "USD"}}}},
		{CapsuleID: "c2", Event: dealEvent{Kind: "disclosure", Disclosure: &dealDisclosure{To: "counterparty", Fields: []dealDisclosureField{{Class: "email", Value: homeEmail}}}}},
		{CapsuleID: "c3", Event: dealEvent{Kind: "act", Act: &dealAct{Action: "pay"}}},
		{CapsuleID: "c4", Event: dealEvent{Kind: "disclosure", Disclosure: &dealDisclosure{To: "platform", Who: &dealWho{Name: "Craigslist", Payee: "CL RELAY LLC", Domain: "craigslist.org"}, Fields: []dealDisclosureField{{Class: "email", Value: homeEmail}}}}},
	}
	steps := dealJourney(events, cli.BuiltinWording())
	require.Len(t, steps, 4)
	assert.Equal(t, "Told AliExpress your email", steps[1].Label)
	assert.Equal(t, "Paid", steps[2].Label)
	assert.Equal(t, "Told Craigslist your email", steps[3].Label, "a named recipient by its own name")
}

// The user's own answer to a pause is their move (capsule-cli#410): an OK
// after the pause it answered and before the act it allowed, a no as a no.
// A pause with no sealed answer of the user's has no such row, and an
// agent's card answer is not the user's.
func TestTheUsersAnswerIsTheirMove(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, fawnErrand))["deal_id"].(string)
	note(t, id, "intent", `{"verbatim":"lets change to ebay","who":{"name":"eBay","domain":"ebay.com"}}`)
	note(t, id, "chose", `{"item":"Fawn Sticker","price_minor":175}`)
	share := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["name","address"]}`))
	require.Equal(t, "pause", share["verdict"], share["card"])
	assert.NotContains(t, journeyMoves(t, id), "user:approved", "a pause with no answer yet has no OK")
	_, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", id, "--kind", "approval", "--check", share["check_id"].(string),
		"--choice", "hold", "--said", "no, not my address", "--shown-card", shownCard(t, share["card"].(string)))
	require.NoError(t, err)
	var labels []string
	for _, s := range dealRun(t, "report", "--deal", id)["journey"].([]any) {
		m := s.(map[string]any)
		labels = append(labels, m["by"].(string)+":"+m["label"].(string))
	}
	assert.Contains(t, labels, "user:Said no to sharing your name and address")
	assert.NotContains(t, labels, "user:OK'd sharing your name and address")

	// A click on the agent's card carries no words of the user's: no move.
	pay := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":190,"authorized_max_minor":190,"terms":{"item":"Fawn Sticker","price_minor":175},"recourse":{"rail":"card","refundable":true}}`))
	require.Equal(t, "pause", pay["verdict"], pay["card"])
	dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", pay["check_id"].(string), "--choice", "proceed")
	moves := journeyMoves(t, id)
	assert.Equal(t, 1, strings.Count(strings.Join(moves, " "), "user:declined"), moves)
	assert.NotContains(t, moves, "user:approved", "a card click is not the user's OK")

	// The page words the no as the report does.
	var want []any
	for _, s := range dealRun(t, "report", "--deal", id)["journey"].([]any) {
		want = append(want, s.(map[string]any)["label"])
	}
	assert.Equal(t, want, renderJourney(t, id, false)["labels"])
}

// An answer to an offer's or a commitment's check is worded by its amount,
// in the card's currency; an answer not applied, or another choice, is no
// move.
func TestTheUsersAnswerToAnOfferNamesItsAmount(t *testing.T) {
	amount := int64(190000)
	events := []sealedEvent{
		{CapsuleID: "open", Event: dealEvent{Kind: "open", Open: &dealOpen{Terms: dealTerms{Currency: "USD"}}}},
		{CapsuleID: "snap", Event: dealEvent{Kind: "snapshot", Snapshot: &dealSnapshot{Action: "offer", AmountMinor: &amount}}},
		{CapsuleID: "check", Event: dealEvent{Kind: "check", Check: &dealCheckResult{Action: "offer", Snapshot: "snap", Verdict: "pause"}}},
		{CapsuleID: "ok", Event: dealEvent{Kind: "approval", Approval: &dealApproval{Check: "check", Approver: "user", Proceed: true}}},
		{CapsuleID: "snap2", Event: dealEvent{Kind: "snapshot", Snapshot: &dealSnapshot{Action: "commit", AmountMinor: &amount}}},
		{CapsuleID: "check2", Event: dealEvent{Kind: "check", Check: &dealCheckResult{Action: "commit", Snapshot: "snap2", Verdict: "pause"}}},
		{CapsuleID: "no", Event: dealEvent{Kind: "approval", Approval: &dealApproval{Check: "check2", Approver: "user", Choice: "hold"}}},
		// An answer not applied (the check was answered already) and a
		// choice that is neither an OK nor a hold are no move.
		{CapsuleID: "again", Event: dealEvent{Kind: "approval", Approval: &dealApproval{Check: "check2", Approver: "user", Proceed: true, Reason: "answered"}}},
		{CapsuleID: "verify", Event: dealEvent{Kind: "approval", Approval: &dealApproval{Check: "check2", Approver: "user", Choice: "verify_contact"}}},
		// The amount is in the card's currency, the snapshot's own.
		{CapsuleID: "snap3", Event: dealEvent{Kind: "snapshot", Snapshot: &dealSnapshot{Action: "pay", AmountMinor: &amount, Terms: &dealTerms{Currency: "EUR"}}}},
		{CapsuleID: "check3", Event: dealEvent{Kind: "check", Check: &dealCheckResult{Action: "pay", Snapshot: "snap3", Verdict: "pause"}}},
		{CapsuleID: "eur", Event: dealEvent{Kind: "approval", Approval: &dealApproval{Check: "check3", Approver: "user", Proceed: true}}},
		// A check of anything else is worded without its details.
		{CapsuleID: "snap4", Event: dealEvent{Kind: "snapshot", Snapshot: &dealSnapshot{Action: "sign"}}},
		{CapsuleID: "check4", Event: dealEvent{Kind: "check", Check: &dealCheckResult{Action: "sign", Snapshot: "snap4", Verdict: "pause"}}},
		{CapsuleID: "signed", Event: dealEvent{Kind: "approval", Approval: &dealApproval{Check: "check4", Approver: "user", Choice: "hold"}}},
	}
	var labels []string
	for _, s := range dealJourney(events, cli.BuiltinWording()) {
		labels = append(labels, s.By+": "+s.Label)
	}
	assert.Equal(t, []string{"user: Asked", "agent: Waited for your OK", "user: OK'd offering $1,900", "agent: Waited for your OK", "user: Said no to agreeing to $1,900",
		"agent: Waited for your OK", "user: OK'd paying €1,900", "agent: Waited for your OK", "user: Said no"}, labels)
}

// A tapped button's label is the user's answer (capsule-cli#416): passed as
// --said, the approval is sealed as the user's, and it is their OK row. Left
// out, the same tap is sealed as the card's and is no row.
func TestATappedLabelIsTheUsersAnswer(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, fawnErrand))["deal_id"].(string)
	note(t, id, "intent", `{"verbatim":"lets change to ebay","who":{"name":"eBay","domain":"ebay.com"}}`)
	share := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["name","address"]}`))
	require.Equal(t, "pause", share["verdict"], share["card"])
	dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", share["check_id"].(string),
		"--choice", "proceed", "--said", "Share anyway", "--shown-card", shownCard(t, share["card"].(string)))
	var approvers []any
	for _, r := range checkApprovals(t, id) {
		approvers = append(approvers, r["approver"])
	}
	assert.Equal(t, []any{"user"}, approvers, "a tapped label is the user's answer")
	assert.Contains(t, journeyMoves(t, id), "user:approved")
}

// checkApprovals are the deal's sealed approval record bodies, in order.
func checkApprovals(t *testing.T, id string) []map[string]any {
	t.Helper()
	records, _ := exportRecords(t, id)
	var out []map[string]any
	for _, r := range records {
		if b, ok := r["x-deal-v0"].(map[string]any); ok && b["record_type"] == "approval" {
			out = append(out, r["body"].(map[string]any))
		}
	}
	return out
}
