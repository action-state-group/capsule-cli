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
	assert.Equal(t, []string{"Hold", "Share"}, optionLabels(check), "nothing the user set was broken")
	text := promptOnce(t, check, "rules were not checked", "Your rules", "First time", "Step 1 of 2", "$44.37", "no record of earlier counterparties",
		"street address", "email", "name", "phone", "The agent picked")
	assert.Equal(t, "Share your contact details: your details for the order · to Defender Marine (defender.com) · by card · refundable\n"+
		"Step 1 of 2: I will ask again before paying $44.37.\n"+
		"• Your rules were not checked: the rules checker changed since it was pinned, so it needs re-pinning.\n"+
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
		"• Your rules were not checked: no rules checker configured.\n"+
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
		"• Your rules (example-rules/1.0.0, digest abababab): allowed, judged (not stated).", text)

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

// The proceed option is an override ("… anyway") only when something the
// user set was broken: a failing rule of theirs, or a limit they gave.
// Steven's ruling, 2026-10-11 (u830).
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
		assert.Equal(t, []string{"Hold", "Pay"}, optionLabels(check))
	})
	t.Run("a rule failed", func(t *testing.T) {
		check := pinned(t, "escalate", denyFinding)
		require.Equal(t, "pause", check["verdict"])
		assert.Equal(t, []string{"Hold", "Pay anyway"}, optionLabels(check))
	})
	t.Run("a rule denies", func(t *testing.T) {
		check := pinned(t, "deny", denyFinding)
		require.Equal(t, "deny", check["verdict"])
		assert.Equal(t, []string{"Hold"}, optionLabels(check), "no way to proceed")
	})
	t.Run("a limit the user gave", func(t *testing.T) {
		id := stickerDeal(t, "card", false)
		check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 954)))
		require.Equal(t, "pause", check["verdict"])
		assert.Equal(t, []string{"Hold", "Pay anyway"}, optionLabels(check))
	})
}

// A share paused before a payment says it is the first of two steps, with
// the deal's total; a share with no payment to follow does not.
func TestASharePauseBeforePayingSaysStepOneOfTwo(t *testing.T) {
	check := defenderShareCheck(t)
	assert.Equal(t, "Step 1 of 2: I will ask again before paying $44.37.", strings.Split(approvalText(check), "\n")[1])

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
		assert.NotContains(t, approvalText(check), "Step 1 of 2")
	})
	t.Run("the payment is already made", func(t *testing.T) {
		id := stickerDeal(t, "card", false)
		dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 480)))
		dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":454,"payee":"Sticker Marketplace","rail":"card"}`))
		dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"send them my address","allowed":["pay","share_contact"]}`))
		check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, share))
		require.Equal(t, "pause", check["verdict"], approvalText(check))
		assert.NotContains(t, approvalText(check), "Step 1 of 2")
	})
}

// What the user asked for and what the agent picked are one fact per line;
// a pick the check paused on is said only by its difference.
func TestApprovalBlockPicksAreOneFactPerLine(t *testing.T) {
	r := dealCheckResult{
		Asked:       []cli.DealAttribute{{Field: "when", Label: "dates", Value: "2026-10-03"}},
		Picked:      []cli.DealAttribute{{Field: "item", Label: "item", Value: "double room"}, {Field: "conditions.size", Label: "size", Value: "small"}},
		Differences: []dealDifference{{Question: "asked", Rule: "agent_picked", Field: "conditions.size", Text: "I picked size small; you didn't choose it"}},
	}
	assert.Equal(t, []string{"You asked for: dates 2026-10-03", "The agent picked, not you: item double room"}, picksFindings(r))
}

// The skill's presentation rules (Steven's rulings, 2026-10-11): the block
// shown as returned, one lead-in sentence of the agent's own and nothing
// else, options as buttons and never as text, step 1 of 2 kept. The skill
// is public and host-neutral: it names "the host", never a product.
func TestTheDealSkillPresentsTheBlockAsRuled(t *testing.T) {
	raw, err := os.ReadFile("../../../skills/deal/SKILL.md")
	require.NoError(t, err)
	text := strings.Join(strings.Fields(string(raw)), " ")
	for _, rule := range []string{
		"show `approval_text` exactly as returned, once",
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
