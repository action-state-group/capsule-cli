package cli

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dealCapsules is a deal's sealed Capsules by capsule_id, read from its own
// bundle.
func dealCapsules(t *testing.T, dealID string) map[string]map[string]any {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bundle.json")
	out, err := invoke(t, "", "--profile", "deal", "bundle", "--deal", dealID, "--out", path)
	require.NoError(t, err, out)
	var bundle map[string]any
	require.NoError(t, json.Unmarshal(mustRead(t, path), &bundle))
	capsules := map[string]map[string]any{}
	for _, r := range bundle["records"].([]any) {
		c := r.(map[string]any)
		capsules[c["capsule_id"].(string)] = c
	}
	return capsules
}

// openSticker opens a purchase with a limit of $8.00 and the given refund
// recourse; pay checks at or under 800 pass, above it pause.
func openSticker(t *testing.T, refundable bool) string {
	t.Helper()
	return dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"buy me an otter sticker under 8 bucks","max_total_minor":800,"allowed":["pay","commit","cancel"]},
		"who":{"name":"Sticker Marketplace","domain":"stickers.example"},
		"terms":{"item":"otter sticker","price_minor":600,"currency":"USD"},
		"recourse":{"rail":"card","refundable":`+strconv.FormatBool(refundable)+`}}`))["deal_id"].(string)
}

func checkAct(t *testing.T, dealID, action string, amount int) map[string]any {
	t.Helper()
	body := `{"action":"` + action + `","terms":{"item":"otter sticker","price_minor":` + strconv.Itoa(amount) + `}`
	if action == "pay" {
		body += `,"amount_minor":` + strconv.Itoa(amount) + `,"authorized_max_minor":` + strconv.Itoa(amount)
	}
	return dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, body+`}`))
}

func act(t *testing.T, dealID, action string, amount int) map[string]any {
	t.Helper()
	body := `{"action":"` + action + `"`
	if action == "pay" {
		body += `,"amount_minor":` + strconv.Itoa(amount)
	}
	return dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, body+`}`))
}

// A pay the user approved in their own words (a sealed approval with
// said_commitment) is a decide Capsule: accepted by a human and executed;
// its effect a registered send_payment the agent reports it dispatched, so
// effect_mode is dispatched_unconfirmed, recoverable since the deal is
// refundable.
func TestDealPayTheUserApprovedIsADecideCapsule(t *testing.T) {
	dealFixture(t)
	id := openSticker(t, true)
	c := checkAct(t, id, "pay", 3000)
	require.Equal(t, "pause", c["verdict"], "over the $8.00 limit")
	approval := dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", c["check_id"].(string), "--choice", "proceed", "--said", "yes, 30 is fine for this one")
	paid := act(t, id, "pay", 3000)
	require.Equal(t, approval["capsule_id"], paid["authorized_by"])

	capsule := dealCapsules(t, id)[paid["capsule_id"].(string)]
	assert.Equal(t, "decide", capsule["action_type"])
	assert.Equal(t, map[string]any{"decision": "accept", "approver": "human", "human_disposed": true, "verdict_class": "executed"}, capsule["disposition"])
	assert.Equal(t, map[string]any{"type": "send_payment", "status": "dispatched", "effect_attestation": "runtime_claimed", "irreversibility_class": "one_way_recoverable"}, capsule["effect"])
	assert.Equal(t, "dispatched_unconfirmed", capsule["assurance"].(map[string]any)["effect_mode"])
}

// A pay a passing check approved under the user's standing intent records no
// words of theirs: its approver is policy, and a deal with no refund is
// one_way_consequential.
func TestDealPayApprovedByStandingIntentIsPolicyDisposed(t *testing.T) {
	dealFixture(t)
	id := openSticker(t, false)
	c := checkAct(t, id, "pay", 600)
	require.Equal(t, "pass", c["verdict"])
	paid := act(t, id, "pay", 600)
	require.Equal(t, c["approval_id"], paid["authorized_by"])

	capsule := dealCapsules(t, id)[paid["capsule_id"].(string)]
	assert.Equal(t, "decide", capsule["action_type"])
	assert.Equal(t, map[string]any{"decision": "accept", "approver": "policy", "human_disposed": false, "verdict_class": "executed"}, capsule["disposition"])
	assert.Equal(t, "one_way_consequential", capsule["effect"].(map[string]any)["irreversibility_class"])
}

// What the record does not show as an approved action with a registered
// effect type stays fyi: an unchecked pay (the deal record seals it as an
// outcome, never as an action), and authorized acts with no registered AAC
// effect.type (commit, cancel).
func TestDealRecordsWithoutARegisteredActionStayFYI(t *testing.T) {
	dealFixture(t)
	id := openSticker(t, true)
	unchecked := act(t, id, "pay", 600)
	require.Equal(t, true, unchecked["unchecked"])
	checkAct(t, id, "commit", 600)
	committed := act(t, id, "commit", 0)
	require.NotEmpty(t, committed["authorized_by"])
	checkAct(t, id, "cancel", 600)
	cancelled := act(t, id, "cancel", 0)
	require.NotEmpty(t, cancelled["authorized_by"])

	capsules := dealCapsules(t, id)
	for name, step := range map[string]map[string]any{"unchecked pay": unchecked, "commit": committed, "cancel": cancelled} {
		capsule := capsules[step["capsule_id"].(string)]
		assert.Equal(t, "fyi", capsule["action_type"], name)
		assert.Nil(t, capsule["disposition"], name)
		assert.Nil(t, capsule["effect"], name)
		assert.Equal(t, "not_applicable", capsule["assurance"].(map[string]any)["effect_mode"], name)
	}
	assert.Equal(t, map[string]string{"pay": "send_payment"}, dealEffectTypes,
		"only pay has a registered AAC effect.type; the rest await registration")
}

// The acceptance case: a caps check keyed on the payment's class (the
// registered effect.type, counting accepted decisions only, as a spend fold
// does) selects an over-cap payment. The all-fyi Capsule earlier releases
// sealed for the same step gives such a check nothing to select.
func TestDealOverCapPaymentIsSelectedByItsClass(t *testing.T) {
	dealFixture(t)
	id := openSticker(t, true)
	c := checkAct(t, id, "pay", 3000)
	dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", c["check_id"].(string), "--choice", "proceed", "--said", "yes, 30 is fine for this one")
	paid := act(t, id, "pay", 3000)

	const capMinor, payment = 800, "send_payment"
	selected := func(capsules []map[string]any) []string {
		var ids []string
		for _, capsule := range capsules {
			effect, _ := capsule["effect"].(map[string]any)
			disposition, _ := capsule["disposition"].(map[string]any)
			if capsule["action_type"] == "decide" && effect["type"] == payment && disposition["decision"] == "accept" {
				ids = append(ids, capsule["capsule_id"].(string))
			}
		}
		return ids
	}
	var all []map[string]any
	for _, capsule := range dealCapsules(t, id) {
		all = append(all, capsule)
	}
	assert.Equal(t, []string{paid["capsule_id"].(string)}, selected(all), "the payment, and only it")
	assert.Greater(t, 3000, capMinor, "its amount (in the deal record) is over the cap")

	// The same step as an earlier release sealed it.
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.close() })
	require.NoError(t, s.useDeal(t.Context(), id, false))
	events, err := s.load(t.Context(), id)
	require.NoError(t, err)
	last := events[len(events)-1]
	legacy := dealCapsuleInput(events[:len(events)-1], last.Event, nil, p.Name, time.Now(), true)
	assert.Equal(t, "fyi", string(legacy.ActionType))
	assert.Nil(t, legacy.Effect, "nothing for a check keyed on the class to select")
}

// A step an earlier release prepared (all fyi) but never published is still
// recovered: the Capsule it prepared is the legacy one, so recovery uses it.
func TestDealRecoversAStepAnEarlierReleasePrepared(t *testing.T) {
	dealFixture(t)
	id := openSticker(t, true)
	c := checkAct(t, id, "pay", 600)
	require.Equal(t, "pass", c["verdict"])

	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	require.NoError(t, s.useDeal(t.Context(), id, false))
	events, err := s.load(t.Context(), id)
	require.NoError(t, err)
	a := &dealAct{Action: "pay", AmountMinor: ptr(int64(600))}
	a.AuthorizedBy, a.Reason, a.Rule = authorizeAct(events, *a, dealClock().UTC())
	require.NotEmpty(t, a.AuthorizedBy)
	_, prepared, err := s.prepareStep(t.Context(), id, events, dealEvent{Kind: "act", Act: a})
	require.NoError(t, err)
	// Rewrite the index row as the earlier release would have written it.
	request, _, err := s.stepRequest(events, prepared.Event, true)
	require.NoError(t, err)
	record, err := seal(request, s.key)
	require.NoError(t, err)
	require.NotEqual(t, prepared.CapsuleID, record.CapsuleID)
	_, err = s.db.ExecContext(t.Context(), `UPDATE deal_steps SET capsule_id=? WHERE deal_id=? AND n=?`, record.CapsuleID, id, prepared.Event.N)
	require.NoError(t, err)
	require.NoError(t, s.close())

	capsule, ok := dealCapsules(t, id)[record.CapsuleID]
	require.True(t, ok, "the step was recovered as the Capsule it was prepared as")
	assert.Equal(t, "fyi", capsule["action_type"])
}

// dealApprovers is the approver each sealed approval of the deal records, by capsule_id.
func dealApprovers(t *testing.T, dealID string) map[string]string {
	t.Helper()
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.NoError(t, s.useDeal(t.Context(), dealID, false))
	events, err := s.load(t.Context(), dealID)
	require.NoError(t, err)
	out := map[string]string{}
	for _, se := range events {
		if se.Event.Approval != nil {
			out[se.CapsuleID] = se.Event.Approval.Approver
		}
	}
	return out
}

// Three authority values, and only one certifies the user's consent. approver
// user is sealed only with the user's own words (said_commitment) on an
// approval of the check, which pins the proposed action; a click on a card
// the agent composed (no words) seals agent_card, and the act it authorizes
// stays fyi; a passing check seals standing_intent (policy).
func TestDealApprovalAuthorityHasThreeValues(t *testing.T) {
	for _, tc := range []struct {
		name, said, approver, actionType string
		pass                             bool
	}{
		{name: "the user's words", said: "yes, 30 is fine for this one", approver: "user", actionType: "decide"},
		{name: "a card click", said: "", approver: "agent_card", actionType: "fyi"},
		{name: "a card click with blank words", said: "   ", approver: "agent_card", actionType: "fyi"},
		{name: "a passing check", pass: true, approver: "standing_intent", actionType: "decide"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dealFixture(t)
			id := openSticker(t, true)
			amount := 3000
			if tc.pass {
				amount = 600
			}
			c := checkAct(t, id, "pay", amount)
			approvalID, _ := c["approval_id"].(string)
			if !tc.pass {
				require.Equal(t, "pause", c["verdict"])
				args := []string{"note", "--deal", id, "--kind", "approval", "--check", c["check_id"].(string), "--choice", "proceed"}
				if tc.said != "" {
					args = append(args, "--said", tc.said)
				}
				approvalID = dealRun(t, args...)["capsule_id"].(string)
			}
			paid := act(t, id, "pay", amount)
			require.Equal(t, approvalID, paid["authorized_by"], "the deal's own rules still let the answer authorize the act")

			assert.Equal(t, tc.approver, dealApprovers(t, id)[approvalID], "the approver the record seals")
			capsule := dealCapsules(t, id)[paid["capsule_id"].(string)]
			assert.Equal(t, tc.actionType, capsule["action_type"])
			switch tc.approver {
			case "user":
				assert.Equal(t, "human", capsule["disposition"].(map[string]any)["approver"])
				assert.Equal(t, true, capsule["disposition"].(map[string]any)["human_disposed"])
			case "standing_intent":
				assert.Equal(t, "policy", capsule["disposition"].(map[string]any)["approver"])
			case "agent_card":
				assert.Nil(t, capsule["disposition"], "a card click certifies no one's consent")
			}
			// The exported records pass the profile's own checker.
			if python := profilePython(t); python != "" {
				export := filepath.Join(t.TempDir(), "deal.json")
				dealRun(t, "export", "--deal", id, "--output", export)
				result, err := exec.Command(python, dealProfileDir+"/check_profile.py", export).CombinedOutput()
				require.NoError(t, err, string(result))
				assert.Contains(t, string(result), "ALL OK")
			}
		})
	}
}

// Raising the user's limits takes their own words: a card click can't.
func TestDealConfirmLimitsNeedsTheUsersWords(t *testing.T) {
	dealFixture(t)
	id := openSticker(t, true)
	note := dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"up to 50","max_total_minor":5000,"allowed":["pay"]}`))
	_, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", id, "--kind", "approval", "--check", note["capsule_id"].(string), "--choice", "confirm_limits")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "confirm_limits needs --said")
}

// Keying the Capsule's human approver on approver user is sound because every
// user approval this producer seals commits the user's words (said_commitment)
// and refs the check it approves; an agent_card approval commits none.
func TestDealUserApprovalsCarrySaidCommitment(t *testing.T) {
	dealFixture(t)
	id := openSticker(t, true)
	c1 := checkAct(t, id, "pay", 3000)
	user := dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", c1["check_id"].(string), "--choice", "hold", "--said", "not yet")
	c2 := checkAct(t, id, "pay", 3000)
	card := dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", c2["check_id"].(string), "--choice", "hold")

	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.NoError(t, s.useDeal(t.Context(), id, false))
	events, err := s.load(t.Context(), id)
	require.NoError(t, err)
	records := map[string]map[string]any{}
	for i, se := range events {
		if se.Event.Approval == nil {
			continue
		}
		raw, _, err := encodeDealRecord(se.Event, events[:i], s.dkey)
		require.NoError(t, err)
		var record map[string]any
		require.NoError(t, json.Unmarshal(raw, &record))
		records[se.CapsuleID] = record
	}
	digests := map[string]string{}
	for _, se := range events {
		digests[se.CapsuleID] = se.Digest
	}

	u := records[user["capsule_id"].(string)]
	assert.Equal(t, "user", u["body"].(map[string]any)["approver"])
	assert.Regexp(t, `^[0-9a-f]{64}$`, u["body"].(map[string]any)["said_commitment"], "the user's words, committed")
	refs := u["x-deal-v0"].(map[string]any)["refs"].([]any)
	assert.Equal(t, "approves", refs[0].(map[string]any)["rel"])
	assert.Equal(t, digests[c1["check_id"].(string)], refs[0].(map[string]any)["digest"], "on the check it answers")

	k := records[card["capsule_id"].(string)]
	assert.Equal(t, "agent_card", k["body"].(map[string]any)["approver"])
	assert.NotContains(t, k["body"], "said_commitment", "a card click commits no words")
}
