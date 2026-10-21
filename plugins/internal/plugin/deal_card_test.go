package plugin

import (
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
		{"first payment: r02 and r06 are one reason", dealCheckResult{Differences: diffs("rules_escalate", "rules_escalate"), Recipient: firstTime}, pay, []string{"first payment to this seller"}},
		{"first time this seller gets your details: every first telling is one reason", dealCheckResult{Differences: diffs("first_disclosure", "first_disclosure", "first_disclosure", "first_disclosure", "rules_escalate", "rules_escalate")}, share, []string{"first time this seller gets your details"}},
		{"over the limit", dealCheckResult{Differences: diffs("over_limit")}, pay, []string{"it's over your $2 limit"}},
		{"payee changed comes first", dealCheckResult{Differences: diffs("first_disclosure", "payee_or_contact_changed", "over_limit")}, pay, []string{"the payee or contact changed since you first talked", "it's over your $2 limit"}},
		{"a login and a code are one reason", dealCheckResult{Differences: diffs("verification_code_request", "credentials_requested")}, share, []string{"they asked for a login or a code"}},
		{"what you asked vs this", dealCheckResult{Differences: diffs("not_asked", "not_asked")}, pay, []string{"it's not what you asked for"}},
		{"a deny says the rules don't allow it", dealCheckResult{Differences: diffs("rules_deny", "rules_deny", "first_disclosure")}, share, []string{"your rules don't allow this", "first time this seller gets your details"}},
		{"a rules escalation alone speaks", dealCheckResult{Differences: diffs("rules_escalate")}, pay, []string{"your rules ask for your OK on this"}},
		{"rules not checked", dealCheckResult{Differences: diffs("rules_not_checked")}, pay, []string{"I couldn't check this against your rules"}},
		{"at most two", dealCheckResult{Differences: diffs("payee_or_contact_changed", "pay_before_seeing", "over_limit", "not_asked")}, pay, []string{"the payee or contact changed since you first talked", "you haven't seen the item yet"}},
		{"no way back", dealCheckResult{Differences: diffs("irreversible_rail", "recourse_changed")}, pay, []string{"the way to get your money back changed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, cardWhy(w, tc.r, tc.snap, cardFacts{limit: "$2"}))
		})
	}
	// Every row says something in the built-in pack.
	for _, row := range cardReasons {
		assert.NotEmpty(t, w.Line(row.key, cli.V{"limit": "$2", "floor": "$1", "what": "size small"}), row.key)
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

	var got strings.Builder
	for _, name := range []string{"share", "pay", "deny", "over-limit", "payee-changed"} {
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

// A pay check that passes after a share card promised to ask again before
// paying still asks: the promise is kept, and nothing is sealed for it.
func TestAPromisedPaymentIsAskedEvenWhenItPasses(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
	share := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, fawnShare))
	require.Equal(t, "pause", share["verdict"])
	assert.Contains(t, share["card"], "Next: I'll ask you again before paying.")
	_, err := answer(t, id, share["check_id"].(string), "yes", shownCard(t, share["card"].(string)))
	require.NoError(t, err)
	pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules-allow", checkerPrints("allow", passFinding))}})
	pay := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, fawnPay))
	require.Equal(t, "pass", pay["verdict"])
	assert.Equal(t, "", pay["card"], "a pass seals no card")
	assert.Equal(t, cli.VoiceAsk, pay["voice"])
	ask := pay["ask"].(map[string]any)
	assert.Equal(t, []any{"you asked me to check with you before paying"}, ask["why"])
	assert.Equal(t, []any{map[string]any{"label": "Hold", "choice": "hold"}, map[string]any{"label": "Pay $1.90", "choice": "proceed"}}, ask["options"])

	dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":190,"rail":"card"}`))
	again := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, fawnPay))
	assert.Equal(t, cli.VoiceSilent, again["voice"], "once paid, the promise is kept")
}
