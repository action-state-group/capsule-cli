package cli

import (
	"encoding/json"
	"testing"

	emit "github.com/action-state-group/capsule-emit-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// In a typed deal the decide Capsule of a payment presents the action
// record's own authority_basis: the user's own approval makes it human,
// with the platform's approval beside it changing nothing.
func TestTypedCapsuleIsHumanOnTheUsersApproval(t *testing.T) {
	dealFixture(t)
	id := openTypedFlight(t)
	c := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, merchantCheck))
	platformApproval(t, id, c["check_id"].(string), "Approve $558.80 purchase", "55880")
	_, err := answerCheck(t, id, c["check_id"].(string), "yes, the new site is fine", c["card"].(string))
	require.NoError(t, err)
	paid := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, merchantPay))

	capsule := dealCapsules(t, id)[paid["capsule_id"].(string)]
	assert.Equal(t, "decide", capsule["action_type"])
	assert.Equal(t, map[string]any{"decision": "accept", "approver": "human", "human_disposed": true, "verdict_class": "executed"}, capsule["disposition"])
	basis := bodyOf(chainRecords(t, id)[paid["capsule_id"].(string)])["authority_basis"].([]any)
	require.Len(t, basis, 3, "task authority, the user's approval, the platform's")
}

// A DO, within the user's rules, rests on the task authority: policy, never
// human, even with a platform's own approval observed beside it.
func TestTypedCapsuleIsPolicyOnADO(t *testing.T) {
	dealFixture(t)
	id := openTypedSticker(t)
	c := checkPay(t, id, 600)
	platformApproval(t, id, c["check_id"].(string), "Buy otter sticker for $6.00", "600")
	paid := payNow(t, id, 600)

	capsule := dealCapsules(t, id)[paid["capsule_id"].(string)]
	assert.Equal(t, "decide", capsule["action_type"])
	assert.Equal(t, map[string]any{"decision": "accept", "approver": "policy", "human_disposed": false, "verdict_class": "executed"}, capsule["disposition"])
}

// An action done without authority, on the platform's approval or a card
// answer alone, stays fyi.
func TestTypedCapsuleOfAnUncoveredActionIsFYI(t *testing.T) {
	dealFixture(t)
	id := openTypedFlight(t)
	c := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, merchantCheck))
	platformApproval(t, id, c["check_id"].(string), "Approve $558.80 purchase", "55880")
	paid := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, merchantPay))
	require.Equal(t, true, paid["unchecked"])
	capsule := dealCapsules(t, id)[paid["capsule_id"].(string)]
	assert.Equal(t, "fyi", capsule["action_type"])
	assert.Nil(t, capsule["disposition"])
}

// The mapping reads only the record's authority_basis: each layer list maps
// as decide/fyi presentation of it, and an x-deal-v0 record is left to the
// approval it names.
func TestBasisDispositionReadsTheRecord(t *testing.T) {
	record := func(layers ...string) []byte {
		basis := make([]map[string]any, len(layers))
		for i, l := range layers {
			basis[i] = map[string]any{"type": l, "ref": typedRef("00")}
		}
		raw, err := json.Marshal(map[string]any{"type": typeActionRecord, "body": map[string]any{"action": "pay", "authority_basis": basis}})
		require.NoError(t, err)
		return raw
	}
	policy := emit.Disposition{Decision: emit.DecisionAccept, VerdictClass: emit.VerdictExecuted, Approver: emit.ApproverPolicy}
	human := emit.Disposition{Decision: emit.DecisionAccept, VerdictClass: emit.VerdictExecuted, Approver: emit.ApproverHuman, HumanDisposed: true}
	for _, tc := range []struct {
		name   string
		layers []string
		want   emit.Disposition
		ok     bool
	}{
		{"the task authority alone (DO)", []string{"task_authority"}, policy, true},
		{"with a platform's approval", []string{"task_authority", "platform_approval"}, policy, true},
		{"with the user's approval", []string{"task_authority", "user_approval"}, human, true},
		{"all three", []string{"task_authority", "user_approval", "platform_approval"}, human, true},
		{"a reserved override", []string{"task_authority", "one_shot_override"}, emit.Disposition{}, false},
		{"no task authority first", []string{"user_approval"}, emit.Disposition{}, false},
		{"empty", nil, emit.Disposition{}, false},
	} {
		d, ok, typed := basisDisposition(record(tc.layers...))
		assert.True(t, typed, tc.name)
		assert.Equal(t, tc.ok, ok, tc.name)
		assert.Equal(t, tc.want, d, tc.name)
	}
	_, _, typed := basisDisposition([]byte(`{"x-deal-v0":{"record_type":"action"},"body":{}}`))
	assert.False(t, typed, "an x-deal-v0 record maps by the approval it names")
}
