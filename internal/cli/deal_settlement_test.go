package cli

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	emit "github.com/action-state-group/capsule-emit-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// One x402 payment of 1.5 USDC on Base, as each side's system saw it.
const (
	x402Network = "eip155:8453"
	x402Asset   = "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"
	x402USDC    = "eip155:8453/erc20:0x833589fcd6edb6e08f4c7c32d4f71b54bda02913"
	x402PayTo   = "0x1111111111111111111111111111111111111aaa"
	x402Payer   = "0x2222222222222222222222222222222222222bbb"
	x402Tx      = "0x5A1F00000000000000000000000000000000000000000000000000000000C3D2"
)

var x402Requirements = `{"scheme":"exact","network":"` + x402Network + `","amount":"1500000","asset":"` + x402Asset +
	`","payTo":"` + x402PayTo + `","maxTimeoutSeconds":60,"extra":{"name":"USD Coin","version":"2"}}`

func x402Settle(success bool) string {
	ok := "true"
	if !success {
		ok = "false"
	}
	return `{"success":` + ok + `,"transaction":"` + x402Tx + `","network":"` + x402Network + `","payer":"` + x402Payer + `"}`
}

var x402PaymentPayload = []byte(`{"x402Version":2,"accepted":{"scheme":"exact","network":"` + x402Network + `","amount":"1500000","asset":"` +
	x402Asset + `","payTo":"` + x402PayTo + `"},"payload":{"authorization":{"from":"` + x402Payer + `","to":"` + x402PayTo + `"}}}`)

var deliveredContent = []byte("the example data set, exactly as handed over\n")

// buyerSide seals the buyer's legs with the buyer's own key, as the buyer's
// system would, and writes each as a bare Capsule file.
type buyerSide struct {
	t   *testing.T
	key ed25519.PrivateKey
	dir string
}

func newBuyerSide(t *testing.T) *buyerSide {
	_, key, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	return &buyerSide{t: t, key: key, dir: t.TempDir()}
}

func (b *buyerSide) seal(name string, member map[string]any) (path, capsuleID string) {
	t := b.t
	value, err := json.Marshal(member)
	require.NoError(t, err)
	built, err := emit.Build(emit.Input{ActionID: "buyer/" + name, ActionType: emit.ActionTypeFYI, Operator: "buyer", Developer: "buyer-agent",
		Timestamp: time.Date(2026, 9, 27, 17, 0, 0, 0, time.UTC), Extensions: []emit.Extension{{Name: "settlement", Value: value}}})
	require.NoError(t, err)
	identity, err := emit.NewEd25519SigningIdentity(b.key)
	require.NoError(t, err)
	envelope, err := emit.Sign(built, identity)
	require.NoError(t, err)
	capsule := map[string]any{}
	for k, v := range built.Value {
		capsule[k] = v
	}
	capsule["signature"] = hex.EncodeToString(envelope)
	capsule["key_id"] = hex.EncodeToString(b.key.Public().(ed25519.PublicKey))
	data, err := json.Marshal(capsule)
	require.NoError(t, err)
	path = filepath.Join(b.dir, name+".json")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path, built.CapsuleID
}

func usdc(value string) map[string]any {
	return map[string]any{"value": value, "assetCode": x402USDC, "assetScale": 6}
}

// terms is the buyer's terms leg: 1.5 USDC for content with a known digest.
func (b *buyerSide) terms() (string, string) {
	return b.seal("terms", map[string]any{"version": "0", "leg": "terms", "sealer_role": "payer", "amount": usdc("1500000"),
		"deliverable": map[string]any{"content_digest": sha256Hex(deliveredContent)}})
}

func (b *buyerSide) payerObserved(termsRef string) string {
	path, _ := b.seal("payer-observed", map[string]any{"version": "0", "leg": "payer_observed", "sealer_role": "payer", "terms_ref": termsRef,
		"amount": usdc("1500000"), "routing_fee": usdc("0"), "status": "settled", "observed_at": "2026-09-27T17:30:00Z",
		"payment_ref": map[string]any{"type": "x402.transaction", "value": strings.ToLower(x402Tx), "network": x402Network},
		"wrapped":     []any{map[string]any{"type": "x402.payment-payload", "digest_alg": "SHA-256", "digest": sha256Hex(x402PaymentPayload)}}})
	return path
}

func (b *buyerSide) received(termsRef string, content []byte) string {
	path, _ := b.seal("delivered-received", map[string]any{"version": "0", "leg": "delivered", "sealer_role": "payer", "terms_ref": termsRef,
		"observed_at": "2026-09-27T17:45:00Z", "delivery": map[string]any{"direction": "received", "content_digest": sha256Hex(content)}})
	return path
}

func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

func dealRunErr(t *testing.T, args ...string) error {
	t.Helper()
	_, err := invoke(t, "", append([]string{"--profile", "deal", "deal"}, args...)...)
	return err
}

// sellerDealWithDueDelivery opens a typed seller deal and notes a deliver_by
// obligation; it returns the deal and the obligation's step.
func sellerDealWithDueDelivery(t *testing.T) (string, string) {
	t.Helper()
	dealFixture(t)
	id := dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, sellerOpen))["deal_id"].(string)
	due := dealRun(t, "note", "--deal", id, "--kind", "evidence", "--input", writeJSON(t,
		`{"about": "the sale", "source": "agent", "obligation": {"kind": "deliver_by", "due_by": "2026-10-03"}}`))
	return id, jsonString(due["step"])
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func notePaymentReceived(t *testing.T, id, terms string, settle string) map[string]any {
	t.Helper()
	return dealRun(t, "note", "--deal", id, "--kind", "payment_received", "--terms", terms,
		"--settle-response", writeFile(t, "settle.json", []byte(settle)), "--requirements", writeFile(t, "requirements.json", []byte(x402Requirements)))
}

type dealSettlementOutput struct {
	Settlement struct {
		Conforming  bool `json:"conforming"`
		Settlements []struct {
			PaymentState     string   `json:"payment_state"`
			AgreedStatus     string   `json:"agreed_status"`
			TermsAmount      string   `json:"terms_amount"`
			DeliveryState    string   `json:"delivery_state"`
			DeliveryEvidence []string `json:"delivery_evidence"`
		} `json:"settlements"`
		Failures []struct {
			Code string `json:"code"`
		} `json:"failures"`
	} `json:"settlement"`
	Readings []struct {
		Payment  string `json:"payment"`
		Delivery string `json:"delivery"`
	} `json:"readings"`
}

func dealSettlement(t *testing.T, args ...string) dealSettlementOutput {
	t.Helper()
	out, err := invoke(t, "", append([]string{"--profile", "deal", "deal", "settlement"}, args...)...)
	require.NoError(t, err, out)
	var result dealSettlementOutput
	require.NoError(t, json.Unmarshal([]byte(out), &result), out)
	return result
}

func TestASellerSealsItsPayeeLegsAndTheyJoinTheBuyers(t *testing.T) {
	id, dueStep := sellerDealWithDueDelivery(t)
	buyer := newBuyerSide(t)
	terms, termsID := buyer.terms()

	notePaymentReceived(t, id, terms, x402Settle(true))
	dealRun(t, "note", "--deal", id, "--kind", "delivered", "--terms", terms,
		"--content", writeFile(t, "content.bin", deliveredContent), "--resolves", dueStep)

	// The seller's side alone: one side's statements, read as such.
	alone := dealSettlement(t, "--deal", id, "--leg", terms)
	require.True(t, alone.Settlement.Conforming, alone.Settlement.Failures)
	s := alone.Settlement.Settlements[0]
	assert.Equal(t, "payee_stated", s.PaymentState)
	assert.Contains(t, alone.Readings[0].Payment, "a stated claim by the payee alone")
	assert.Equal(t, "stated", s.DeliveryState)
	assert.Equal(t, []string{"seller_handoff"}, s.DeliveryEvidence, "the seller's own leg is a handoff, never proof of receipt")
	assert.Contains(t, alone.Readings[0].Delivery, "not proof of receipt")

	// With the buyer's legs: the two sides agree, and the buyer confirms the content.
	both := dealSettlement(t, "--deal", id, "--leg", terms, "--leg", buyer.payerObserved(termsID),
		"--leg", buyer.received(termsID, deliveredContent), "--object", writeFile(t, "payload.json", x402PaymentPayload))
	require.True(t, both.Settlement.Conforming, both.Settlement.Failures)
	s = both.Settlement.Settlements[0]
	assert.Equal(t, "agreed", s.PaymentState)
	assert.Equal(t, "settled", s.AgreedStatus)
	assert.Equal(t, "equal", s.TermsAmount)
	assert.Equal(t, "matched", s.DeliveryState)
	assert.Equal(t, []string{"seller_handoff", "buyer_receipt"}, s.DeliveryEvidence)

	// The buyer received other content: the sides do not match.
	other := dealSettlement(t, "--deal", id, "--leg", terms, "--leg", buyer.received(termsID, []byte("something else")))
	assert.Equal(t, "mismatch", other.Settlement.Settlements[0].DeliveryState)

	// No account identifier reaches the deal's own records, nor a shared copy.
	own := strings.ToLower(dealOwnBundle(t, id))
	assert.NotContains(t, own, strings.ToLower(x402PayTo))
	assert.NotContains(t, own, strings.ToLower(x402Payer))
	assert.Contains(t, own, `"received_to":{"fp_alg":"hmac-sha256-deal-key","payee":"`)
	shared := filepath.Join(t.TempDir(), "shared.json")
	out, err := invoke(t, "", "--profile", "deal", "disclose", "--deal", id, "--share", "counterparty", "--to", "Example Buyer", "--out", shared)
	require.NoError(t, err, out)
	sharedText := strings.ToLower(string(mustRead(t, shared)))
	assert.NotContains(t, sharedText, strings.ToLower(x402PayTo))
	assert.NotContains(t, sharedText, strings.ToLower(x402Payer))
	// The buyer's copy carries the payee's leg on its Capsule (the payment
	// reference it joins on), so the buyer can check it against their own.
	assert.Contains(t, sharedText, strings.ToLower(x402Tx))
}

func TestAPaymentToAnotherAccountIsRefused(t *testing.T) {
	id, _ := sellerDealWithDueDelivery(t)
	terms, _ := newBuyerSide(t).terms()
	notePaymentReceived(t, id, terms, x402Settle(true))
	other := strings.Replace(x402Requirements, x402PayTo, "0x3333333333333333333333333333333333333ccc", 1)
	err := dealRunErr(t, "note", "--deal", id, "--kind", "payment_received", "--terms", terms,
		"--settle-response", writeFile(t, "settle.json", []byte(x402Settle(true))), "--requirements", writeFile(t, "req.json", []byte(other)))
	assert.ErrorContains(t, err, "received to another account")
}

func TestSettlementNotesAreRefusedOutsideTheirPlace(t *testing.T) {
	id, _ := sellerDealWithDueDelivery(t)
	terms, termsID := newBuyerSide(t).terms()

	// Only the settlement kinds build a leg: never an evidence --input.
	err := dealRunErr(t, "note", "--deal", id, "--kind", "evidence", "--input", writeJSON(t,
		`{"about": "x", "source": "agent", "settlement": {"version": "0"}}`))
	assert.ErrorContains(t, err, "cannot set a settlement leg")

	// A delivery resolves a due date, not a cancel-by date.
	renewal := dealRun(t, "note", "--deal", id, "--kind", "evidence", "--input", writeJSON(t,
		`{"about": "the plan", "source": "agent", "obligation": {"kind": "renewal", "cancel_by": "2026-10-03"}}`))
	err = dealRunErr(t, "note", "--deal", id, "--kind", "delivered", "--terms", terms,
		"--content-digest", sha256Hex(deliveredContent), "--resolves", jsonString(renewal["step"]))
	assert.ErrorContains(t, err, "a delivery resolves deliver_by or perform_by")

	// The terms must be a terms leg; a delivery must say what was handed over.
	payer := newBuyerSide(t).payerObserved(termsID)
	assert.ErrorContains(t, dealRunErr(t, "note", "--deal", id, "--kind", "delivered", "--terms", payer, "--content-digest", sha256Hex(deliveredContent)), "not a terms leg")
	assert.ErrorContains(t, dealRunErr(t, "note", "--deal", id, "--kind", "delivered", "--terms", terms), "what was handed over")
	assert.ErrorContains(t, dealRunErr(t, "note", "--deal", id, "--kind", "payment_received", "--terms", terms), "needs --settle-response")

	// The payee's legs belong to a deal where the user sells.
	buyerDeal := dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, strings.Replace(sellerOpen, `"party_role": "seller",`, "", 1)))["deal_id"].(string)
	err = dealRunErr(t, "note", "--deal", buyerDeal, "--kind", "delivered", "--terms", terms, "--content-digest", sha256Hex(deliveredContent))
	assert.ErrorContains(t, err, "where the user sells")
}

func TestAFailedSettlementIsRecordedAsFailed(t *testing.T) {
	id, _ := sellerDealWithDueDelivery(t)
	buyer := newBuyerSide(t)
	terms, termsID := buyer.terms()
	notePaymentReceived(t, id, terms, x402Settle(false))
	out := dealSettlement(t, "--deal", id, "--leg", terms, "--leg", buyer.payerObserved(termsID), "--object", writeFile(t, "payload.json", x402PaymentPayload))
	s := out.Settlement.Settlements[0]
	assert.Equal(t, "mismatch", s.PaymentState, "the payer reports settled, the payee's facilitator reported failure")
}
