package cli

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cancelSteps is a deal's cancel check and cancel act, with their records,
// and the digest of the pay act's step.
type cancelSteps struct {
	check, act map[string]any
	payDigest  string
}

func cancelStepsOf(t *testing.T, id string) cancelSteps {
	t.Helper()
	events, records := recordsOf(t, id)
	var out cancelSteps
	for i, se := range events {
		switch {
		case se.Event.Act != nil && se.Event.Act.Action == "pay":
			out.payDigest = se.Digest
		case se.Event.Act != nil && se.Event.Act.Action == "cancel":
			out.act = records[i]
		case se.Event.Snapshot != nil && se.Event.Snapshot.Action == "cancel":
			out.check = records[i]
		}
	}
	require.NotEmpty(t, out.payDigest)
	require.NotNil(t, out.check)
	require.NotNil(t, out.act)
	return out
}

// reversesRel is the digest an act's refs[rel=reverses] names, or "".
func reversesRel(record map[string]any) string {
	block, _ := record[dealProfile].(map[string]any)
	refs, _ := block["refs"].([]any)
	for _, r := range refs {
		if ref := r.(map[string]any); ref["rel"] == "reverses" {
			return ref["digest"].(string)
		}
	}
	return ""
}

// A refund's check names the pay it reverses, in the typed reference shape,
// by the same digest the refund act's refs[rel=reverses] names. Its amount
// stays amount_minor, as on the act, so no returned_minor is sealed.
func TestARefundCheckNamesThePayItReverses(t *testing.T) {
	steps := cancelStepsOf(t, cancelAfterPurchase(t))
	check := bodyOf(steps.check)
	require.Equal(t, map[string]any{"type": "record", "digest_alg": "SHA-256", "digest": steps.payDigest}, check["reverses_ref"])
	assert.Equal(t, steps.payDigest, reversesRel(steps.act), "the check and the act name the same pay")
	act := bodyOf(steps.act)
	for _, body := range []map[string]any{check, act} {
		assert.EqualValues(t, 55880, body["amount_minor"])
		assert.Equal(t, "in", body["direction"])
		assert.NotContains(t, body, "cancelled_amount_minor")
		assert.NotContains(t, body, "returned_minor")
	}
	assert.NotContains(t, act, "reverses_ref", "the act names it by refs[rel=reverses]")
}

// A partial cancel returns no sealed payment: its check names no pay, and its
// amount is cancelled_amount_minor, as on its act.
func TestAPartialCancelCheckNamesNoPay(t *testing.T) {
	steps := cancelStepsOf(t, cancelAfterPurchaseOf(t, 20000))
	check, act := bodyOf(steps.check), bodyOf(steps.act)
	assert.NotContains(t, check, "reverses_ref")
	assert.Empty(t, reversesRel(steps.act))
	for _, body := range []map[string]any{check, act} {
		assert.EqualValues(t, 20000, body["cancelled_amount_minor"])
		assert.Equal(t, "in", body["direction"])
		assert.NotContains(t, body, "amount_minor")
		assert.NotContains(t, body, "returned_minor")
	}
}

// A purchase's check carries neither.
func TestAPurchaseCheckCarriesNoReversal(t *testing.T) {
	events, records := recordsOf(t, cancelAfterPurchase(t))
	checked := 0
	for i, se := range events {
		if se.Event.Snapshot == nil || se.Event.Snapshot.Action != "pay" {
			continue
		}
		checked++
		assert.NotContains(t, bodyOf(records[i]), "reverses_ref")
		assert.NotContains(t, bodyOf(records[i]), "returned_minor")
	}
	assert.Equal(t, 1, checked)
}

// A typed deal's refund check, a proposed-action/v0, carries the same
// reference.
func TestATypedRefundProposedActionNamesThePayItReverses(t *testing.T) {
	dealFixture(t)
	id := openTypedSticker(t)
	require.Equal(t, "pass", checkPay(t, id, 600)["verdict"])
	payNow(t, id, 600)
	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"cancel","amount_minor":600}`))
	t.Logf("cancel check: %v", check["verdict"])
	events, records := recordsOf(t, id)
	var pay, proposed string
	var ref any
	for i, se := range events {
		if se.Event.Act != nil && se.Event.Act.Action == "pay" {
			pay = se.Digest
		}
		if typeOf(records[i]) == typeProposedAction && bodyOf(records[i])["action"] == "cancel" {
			proposed, ref = se.Digest, bodyOf(records[i])["reverses_ref"]
		}
	}
	require.NotEmpty(t, proposed, "the refund check is a proposed action")
	assert.Equal(t, map[string]any{"type": "record", "digest_alg": "SHA-256", "digest": pay}, ref)
}

// A step sealed before refund checks named their pay re-derives without
// reverses_ref, every other byte the same.
func TestARefundCheckSealedBeforeReDerivesWithoutTheReference(t *testing.T) {
	id := cancelAfterPurchase(t)
	events := chainSteps(t, id)
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.NoError(t, s.useDeal(t.Context(), id, false))
	named := 0
	for i, se := range events {
		require.Equal(t, dealReversesRefVersion, se.Event.ReversesRef, "step %d", se.Event.N)
		now, _, err := encodeDealRecord(se.Event, events[:i], s.dkey)
		require.NoError(t, err, "step %d", se.Event.N)
		legacy := se.Event
		legacy.ReversesRef = ""
		raw, _, err := encodeDealRecord(legacy, events[:i], s.dkey)
		require.NoError(t, err, "step %d", se.Event.N)
		assert.NotContains(t, string(raw), "reverses_ref", "step %d", se.Event.N)
		var record map[string]any
		require.NoError(t, json.Unmarshal(now, &record))
		if _, ok := bodyOf(record)["reverses_ref"]; !ok {
			assert.Equal(t, string(now), string(raw), "step %d", se.Event.N)
			continue
		}
		named++
		delete(bodyOf(record), "reverses_ref")
		var legacyRecord map[string]any
		require.NoError(t, json.Unmarshal(raw, &legacyRecord))
		assert.Equal(t, record, legacyRecord, "step %d: only the reference differs", se.Event.N)
	}
	assert.Equal(t, 1, named, "the refund's check")
}

// The reference discloses nothing new in a shared copy: its members are a
// vocabulary token and a digest of the deal's own chain, judged like the
// act's refs[rel=reverses], and a record shared or withheld without it is
// shared or withheld with it.
func TestTheRefundReferenceSharesLikeTheActsReverses(t *testing.T) {
	id := cancelAfterPurchase(t)
	events := chainSteps(t, id)
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.NoError(t, s.useDeal(t.Context(), id, false))
	private := dealPrivateValues(events)
	for _, audience := range []string{dealAudienceCounterparty, dealAudienceAdjudicator} {
		for i, se := range events {
			shareable := map[string]bool{}
			for _, version := range []string{dealReversesRefVersion, ""} {
				ev := se.Event
				ev.ReversesRef = version
				raw, _, err := encodeDealRecord(ev, events[:i], s.dkey)
				require.NoError(t, err)
				var record map[string]any
				require.NoError(t, json.Unmarshal(raw, &record))
				shareable[version] = dealRecordShareable(record, "", audience, private)
			}
			assert.Equal(t, shareable[""], shareable[dealReversesRefVersion], "%s, step %d (%s)", audience, se.Event.N, se.Event.Kind)
		}
	}
	for _, k := range []string{"type", "digest_alg", "digest", "rel"} {
		assert.True(t, dealShareKeys[k], k)
	}
	assert.True(t, private.clean(typedRecordRef))
}
