package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// historyByID is a check input's history entries by capsule id.
func historyByID(input map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, h := range input["history"].([]any) {
		entry := h.(map[string]any)
		out[entry["capsule_id"].(string)] = entry
	}
	return out
}

// In a deal sealed in typed records, a history act carries its check's
// companion value just as an x-deal-v0 act does, so a second payment to the
// same payee is seen as a repeat. The act rests on its check directly when
// the check needed no approval, or on the check the user's approval answers.
func TestATypedHistoryActCarriesItsChecksProfileFingerprint(t *testing.T) {
	first := stickerDeal(t, "card", true)
	pass := stubChecker(t, "rules", checkerPrints("allow", passFinding))
	pinChecker(t, map[string]any{"command": []string{pass}})
	require.Equal(t, "pass", dealRun(t, "check", "--deal", first, "--input", writeJSON(t, stickerPay))["verdict"])
	direct := payNow(t, first, 454)
	require.Equal(t, false, direct["unchecked"])
	require.Equal(t, "check", stepKind(t, first, direct["authorized_by"].(string)), "a check that needed no approval authorizes the act")

	// One the user approved after the checker escalated.
	pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules-escalate", checkerPrints("escalate", denyFinding))}})
	c := dealRun(t, "check", "--deal", first, "--input", writeJSON(t, payCheck("card", 40, 40)))
	require.Equal(t, "pause", c["verdict"])
	approved(t, first, c)
	viaApproval := payNow(t, first, 40)
	require.Equal(t, false, viaApproval["unchecked"])
	require.Equal(t, "approval", stepKind(t, first, viaApproval["authorized_by"].(string)))

	// One done without a check, so with no companion.
	unchecked := payNow(t, first, 10)
	require.Equal(t, true, unchecked["unchecked"])

	second := dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"another otter sticker for at most 5 dollars","max_total_minor":500,"allowed":["pay"]},
		"who":{"name":"Sticker Marketplace","domain":"stickers.example"},
		"terms":{"item":"otter sticker","price_minor":454,"currency":"USD"},
		"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	_, _, input := ruleInputs(t, second, stickerPay)
	now := wantProfileShape(t, input["record"].(map[string]any)["counterparty_profile"])
	byID := historyByID(input)
	for name, act := range map[string]map[string]any{"rests on its check": direct, "rests on an approval": viaApproval} {
		require.Contains(t, byID, act["capsule_id"], name)
		assert.Equal(t, now, wantProfileShape(t, byID[act["capsule_id"].(string)]["counterparty_profile"]), name)
	}
	require.Contains(t, byID, unchecked["capsule_id"])
	assert.NotContains(t, byID[unchecked["capsule_id"].(string)], "counterparty_profile", "no check, no companion")
	assert.Equal(t, profileSteps(t, first)[0].payee, now)
}

// A typed act whose check named no payee has no companion, so its history
// entry carries none.
func TestATypedHistoryActWithoutACompanionCarriesNone(t *testing.T) {
	dealFixture(t)
	pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules", checkerPrints("allow", passFinding))}})
	id := dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, noPayeeOpen))["deal_id"].(string)
	require.Equal(t, "pass", dealRun(t, "check", "--deal", id, "--input", writeJSON(t, stickerPay))["verdict"])
	paid := payNow(t, id, 454)
	require.Equal(t, false, paid["unchecked"])
	assert.Empty(t, profileSteps(t, id))

	_, _, input := ruleInputs(t, stickerDealKeep(t), stickerPay)
	byID := historyByID(input)
	require.Contains(t, byID, paid["capsule_id"])
	assert.NotContains(t, byID[paid["capsule_id"].(string)], "counterparty_profile")
}

// stickerDealKeep opens a typed sticker deal on the current fixture.
func stickerDealKeep(t *testing.T) string {
	t.Helper()
	return dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"an otter sticker for at most 5 dollars","max_total_minor":500,"allowed":["pay"]},
		"who":{"name":"Sticker Marketplace","domain":"stickers.example"},
		"terms":{"item":"otter sticker","price_minor":454,"currency":"USD"},
		"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
}

// stepKind is the kind of a deal's step by capsule id.
func stepKind(t *testing.T, dealID, capsuleID string) string {
	t.Helper()
	for _, se := range chainSteps(t, dealID) {
		if se.CapsuleID == capsuleID {
			return se.Event.Kind
		}
	}
	t.Fatalf("no step %s", capsuleID)
	return ""
}
