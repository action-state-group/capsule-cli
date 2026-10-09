package settlement

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// This file builds the two legs the payee seals: payee_observed, from the
// rail's own objects, and delivered (sent). Every leg it returns passes the
// same structure checks the verifier applies (structureFailures), so a leg
// that a verifier would report settlement_malformed is never sealed.

// Built is one leg's settlement member and the wrapped objects' octets it
// binds by digest. Objects stay with the sealer: a leg never carries them
// (Member has no content), and they are disclosed out of band.
type Built struct {
	Member  map[string]any
	Objects map[string][]byte
	// AccountIDs are the account identifiers the rail's objects carry (the
	// payee's pay-to and the payer's address). They never enter the leg;
	// the caller keeps them out of every sealed record.
	AccountIDs []string
}

// JSON is the member as JSON, ready to be carried as the Capsule's
// settlement extension.
func (b Built) JSON() (json.RawMessage, error) {
	return json.Marshal(b.Member)
}

// knownAssetScales are the decimals of assets a payee commonly receives over
// x402, keyed by CAIP-19 asset type. Any other asset needs its scale given.
var knownAssetScales = map[string]int{
	"eip155:8453/erc20:0x833589fcd6edb6e08f4c7c32d4f71b54bda02913":  6, // USDC on Base
	"eip155:84532/erc20:0x036cbd53842c5426634e7929541ec2318f3dcf7e": 6, // USDC on Base Sepolia
}

// X402Receipt is what the payee's own system holds for one x402 payment: the
// facilitator's settlement response (the body as the facilitator returned
// it), the payee's own payment requirements for the request, and optionally
// the signed receipt the payee issued.
type X402Receipt struct {
	TermsRef       string
	SettleResponse []byte
	Requirements   []byte
	// Receipt is the x402 signed receipt, as issued: a JWS compact
	// serialization, or the EIP-712 envelope object.
	Receipt []byte
	// AssetScale is the asset's decimals, needed when the asset is not in
	// knownAssetScales.
	AssetScale *int
	// ReceiveFee is what the payee's system reports deducted on the
	// receiving side, in the asset's atomic units. Required for any scheme
	// other than exact, for which receive fees are not applicable (§7.2).
	ReceiveFee *string
	ObservedAt string
}

var caip2Pattern = regexp.MustCompile(`^[-a-z0-9]{3,8}:[-_a-zA-Z0-9]{1,32}$`)

// PayeeObservedX402 builds the payee-observed leg for an x402 payment
// (§5.3, §7.2). Every value is read from the rail's objects, never typed by
// the caller: payment_ref and status from the settlement response, received
// from the payee's own requirements. It requests and moves nothing.
func PayeeObservedX402(in X402Receipt) (Built, error) {
	var settle struct {
		Success     *bool  `json:"success"`
		Transaction string `json:"transaction"`
		Network     string `json:"network"`
		Payer       string `json:"payer"`
	}
	if err := strictUnmarshal(in.SettleResponse, &settle); err != nil || settle.Success == nil {
		return Built{}, errors.New("the settlement response is not an x402 settlement response (success, transaction, network)")
	}
	var req struct {
		Scheme            string `json:"scheme"`
		Network           string `json:"network"`
		Amount            string `json:"amount"`
		MaxAmountRequired string `json:"maxAmountRequired"`
		Asset             string `json:"asset"`
		PayTo             string `json:"payTo"`
	}
	if err := strictUnmarshal(in.Requirements, &req); err != nil {
		return Built{}, errors.New("the payment requirements are not JSON")
	}
	amount := req.Amount
	if amount == "" {
		amount = req.MaxAmountRequired
	}
	switch {
	case req.Scheme == "" || req.Asset == "" || req.PayTo == "":
		return Built{}, errors.New("the payment requirements need scheme, network, amount, asset and payTo")
	case !caip2Pattern.MatchString(req.Network):
		return Built{}, errors.New("the payment requirements' network must be a CAIP-2 identifier (for example eip155:8453)")
	case settle.Network != req.Network:
		return Built{}, errors.New("the settlement response is for another network than the payment requirements")
	case settle.Transaction == "":
		return Built{}, errors.New("the settlement response names no transaction: a leg joins on it, so there is nothing to record")
	case !valuePattern.MatchString(amount):
		return Built{}, errors.New("the payment requirements' amount must be an integer string in the asset's atomic units")
	}
	assetCode := req.Network + "/erc20:" + strings.ToLower(req.Asset)
	if !strings.HasPrefix(req.Network, "eip155:") {
		assetCode = req.Network + "/token:" + req.Asset
	}
	scale, known := knownAssetScales[assetCode]
	if in.AssetScale != nil {
		if known && *in.AssetScale != scale {
			return Built{}, fmt.Errorf("the asset's scale is %d, not %d", scale, *in.AssetScale)
		}
		scale, known = *in.AssetScale, true
	}
	if !known || scale < 0 || scale > 255 {
		return Built{}, errors.New("the asset's scale (its decimals, 0 to 255) is needed for this asset")
	}
	fee := "0"
	switch {
	case in.ReceiveFee != nil:
		if req.Scheme == "exact" && *in.ReceiveFee != "0" {
			return Built{}, errors.New("the x402 exact scheme transfers exactly the amount: it has no receive fee")
		}
		fee = *in.ReceiveFee
	case req.Scheme != "exact":
		return Built{}, errors.New("a receive fee may apply for the " + req.Scheme + " scheme: give the fee the payee's system reported (0 if none)")
	}
	if !valuePattern.MatchString(fee) {
		return Built{}, errors.New("the receive fee must be an integer string in the asset's atomic units")
	}
	ref := settle.Transaction
	if strings.HasPrefix(req.Network, "eip155:") {
		ref = strings.ToLower(ref)
	}
	status := "failed"
	if *settle.Success {
		status = "settled"
	}
	b := Built{Objects: map[string][]byte{}, AccountIDs: nonEmpty(req.PayTo, settle.Payer)}
	wrapped := []any{b.wrap("x402.settle-response", in.SettleResponse)}
	if len(in.Receipt) > 0 {
		wrapped = append(wrapped, b.wrap("x402.receipt", in.Receipt))
	}
	amountOf := func(value string) map[string]any {
		return map[string]any{"value": value, "assetCode": assetCode, "assetScale": json.Number(fmt.Sprint(scale))}
	}
	b.Member = map[string]any{
		"version": Version, "leg": LegPayeeObserved, "sealer_role": RolePayee, "terms_ref": in.TermsRef,
		"received": amountOf(amount), "receive_fee": amountOf(fee),
		"payment_ref": map[string]any{"type": "x402.transaction", "value": ref, "network": req.Network},
		"status":      status, "observed_at": in.ObservedAt, "wrapped": wrapped,
	}
	return b, b.check()
}

// Delivery is what the payee handed over or performed under the terms.
type Delivery struct {
	TermsRef   string
	ObservedAt string
	// ContentDigest is SHA-256 over the exact octets handed over, or the
	// JSON digest of a JSON value (§5.4).
	ContentDigest string
	// For physical goods.
	Carrier        string
	Status         string
	ShippedAt      string
	DeliveredAt    string
	TrackingDigest string
	// Proof is a carrier's proof-of-delivery document: wrapped by digest as
	// delivery.proof, an external party's document, never verified here.
	Proof []byte
}

// DeliveredSent builds the payee's delivered leg (§5.4): direction sent. It
// is the seller's statement of a handoff, never proof of receipt.
func DeliveredSent(in Delivery) (Built, error) {
	d := map[string]any{"direction": "sent"}
	set := func(name, value string) {
		if value != "" {
			d[name] = value
		}
	}
	set("content_digest", in.ContentDigest)
	set("carrier", in.Carrier)
	set("status", in.Status)
	set("shipped_at", in.ShippedAt)
	set("delivered_at", in.DeliveredAt)
	set("tracking_digest", in.TrackingDigest)
	b := Built{Objects: map[string][]byte{}}
	b.Member = map[string]any{
		"version": Version, "leg": LegDelivered, "sealer_role": RolePayee, "terms_ref": in.TermsRef,
		"observed_at": in.ObservedAt, "delivery": d,
	}
	if len(in.Proof) > 0 {
		b.Member["wrapped"] = []any{b.wrap("delivery.proof", in.Proof)}
	}
	return b, b.check()
}

// wrap binds an object by the SHA-256 of its exact octets and keeps the
// octets with the sealer; content is never carried (the rail's objects carry
// account identifiers).
func (b Built) wrap(kind string, octets []byte) map[string]any {
	sum := sha256.Sum256(octets)
	digest := hex.EncodeToString(sum[:])
	b.Objects[digest] = octets
	return map[string]any{"type": kind, "digest_alg": "SHA-256", "digest": digest}
}

// check refuses a leg the verifier would report malformed, or one that
// carries an account identifier anywhere but in a CAIP-19 assetCode (which
// names the token's contract, not an account).
func (b Built) check() error {
	if failures := structureFailures(b.Member); len(failures) > 0 {
		return fmt.Errorf("the leg would not verify: %s", strings.Join(failures, ", "))
	}
	return NeverEnters(b.Member, b.AccountIDs)
}

// NeverEnters refuses a settlement member in which any value other than an
// amount's assetCode equals or contains one of the account identifiers.
func NeverEnters(member map[string]any, accounts []string) error {
	var walk func(value any, key string) error
	walk = func(value any, key string) error {
		switch typed := value.(type) {
		case string:
			if key == "assetCode" {
				return nil
			}
			for _, account := range accounts {
				if account != "" && strings.Contains(strings.ToLower(typed), strings.ToLower(account)) {
					return errors.New("refusing to seal: the leg would carry an account identifier in clear")
				}
			}
		case map[string]any:
			for name, item := range typed {
				if err := walk(item, name); err != nil {
					return err
				}
			}
		case []any:
			for _, item := range typed {
				if err := walk(item, key); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(member, "")
}

func nonEmpty(values ...string) []string {
	var out []string
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			out = append(out, strings.TrimSpace(v))
		}
	}
	return out
}

func strictUnmarshal(data []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("trailing data")
	}
	return nil
}
