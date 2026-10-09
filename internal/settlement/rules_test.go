package settlement

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"testing"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	emit "github.com/action-state-group/capsule-emit-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestEachRuleMovesTheDerivedState(t *testing.T) {
	const payee = "x402-payee-observed"
	base := newFixture(t, "pos-x402-two-sided-agreed")
	assert.Equal(t, PaymentAgreed, Verify(base.in).Settlements[0].PaymentState)

	status := newFixture(t, "pos-x402-two-sided-agreed").edit(payee, func(s map[string]any) { s["status"] = "pending" })
	assert.Equal(t, []string{"status"}, status.Settlements[0].Differs)
	assert.Equal(t, []string{CodeInProgress}, codes(status.Findings), "pending against settled is flagged in flight, still a -00 mismatch")

	failed := newFixture(t, "pos-x402-two-sided-agreed").edit(payee, func(s map[string]any) { s["status"] = "failed" })
	assert.Equal(t, PaymentMismatch, failed.Settlements[0].PaymentState)
	assert.Empty(t, failed.Findings)

	tx := base.settlement(payee)["payment_ref"].(map[string]any)["value"].(string)
	other := newFixture(t, "pos-x402-two-sided-agreed").edit(payee, func(s map[string]any) {
		s["payment_ref"].(map[string]any)["value"] = tx[:len(tx)-1] + map[bool]string{true: "0", false: "f"}[tx[len(tx)-1] == 'f']
	})
	assert.Equal(t, []string{"payment_ref"}, other.Settlements[0].Differs)

	upper := newFixture(t, "pos-x402-two-sided-agreed").edit(payee, func(s map[string]any) {
		s["payment_ref"].(map[string]any)["value"] = "0x" + upperHex(tx[2:])
	})
	assert.Equal(t, PaymentAgreed, upper.Settlements[0].PaymentState, "eip155 hashes compare in their lowercase normal form")

	oneUnit := newFixture(t, "pos-x402-two-sided-agreed").edit(payee, func(s map[string]any) {
		s["received"].(map[string]any)["value"] = "1500001"
	})
	assert.Equal(t, []string{"amount"}, oneUnit.Settlements[0].Differs, "no tolerance")

	role := newFixture(t, "pos-x402-two-sided-agreed").edit(payee, func(s map[string]any) { s["sealer_role"] = "payer" })
	assert.Equal(t, CodeLegRoleMismatch, role.Failures[0].Code)

	decimal := newFixture(t, "pos-x402-two-sided-agreed").edit(payee, func(s map[string]any) {
		s["received"].(map[string]any)["value"] = "1.5"
	})
	assert.Equal(t, CodeAmountNotExact, decimal.Failures[0].Code)

	// x402 exact declares receive fees not applicable: absent reads as zero...
	exact := newFixture(t, "pos-x402-two-sided-agreed").edit(payee, func(s map[string]any) { delete(s, "receive_fee") })
	assert.Equal(t, PaymentAgreed, exact.Settlements[0].PaymentState)
	// ...only when the scheme is established from octets the verifier holds.
	blind := newFixture(t, "pos-x402-two-sided-agreed")
	blind.in.Objects = nil
	result := blind.edit(payee, func(s map[string]any) { delete(s, "receive_fee") })
	assert.Equal(t, PaymentUnjoined, result.Settlements[0].PaymentState)
	assert.Equal(t, []string{CodeFeeUnstated}, codes(result.Findings))

	feeAsset := newFixture(t, "pos-x402-two-sided-agreed").edit(payee, func(s map[string]any) {
		s["receive_fee"].(map[string]any)["assetCode"] = "USD"
	})
	assert.Equal(t, PaymentUnjoined, feeAsset.Settlements[0].PaymentState)
	assert.Equal(t, []string{CodeFeeAssetDiffers}, codes(feeAsset.Findings))

	unknownRef := newFixture(t, "pos-x402-two-sided-agreed").edit(payee, func(s map[string]any) {
		s["payment_ref"] = map[string]any{"type": "ln.payment_hash", "value": tx}
	})
	assert.Equal(t, PaymentUnjoined, unknownRef.Settlements[0].PaymentState, "references of different types never join")
}

func upperHex(s string) string {
	out := []byte(s)
	for i, c := range out {
		if c >= 'a' && c <= 'f' {
			out[i] = c - 'a' + 'A'
		}
	}
	return string(out)
}

func TestTheReceiveFeeRuleNotNaiveEqualityDecidesAgreement(t *testing.T) {
	run := func(change func(s map[string]any)) string {
		f := newFixture(t, "pos-ln-receive-fee-two-payments")
		return f.edit("ln-payee-observed-1", change).Settlements[0].PaymentState
	}
	setFee := func(v string) func(map[string]any) {
		return func(s map[string]any) { s["receive_fee"].(map[string]any)["value"] = v }
	}
	assert.Equal(t, PaymentAgreed, run(func(map[string]any) {}))
	assert.Equal(t, PaymentMismatch, run(setFee("0")))
	assert.Equal(t, PaymentMismatch, run(setFee("4")))
	assert.Equal(t, PaymentMismatch, run(setFee("6")))
	assert.Equal(t, PaymentMismatch, run(func(s map[string]any) { s["received"].(map[string]any)["value"] = "1000" }))
	assert.Equal(t, PaymentUnjoined, run(func(s map[string]any) { delete(s, "receive_fee") }), "Lightning fees may apply: absent is not zero")
}

func TestConflatedSealersNeverAgreeWithoutAKeyPolicy(t *testing.T) {
	f := newFixture(t, "pos-x402-two-sided-agreed")
	f.in.Policy = nil
	f.reseal("x402-payee-observed", "payer")
	result := Verify(f.in)
	assert.Equal(t, "not_applied", result.KeyPolicy)
	assert.Equal(t, []string{CodeSealerConflation}, codes(result.Failures))
	assert.Equal(t, PaymentUnjoined, result.Settlements[0].PaymentState)
	for _, l := range result.Legs {
		assert.NotEmpty(t, l.AuthenticatedKey, "every leg's authenticated key is returned for the caller's policy (§3)")
	}
}

func TestAMissingEnvelopeIsReportedAndExcludesTheLeg(t *testing.T) {
	f := newFixture(t, "pos-x402-two-sided-agreed")
	f.leg("x402-payee-observed").Envelope = nil
	result := Verify(f.in)
	assert.Equal(t, []Report{{Records: []string{"x402-payee-observed"}, Code: CodeEnvelopeInvalid}}, result.Failures)
	assert.Equal(t, PaymentPayerStated, result.Settlements[0].PaymentState)
}

func TestTermsRefMustResolve(t *testing.T) {
	f := newFixture(t, "pos-x402-two-sided-agreed")
	result := f.edit("x402-payee-observed", func(s map[string]any) { s["terms_ref"] = repeat("a", 64) })
	assert.Equal(t, []string{CodeTermsRefUnresolved}, codes(result.Failures))
	assert.Equal(t, PaymentPayerStated, result.Settlements[0].PaymentState)
}

func repeat(s string, n int) string {
	out := ""
	for range n {
		out += s
	}
	return out
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

	// A second leg that supersedes nothing leaves two candidates: never a
	// silent pick.
	f.leg("x402-payee-observed-final").Capsule["chain"] = map[string]any{"parent_capsule_id": earlierID, "relation": "follows"}
	f.reseal("x402-payee-observed-final", "payee")
	result = Verify(f.in)
	assert.Equal(t, PaymentUnjoined, result.Settlements[0].PaymentState)
	assert.Equal(t, []string{CodeObservedLegNotUnique}, codes(result.Findings))
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
		s["wrapped"] = []any{map[string]any{"type": "delivery.proof", "digest_alg": "SHA-256", "digest": repeat("c", 64)}}
	})
	require.True(t, result.Conforming, result.Failures)
	assert.Equal(t, DeliveryStated, result.Settlements[0].DeliveryState, "a carrier document by digest does not make delivery matched")
	assert.Equal(t, []string{EvidenceSellerHandoff, EvidenceCarrierConfirmation}, result.Settlements[0].DeliveryEvidence)

	none := Verify(newFixture(t, "pos-x402-one-sided-payee").in).Settlements[0]
	assert.Equal(t, DeliveryNone, none.DeliveryState)
	assert.Nil(t, none.DeliveryEvidence)
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

func TestStructureRules(t *testing.T) {
	ok := func(s map[string]any) bool { return len(structureFailures(s)) == 0 }
	payee := func() map[string]any {
		return map[string]any{
			"version": "0", "leg": "payee_observed", "sealer_role": "payee", "terms_ref": repeat("a", 64),
			"received":    map[string]any{"value": "995", "assetCode": "BTC", "assetScale": json.Number("11")},
			"receive_fee": map[string]any{"value": "5", "assetCode": "BTC", "assetScale": json.Number("11")},
			"payment_ref": map[string]any{"type": "ln.payment_hash", "value": repeat("b", 64)},
			"status":      "settled", "observed_at": "2026-10-01T12:00:05Z",
		}
	}
	assert.True(t, ok(payee()))
	for name, change := range map[string]func(map[string]any){
		"no version":        func(s map[string]any) { delete(s, "version") },
		"forbidden member":  func(s map[string]any) { s["amount"] = s["received"] },
		"stated state":      func(s map[string]any) { s["state"] = "agreed" },
		"unknown status":    func(s map[string]any) { s["status"] = "done" },
		"local time":        func(s map[string]any) { s["observed_at"] = "2026-10-01T12:00:05+02:00" },
		"short terms_ref":   func(s map[string]any) { s["terms_ref"] = "abc" },
		"qualifier missing": func(s map[string]any) { s["payment_ref"] = map[string]any{"type": "x402.transaction", "value": "0x1"} },
		"extra qualifier":   func(s map[string]any) { s["payment_ref"].(map[string]any)["network"] = "x" },
		"non-string ref":    func(s map[string]any) { s["payment_ref"].(map[string]any)["value"] = json.Number("1") },
		"wrapped extra": func(s map[string]any) {
			s["wrapped"] = []any{map[string]any{"type": "x", "digest_alg": "SHA-256", "digest": repeat("a", 64), "note": "x"}}
		},
		"wrapped bad content": func(s map[string]any) {
			s["wrapped"] = []any{map[string]any{"type": "x", "digest_alg": "SHA-256", "digest": repeat("a", 64), "content": "a+b="}}
		},
		"wrapped alg": func(s map[string]any) {
			s["wrapped"] = []any{map[string]any{"type": "x", "digest_alg": "SHA-1", "digest": repeat("a", 64)}}
		},
	} {
		s := payee()
		change(s)
		assert.Equal(t, []string{CodeSettlementMalformed}, structureFailures(s), name)
	}
	// An unregistered x- type is well-formed: it is a finding, not a failure.
	s := payee()
	s["payment_ref"] = map[string]any{"type": "x-private", "value": "1", "scope": "y"}
	assert.True(t, ok(s))

	delivered := func(d map[string]any) map[string]any {
		return map[string]any{"version": "0", "leg": "delivered", "sealer_role": "payee", "terms_ref": repeat("a", 64), "observed_at": "2026-10-01T12:00:05Z", "delivery": d}
	}
	assert.True(t, ok(delivered(map[string]any{"direction": "sent", "content_digest": repeat("d", 64)})))
	assert.True(t, ok(delivered(map[string]any{"direction": "sent", "carrier": "ups", "status": "in_transit", "tracking_digest": repeat("e", 64), "shipped_at": "2026-10-01T12:00:05Z"})))
	assert.False(t, ok(delivered(map[string]any{"direction": "received", "content_digest": repeat("d", 64)})), "a payee never seals a received leg")
	assert.False(t, ok(delivered(map[string]any{"direction": "sent"})), "a content digest or a carrier")
	assert.False(t, ok(delivered(map[string]any{"direction": "sent", "carrier": "ups", "tracking_number": "1Z999"})), "the tracking number itself is never carried")
	assert.False(t, ok(delivered(map[string]any{"direction": "sent", "carrier": "ups", "status": "lost"})))
}

func TestAmountsAreExact(t *testing.T) {
	amount := func(value string, scale string) map[string]any {
		return map[string]any{"value": value, "assetCode": "X", "assetScale": json.Number(scale)}
	}
	for _, bad := range []map[string]any{
		amount("01", "0"), amount("-1", "0"), amount("1.5", "6"), amount("1e3", "0"), amount(" 1", "0"),
		amount("1", "256"), amount("1", "1.0"), amount("1", "-1"),
		{"value": json.Number("1"), "assetCode": "X", "assetScale": json.Number("0")},
		{"value": "1", "assetCode": "X", "assetScale": true},
		{"value": "1", "assetCode": "", "assetScale": json.Number("0")},
		{"value": "1", "assetCode": "X", "assetScale": json.Number("0"), "note": "x"},
	} {
		_, exact := parseAmount(bad)
		assert.False(t, exact, bad)
	}
	parse := func(value, scale string) Amount {
		a, exact := parseAmount(amount(value, scale))
		require.True(t, exact)
		return a
	}
	assert.True(t, parse("1500000", "6").Equal(parse("150000000", "8")))
	assert.False(t, parse("1500000", "6").Equal(parse("150000001", "8")))
	assert.True(t, parse("123456", "2").Equal(parse("1234560", "3")))
	assert.True(t, parse("0", "255").Equal(parse("0", "0")))
	huge := parse("123456789012345678901234567890", "0")
	assert.Equal(t, 0, huge.Value.Cmp(func() *big.Int { v, _ := new(big.Int).SetString("123456789012345678901234567890", 10); return v }()), "beyond 64 bits, exactly")
	assert.True(t, amountRuleHolds(parse("1000", "11"), parse("995", "11"), parse("50", "12")))
	other := parse("5", "11")
	other.Asset = "Y"
	assert.False(t, amountRuleHolds(parse("1000", "11"), parse("995", "11"), other))
}

func TestTheX402SchemeIsReadFromHeldOctetsOnly(t *testing.T) {
	jws := func(payload string) []byte {
		return []byte("eyJhbGciOiJFZERTQSJ9." + base64URL(payload) + ".c2ln")
	}
	assert.Equal(t, "exact", offerScheme(jws(`{"scheme":"exact"}`)))
	assert.Equal(t, "", offerScheme([]byte("a.!!.c")))
	assert.Equal(t, "upto", offerScheme([]byte(`{"format":"eip712","payload":{"scheme":"upto"},"signature":"0x00"}`)))
	assert.Equal(t, "", offerScheme([]byte(`not json`)))

	digest := repeat("f", 64)
	payer := leg{s: map[string]any{"wrapped": []any{map[string]any{"type": "x402.payment-payload", "digest": digest}}}}
	assert.Equal(t, "", x402Scheme([]leg{payer}, nil), "octets not held: the scheme is not established")
	assert.Equal(t, "exact", x402Scheme([]leg{payer}, map[string][]byte{digest: []byte(`{"accepted":{"scheme":"exact"}}`)}))
	assert.Equal(t, "", x402Scheme([]leg{payer}, map[string][]byte{digest: []byte(`[`)}))
	inline := leg{s: map[string]any{"wrapped": []any{map[string]any{"type": "x402.offer", "digest": digest, "content": base64URL(string(jws(`{"scheme":"exact"}`)))}}}}
	assert.Equal(t, "exact", x402Scheme([]leg{inline}, nil))
}

func base64URL(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
