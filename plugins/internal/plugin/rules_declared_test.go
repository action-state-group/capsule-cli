package plugin

import (
	"encoding/json"
	"testing"

	"github.com/action-state-group/capsule-cli/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Three rules the ruleset declares and the checker does not measure, as a
// checker reports them (the reason is the contract's exact words).
const declaredFindings = `{"id":"r14-commits-the-user","verdict":"not_evaluable","reason":"declared, not measured"},` +
	`{"id":"r24-outside-instructions","verdict":"not_evaluable","reason":"declared, not measured"},` +
	`{"id":"r25-no-bypassing","verdict":"not_evaluable","reason":"declared, not measured"}`

const declaredLine = "3 rules are declared by the ruleset but not measured by this checker (r14, r24, r25)."

func checkWithRules(t *testing.T, verdict, findings string) (string, map[string]any) {
	t.Helper()
	id := stickerDeal(t, "card", false)
	out := `printf '%s' '{"schema":"external-check-result/v0","ruleset_id":"example-rules/1.0.0","definition_digest":"` + exampleRulesDigest + `","verdict":"` + verdict + `","tier":"recomputed","findings":[` + findings + `]}'`
	pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules", out)}})
	return id, dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 454)))
}

// checkRules is the check's rules as the check result types them.
func checkRules(t *testing.T, check map[string]any) *dealRules {
	t.Helper()
	raw, err := json.Marshal(check["rules"])
	require.NoError(t, err)
	var r dealRules
	require.NoError(t, json.Unmarshal(raw, &r))
	return &r
}

func differenceRules(check map[string]any) []string {
	var out []string
	for _, d := range check["differences"].([]any) {
		out = append(out, d.(map[string]any)["rule"].(string))
	}
	return out
}

// An allowed purchase with declared rules passes quietly: the check's rules
// carry the declared rules beside the passing verdict; no card, no
// difference, and the prompt does not name them (capsule-cli#344).
func TestDeclaredRulesBesideAPassingVerdictAreQuiet(t *testing.T) {
	_, check := checkWithRules(t, "allow", passFinding+","+declaredFindings)
	assert.Equal(t, "pass", check["verdict"], "the verdict is unaffected")
	assert.Equal(t, "", check["card"])
	assert.Empty(t, differenceRules(check))
	assert.Equal(t, []string{"r14-commits-the-user", "r24-outside-instructions", "r25-no-bypassing"}, declaredRules(checkRules(t, check)))
	assert.Equal(t, declaredLine, declaredRulesLine(declaredRules(checkRules(t, check))), "as the receipt says them")
	assert.NotContains(t, approvalText(check), "declared")
	assert.NotContains(t, approvalText(check), "not checked")
	assert.NotContains(t, approvalText(check), "not fully checked")
}

// An escalation names only what escalated: the declared rules are not
// differences, and the check's rules still carry them quietly.
func TestDeclaredRulesAreNotDifferencesOnAnEscalation(t *testing.T) {
	_, check := checkWithRules(t, "escalate", denyFinding+","+declaredFindings)
	assert.Equal(t, "pause", check["verdict"])
	texts := differenceTexts(check)
	assert.Contains(t, texts, "Your rules ask for approval: per-purchase")
	assert.NotContains(t, texts, "declared, not measured")
	assert.NotContains(t, texts, "r14-commits-the-user")
	assert.NotContains(t, check["card"], "r14")
	assert.Len(t, declaredRules(checkRules(t, check)), 3)
}

// A genuine not_evaluable (the checker could not evaluate a rule on this
// record) is still said loudly, beside declared rules or not.
func TestAGenuineNotEvaluableStaysLoud(t *testing.T) {
	missing := `{"id":"r09-terms","verdict":"not_evaluable","reason":"the record states no terms"}`
	for verdict, words := range map[string]string{
		"not_evaluable": "Your rules were not fully checked: r09-terms: the record states no terms",
		// Its own state, whatever the verdict (capsule-cli#341).
		"escalate": "Your rules were not fully checked: r09-terms: the record states no terms",
	} {
		t.Run(verdict, func(t *testing.T) {
			_, check := checkWithRules(t, verdict, missing+","+declaredFindings)
			assert.Equal(t, "pause", check["verdict"])
			assert.Contains(t, differenceTexts(check), words)
			assert.Len(t, differenceRules(check), 1, "the genuine one only")
		})
	}
}

// The receipt's check line says the declared rules quietly in the user's own
// copy; a shared copy does not name the user's rules.
func TestTheReceiptSaysDeclaredRulesQuietly(t *testing.T) {
	id, _ := checkWithRules(t, "allow", passFinding+","+declaredFindings)
	report := dealRun(t, "report", "--deal", id)
	raw, err := json.Marshal(report["did"])
	require.NoError(t, err)
	assert.Contains(t, string(raw), "Checked before paying: no differences; 3 rules are declared by the ruleset but not measured by this checker (r14, r24, r25)")
	_, shared := sharedCopy(t, id, cli.DealAudienceCounterparty, "the shop")
	assert.NotContains(t, shared, "declared by the ruleset")
}
