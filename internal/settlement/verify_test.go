package settlement

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	emit "github.com/action-state-group/capsule-emit/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The rules themselves are agent-action-capsule's and tested there; these
// tests hold what capsule-cli adds: the mapping of its result back to the
// caller's labels, its report shape, and the delivery evidence sources.

// fixture is one vector case as a mutable verifier input, with the seeds to
// reseal a leg after an edit.
type fixture struct {
	t     *testing.T
	in    Input
	seeds map[string]string
}

func newFixture(t *testing.T, id string) *fixture {
	t.Helper()
	file := loadVectors(t)
	seeds := map[string]string{}
	for name, key := range file.Keys {
		seeds[name] = key.SeedHex
	}
	for _, c := range file.Cases {
		if c.ID == id {
			in := c.input(t)
			for i := range in.Legs {
				in.Legs[i].Capsule = deepCopy(t, in.Legs[i].Capsule)
			}
			return &fixture{t: t, in: in, seeds: seeds}
		}
	}
	t.Fatalf("no vector case %s", id)
	return nil
}

func deepCopy(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	var out map[string]any
	decodeNumbers(t, encoded, &out)
	return out
}

func (f *fixture) leg(label string) *Leg {
	for i := range f.in.Legs {
		if f.in.Legs[i].Label == label {
			return &f.in.Legs[i]
		}
	}
	f.t.Fatalf("no leg %s", label)
	return nil
}

func (f *fixture) settlement(label string) map[string]any {
	return f.leg(label).Capsule["settlement"].(map[string]any)
}

// reseal recomputes a leg's Capsule ID and signs a new Producer Envelope with
// the named key, as its sealer would after the edit.
func (f *fixture) reseal(label, signer string) string {
	f.t.Helper()
	l := f.leg(label)
	delete(l.Capsule, "capsule_id")
	id, err := canonical.ComputeCapsuleID(l.Capsule)
	require.NoError(f.t, err)
	l.Capsule["capsule_id"] = id
	encoded, err := canonical.JCS(l.Capsule)
	require.NoError(f.t, err)
	seed, err := hex.DecodeString(f.seeds[signer])
	require.NoError(f.t, err)
	identity, err := emit.NewEd25519SigningIdentity(ed25519.NewKeyFromSeed(seed))
	require.NoError(f.t, err)
	l.Envelope, err = emit.Sign(emit.BuiltPayload{CapsuleID: id, Value: l.Capsule, JSON: encoded}, identity)
	require.NoError(f.t, err)
	return id
}

func (f *fixture) edit(label string, change func(s map[string]any)) Result {
	f.t.Helper()
	change(f.settlement(label))
	f.reseal(label, f.settlement(label)["sealer_role"].(string))
	return Verify(f.in)
}

func codes(reports []Report) []string {
	var out []string
	for _, r := range reports {
		out = append(out, r.Code)
	}
	return out
}

func without(legs []Leg, label string) []Leg {
	var out []Leg
	for _, l := range legs {
		if l.Label != label {
			out = append(out, l)
		}
	}
	return out
}

func TestVerifyReportsByTheCallersLabels(t *testing.T) {
	f := newFixture(t, "pos-x402-two-sided-agreed")
	result := Verify(f.in)
	require.True(t, result.Conforming, result.Failures)
	assert.Equal(t, "applied", result.KeyPolicy)
	assert.Equal(t, "x402-terms", result.Settlements[0].Terms)
	require.Len(t, result.Legs, len(f.in.Legs))
	for i, l := range result.Legs {
		assert.Equal(t, f.in.Legs[i].Label, l.Record)
		assert.Equal(t, f.in.Legs[i].Capsule["capsule_id"], l.CapsuleID)
		assert.Len(t, l.AuthenticatedKey, 64)
	}

	f.leg("x402-payee-observed").Envelope = nil
	missing := Verify(f.in)
	assert.Equal(t, []Report{{Records: []string{"x402-payee-observed"}, Code: "envelope_invalid"}}, missing.Failures)
	assert.Equal(t, PaymentPayerStated, missing.Settlements[0].PaymentState)
	assert.Equal(t, LegKey{Record: "x402-payee-observed"}, missing.Legs[2], "no key is reported for a leg whose envelope is absent")

	f.leg("x402-payer-observed").Capsule["capsule_id"] = json.Number("7")
	noID := Verify(f.in)
	assert.Contains(t, noID.Failures, Report{Records: []string{"x402-payer-observed"}, Code: "capsule_invalid"}, "a capsule_id that is not a string still names its leg")
}

func TestTermsRefMustResolve(t *testing.T) {
	f := newFixture(t, "pos-x402-two-sided-agreed")
	result := f.edit("x402-payee-observed", func(s map[string]any) { s["terms_ref"] = strings.Repeat("a", 64) })
	assert.Equal(t, []Report{{Records: []string{"x402-payee-observed"}, Code: "terms_ref_unresolved"}}, result.Failures)
	assert.Equal(t, PaymentPayerStated, result.Settlements[0].PaymentState)
}

// No state of the draft applies to a conflated pair that otherwise matches:
// capsule-cli reports it unjoined, never agreed.
func TestConflatedSealersNeverAgreeWithoutAKeyPolicy(t *testing.T) {
	f := newFixture(t, "pos-x402-two-sided-agreed")
	f.in.Policy = nil
	f.reseal("x402-payee-observed", "payer")
	result := Verify(f.in)
	assert.Equal(t, "not_applied", result.KeyPolicy)
	assert.Equal(t, []Report{{Records: []string{"x402-payer-observed", "x402-payee-observed"}, Code: "sealer_conflation"}}, result.Failures)
	assert.Equal(t, PaymentUnjoined, result.Settlements[0].PaymentState)
	for _, l := range result.Legs {
		assert.NotEmpty(t, l.AuthenticatedKey, "every leg's authenticated key is returned for the caller's policy (§3)")
	}
}

// Pending on one side and settled on the other is a status mismatch. The
// in_progress finding capsule-cli once added on top is not derived by
// agent-action-capsule's verifier and is no longer reported.
func TestPendingAgainstSettledIsAStatusMismatchWithoutAFinding(t *testing.T) {
	result := newFixture(t, "pos-x402-two-sided-agreed").edit("x402-payee-observed", func(s map[string]any) { s["status"] = "pending" })
	assert.Equal(t, PaymentMismatch, result.Settlements[0].PaymentState)
	assert.Equal(t, []string{"status"}, result.Settlements[0].Differs)
	assert.Empty(t, result.Findings)
}

func TestSupersedesPicksTheLaterObservedLeg(t *testing.T) {
	f := newFixture(t, "pos-x402-two-sided-agreed")
	pending := f.edit("x402-payee-observed", func(s map[string]any) { s["status"] = "pending" })
	require.Equal(t, PaymentMismatch, pending.Settlements[0].PaymentState)
	earlier := f.leg("x402-payee-observed")
	earlierID := earlier.Capsule["capsule_id"].(string)

	later := Leg{Label: "x402-payee-observed-final", Capsule: deepCopy(t, earlier.Capsule)}
	later.Capsule["settlement"].(map[string]any)["status"] = "settled"
	later.Capsule["settlement"].(map[string]any)["observed_at"] = "2026-10-01T12:05:00Z"
	later.Capsule["chain"] = map[string]any{"parent_capsule_id": earlierID, "relation": "supersedes"}
	later.Capsule["action_id"] = "settle-payee-final"
	f.in.Legs = append(f.in.Legs, later)
	f.reseal("x402-payee-observed-final", "payee")
	result := Verify(f.in)
	require.True(t, result.Conforming, result.Failures)
	assert.Equal(t, PaymentAgreed, result.Settlements[0].PaymentState, "the head of the supersedes chain is the payee's observation")

	// A second leg that supersedes nothing leaves two payee heads, and they
	// disagree: a mismatch, never a silent pick. (capsule-cli once reported
	// observed_leg_not_unique and unjoined here.)
	f.leg("x402-payee-observed-final").Capsule["chain"] = map[string]any{"parent_capsule_id": earlierID, "relation": "follows"}
	f.reseal("x402-payee-observed-final", "payee")
	result = Verify(f.in)
	assert.Equal(t, PaymentMismatch, result.Settlements[0].PaymentState)
	assert.Equal(t, []string{"status"}, result.Settlements[0].Differs)
	assert.Empty(t, result.Findings)

	// A supersedes link counts only under the superseded leg's own key.
	f.in.Policy = nil
	f.leg("x402-payee-observed-final").Capsule["chain"] = map[string]any{"parent_capsule_id": earlierID, "relation": "supersedes"}
	f.reseal("x402-payee-observed-final", "payer")
	result = Verify(f.in)
	assert.NotEqual(t, PaymentAgreed, result.Settlements[0].PaymentState, "a supersedes link under another key is ignored")
}

// The seller's own delivered leg is a stated handoff, never proof of receipt:
// the sources behind a delivery state stay distinct and are reported.
func TestDeliveryEvidenceKeepsItsThreeSourcesDistinct(t *testing.T) {
	sellerOnly := newFixture(t, "pos-x402-two-sided-agreed")
	sellerOnly.in.Legs = without(sellerOnly.in.Legs, "x402-delivered-received")
	s := Verify(sellerOnly.in).Settlements[0]
	assert.Equal(t, DeliveryStated, s.DeliveryState)
	assert.Equal(t, []string{EvidenceSellerHandoff}, s.DeliveryEvidence, "a seller-sealed leg alone is the seller's claim")

	both := Verify(newFixture(t, "pos-x402-two-sided-agreed").in).Settlements[0]
	assert.Equal(t, DeliveryMatched, both.DeliveryState)
	assert.Equal(t, []string{EvidenceSellerHandoff, EvidenceBuyerReceipt}, both.DeliveryEvidence)

	buyerOnly := newFixture(t, "pos-x402-two-sided-agreed")
	buyerOnly.in.Legs = without(buyerOnly.in.Legs, "x402-delivered-sent")
	assert.Equal(t, []string{EvidenceBuyerReceipt}, Verify(buyerOnly.in).Settlements[0].DeliveryEvidence)

	carrier := newFixture(t, "pos-x402-two-sided-agreed")
	carrier.in.Legs = without(carrier.in.Legs, "x402-delivered-received")
	result := carrier.edit("x402-delivered-sent", func(s map[string]any) {
		s["wrapped"] = []any{map[string]any{"type": "delivery.proof", "digest_alg": "SHA-256", "digest": strings.Repeat("c", 64)}}
	})
	require.True(t, result.Conforming, result.Failures)
	assert.Equal(t, DeliveryStated, result.Settlements[0].DeliveryState, "a carrier document by digest does not make delivery matched")
	assert.Equal(t, []string{EvidenceSellerHandoff, EvidenceCarrierConfirmation}, result.Settlements[0].DeliveryEvidence)

	// A delivered leg that fails its own checks backs nothing.
	failing := newFixture(t, "pos-x402-two-sided-agreed")
	failing.leg("x402-delivered-received").Envelope = nil
	s = Verify(failing.in).Settlements[0]
	assert.Equal(t, DeliveryStated, s.DeliveryState)
	assert.Equal(t, []string{EvidenceSellerHandoff}, s.DeliveryEvidence)

	none := Verify(newFixture(t, "pos-x402-one-sided-payee").in).Settlements[0]
	assert.Equal(t, DeliveryNone, none.DeliveryState)
	assert.Nil(t, none.DeliveryEvidence)
}

// Both sides can agree with each other on what moved while both differ from
// the terms: the state is agreed (§9.2), and terms_amount says differs.
func TestAgreedPaymentThatDiffersFromTheTerms(t *testing.T) {
	f := newFixture(t, "pos-x402-two-sided-agreed")
	f.edit("x402-payer-observed", func(s map[string]any) { s["amount"].(map[string]any)["value"] = "1400000" })
	result := f.edit("x402-payee-observed", func(s map[string]any) { s["received"].(map[string]any)["value"] = "1400000" })
	require.True(t, result.Conforming, result.Failures)
	s := result.Settlements[0]
	assert.Equal(t, PaymentAgreed, s.PaymentState)
	assert.Equal(t, "differs", s.TermsAmount)
	assert.Equal(t, "equal", Verify(newFixture(t, "pos-x402-two-sided-agreed").in).Settlements[0].TermsAmount)
}

// duplicate appends a copy of a leg under a new label.
func (f *fixture) duplicate(label, as string) *Leg {
	original := f.leg(label)
	f.in.Legs = append(f.in.Legs, Leg{Label: as, Capsule: deepCopy(f.t, original.Capsule), Envelope: append([]byte(nil), original.Envelope...)})
	return f.leg(as)
}

// The verifier names a record by capsule_id. Two legs with one capsule_id,
// one valid and one not, are told apart: the failure names only the broken
// copy, and the valid copy still takes part, as each leg did on its own
// before.
func TestADuplicateCapsuleIDIsReportedPerLeg(t *testing.T) {
	f := newFixture(t, "pos-x402-two-sided-agreed")
	f.duplicate("x402-payee-observed", "payee-copy").Envelope = nil
	f.duplicate("x402-delivered-received", "received-copy").Envelope = nil
	result := Verify(f.in)
	assert.False(t, result.Conforming)
	assert.Equal(t, []Report{
		{Records: []string{"payee-copy"}, Code: "envelope_invalid"},
		{Records: []string{"received-copy"}, Code: "envelope_invalid"},
	}, result.Failures)
	require.Len(t, result.Settlements, 1)
	s := result.Settlements[0]
	assert.Equal(t, PaymentAgreed, s.PaymentState)
	assert.Equal(t, DeliveryMatched, s.DeliveryState)
	assert.Equal(t, []string{EvidenceSellerHandoff, EvidenceBuyerReceipt}, s.DeliveryEvidence, "the valid copy still backs the delivery")
	assert.Equal(t, LegKey{Record: "payee-copy"}, result.Legs[5])
	assert.Len(t, result.Legs[2].AuthenticatedKey, 64)

	// Re-signed under the payer's key: the copy is not authorized for the
	// payee role and is conflated with the payer's leg; the original is not.
	g := newFixture(t, "pos-x402-two-sided-agreed")
	g.duplicate("x402-payee-observed", "payee-resigned")
	g.reseal("payee-resigned", "payer")
	result = Verify(g.in)
	assert.Equal(t, []Report{
		{Records: []string{"payee-resigned"}, Code: "sealer_not_authorized_for_role"},
		{Records: []string{"x402-payer-observed", "payee-resigned"}, Code: "sealer_conflation"},
	}, result.Failures)
	assert.Equal(t, PaymentAgreed, result.Settlements[0].PaymentState)
}

// A copy whose content no longer matches its capsule_id fails
// capsule_invalid and has no identity of its own, even though its envelope
// and capsule_id are those of the valid original.
func TestAnInvalidDuplicateHasNoIdentity(t *testing.T) {
	f := newFixture(t, "pos-x402-two-sided-agreed")
	f.duplicate("x402-payee-observed", "payee-altered").Capsule["settlement"].(map[string]any)["status"] = "failed"
	result := Verify(f.in)
	assert.Equal(t, []Report{{Records: []string{"payee-altered"}, Code: "capsule_invalid"}}, result.Failures)
	assert.Equal(t, LegKey{Record: "payee-altered"}, result.Legs[5])
	assert.Equal(t, f.leg("x402-payee-observed").Capsule["capsule_id"], result.Legs[2].CapsuleID)
	assert.Len(t, result.Legs[2].AuthenticatedKey, 64)
	assert.Equal(t, PaymentAgreed, result.Settlements[0].PaymentState)
}

// terms_ref_unresolved is one entry per record: a leg re-sealed under
// another key is a second record, and each entry names one leg.
func TestTermsRefUnresolvedNamesEachRecordOnce(t *testing.T) {
	f := newFixture(t, "pos-x402-two-sided-agreed")
	f.in.Policy = nil
	f.duplicate("x402-delivered-received", "received-resigned")
	f.reseal("received-resigned", "payee")
	f.in.Legs = without(f.in.Legs, "x402-terms")
	result := Verify(f.in)
	var unresolved []Report
	for _, r := range result.Failures {
		if r.Code == "terms_ref_unresolved" {
			unresolved = append(unresolved, r)
		}
	}
	var want []Report
	for _, label := range []string{"x402-payer-observed", "x402-payee-observed", "x402-delivered-sent", "x402-delivered-received", "received-resigned"} {
		want = append(want, Report{Records: []string{label}, Code: "terms_ref_unresolved"})
	}
	assert.Equal(t, want, unresolved)
	assert.Empty(t, result.Settlements)
}

// An altered copy that keeps a conflated leg's capsule_id and envelope fails
// capsule_invalid and is not named in the conflation.
func TestAnInvalidDuplicateIsNotNamedInAConflation(t *testing.T) {
	f := newFixture(t, "pos-x402-two-sided-agreed")
	f.in.Policy = nil
	f.reseal("x402-payee-observed", "payer")
	f.duplicate("x402-payee-observed", "payee-altered").Capsule["settlement"].(map[string]any)["status"] = "failed"
	result := Verify(f.in)
	assert.Equal(t, []Report{
		{Records: []string{"payee-altered"}, Code: "capsule_invalid"},
		{Records: []string{"x402-payer-observed", "x402-payee-observed"}, Code: "sealer_conflation"},
	}, result.Failures)
}

// When a payer observation is superseded under its own key, only the head is
// compared, and only the head is named for its routing fee.
func TestFeeAssetDiffersNamesOnlyThePayerHead(t *testing.T) {
	f := newFixture(t, "pos-x402-two-sided-agreed")
	f.edit("x402-payer-observed", func(s map[string]any) {
		s["routing_fee"] = map[string]any{"value": "1", "assetCode": "USD", "assetScale": json.Number("2")}
	})
	earlierID := f.leg("x402-payer-observed").Capsule["capsule_id"].(string)
	final := f.duplicate("x402-payer-observed", "payer-final")
	final.Capsule["chain"] = map[string]any{"parent_capsule_id": earlierID, "relation": "supersedes"}
	final.Capsule["action_id"] = "settle-payer-final"
	final.Capsule["settlement"].(map[string]any)["observed_at"] = "2026-10-01T12:05:00Z"
	f.reseal("payer-final", "payer")
	result := Verify(f.in)
	require.True(t, result.Conforming, result.Failures)
	assert.Equal(t, PaymentUnjoined, result.Settlements[0].PaymentState)
	assert.Equal(t, []Report{{Records: []string{"payer-final"}, Code: "fee_asset_differs"}}, result.Findings)
}

// One capsule passed three times under two keys is two records: the third
// copy, under the first key, is reported with the first.
func TestACapsulePassedThreeTimesUnderTwoKeysIsTwoRecords(t *testing.T) {
	f := newFixture(t, "pos-x402-two-sided-agreed")
	f.in.Policy = nil
	f.edit("x402-payee-observed", func(s map[string]any) {
		s["wrapped"] = []any{map[string]any{"type": "x402.receipt", "digest_alg": "SHA-256", "digest": strings.Repeat("c", 64), "content": "eA"}}
	})
	f.duplicate("x402-payee-observed", "payee-under-payer-key")
	f.reseal("payee-under-payer-key", "payer")
	f.duplicate("x402-payee-observed", "payee-again")
	result := Verify(f.in)
	var mismatches []Report
	for _, r := range result.Failures {
		if r.Code == "wrapped_digest_mismatch" {
			mismatches = append(mismatches, r)
		}
	}
	assert.Equal(t, []Report{
		{Records: []string{"x402-payee-observed", "payee-again"}, Code: "wrapped_digest_mismatch"},
		{Records: []string{"payee-under-payer-key"}, Code: "wrapped_digest_mismatch"},
	}, mismatches)
}

// Two terms legs with one capsule_id answer the same legs: each is reported
// with the same state, as each terms leg was before.
func TestTwoTermsLegsWithOneCapsuleIDAreEachReported(t *testing.T) {
	f := newFixture(t, "pos-x402-two-sided-agreed")
	f.duplicate("x402-terms", "terms-copy")
	result := Verify(f.in)
	require.True(t, result.Conforming, result.Failures)
	require.Len(t, result.Settlements, 2)
	assert.Equal(t, "x402-terms", result.Settlements[0].Terms)
	assert.Equal(t, "terms-copy", result.Settlements[1].Terms)
	second := result.Settlements[1]
	second.Terms = "x402-terms"
	assert.Equal(t, result.Settlements[0], second)
	assert.Equal(t, PaymentAgreed, second.PaymentState)

	// A broken copy of the terms leg is not reported as a settlement.
	g := newFixture(t, "pos-x402-two-sided-agreed")
	g.duplicate("x402-terms", "terms-copy").Envelope = nil
	result = Verify(g.in)
	require.Len(t, result.Settlements, 1)
	assert.Equal(t, "x402-terms", result.Settlements[0].Terms)
	assert.Equal(t, []Report{{Records: []string{"terms-copy"}, Code: "envelope_invalid"}}, result.Failures)
}

// fee_asset_differs names the leg whose fee is in another asset.
func TestFeeAssetDiffersNamesTheLegWhoseFeeDiffers(t *testing.T) {
	payer := newFixture(t, "pos-x402-two-sided-agreed").edit("x402-payer-observed", func(s map[string]any) {
		s["routing_fee"] = map[string]any{"value": "1", "assetCode": "USD", "assetScale": json.Number("2")}
	})
	require.True(t, payer.Conforming, payer.Failures)
	assert.Equal(t, PaymentUnjoined, payer.Settlements[0].PaymentState)
	assert.Equal(t, []Report{{Records: []string{"x402-payer-observed"}, Code: "fee_asset_differs"}}, payer.Findings)

	payee := newFixture(t, "pos-x402-two-sided-agreed").edit("x402-payee-observed", func(s map[string]any) {
		s["receive_fee"].(map[string]any)["assetCode"] = "USD"
	})
	assert.Equal(t, []Report{{Records: []string{"x402-payee-observed"}, Code: "fee_asset_differs"}}, payee.Findings)
}

// A delivered leg naming a carrier with an empty name is accepted.
// (capsule-cli once reported it settlement_malformed.)
func TestAnEmptyCarrierNameIsAccepted(t *testing.T) {
	f := newFixture(t, "pos-x402-two-sided-agreed")
	result := f.edit("x402-delivered-sent", func(s map[string]any) {
		s["delivery"] = map[string]any{"direction": "sent", "carrier": ""}
	})
	assert.True(t, result.Conforming, result.Failures)
}

// A wrapped entry of an unregistered type is refused. (capsule-cli once
// accepted it.)
func TestAnUnregisteredWrappedTypeIsRefused(t *testing.T) {
	f := newFixture(t, "pos-x402-two-sided-agreed")
	result := f.edit("x402-delivered-sent", func(s map[string]any) {
		s["wrapped"] = []any{map[string]any{"type": "x-private.document", "digest_alg": "SHA-256", "digest": strings.Repeat("c", 64)}}
	})
	assert.False(t, result.Conforming)
	assert.Equal(t, []Report{{Records: []string{"x402-delivered-sent"}, Code: "settlement_malformed"}}, result.Failures)
}

// A payment reference of an unregistered type, the empty string included,
// is never rejected: the leg stays a valid record (the draft's never-reject
// rule), the pair cannot join, and payment_ref_type_unknown says why.
// (capsule-cli once reported an empty type settlement_malformed.)
func TestAnUnregisteredPaymentReferenceTypeNeverJoins(t *testing.T) {
	for _, ref := range []map[string]any{
		{"type": "", "value": "x"},
		{"type": "x-private.ref", "value": "x", "network": json.Number("5")},
	} {
		f := newFixture(t, "pos-x402-two-sided-agreed")
		result := f.edit("x402-payee-observed", func(s map[string]any) { s["payment_ref"] = ref })
		assert.True(t, result.Conforming, result.Failures)
		assert.Contains(t, result.Findings, Report{Records: []string{"x402-payee-observed"}, Code: "payment_ref_type_unknown"})
		require.Len(t, result.Settlements, 1)
		assert.Equal(t, "unjoined", result.Settlements[0].PaymentState)
	}
}

// A wrapped SD-JWT re-signed with the leg's own key is wrapped_resigned with
// or without disclosures: the signature covers the issuer JWS before the
// first "~". (capsule-cli's own verifier caught JWT~disclosure~; AAC go
// v0.9.0 did not, and v0.9.1 does.)
func TestAReSignedSDJWTIsWrappedResigned(t *testing.T) {
	for _, id := range []string{"neg-wrapped-sd-jwt-resigned-with-disclosure", "neg-wrapped-sd-jwt-resigned-no-disclosure"} {
		result := Verify(newFixture(t, id).in)
		assert.False(t, result.Conforming, id)
		assert.Equal(t, []Report{{Records: []string{"ap2-payee-observed"}, Code: "wrapped_resigned"}}, result.Failures, id)
	}
}
