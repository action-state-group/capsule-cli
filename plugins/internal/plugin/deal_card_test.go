package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func diffs(rules ...string) []dealDifference {
	out := make([]dealDifference, len(rules))
	for i, r := range rules {
		out[i] = dealDifference{Rule: r}
	}
	return out
}

// The collapse table, row by row: differences that say the same thing are
// one reason, the higher row wins, at most two are said, and a rules
// escalation is not said again when a row above already says why.
func TestTheCardCollapsesReasons(t *testing.T) {
	w := cli.BuiltinWording()
	pay := dealSnapshot{Action: "pay"}
	share := dealSnapshot{Action: "share_contact"}
	firstTime := &dealRecipient{Name: "shop", FirstTime: true}
	for _, tc := range []struct {
		name string
		r    dealCheckResult
		snap dealSnapshot
		want []string
	}{
		{"r02 and r06 to someone first dealt with: the rules asked, beside the first dealing", dealCheckResult{Differences: diffs("rules_escalate", "rules_escalate"), Recipient: firstTime}, pay, []string{"your rules ask for your OK on this", "first time dealing with this seller"}},
		{"a first dealing alone is no reason", dealCheckResult{Differences: diffs("terms_changed"), Recipient: firstTime}, pay, []string{"the terms changed since you agreed"}},
		{"every first telling is one reason", dealCheckResult{Differences: diffs("first_disclosure", "first_disclosure", "first_disclosure", "first_disclosure", "rules_escalate", "rules_escalate"), Recipient: firstTime}, share, []string{"first time they get your details"}},
		{"over the limit", dealCheckResult{Differences: diffs("over_limit")}, pay, []string{"it's over your $2 limit"}},
		{"payee changed comes first", dealCheckResult{Differences: diffs("first_disclosure", "payee_or_contact_changed", "over_limit")}, pay, []string{"the payee or contact changed since you first talked", "it's over your $2 limit"}},
		{"a login and a code are one reason", dealCheckResult{Differences: diffs("verification_code_request", "credentials_requested")}, share, []string{"they asked for a login or a code"}},
		{"what you asked vs this", dealCheckResult{Differences: diffs("not_asked", "not_asked")}, pay, []string{"it's not what you asked for"}},
		{"a deny says the rules don't allow it", dealCheckResult{Differences: diffs("rules_deny", "rules_deny", "first_disclosure")}, share, []string{"your rules don't allow this", "first time they get your details"}},
		{"a rules escalation alone speaks", dealCheckResult{Differences: diffs("rules_escalate")}, pay, []string{"your rules ask for your OK on this"}},
		{"rules not checked", dealCheckResult{Differences: diffs("rules_not_checked")}, pay, []string{"I couldn't check this against your rules"}},
		{"a first dealing never stands in for the rules", dealCheckResult{Differences: diffs("terms_changed", "rules_escalate"), Recipient: firstTime}, pay, []string{"the terms changed since you agreed"}},
		{"at most two", dealCheckResult{Differences: diffs("payee_or_contact_changed", "pay_before_seeing", "over_limit", "not_asked")}, pay, []string{"the payee or contact changed since you first talked", "you haven't seen the item yet"}},
		{"a changed way back", dealCheckResult{Differences: diffs("recourse_changed")}, pay, []string{"the way to get your money back changed"}},
		{"a rail with no way back, as agreed", dealCheckResult{Differences: diffs("irreversible_rail")}, pay, []string{"money paid this way can't be pulled back"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, cardWhy(w, tc.r, tc.snap, cardFacts{limit: "$2"}))
		})
	}
	// Every row says something in the built-in pack.
	for _, row := range cardReasons {
		assert.NotEmpty(t, w.Line(row.key, cli.V{"limit": "$2", "floor": "$1", "what": "size small", "days": "31"}), row.key)
	}
	// A purchase the user named, over their limit, is asked as an exception.
	assert.Equal(t, []string{"you asked for this one, but it's over your $2 limit"}, cardWhy(w, dealCheckResult{Differences: diffs("over_limit")}, pay, cardFacts{limit: "$2", askedOver: true}))
	// A remote reader's own rules are its reason.
	assert.Equal(t, []string{"an outside check asked to pause"}, cardWhy(w, dealCheckResult{Differences: []dealDifference{{Question: "remote", Rule: "extra"}}}, pay, cardFacts{}))
}

func TestTheCardTagNamesWhatKindOfStopItIs(t *testing.T) {
	w := cli.BuiltinWording()
	assert.Equal(t, "PAUSED FOR YOU", cardTag(w, dealCheckResult{Verdict: "pause", Differences: diffs("first_disclosure")}, false))
	assert.Equal(t, "NOT ALLOWED BY YOUR RULES", cardTag(w, dealCheckResult{Verdict: "deny", Differences: diffs("rules_deny", "payee_or_contact_changed")}, false))
	for _, rule := range []string{"payee_or_contact_changed", "credentials_requested", "verification_code_request", "pay_before_seeing"} {
		assert.Equal(t, "STOP: CHECK THIS", cardTag(w, dealCheckResult{Verdict: "pause", Differences: diffs(rule)}, false), rule)
	}
	assert.Equal(t, "DEMO · PAUSED FOR YOU", cardTag(w, dealCheckResult{Verdict: "pause"}, true))
}

// A default is never shown as the agent's pick.
func TestDefaultsAreNotPicks(t *testing.T) {
	w := cli.BuiltinWording()
	r := dealCheckResult{Picked: []cli.DealAttribute{{Field: "quantity", Label: "quantity", Value: "1"}, {Field: "conditions.shipping", Label: "shipping", Value: "Free shipping"}}}
	assert.Empty(t, cardGoodToKnow(w, r))
	r.Picked = append(r.Picked, cli.DealAttribute{Field: "item", Label: "item", Value: "Fawn Sticker"})
	assert.Equal(t, "I chose item Fawn Sticker for you.", cardGoodToKnow(w, r))
	assert.Equal(t, "Listed at $3.00 (not checked).", cardGoodToKnow(w, dealCheckResult{Unverified: []string{"Listed at $3.00"}}))
}

// cardOf is a check's card as the user reads it, then its buttons.
func cardOf(check map[string]any) string {
	var labels []string
	if check["verdict"] != "pass" && check["card"] != check["approval_text"] {
		panic("the card sealed is not the card shown")
	}
	for _, o := range check["options"].([]any) {
		labels = append(labels, "<"+o.(map[string]any)["label"].(string)+">")
	}
	return check["card"].(string) + "\n" + strings.Join(labels, " ")
}

// The cards of the 2026-10-20 purchase (a synthetic seller, the same
// shape), and the deny, over-limit and payee-changed cards, as goldens.
func TestTheCardGoldens(t *testing.T) {
	cards := map[string]string{}
	everyday := func(t *testing.T, verdict, findings string) {
		t.Helper()
		pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules", checkerPrints(verdict, findings))}})
	}
	open := func(t *testing.T, body string) string {
		t.Helper()
		dealFixture(t)
		return dealRun(t, "open", "--input", writeJSON(t, body))["deal_id"].(string)
	}

	t.Run("share", func(t *testing.T) {
		id := open(t, fawnOpen)
		everyday(t, "escalate", `{"id":"r11","check":"first-disclosure/1","verdict":"fail"},{"id":"r12","check":"first-disclosure/1","verdict":"fail"}`)
		check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, fawnShare))
		require.Equal(t, "pause", check["verdict"])
		cards["share"] = cardOf(check)
	})
	t.Run("pay", func(t *testing.T) {
		id := open(t, fawnOpen)
		everyday(t, "allow", passFinding)
		share := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, fawnShare))
		_, err := answer(t, id, share["check_id"].(string), "yes", shownCard(t, share["card"].(string)))
		require.NoError(t, err)
		dealRun(t, "note", "--deal", id, "--kind", "disclosure", "--input", writeJSON(t,
			`{"channel":"web","fields":[{"class":"name","value":"A. Buyer"},{"class":"email","value":"buyer@example.com"},{"class":"address","value":"1 Example St"},{"class":"phone","value":"555-0100"}]}`))
		everyday(t, "escalate", `{"id":"r02","check":"first-payee/1","verdict":"fail"},{"id":"r06","check":"new-merchant/1","verdict":"fail"}`)
		check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, fawnPay))
		require.Equal(t, "pause", check["verdict"])
		cards["pay"] = cardOf(check)
	})
	t.Run("deny", func(t *testing.T) {
		id := open(t, fawnOpen)
		everyday(t, "deny", `{"id":"r03","check":"personal-data/1","verdict":"fail"}`)
		check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"share_contact","disclosing_to":"other","recipient":{"name":"Classifieds Example","domain":"classifieds.example"},"disclosing":["address"]}`))
		require.Equal(t, "deny", check["verdict"])
		cards["deny"] = cardOf(check)
	})
	t.Run("over limit", func(t *testing.T) {
		id := open(t, fawnOpen)
		everyday(t, "allow", passFinding)
		check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":250,"authorized_max_minor":250,"terms":{"item":"Fawn Sticker","price_minor":175},"recourse":{"rail":"card","refundable":true}}`))
		require.Equal(t, "pause", check["verdict"])
		cards["over-limit"] = cardOf(check)
	})
	t.Run("payee changed", func(t *testing.T) {
		id := open(t, strings.Replace(fawnOpen, `"who":{"name":"sticker_shop_example","domain":"marketplace.example"}`,
			`"who":{"name":"sticker_shop_example","domain":"marketplace.example","phone":"+1 555 0100"}`, 1))
		everyday(t, "allow", passFinding)
		check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":190,"authorized_max_minor":190,"who":{"payee":"sticker-payouts-2","phone":"+1 555 0199"},"terms":{"item":"Fawn Sticker","price_minor":175},"recourse":{"rail":"card","refundable":true}}`))
		require.Equal(t, "pause", check["verdict"])
		text := cardOf(check)
		assert.Contains(t, text, "<Call +1 555 0100 first>", "the number from first contact, never the changed one")
		assert.NotContains(t, text, "0199")
		cards["payee-changed"] = text
	})

	t.Run("demo stop", func(t *testing.T) {
		id := open(t, strings.Replace(strings.Replace(fawnOpen, `"type":"purchase",`, `"type":"purchase","demo":true,`, 1), `"who":{"name":"sticker_shop_example","domain":"marketplace.example"}`,
			`"who":{"name":"sticker_shop_example","domain":"marketplace.example","phone":"+1 555 0100"}`, 1))
		check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":190,"authorized_max_minor":190,"who":{"payee":"sticker-payouts-2"},"terms":{"item":"Fawn Sticker","price_minor":175},"recourse":{"rail":"card","refundable":true}}`))
		require.Equal(t, "pause", check["verdict"])
		assert.True(t, strings.HasPrefix(check["card"].(string), "DEMO · STOP: CHECK THIS\n"))
		cards["demo-stop"] = cardOf(check)
	})

	var got strings.Builder
	for _, name := range []string{"share", "pay", "deny", "over-limit", "payee-changed", "demo-stop"} {
		got.WriteString("== " + name + "\n" + cards[name] + "\n")
		for _, never := range []string{"r02", "r03", "r06", "r11", "r12", "selector", "digest", "not measured", "quantity 1", "deal-", "UTC"} {
			assert.NotContains(t, cards[name], never, name)
		}
	}
	golden := filepath.Join("testdata", "narration", "cards.txt")
	if os.Getenv("UPDATE_NARRATION_GOLDEN") != "" {
		require.NoError(t, os.WriteFile(golden, []byte(got.String()), 0o644))
	}
	want, err := os.ReadFile(golden)
	require.NoError(t, err)
	assert.Equal(t, string(want), got.String())
}

// A pay after a share card that promised to ask again pauses for that
// promise, and the user's answer is sealed: the receipt counts it
// (Sherlock2's review of #351).
func TestAPromisedPaymentIsAskedAndSealed(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
	share := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, fawnShare))
	require.Equal(t, "pause", share["verdict"])
	assert.Contains(t, share["card"], "Next: I'll ask you again before paying.")
	_, err := answer(t, id, share["check_id"].(string), "yes", shownCard(t, share["card"].(string)))
	require.NoError(t, err)
	pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules-allow", checkerPrints("allow", passFinding))}})
	pay := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, fawnPay))
	require.Equal(t, "pause", pay["verdict"], "the promise is kept by a pause")
	assert.Contains(t, pay["card"], "Why I'm asking: I said I'd check with you before paying.")
	assert.Equal(t, []string{"Hold", "Pay $1.90"}, optionLabels(pay))
	assertPlainCard(t, pay)
	_, err = answer(t, id, pay["check_id"].(string), "ok pay", shownCard(t, pay["card"].(string)))
	require.NoError(t, err)
	// A re-check before paying (a check gone stale) does not ask again: the
	// promise was kept by the yes.
	recheck := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, fawnPay))
	assert.Equal(t, "pass", recheck["verdict"])
	act := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":190,"rail":"card"}`))
	assert.Contains(t, act["say"], "You OK'd 2 things on the way: sharing your details and paying $1.90.")
	raw, err := json.Marshal(dealRun(t, "report", "--deal", id)["anomalies"])
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "asked_vs_did", "a promise kept is no anomaly")
	again := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, fawnPay))
	assert.Equal(t, "pass", again["verdict"], "once paid, the promise is kept")
	assert.Equal(t, cli.VoiceSilent, again["voice"])
}

// A rules escalation on a first-time pay says the rules asked, and the
// first dealing beside it as a fact: capsulectl never says which rule asked.
func TestAnEscalationOnAFirstPayKeepsItsCause(t *testing.T) {
	w := cli.BuiltinWording()
	r := dealCheckResult{Differences: []dealDifference{{Rule: "rules_escalate", Field: "per_purchase"}}, Recipient: &dealRecipient{FirstTime: true}}
	assert.Equal(t, []string{"your rules ask for your OK on this", "first time dealing with this seller"}, cardWhy(w, r, dealSnapshot{Action: "pay"}, cardFacts{}))
}

// A deny says what will not happen and why it stopped; its one button
// acknowledges it.
func TestADenySaysItWillNotAndWhy(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
	pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules-deny", checkerPrints("deny", `{"id":"r03","check":"personal-data/1","verdict":"fail"}`))}})
	denied := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, fawnShare))
	require.Equal(t, "deny", denied["verdict"])
	assert.Equal(t, "NOT ALLOWED BY YOUR RULES\n"+
		"I won't share your name, email, phone and address with sticker_shop_example (marketplace.example).\n"+
		"Why I stopped: your rules don't allow this; first time they get your details.", denied["card"])
	assert.Equal(t, []string{"OK"}, optionLabels(denied))
}

// Every card, as the user reads it, against what a card never holds.
func assertPlainCard(t *testing.T, check map[string]any) {
	t.Helper()
	if check["verdict"] == "pass" {
		assert.Equal(t, "", check["card"], "a pass seals no card")
		return
	}
	assert.Equal(t, check["approval_text"], check["card"], "the card sealed is the card shown")
	assert.Equal(t, check["card"], check["say"], "the card is what is said")
	for _, never := range []string{"[Hold]", "--", "profile update", "digest", "selector", "not measured", "UTC", "deal-", "•"} {
		assert.NotContains(t, check["card"], never)
	}
	assert.NotRegexp(t, `\b[rR][0-9]{2}\b`, check["card"], "no rule ids")
}

// The facts line says over exactly when the check found it over.
func TestTheFactsAndTheReasonAgreeOnTheLimit(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
	// The price is over the limit though the charge is not: the check
	// binds the price.
	over := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":190,"authorized_max_minor":190,"terms":{"item":"Fawn Sticker","price_minor":250},"recourse":{"rail":"card","refundable":true}}`))
	require.Equal(t, "pause", over["verdict"])
	assert.Contains(t, over["card"], "over your $2")
	assert.Contains(t, over["card"], "it's over your $2 limit")
	assert.NotContains(t, over["card"], "within your")
	assertPlainCard(t, over)
}

// A purchase the user named, at its price, over the limit, is an exception
// on the card exactly when the check sealed it as one.
func TestTheExceptionFollowsTheCheck(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"buy the Otterly Chaos poster for $60","asked":{"item":"Otterly Chaos poster","price_minor":6000},"max_total_minor":2500,"allowed":["pay"]},
		"who":{"name":"Sticker Marketplace","domain":"stickers.example"},
		"terms":{"item":"Otterly Chaos poster","price_minor":6000,"currency":"USD"},
		"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	asked := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":6000,"authorized_max_minor":6000,"recourse":{"rail":"card","refundable":true}}`))
	assert.Contains(t, asked["card"], "you asked for this one, but it's over your $25 limit")
	// A larger authorized maximum is not the price the user named: plain
	// over the limit, as the sealed difference says.
	held := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":6000,"authorized_max_minor":7000,"recourse":{"rail":"card","refundable":true}}`))
	assert.Contains(t, held["card"], "it's over your $25 limit")
	assert.NotContains(t, held["card"], "you asked for this one")
}

// A recipient already dealt with is told what they get for the first time,
// never that it is their first time.
func TestAKnownRecipientIsToldWhatIsNew(t *testing.T) {
	dealFixture(t)
	first := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
	share := dealRun(t, "check", "--deal", first, "--input", writeJSON(t, `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["email"]}`))
	_, err := answer(t, first, share["check_id"].(string), "yes", shownCard(t, share["card"].(string)))
	require.NoError(t, err)
	dealRun(t, "note", "--deal", first, "--kind", "disclosure", "--input", writeJSON(t, `{"channel":"web","fields":[{"class":"email","value":"buyer@example.com"}]}`))
	second := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
	again := dealRun(t, "check", "--deal", second, "--input", writeJSON(t, `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["email","address"]}`))
	require.Equal(t, "pause", again["verdict"])
	assert.Contains(t, again["card"], "Why I'm asking: first time they get your address.")
	assertPlainCard(t, again)
}

// No promise was made by a denied share, or by one in a deal that could
// not pay then: the pay check that passes stays silent.
func TestOnlyAPromiseMadeIsKept(t *testing.T) {
	t.Run("a denied share", func(t *testing.T) {
		dealFixture(t)
		id := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
		pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules-deny", checkerPrints("deny", `{"id":"r03","check":"personal-data/1","verdict":"fail"}`))}})
		denied := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, fawnShare))
		require.Equal(t, "deny", denied["verdict"])
		assert.NotContains(t, denied["card"], "Next:")
		assertPlainCard(t, denied)
		pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules-allow", checkerPrints("allow", passFinding))}})
		pay := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, fawnPay))
		require.Equal(t, "pass", pay["verdict"])
		assert.Equal(t, cli.VoiceSilent, pay["voice"])
	})
	t.Run("pay allowed only later", func(t *testing.T) {
		dealFixture(t)
		id := dealRun(t, "open", "--input", writeJSON(t, strings.Replace(fawnOpen, `"allowed":["pay","share_contact"]`, `"allowed":["share_contact"]`, 1)))["deal_id"].(string)
		share := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, fawnShare))
		require.Equal(t, "pause", share["verdict"])
		assert.NotContains(t, share["card"], "Next:")
		raise := dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"you can pay now","max_total_minor":200,"allowed":["pay","share_contact"]}`))
		dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", raise["capsule_id"].(string), "--choice", "confirm_limits", "--said", "yes you can pay")
		pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules-allow", checkerPrints("allow", passFinding))}})
		pay := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, fawnPay))
		require.Equal(t, "pass", pay["verdict"])
		assert.Equal(t, cli.VoiceSilent, pay["voice"])
	})
}

// The skill's card rules (Steven's rulings, 2026-10-11, kept for the new
// card): the card shown as returned, once; one lead-in sentence of the
// agent's own; options as buttons, never text; "Next" is a promise. The
// skill is public and names no product.
func TestTheDealSkillPresentsTheCardAsRuled(t *testing.T) {
	raw, err := os.ReadFile("../../../skills/deal/SKILL.md")
	require.NoError(t, err)
	text := strings.Join(strings.Fields(string(raw)), " ")
	for _, rule := range []string{
		"show it exactly as returned, once",
		"You may put one sentence of your own before it, as a lead-in. Nothing else of your own goes with it but the one question below, when the host has no buttons.",
		"Never print an option label as text",
		"Offer the check's `options` as real buttons where your agent host supports them; where it does not, end with one plain question in your own words",
		"ask the user before you pay, even when the pay check passes",
	} {
		assert.Contains(t, text, rule)
	}
	assert.Equal(t, 1, strings.Count(text, "exactly as returned"), "one show-it-verbatim rule")
	for _, product := range []string{"Muse", "Buzz", "Action State", "Checkup"} {
		assert.NotContains(t, text, product, "a public skill names no product")
	}
}

// A Hold on the promised pay is sealed like any answer, and the next pay
// check still asks: the promise stands until the user pays.
func TestAHoldOnThePromisedPayIsSealedAndAskedAgain(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
	share := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, fawnShare))
	_, err := answer(t, id, share["check_id"].(string), "yes", shownCard(t, share["card"].(string)))
	require.NoError(t, err)
	pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules-allow", checkerPrints("allow", passFinding))}})
	pay := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, fawnPay))
	require.Equal(t, "pause", pay["verdict"])
	held := dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", pay["check_id"].(string), "--choice", "hold", "--said", "not yet", "--shown-card", shownCard(t, pay["card"].(string)))
	assert.Equal(t, false, held["proceed"])
	assert.NotEmpty(t, held["capsule_id"], "the hold is sealed")
	assert.Equal(t, words("say.approval.hold"), held["say"])
	again := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, fawnPay))
	assert.Equal(t, "pause", again["verdict"], "the promise stands until the user pays")
	assert.Contains(t, again["card"], "I said I'd check with you before paying")
}
