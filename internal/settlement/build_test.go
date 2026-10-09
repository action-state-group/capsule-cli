package settlement

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testPayTo = "0x1111111111111111111111111111111111111aaa"
	testPayer = "0x2222222222222222222222222222222222222bbb"
	testTx    = "0x5A1F00000000000000000000000000000000000000000000000000000000C3D2"
)

func x402Input(scheme string) X402Receipt {
	return X402Receipt{
		TermsRef:       strings.Repeat("a", 64),
		ObservedAt:     "2026-10-01T12:00:05Z",
		SettleResponse: []byte(`{"success":true,"transaction":"` + testTx + `","network":"eip155:8453","payer":"` + testPayer + `"}`),
		Requirements: []byte(`{"scheme":"` + scheme + `","network":"eip155:8453","amount":"1500000","asset":"0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913","payTo":"` +
			testPayTo + `","maxTimeoutSeconds":60}`),
	}
}

func TestPayeeObservedX402ReadsEveryValueFromTheRail(t *testing.T) {
	b, err := PayeeObservedX402(x402Input("exact"))
	require.NoError(t, err)
	assert.Empty(t, structureFailures(b.Member))
	assert.Equal(t, LegPayeeObserved, b.Member["leg"])
	assert.Equal(t, "settled", b.Member["status"])
	ref := b.Member["payment_ref"].(map[string]any)
	assert.Equal(t, strings.ToLower(testTx), ref["value"], "eip155 hashes in their lowercase normal form")
	assert.Equal(t, "eip155:8453", ref["network"])
	received, _ := parseAmount(b.Member["received"])
	assert.Equal(t, "eip155:8453/erc20:0x833589fcd6edb6e08f4c7c32d4f71b54bda02913", received.Asset)
	assert.Equal(t, 6, received.Scale)
	assert.Equal(t, "1500000", received.Value.String())
	fee, _ := parseAmount(b.Member["receive_fee"])
	assert.Equal(t, "0", fee.Value.String(), "exact: recorded explicitly as zero")
	assert.Equal(t, []string{testPayTo, testPayer}, b.AccountIDs)

	wrapped := b.Member["wrapped"].([]any)
	require.Len(t, wrapped, 1)
	w := wrapped[0].(map[string]any)
	assert.Equal(t, "x402.settle-response", w["type"])
	assert.NotContains(t, w, "content", "the rail's objects carry account identifiers: never their content")
	assert.Equal(t, x402Input("exact").SettleResponse, b.Objects[w["digest"].(string)])

	encoded, err := b.JSON()
	require.NoError(t, err)
	assert.NotContains(t, strings.ToLower(string(encoded)), strings.ToLower(testPayTo))
	assert.NotContains(t, strings.ToLower(string(encoded)), strings.ToLower(testPayer))

	in := x402Input("exact")
	in.SettleResponse = []byte(strings.Replace(string(in.SettleResponse), "true", "false", 1))
	failed, err := PayeeObservedX402(in)
	require.NoError(t, err)
	assert.Equal(t, "failed", failed.Member["status"])

	in = x402Input("exact")
	in.Receipt = []byte("eyJhbGciOiJFZERTQSJ9.e30.c2ln")
	withReceipt, err := PayeeObservedX402(in)
	require.NoError(t, err)
	assert.Len(t, withReceipt.Member["wrapped"], 2)
}

func TestPayeeObservedX402Refusals(t *testing.T) {
	scale := func(n int) *int { return &n }
	fee := func(v string) *string { return &v }
	for name, change := range map[string]func(*X402Receipt){
		"not a settle response": func(in *X402Receipt) { in.SettleResponse = []byte(`{"transaction":"x"}`) },
		"no transaction":        func(in *X402Receipt) { in.SettleResponse = []byte(`{"success":true,"network":"eip155:8453"}`) },
		"other network": func(in *X402Receipt) {
			in.SettleResponse = []byte(`{"success":true,"transaction":"0x1","network":"eip155:1"}`)
		},
		"v1 network name": func(in *X402Receipt) {
			in.Requirements = []byte(strings.Replace(string(in.Requirements), "eip155:8453", "base", 1))
		},
		"decimal amount": func(in *X402Receipt) {
			in.Requirements = []byte(strings.Replace(string(in.Requirements), `"1500000"`, `"1.5"`, 1))
		},
		"no payTo": func(in *X402Receipt) {
			in.Requirements = []byte(strings.Replace(string(in.Requirements), `"payTo"`, `"to"`, 1))
		},
		"wrong scale":            func(in *X402Receipt) { in.AssetScale = scale(18) },
		"fee on exact":           func(in *X402Receipt) { in.ReceiveFee = fee("5") },
		"trailing data":          func(in *X402Receipt) { in.SettleResponse = append(in.SettleResponse, []byte(" {}")...) },
		"local observed time":    func(in *X402Receipt) { in.ObservedAt = "2026-10-01T12:00:05+02:00" },
		"terms_ref not a digest": func(in *X402Receipt) { in.TermsRef = "abc" },
		"unknown asset, no scale": func(in *X402Receipt) {
			in.Requirements = []byte(strings.Replace(string(in.Requirements), "0x833589", "0x999999", 1))
		},
	} {
		in := x402Input("exact")
		change(&in)
		_, err := PayeeObservedX402(in)
		assert.Error(t, err, name)
	}

	// Any scheme but exact: a receive fee may apply, so it must be stated.
	_, err := PayeeObservedX402(x402Input("upto"))
	assert.ErrorContains(t, err, "receive fee may apply")
	in := x402Input("upto")
	in.ReceiveFee = fee("5")
	b, err := PayeeObservedX402(in)
	require.NoError(t, err)
	stated, _ := parseAmount(b.Member["receive_fee"])
	assert.Equal(t, "5", stated.Value.String())

	in = x402Input("exact")
	in.Requirements = []byte(strings.Replace(string(in.Requirements), "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", "0x9999999999999999999999999999999999999999", 1))
	in.AssetScale = scale(18)
	_, err = PayeeObservedX402(in)
	assert.NoError(t, err, "an unknown asset with its scale given")
}

func TestDeliveredSentIsTheSellersHandoff(t *testing.T) {
	b, err := DeliveredSent(Delivery{TermsRef: strings.Repeat("a", 64), ObservedAt: "2026-10-01T12:00:05Z", ContentDigest: strings.Repeat("d", 64)})
	require.NoError(t, err)
	d := b.Member["delivery"].(map[string]any)
	assert.Equal(t, "sent", d["direction"])
	assert.Equal(t, RolePayee, b.Member["sealer_role"])
	assert.NotContains(t, b.Member, "wrapped")

	goods, err := DeliveredSent(Delivery{TermsRef: strings.Repeat("a", 64), ObservedAt: "2026-10-01T12:00:05Z", Carrier: "ups", Status: "in_transit",
		ShippedAt: "2026-10-01T09:00:00Z", TrackingDigest: strings.Repeat("e", 64), Proof: []byte("%PDF proof")})
	require.NoError(t, err)
	w := goods.Member["wrapped"].([]any)[0].(map[string]any)
	assert.Equal(t, "delivery.proof", w["type"])
	assert.NotContains(t, w, "content")

	for name, in := range map[string]Delivery{
		"nothing handed over": {TermsRef: strings.Repeat("a", 64), ObservedAt: "2026-10-01T12:00:05Z"},
		"bad digest":          {TermsRef: strings.Repeat("a", 64), ObservedAt: "2026-10-01T12:00:05Z", ContentDigest: "abc"},
		"bad status":          {TermsRef: strings.Repeat("a", 64), ObservedAt: "2026-10-01T12:00:05Z", Carrier: "ups", Status: "lost"},
	} {
		_, err := DeliveredSent(in)
		assert.Error(t, err, name)
	}
}

func TestNeverEntersExemptsOnlyTheAssetCode(t *testing.T) {
	contract := "0x833589fcd6edb6e08f4c7c32d4f71b54bda02913"
	member := map[string]any{"received": map[string]any{"assetCode": "eip155:8453/erc20:" + contract}}
	assert.NoError(t, NeverEnters(member, []string{contract}), "a CAIP-19 code names the token contract, not an account")
	member["payment_ref"] = map[string]any{"value": "paid to " + strings.ToUpper(contract[2:])}
	assert.Error(t, NeverEnters(member, []string{contract[2:]}))
	assert.Error(t, NeverEnters(map[string]any{"wrapped": []any{map[string]any{"note": testPayTo}}}, []string{testPayTo}))
	assert.NoError(t, NeverEnters(map[string]any{"x": "y"}, []string{""}))
}
