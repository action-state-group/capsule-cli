package deal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/action-state-group/capsule-cli/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// defenderShareCheck is the pause from a first purchase at a new merchant on
// a new profile: four details told for the first time, and a rules checker
// swapped since it was pinned, so the rules were not checked.
func defenderShareCheck(t *testing.T) map[string]any {
	t.Helper()
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"buy the Wichard shackle","allowed":["pay","share_contact"],"max_total_minor":5000},
		"who":{"name":"Defender Marine","domain":"defender.com"},
		"terms":{"item":"Wichard shackle","price_minor":4437,"currency":"USD"},
		"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	checker := stubChecker(t, "rules", checkerPrints("allow", passFinding))
	pinChecker(t, map[string]any{"command": []string{checker}})
	require.NoError(t, os.WriteFile(checker, []byte("#!/bin/sh\n"+checkerPrints("allow", passFinding)+"\n# updated\n"), 0o700))
	return dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"share_contact","disclosing_to":"counterparty",
		"description":"your details for the order","disclosing":["name","email","address","phone"]}`))
}

// otterPayCheck is a pay paused on a size the agent picked, with a reference
// price the check could not verify.
func otterPayCheck(t *testing.T) map[string]any {
	t.Helper()
	dealFixture(t)
	id := dealRun(t, "open", "--input", filepath.Join(otterFixture, "open.json"))["deal_id"].(string)
	dealRun(t, "note", "--deal", id, "--kind", "claim", "--input", filepath.Join(otterFixture, "claim-price.json"))
	dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", filepath.Join(otterFixture, "intent-picked.json"))
	return dealRun(t, "check", "--deal", id, "--input", filepath.Join(otterFixture, "check-pay.json"))
}

// passCheck is a pay the user's rules allow and the check passes, of the
// item the user named.
func passCheck(t *testing.T) map[string]any {
	t.Helper()
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"buy the otter sticker","asked":{"item":"otter sticker"},"max_total_minor":500,"allowed":["pay"]},
		"who":{"name":"Sticker Marketplace","domain":"stickers.example"},
		"terms":{"item":"otter sticker","price_minor":454,"currency":"USD"},
		"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	return passCheckOf(t, id)
}

// passCheckOf is the passing pay of passCheck on an open deal.
func passCheckOf(t *testing.T, id string) map[string]any {
	t.Helper()
	pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules-allow", checkerPrints("allow", passFinding))}})
	return dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 480)))
}

// optionLabels are the check's one-tap options, as the host shows them.
func optionLabels(check map[string]any) []string {
	var out []string
	for _, o := range check["options"].([]any) {
		out = append(out, o.(map[string]any)["label"].(string))
	}
	return out
}

// The card a share paused on: one question, the reasons collapsed, the
// agent's pick, and that the payment is still to come. No rule ids, no
// ruleset, no profile notes (capsule-cli#344).
func TestTheSharePauseIsOneQuestionAndItsReasons(t *testing.T) {
	check := defenderShareCheck(t)
	require.Equal(t, "pause", check["verdict"])
	assert.Equal(t, []string{"Hold", "Share"}, optionLabels(check), "nothing the user set was broken")
	assert.Equal(t, check["approval_text"], check["card"], "the card sealed is the card shown")
	assert.Equal(t, "PAUSED FOR YOU\n"+
		"Share your name, email, phone and address with Defender Marine (defender.com) for the Wichard shackle?\n"+
		"Why I'm asking: first time they get your details; I couldn't check this against your rules.\n"+
		"Good to know: I chose item Wichard shackle for you.\n"+
		"Next: I'll ask you again before paying.", check["card"])
	rules := check["rules"].(map[string]any)
	assert.Equal(t, "the rules checker changed since it was pinned"+rePinHint, rules["reason"], "the sealed reason is unchanged")
}

// A pay paused on the agent's pick, with an unverified claim.
func TestThePayPauseSaysTheFactsAndTheReasons(t *testing.T) {
	check := otterPayCheck(t)
	require.Equal(t, "pause", check["verdict"])
	assert.Equal(t, "PAUSED FOR YOU\n"+
		"Pay $3 to Sticker Marketplace (stickers.example) for the Otterly Chaos - Unsupervised and Thriving Funny Otter…?\n"+
		"$3 · by card · refundable · within your $8 limit\n"+
		"Why I'm asking: I picked size small 3.8in x 2.4in, not you.\n"+
		"Good to know: Sticker listed on sale at $3.00 (list $3.50) (not checked).", check["card"])
	assert.Equal(t, []string{"Hold", "Pay $3"}, optionLabels(check))
}

// A pass has no card; its approval text is the question and the facts.
func TestAPassHasNoCard(t *testing.T) {
	check := passCheck(t)
	assert.Equal(t, "", check["card"])
	assert.Equal(t, "Pay $4.54 to Sticker Marketplace (stickers.example) for the otter sticker?\n$4.54, up to $4.80 · by card · refundable · within your $5 limit", check["approval_text"])
	assert.Equal(t, cli.VoiceSilent, check["voice"])
}

// The trail says a card on one line, whether it was sealed as one block of
// lines or, before, as one line.
func TestTheTrailSaysACardOnOneLine(t *testing.T) {
	block := dealEvent{Kind: "check", Check: &dealCheckResult{Action: "pay", Verdict: "pause", Card: "Pay: x · $1.00\nYour rules were not checked: no rules checker configured.\n• Zelle = no card protection"}}
	assert.Equal(t, "checked pay: flagged — Pay: x · $1.00 · Your rules were not checked: no rules checker configured. · Zelle = no card protection", trailLine(block))
	sealed := dealEvent{Kind: "check", Check: &dealCheckResult{Action: "pay", Verdict: "pause", Card: "⚠️ Zelle = no card protection · [Hold] [Pay anyway]"}}
	assert.Equal(t, "checked pay: flagged — ⚠️ Zelle = no card protection · [Hold] [Pay anyway]", trailLine(sealed), "a card sealed before is said as it was")
}

// The proceed option is an override ("… anyway") only when something the
// user set was broken: a limit they gave (Steven's ruling, 2026-10-11,
// u830). A rule that asks is not broken (#344): its button is plain.
func TestProceedIsAnOverrideOnlyWhenARuleFailed(t *testing.T) {
	pinned := func(t *testing.T, verdict, findings string) map[string]any {
		t.Helper()
		id := stickerDeal(t, "card", false)
		pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules", checkerPrints(verdict, findings))}})
		return dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 480)))
	}
	t.Run("rules not checked", func(t *testing.T) {
		check := defenderShareCheck(t)
		assert.Equal(t, []string{"Hold", "Share"}, optionLabels(check))
	})
	t.Run("rules passed, differences flagged", func(t *testing.T) {
		dealFixture(t)
		id := dealRun(t, "open", "--input", filepath.Join(otterFixture, "open.json"))["deal_id"].(string)
		dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", filepath.Join(otterFixture, "intent-picked.json"))
		pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules-allow", checkerPrints("allow", passFinding))}})
		check := dealRun(t, "check", "--deal", id, "--input", filepath.Join(otterFixture, "check-pay.json"))
		require.Equal(t, "pause", check["verdict"])
		assert.Equal(t, "allow", check["rules"].(map[string]any)["verdict"])
		assert.Equal(t, []string{"Hold", "Pay $3"}, optionLabels(check))
	})
	t.Run("a rule asks", func(t *testing.T) {
		// A rule that asks for approval is not broken: it asks (#344).
		check := pinned(t, "escalate", denyFinding)
		require.Equal(t, "pause", check["verdict"])
		assert.Equal(t, []string{"Hold", "Pay $4.54"}, optionLabels(check))
	})
	t.Run("a rule denies", func(t *testing.T) {
		check := pinned(t, "deny", denyFinding)
		require.Equal(t, "deny", check["verdict"])
		assert.Equal(t, []string{"OK"}, optionLabels(check), "no way to proceed: the one button acknowledges the deny")
	})
	t.Run("a limit the user gave", func(t *testing.T) {
		id := stickerDeal(t, "card", false)
		check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 954)))
		require.Equal(t, "pause", check["verdict"])
		assert.Equal(t, []string{"Hold", "Pay $4.54 anyway"}, optionLabels(check))
	})
}

// A share before a payment says the payment is the next stop; one with no
// payment to follow does not.
func TestASharePauseBeforePayingSaysThePaymentIsNext(t *testing.T) {
	check := defenderShareCheck(t)
	assert.True(t, strings.HasSuffix(check["card"].(string), "Next: I'll ask you again before paying."))

	share := `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["address"]}`
	t.Run("the deal does not pay", func(t *testing.T) {
		dealFixture(t)
		id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
			"intent":{"verbatim":"send them my address","allowed":["share_contact"]},
			"who":{"name":"Defender Marine","domain":"defender.com"},
			"terms":{"item":"Wichard shackle","price_minor":4437,"currency":"USD"},
			"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
		check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, share))
		require.Equal(t, "pause", check["verdict"])
		assert.NotContains(t, check["card"], "Next:")
	})
	t.Run("the payment is already made", func(t *testing.T) {
		id := stickerDeal(t, "card", false)
		dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 480)))
		dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":454,"payee":"Sticker Marketplace","rail":"card"}`))
		dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"send them my address","allowed":["pay","share_contact"]}`))
		check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, share))
		require.Equal(t, "pause", check["verdict"], check["card"])
		assert.NotContains(t, check["card"], "Next:")
	})
}
