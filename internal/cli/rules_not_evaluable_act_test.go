package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const stickerOpen = `{"type":"purchase","channel":"web",
	"intent":{"verbatim":"buy me a funny otter sticker for at most 5 dollars","max_total_minor":500,"allowed":["pay"]},
	"who":{"name":"Sticker Marketplace","domain":"stickers.example"},
	"terms":{"item":"otter sticker","price_minor":454,"currency":"USD"},
	"recourse":{"rail":"card","refundable":true}}`

// notEvaluableCases are rules checkers whose verdict is not_evaluable: one
// that cannot evaluate a window, and one whose taxonomy does not match, so
// every class-keyed rule is not_evaluable.
var notEvaluableCases = map[string]string{
	"a window it cannot evaluate": passFinding + `,{"id":"weekly","check":"window","verdict":"not_evaluable","reason":"the record carries no week of history"}`,
	"a taxonomy mismatch": `{"id":"per-class-cap","check":"caps/1","verdict":"not_evaluable","reason":"action_class is not in this ruleset's taxonomy"},` +
		`{"id":"class-allowlist","check":"allow/1","verdict":"not_evaluable","reason":"action_class is not in this ruleset's taxonomy"}`,
}

// pausedOnNotEvaluable opens a sticker deal and checks a pay that the
// pinned checker reports not_evaluable: the check pauses on
// rules_not_evaluable alone.
func pausedOnNotEvaluable(t *testing.T, findings string) (string, map[string]any) {
	t.Helper()
	pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules-unevaluable", checkerPrints("not_evaluable", findings))}})
	id := dealRun(t, "open", "--input", writeJSON(t, stickerOpen))["deal_id"].(string)
	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 480)))
	require.Equal(t, "pause", check["verdict"])
	require.Contains(t, check["card"], "Your rules were not fully checked")
	require.Contains(t, check["card"], "[Pay anyway]")
	var rules []string
	for _, d := range check["differences"].([]any) {
		rules = append(rules, d.(map[string]any)["rule"].(string))
	}
	require.Equal(t, []string{"rules_not_evaluable"}, rules)
	return id, check
}

// A rules checker's not_evaluable never lets an act through on its own.
// An act taken over the pause without the user's choice is recorded, never
// authorized: sealed unchecked (no_sealed_approval), shown as an anomaly,
// and counted in the profile's history as unchecked spending. With the
// user's "Pay anyway", the act is authorized by their approval of the
// check whose only difference is rules_not_evaluable.
func TestANotEvaluableRulesVerdictNeverAuthorizesAnActOnItsOwn(t *testing.T) {
	for name, findings := range notEvaluableCases {
		t.Run(name, func(t *testing.T) {
			dealFixture(t)

			// Without the user's choice: sealed, unchecked.
			id, _ := pausedOnNotEvaluable(t, findings)
			before := len(chainSteps(t, id))
			paid := payNow(t, id, 454)
			assert.Equal(t, true, paid["unchecked"], "no approval: the act is not authorized")
			assert.Equal(t, "no_sealed_approval", paid["rule"])
			assert.Empty(t, paid["authorized_by"])
			assert.Len(t, chainSteps(t, id), before+1, "the act the agent took is recorded")
			assert.Equal(t, "act", stepKind(t, id, paid["capsule_id"].(string)))

			report, _ := ownReportAndChain(t, id)
			var flagged bool
			for _, raw := range report["anomalies"].([]any) {
				a := raw.(map[string]any)
				for _, step := range a["steps"].([]any) {
					if step == paid["capsule_id"] && a["kind"] == "unsealed_approval" {
						flagged = true
						assert.Contains(t, a["text"], "Went ahead without your approval")
					}
				}
			}
			assert.True(t, flagged, "the act is an anomaly in the report")

			// With "Pay anyway": authorized over rules_not_evaluable.
			other, check := pausedOnNotEvaluable(t, findings)
			approval, err := answerCheck(t, other, check["check_id"].(string), "yes, pay anyway", check["card"].(string))
			require.NoError(t, err)
			anyway := payNow(t, other, 454)
			assert.Equal(t, false, anyway["unchecked"])
			assert.Equal(t, approval["capsule_id"], anyway["authorized_by"])
			var answered *dealCheckResult
			for _, se := range chainSteps(t, other) {
				if se.CapsuleID == approval["capsule_id"] {
					require.NotNil(t, se.Event.Approval)
					assert.True(t, se.Event.Approval.Proceed)
					assert.Equal(t, check["check_id"], se.Event.Approval.Check)
				}
				if se.CapsuleID == check["check_id"] {
					answered = se.Event.Check
				}
			}
			require.NotNil(t, answered, "the approval answers the paused check")
			require.Len(t, answered.Differences, 1)
			assert.Equal(t, "rules_not_evaluable", answered.Differences[0].Rule, "the override is of rules_not_evaluable")

			// The next check's history: the unchecked act counts as spending,
			// marked unchecked; the act paid anyway is an ordinary act.
			_, _, input := ruleInputs(t, dealRun(t, "open", "--input", writeJSON(t, stickerOpen))["deal_id"].(string), stickerPay)
			byID := historyByID(input)
			require.Contains(t, byID, paid["capsule_id"], "the unchecked act is in the history")
			body := byID[paid["capsule_id"].(string)]["agent_input"].(map[string]any)["body"].(map[string]any)
			unchecked, ok := body["unchecked"].(map[string]any)
			require.True(t, ok, "it carries the act under body.unchecked")
			assert.Equal(t, float64(454), unchecked["amount_minor"])
			require.Contains(t, byID, anyway["capsule_id"])
			anywayBody := byID[anyway["capsule_id"].(string)]["agent_input"].(map[string]any)["body"].(map[string]any)
			assert.NotContains(t, anywayBody, "unchecked")
			assert.Equal(t, float64(454), anywayBody["amount_minor"])

			_, export := exportRecords(t, id)
			checkProfile(t, export)
		})
	}
}
