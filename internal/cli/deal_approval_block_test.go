package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// promptOnce asserts what every approval block holds to: the agent shows
// one field, the card sealed on a pause is that same block, each phrase
// in once is said exactly once, and no option text or CLI syntax is shown.
func promptOnce(t *testing.T, check map[string]any, once ...string) string {
	t.Helper()
	text := approvalText(check)
	if check["verdict"] == "pass" {
		assert.Equal(t, "", check["card"], "a pass has no card")
	} else {
		assert.Equal(t, text, check["card"], "the card sealed is the block shown")
	}
	for _, phrase := range once {
		assert.Equal(t, 1, strings.Count(text, phrase), "%q said once in:\n%s", phrase, text)
	}
	for _, never := range []string{"[Hold]", "anyway", "[Pay]", "[Share", "--", "profile update"} {
		assert.NotContains(t, text, never)
	}
	return text
}

// Steven's rc15 pause, one fact per line: the rules line once (no CLI
// syntax), the first dealing, the new profile and all four first-time
// details folded into one line.
func TestApprovalBlockSharePauseSaysEachFactOnce(t *testing.T) {
	check := defenderShareCheck(t)
	require.Equal(t, "pause", check["verdict"])
	text := promptOnce(t, check, "rules were not checked", "Your rules", "First time", "no record of earlier counterparties",
		"street address", "email", "name", "phone", "The agent picked")
	assert.Equal(t, "Share your contact details: your details for the order · to Defender Marine (defender.com) · by card · refundable\n"+
		"Your rules were not checked: the rules checker changed since it was pinned, so it needs re-pinning.\n"+
		"• First time dealing with Defender Marine, and they have never had your street address, email, name or phone ("+newProfileNote+")\n"+
		"• The agent picked, not you: item Wichard shackle", text)
	rules := check["rules"].(map[string]any)
	assert.Equal(t, "the rules checker changed since it was pinned"+rePinHint, rules["reason"], "the sealed reason is unchanged")
}

// A pay paused on the agent's pick, with an unverified claim: what the user
// asked for, the pick the check paused on, and the claim, each once.
func TestApprovalBlockPayPauseSaysEachFactOnce(t *testing.T) {
	check := otterPayCheck(t)
	require.Equal(t, "pause", check["verdict"])
	text := promptOnce(t, check, "Your rules", "First time", "size small", "nverified", "Sticker listed on sale", "You asked for", "picked")
	assert.Equal(t, "Pay: Otterly Chaos - Unsupervised and Thriving Funny Otter Design Sticker · $3.00 · to Sticker Marketplace (stickers.example) · by card · refundable\n"+
		"Your rules were not checked: no rules checker configured.\n"+
		"• First time dealing with Sticker Marketplace ("+newProfileNote+")\n"+
		"• You asked for: item Otterly Chaos - Unsupervised and Thriving Funny Otter Design Sticker\n"+
		"• I picked size small 3.8in x 2.4in; you didn't choose it\n"+
		"• Unverified: Sticker listed on sale at $3.00 (list $3.50)", text)
}

// A pass is the headline and the rules line; the agent's own pick, when
// there is one, is said once beside them.
func TestApprovalBlockPassIsTheHeadlineAndTheRules(t *testing.T) {
	text := promptOnce(t, passCheck(t), "Your rules")
	assert.Equal(t, "Pay: otter sticker · $4.54 · to Sticker Marketplace (stickers.example) · by card · refundable\n"+
		"Your rules (example-rules/1.0.0, digest abababab): allowed, judged (not stated).", text)

	text = promptOnce(t, passCheckOf(t, stickerDeal(t, "card", false)), "Your rules", "The agent picked")
	assert.Equal(t, 3, len(strings.Split(text, "\n")), text)
	assert.NotContains(t, text, "First time", "a first dealing that did not pause is not news")
}

// A first-time detail told to someone already dealt with is its own line.
func TestApprovalBlockFoldsFirstDetailsForAKnownRecipient(t *testing.T) {
	assert.Equal(t, "First time giving Defender Marine your email and phone",
		recipientFinding(&dealRecipient{Name: "Defender Marine", First: []string{"email", "phone"}}))
	assert.Equal(t, "First time dealing with Defender Marine",
		recipientFinding(&dealRecipient{Name: "Defender Marine", FirstTime: true}))
	assert.Equal(t, "", recipientFinding(&dealRecipient{Name: "Defender Marine", Repeat: []string{"email"}}))
}

// The trail says a card on one line, whether it was sealed as one block of
// lines or, before, as one line.
func TestTheTrailSaysACardOnOneLine(t *testing.T) {
	block := dealEvent{Kind: "check", Check: &dealCheckResult{Action: "pay", Verdict: "pause", Card: "Pay: x · $1.00\nYour rules were not checked: no rules checker configured.\n• Zelle = no card protection"}}
	assert.Equal(t, "checked pay: flagged — Pay: x · $1.00 · Your rules were not checked: no rules checker configured. · Zelle = no card protection", trailLine(block))
	sealed := dealEvent{Kind: "check", Check: &dealCheckResult{Action: "pay", Verdict: "pause", Card: "⚠️ Zelle = no card protection · [Hold] [Pay anyway]"}}
	assert.Equal(t, "checked pay: flagged — ⚠️ Zelle = no card protection · [Hold] [Pay anyway]", trailLine(sealed), "a card sealed before is said as it was")
}
