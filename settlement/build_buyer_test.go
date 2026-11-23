package settlement

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	aacsettlement "github.com/action-state-group/agent-action-capsule/go/settlement"
	emit "github.com/action-state-group/capsule-emit/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	usdcBase    = "eip155:8453/erc20:0x833589fcd6edb6e08f4c7c32d4f71b54bda02913"
	payloadSig  = "0xdeadbeefcafe5ignature0123456789abcdef0123456789abcdef0123456789abcdef"
	testPayload = `{"x402Version":2,"payload":{"signature":"` + payloadSig + `","authorization":{"from":"` + testPayer + `","to":"` + testPayTo + `","value":"1500000"}}}`
)

func buyerTerms() Terms {
	return Terms{
		Amount:        Amount{Value: "1500000", AssetCode: usdcBase, AssetScale: 6},
		PaymentRef:    &PaymentRef{Type: "x402.transaction", Value: "tbd", Qualifiers: map[string]string{"network": "eip155:8453"}},
		ContentDigest: strings.Repeat("c", 64),
		ValidUntil:    "2026-10-02T00:00:00Z",
	}
}

func buyerPayment() X402Payment {
	in := x402Input("exact")
	return X402Payment{TermsRef: in.TermsRef, SettleResponse: in.SettleResponse, Requirements: in.Requirements, ObservedAt: in.ObservedAt}
}

// seal signs a built member as its sealer, as a Capsule of its own.
func seal(t *testing.T, label string, seed byte, b Built) (Leg, string) {
	t.Helper()
	capsule := map[string]any{
		"spec_version": "draft-mih-scitt-agent-action-capsule-05", "format_version": "4", "canonicalization_id": "jcs",
		"action_id": label, "action_type": "fyi", "operator": "buyer.example", "developer": "test@1",
		"timestamp":  "2026-10-01T12:00:00Z",
		"assurance":  map[string]any{"attestation_mode": "self_attested", "effect_mode": "not_applicable", "ledger_mode": "standalone"},
		"settlement": b.Member,
	}
	id, err := canonical.ComputeCapsuleID(capsule)
	require.NoError(t, err)
	capsule["capsule_id"] = id
	encoded, err := canonical.JCS(capsule)
	require.NoError(t, err)
	key := ed25519.NewKeyFromSeed([]byte(strings.Repeat(string(rune('a'+seed)), 32)))
	identity, err := emit.NewEd25519SigningIdentity(key)
	require.NoError(t, err)
	env, err := emit.Sign(emit.BuiltPayload{CapsuleID: id, Value: capsule, JSON: encoded}, identity)
	require.NoError(t, err)
	return Leg{Label: label, Capsule: capsule, Envelope: env}, hex.EncodeToString(key.Public().(ed25519.PublicKey))
}

// A buyer builds the terms, the payer-observed and the delivered received
// legs; the seller builds the payee-observed and a delivered sent leg; the
// verifier reads the six legs as one agreed, matched exchange.
func TestBuyerLegsRoundTripThroughVerify(t *testing.T) {
	pinned := buyerTerms()
	pinned.ContentDigest = strings.Repeat("d", 64) // the terms pin the deliverable the delivered legs must match
	terms, err := TermsPayer(pinned)
	require.NoError(t, err)
	termsLeg, payerKey := seal(t, "terms", 1, terms)
	termsID := termsLeg.Capsule["capsule_id"].(string)

	pay := buyerPayment()
	pay.TermsRef = termsID
	payer, err := PayerObservedX402(pay)
	require.NoError(t, err)
	payerLeg, _ := seal(t, "payer-observed", 1, payer)

	payeeIn := x402Input("exact")
	payeeIn.TermsRef = termsID
	payee, err := PayeeObservedX402(payeeIn)
	require.NoError(t, err)
	payeeLeg, payeeKey := seal(t, "payee-observed", 2, payee)

	digest := strings.Repeat("d", 64)
	received, err := DeliveredReceived(Delivery{TermsRef: termsID, ObservedAt: "2026-10-01T12:00:09Z", ContentDigest: digest})
	require.NoError(t, err)
	receivedLeg, _ := seal(t, "delivered-received", 1, received)
	sent, err := DeliveredSent(Delivery{TermsRef: termsID, ObservedAt: "2026-10-01T12:00:08Z", ContentDigest: digest})
	require.NoError(t, err)
	sentLeg, _ := seal(t, "delivered-sent", 2, sent)

	for _, b := range []Built{terms, payer, received} {
		assert.Empty(t, aacsettlement.StructureFailures(b.Member))
		assert.Equal(t, RolePayer, b.Member["sealer_role"])
	}
	objects := map[string][]byte{}
	for _, b := range []Built{payer, payee} {
		for d, o := range b.Objects {
			objects[d] = o
		}
	}
	in := Input{
		Legs:    []Leg{termsLeg, payerLeg, payeeLeg, receivedLeg, sentLeg},
		Objects: objects,
		Policy:  map[string][]string{RolePayer: {payerKey}, RolePayee: {payeeKey}},
	}
	result := Verify(in)
	require.True(t, result.Conforming, result.Failures)
	require.Len(t, result.Settlements, 1)
	s := result.Settlements[0]
	assert.Equal(t, PaymentAgreed, s.PaymentState)
	assert.Equal(t, "equal", s.TermsAmount)
	assert.Equal(t, DeliveryMatched, s.DeliveryState)
	assert.Contains(t, s.DeliveryEvidence, EvidenceBuyerReceipt)

	// Without the buyer's terms leg nothing is agreed on: the legs name a
	// terms_ref that resolves to no terms.
	in.Legs = without(in.Legs, "terms")
	assert.Contains(t, codes(Verify(in).Failures), "terms_ref_unresolved")

	// A received digest that differs from the sent one is a mismatch.
	other, err := DeliveredReceived(Delivery{TermsRef: termsID, ObservedAt: "2026-10-01T12:00:09Z", ContentDigest: strings.Repeat("e", 64)})
	require.NoError(t, err)
	otherLeg, _ := seal(t, "delivered-received", 1, other)
	in.Legs = append(without(in.Legs, "delivered-received"), termsLeg, otherLeg)
	assert.Equal(t, DeliveryMismatch, Verify(in).Settlements[0].DeliveryState)
}

func TestTermsPayerBuildsAndRefuses(t *testing.T) {
	b, err := TermsPayer(buyerTerms())
	require.NoError(t, err)
	assert.Equal(t, LegTerms, b.Member["leg"])
	assert.NotContains(t, b.Member, "terms_ref", "a terms leg answers no terms")
	assert.Equal(t, map[string]any{"type": "x402.transaction", "value": "tbd", "network": "eip155:8453"}, b.Member["payment_ref"])
	assert.Equal(t, map[string]any{"content_digest": strings.Repeat("c", 64)}, b.Member["deliverable"])

	minimal, err := TermsPayer(Terms{Amount: Amount{Value: "0", AssetCode: usdcBase, AssetScale: 6}})
	require.NoError(t, err)
	assert.NotContains(t, minimal.Member, "deliverable")

	for name, change := range map[string]func(*Terms){
		"decimal amount":      func(in *Terms) { in.Amount.Value = "1.5" },
		"leading zero":        func(in *Terms) { in.Amount.Value = "01" },
		"no asset":            func(in *Terms) { in.Amount.AssetCode = "" },
		"scale out of range":  func(in *Terms) { in.Amount.AssetScale = 256 },
		"bad deliverable":     func(in *Terms) { in.ContentDigest = "abc" },
		"local validity time": func(in *Terms) { in.ValidUntil = "2026-10-02T00:00:00+02:00" },
		"missing qualifier":   func(in *Terms) { in.PaymentRef.Qualifiers = nil },
		"qualifier replaces the value": func(in *Terms) {
			in.PaymentRef.Qualifiers = map[string]string{"network": "eip155:8453", "value": "other"}
		},
		"qualifier replaces the type": func(in *Terms) {
			in.PaymentRef.Qualifiers = map[string]string{"network": "eip155:8453", "type": "ln.payment_hash"}
		},
		"account hidden as an assetCode qualifier": func(in *Terms) {
			in.PaymentRef = &PaymentRef{Type: "other.rail", Value: "x", Qualifiers: map[string]string{"assetCode": testPayer}}
			in.AccountIDs = []string{testPayer}
		},
		"account in the ref": func(in *Terms) {
			in.PaymentRef.Value = "to " + testPayer
			in.AccountIDs = []string{testPayer}
		},
	} {
		in := buyerTerms()
		change(&in)
		_, err := TermsPayer(in)
		assert.Error(t, err, name)
	}
}

func TestPayerObservedX402ReadsTheBuyersObjects(t *testing.T) {
	in := buyerPayment()
	in.PaymentPayload = []byte(testPayload)
	fee := "7"
	in.RoutingFee = &fee
	b, err := PayerObservedX402(in)
	require.NoError(t, err)
	assert.Empty(t, aacsettlement.StructureFailures(b.Member))
	assert.Equal(t, LegPayerObserved, b.Member["leg"])
	assert.Equal(t, RolePayer, b.Member["sealer_role"])
	assert.Equal(t, "settled", b.Member["status"])
	assert.Equal(t, "1500000", b.Member["amount"].(map[string]any)["value"])
	assert.Equal(t, "7", b.Member["routing_fee"].(map[string]any)["value"], "an exact payment may still pay an intermediary")
	assert.Equal(t, strings.ToLower(testTx), b.Member["payment_ref"].(map[string]any)["value"])
	assert.ElementsMatch(t, []string{testPayTo, testPayer, testPayer, testPayTo}, b.AccountIDs)

	// The payment payload is a bearer instrument until it settles: bound by
	// digest only, its signature and contents never in the leg.
	wrapped := b.Member["wrapped"].([]any)
	require.Len(t, wrapped, 2)
	w := wrapped[1].(map[string]any)
	assert.Equal(t, "x402.payment-payload", w["type"])
	assert.NotContains(t, w, "content")
	assert.Equal(t, []byte(testPayload), b.Objects[w["digest"].(string)])
	encoded, err := b.JSON()
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), payloadSig)
	assert.NotContains(t, strings.ToLower(string(encoded)), strings.ToLower(testPayer))

	// The status is the facilitator's, nothing else.
	failed := buyerPayment()
	failed.SettleResponse = []byte(strings.Replace(string(failed.SettleResponse), "true", "false", 1))
	f, err := PayerObservedX402(failed)
	require.NoError(t, err)
	assert.Equal(t, "failed", f.Member["status"])

	plain, err := PayerObservedX402(buyerPayment())
	require.NoError(t, err)
	assert.NotContains(t, plain.Member, "routing_fee")
	assert.Len(t, plain.Member["wrapped"], 1)
}

func TestPayerObservedX402Refusals(t *testing.T) {
	fee := func(v string) *string { return &v }
	for name, change := range map[string]func(*X402Payment){
		"no settlement response: never settled": func(in *X402Payment) { in.SettleResponse = nil },
		"not a settle response":                 func(in *X402Payment) { in.SettleResponse = []byte(`{"transaction":"x"}`) },
		"no transaction": func(in *X402Payment) {
			in.SettleResponse = []byte(`{"success":false,"network":"eip155:8453"}`)
		},
		"other network": func(in *X402Payment) {
			in.SettleResponse = []byte(`{"success":true,"transaction":"0x1","network":"eip155:1"}`)
		},
		"upto: the paid amount is not in the requirements": func(in *X402Payment) {
			in.Requirements = []byte(strings.Replace(string(in.Requirements), `"exact"`, `"upto"`, 1))
		},
		"decimal routing fee":    func(in *X402Payment) { in.RoutingFee = fee("0.5") },
		"terms_ref not a digest": func(in *X402Payment) { in.TermsRef = "abc" },
		"local observed time":    func(in *X402Payment) { in.ObservedAt = "2026-10-01T12:00:05+02:00" },
		"account in clear": func(in *X402Payment) {
			in.AccountIDs = []string{"0x5A1F"} // the transaction hash stands in for an identifier a caller knows
		},
	} {
		in := buyerPayment()
		change(&in)
		_, err := PayerObservedX402(in)
		assert.Error(t, err, name)
	}
}

// An account only the payment payload names (the buyer's wallet) is guarded
// too: it enters the leg nowhere in clear.
func TestPayloadRules(t *testing.T) {
	failed := buyerPayment()
	failed.SettleResponse = []byte(strings.Replace(string(failed.SettleResponse), "true", "false", 1))
	failed.PaymentPayload = []byte(testPayload)
	_, err := PayerObservedX402(failed)
	assert.ErrorContains(t, err, "live authorization", "a payload of a payment that did not settle is never kept")

	other := buyerPayment()
	other.PaymentPayload = []byte(`{"accepted":{"scheme":"upto"},"payload":{}}`)
	_, err = PayerObservedX402(other)
	assert.ErrorContains(t, err, "another scheme")
}

func TestPayerObservedX402OnANonEVMNetwork(t *testing.T) {
	in := buyerPayment()
	in.SettleResponse = []byte(`{"success":true,"transaction":"5Sig","network":"solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdp"}`)
	in.Requirements = []byte(`{"scheme":"exact","network":"solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdp","amount":"100","asset":"MintAddr","payTo":"PayToAddr"}`)
	scale := 6
	in.AssetScale = &scale
	b, err := PayerObservedX402(in)
	require.NoError(t, err)
	assert.Equal(t, "solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdp/token:MintAddr", b.Member["amount"].(map[string]any)["assetCode"])
	assert.Equal(t, "5Sig", b.Member["payment_ref"].(map[string]any)["value"], "only EVM hashes are lowercased")
}

func TestPayloadOnlyAccountIsGuarded(t *testing.T) {
	in := buyerPayment()
	in.PaymentPayload = []byte(`{"payload":{"authorization":{"from":"0x9999999999999999999999999999999999999ccc"}}}`)
	b, err := PayerObservedX402(in)
	require.NoError(t, err)
	assert.Contains(t, b.AccountIDs, "0x9999999999999999999999999999999999999ccc")
	b.Member["payment_ref"].(map[string]any)["value"] = "0x9999999999999999999999999999999999999CCC"
	assert.Error(t, NeverEnters(b.Member, b.AccountIDs))
}

func TestDeliveredReceivedIsTheBuyersObservation(t *testing.T) {
	b, err := DeliveredReceived(Delivery{TermsRef: strings.Repeat("a", 64), ObservedAt: "2026-10-01T12:00:05Z", ContentDigest: strings.Repeat("d", 64)})
	require.NoError(t, err)
	assert.Equal(t, "received", b.Member["delivery"].(map[string]any)["direction"])
	assert.Equal(t, RolePayer, b.Member["sealer_role"])
	assert.Empty(t, aacsettlement.StructureFailures(b.Member))

	base := Delivery{TermsRef: strings.Repeat("a", 64), ObservedAt: "2026-10-01T12:00:05Z", ContentDigest: strings.Repeat("d", 64)}
	for name, change := range map[string]func(*Delivery){
		"nothing received":   func(in *Delivery) { in.ContentDigest = "" },
		"bad digest":         func(in *Delivery) { in.ContentDigest = "abc" },
		"no terms_ref":       func(in *Delivery) { in.TermsRef = "" },
		"no observed time":   func(in *Delivery) { in.ObservedAt = "" },
		"address in carrier": func(in *Delivery) { in.Carrier, in.AccountIDs = "to "+testPayer, []string{testPayer} },
	} {
		in := base
		change(&in)
		_, err := DeliveredReceived(in)
		assert.Error(t, err, name)
	}
}

// The builders fix role and direction, so a wrong pairing is checked on a
// hand-built member through the same check every leg passes.
func TestAWrongRoleOrDirectionIsRefused(t *testing.T) {
	sent, err := DeliveredSent(Delivery{TermsRef: strings.Repeat("a", 64), ObservedAt: "2026-10-01T12:00:05Z", ContentDigest: strings.Repeat("d", 64)})
	require.NoError(t, err)
	received, err := DeliveredReceived(Delivery{TermsRef: strings.Repeat("a", 64), ObservedAt: "2026-10-01T12:00:05Z", ContentDigest: strings.Repeat("d", 64)})
	require.NoError(t, err)
	payer, err := PayerObservedX402(buyerPayment())
	require.NoError(t, err)

	wrong := func(b Built, change func(m map[string]any)) error {
		member := map[string]any{}
		for k, v := range b.Member {
			member[k] = v
		}
		change(member)
		return Built{Member: member}.check()
	}
	assert.Error(t, wrong(received, func(m map[string]any) { m["sealer_role"] = RolePayee }), "received sealed by the payee")
	assert.Error(t, wrong(sent, func(m map[string]any) { m["sealer_role"] = RolePayer }), "sent sealed by the payer")
	assert.Error(t, wrong(payer, func(m map[string]any) { m["sealer_role"] = RolePayee }), "payer_observed sealed by the payee")
	for _, member := range []string{"terms_ref", "amount", "payment_ref", "status", "observed_at"} {
		assert.Error(t, wrong(payer, func(m map[string]any) { delete(m, member) }), "payer_observed without "+member)
	}
	terms, err := TermsPayer(buyerTerms())
	require.NoError(t, err)
	assert.Error(t, wrong(terms, func(m map[string]any) { delete(m, "amount") }), "terms without amount")
	assert.Error(t, wrong(terms, func(m map[string]any) { m["terms_ref"] = strings.Repeat("a", 64) }), "a terms leg cites no terms")
}

func TestBuilderLegsAreValidJSON(t *testing.T) {
	b, err := TermsPayer(buyerTerms())
	require.NoError(t, err)
	encoded, err := b.JSON()
	require.NoError(t, err)
	assert.True(t, json.Valid(encoded))
}
