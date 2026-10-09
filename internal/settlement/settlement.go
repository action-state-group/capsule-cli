// Package settlement verifies settlement leg records and derives the payment
// and delivery states of draft-mih-agent-settlement-records-00.
//
// A leg is an Agent Action Capsule with a top-level settlement member. This
// package checks each leg (the base profile's Class 1 checks, its Producer
// Envelope, the settlement member, its amounts and wrapped objects, and the
// sealer rules against an optional key policy), groups the legs that pass by
// the terms leg they answer, and derives each settlement's state from the
// legs present (§9). No state is read from any record.
package settlement

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/envelope"
	aacverify "github.com/action-state-group/agent-action-capsule/go/verify"
)

// Leg is one leg record as presented to the verifier.
type Leg struct {
	// Label is the caller's name for the leg, echoed in every report.
	Label string
	// Capsule is the leg decoded with json.Number preserved, without the
	// local-only signature and key_id fields.
	Capsule map[string]any
	// Envelope is the leg's Producer Envelope; nil when it carries none.
	Envelope []byte
}

// Input is a set of leg records and what the verifier holds besides them.
type Input struct {
	Legs []Leg
	// Objects are wrapped objects' octets the verifier holds, keyed by their
	// SHA-256 in lowercase hex. A wrapped entry's own content is used too.
	Objects map[string][]byte
	// Policy maps a sealer role (payer, payee) to the hex Ed25519 public keys
	// the verifier accepts for it. Nil means no key policy: the
	// sealer_not_authorized_for_role check is not performed, and each leg's
	// authenticated key is still reported for the caller's own policy (§3).
	Policy map[string][]string
}

// Report is one failure or finding, naming the legs it is about.
type Report struct {
	Records []string `json:"records"`
	Code    string   `json:"code"`
}

// Settlement is the derived state of the legs that answer one terms leg.
type Settlement struct {
	Terms        string `json:"terms"`
	PaymentState string `json:"payment_state"`
	AgreedStatus string `json:"agreed_status,omitempty"`
	// TermsAmount is set with agreed only: "equal" or "differs", whether the
	// payer's amount equals the terms leg's (§9.2). Agreed is about the
	// payment between the sides; an agreed payment whose amount differs from
	// the terms is not an agreement on the terms, and a caller must not
	// present it as one.
	TermsAmount   string   `json:"terms_amount,omitempty"`
	Differs       []string `json:"differs,omitempty"`
	DeliveryState string   `json:"delivery_state"`
	// DeliveryEvidence names which of the three distinct sources back the
	// delivery state, in this order: seller_handoff (a sent leg: what the
	// payee says it handed over, a claim, never proof of receipt),
	// carrier_confirmation (a delivered leg wrapping a delivery.proof: an
	// external party's document, carried by digest and not verified here),
	// and buyer_receipt (a received leg: the counterparty confirms what it
	// received). A delivery backed by seller_handoff alone is the seller's
	// statement only.
	DeliveryEvidence []string `json:"delivery_evidence,omitempty"`
}

// The three delivery evidence sources, each a distinct kind of claim.
const (
	EvidenceSellerHandoff       = "seller_handoff"
	EvidenceCarrierConfirmation = "carrier_confirmation"
	EvidenceBuyerReceipt        = "buyer_receipt"
)

// LegKey is what the verifier established about one leg's identity.
type LegKey struct {
	Record           string `json:"record"`
	CapsuleID        string `json:"capsule_id,omitempty"`
	AuthenticatedKey string `json:"authenticated_key,omitempty"`
}

// Result is the verifier's whole answer for one Input.
type Result struct {
	// Conforming is false when any leg has a failure.
	Conforming  bool         `json:"conforming"`
	Failures    []Report     `json:"failures"`
	Findings    []Report     `json:"findings"`
	Settlements []Settlement `json:"settlements"`
	Legs        []LegKey     `json:"legs"`
	// KeyPolicy is "applied" or "not_applied".
	KeyPolicy string `json:"key_policy"`
}

var (
	hex64Pattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	base64urlPattern = regexp.MustCompile(`^[A-Za-z0-9_-]*$`)
)

type leg struct {
	label, id, kid string
	s              map[string]any
	// chain is the Capsule's own chain block, nil when absent.
	chain map[string]any
}

// Verify checks every leg and derives the settlements (§9.1-§9.3).
func Verify(in Input) Result {
	result := Result{Failures: []Report{}, Findings: []Report{}, Settlements: []Settlement{}, Legs: []LegKey{}, KeyPolicy: "not_applied"}
	if in.Policy != nil {
		result.KeyPolicy = "applied"
	}
	fail := func(code string, labels ...string) {
		result.Failures = append(result.Failures, Report{Records: labels, Code: code})
	}
	find := func(code string, labels ...string) {
		result.Findings = append(result.Findings, Report{Records: labels, Code: code})
	}

	var all, live []leg
	for _, presented := range in.Legs {
		l := leg{label: presented.Label}
		l.s, _ = presented.Capsule["settlement"].(map[string]any)
		l.chain, _ = presented.Capsule["chain"].(map[string]any)
		var own []string
		if id, kid, code := checkCapsule(presented); code != "" {
			own = append(own, code)
		} else {
			l.id, l.kid = id, kid
		}
		structural := structureFailures(presented.Capsule["settlement"])
		own = append(own, structural...)
		if len(structural) == 0 {
			own = append(own, wrappedFailures(l.s, l.kid, in.Objects)...)
			if in.Policy != nil && l.kid != "" && !slices.Contains(in.Policy[l.s["sealer_role"].(string)], l.kid) {
				own = append(own, CodeSealerNotAuthorizedForRole)
			}
			if ref, _ := l.s["payment_ref"].(map[string]any); ref != nil {
				if _, known := paymentRefTypes[ref["type"].(string)]; !known {
					find(CodePaymentRefTypeUnknown, l.label)
				}
			}
		}
		for _, code := range own {
			fail(code, l.label)
		}
		result.Legs = append(result.Legs, LegKey{Record: l.label, CapsuleID: l.id, AuthenticatedKey: l.kid})
		all = append(all, l)
		if len(own) == 0 {
			live = append(live, l)
		}
	}

	// §3 item 3: a payer-observed and a payee-observed leg for one terms leg
	// under the same key are not two sides. A check on the pair that needs no
	// key policy; it excludes neither leg.
	conflated := map[string]bool{}
	for _, p := range all {
		for _, q := range all {
			if legOf(p) == LegPayerObserved && legOf(q) == LegPayeeObserved && p.kid != "" && p.kid == q.kid && p.s["terms_ref"] == q.s["terms_ref"] {
				fail(CodeSealerConflation, p.label, q.label)
				conflated[p.s["terms_ref"].(string)] = true
			}
		}
	}

	terms := map[string]bool{}
	for _, l := range live {
		if legOf(l) == LegTerms {
			terms[l.id] = true
		}
	}
	for _, l := range live {
		if legOf(l) != LegTerms && !terms[l.s["terms_ref"].(string)] {
			fail(CodeTermsRefUnresolved, l.label)
		}
	}
	for _, t := range live {
		if legOf(t) != LegTerms {
			continue
		}
		var answering []leg
		for _, l := range live {
			if l.s["terms_ref"] == t.id {
				answering = append(answering, l)
			}
		}
		settlement := Settlement{Terms: t.label}
		derivePayment(&settlement, t, answering, conflated[t.id], in.Objects, find)
		settlement.DeliveryState, settlement.DeliveryEvidence = deliveryState(t, answering)
		result.Settlements = append(result.Settlements, settlement)
	}
	result.Conforming = len(result.Failures) == 0
	return result
}

func legOf(l leg) string {
	value, _ := l.s["leg"].(string)
	return value
}

// checkCapsule is §9.1 items 1 and 2: the base profile's Class 1 checks, then
// the Producer Envelope over the leg's Capsule ID and the key it authenticates.
func checkCapsule(presented Leg) (id, kid, code string) {
	if presented.Capsule == nil || !aacverify.Verify(presented.Capsule, nil, nil).OK {
		return "", "", CodeCapsuleInvalid
	}
	id, _ = presented.Capsule["capsule_id"].(string)
	if presented.Envelope == nil {
		return "", "", CodeEnvelopeInvalid
	}
	signed := envelope.Verify(id, presented.Envelope)
	if !signed.OK {
		return "", "", CodeEnvelopeInvalid
	}
	return id, hex.EncodeToString(signed.PublicKey), ""
}

// structureFailures is §4.2, §3 item 4, §5.4, §6 and §7.1 for one settlement
// member: each code at most once, in the order found.
func structureFailures(raw any) []string {
	s, isObject := raw.(map[string]any)
	if !isObject || s["version"] != Version || !legs[str(s["leg"])] || !sealerRoles[str(s["sealer_role"])] {
		return []string{CodeSettlementMalformed}
	}
	var out []string
	add := func(code string) {
		if !slices.Contains(out, code) {
			out = append(out, code)
		}
	}
	leg, role := s["leg"].(string), s["sealer_role"].(string)
	members := legMembers[leg]
	for _, name := range members.required {
		if _, present := s[name]; !present {
			add(CodeSettlementMalformed)
		}
	}
	for name := range s {
		if !slices.Contains(members.required, name) && !slices.Contains(members.optional, name) {
			add(CodeSettlementMalformed)
		}
	}
	if (leg == LegPayerObserved && role != RolePayer) || (leg == LegPayeeObserved && role != RolePayee) {
		add(CodeLegRoleMismatch)
	}
	for _, name := range []string{"amount", "routing_fee", "received", "receive_fee"} {
		if value, present := s[name]; present {
			if _, exact := parseAmount(value); !exact {
				add(CodeAmountNotExact)
			}
		}
	}
	if leg == LegPayerObserved || leg == LegPayeeObserved {
		if !statuses[str(s["status"])] {
			add(CodeSettlementMalformed)
		}
	}
	if leg == LegDelivered && !deliveryWellFormed(s["delivery"], role) {
		add(CodeSettlementMalformed)
	}
	if value, present := s["terms_ref"]; present && !hex64Pattern.MatchString(str(value)) {
		add(CodeSettlementMalformed)
	}
	for _, name := range []string{"observed_at", "valid_until"} {
		if value, present := s[name]; present && !utcTime(value) {
			add(CodeSettlementMalformed)
		}
	}
	if value, present := s["deliverable"]; present && !deliverableWellFormed(value) {
		add(CodeSettlementMalformed)
	}
	if value, present := s["payment_ref"]; present && !paymentRefWellFormed(value) {
		add(CodeSettlementMalformed)
	}
	if value, present := s["wrapped"]; present && !wrappedWellFormed(value) {
		add(CodeSettlementMalformed)
	}
	return out
}

func str(value any) string {
	text, _ := value.(string)
	return text
}

func utcTime(value any) bool {
	text, isString := value.(string)
	if !isString || !strings.HasSuffix(text, "Z") {
		return false
	}
	_, err := time.Parse(time.RFC3339, text)
	return err == nil
}

// deliveryWellFormed is the delivery member's table (§5.4): direction matching
// the sealer, content_digest unless a carrier is named, and the optional
// physical-goods members typed as listed.
func deliveryWellFormed(raw any, role string) bool {
	d, isObject := raw.(map[string]any)
	if !isObject || deliveryDirections[str(d["direction"])] != role {
		return false
	}
	_, hasDigest := d["content_digest"]
	_, hasCarrier := d["carrier"]
	if !hasDigest && !hasCarrier {
		return false
	}
	for name, value := range d {
		var ok bool
		switch name {
		case "direction":
			ok = true
		case "content_digest", "tracking_digest", "address_digest":
			ok = hex64Pattern.MatchString(str(value))
		case "carrier":
			ok = str(value) != ""
		case "status":
			ok = physicalDeliveryStatuses[str(value)]
		case "shipped_at", "delivered_at":
			ok = utcTime(value)
		}
		if !ok {
			return false
		}
	}
	return true
}

// deliverableWellFormed is §5.1: at least one of content_digest and
// description_digest, each a digest.
func deliverableWellFormed(raw any) bool {
	d, isObject := raw.(map[string]any)
	if !isObject {
		return false
	}
	seen := 0
	for _, name := range []string{"content_digest", "description_digest"} {
		if value, present := d[name]; present {
			if !hex64Pattern.MatchString(str(value)) {
				return false
			}
			seen++
		}
	}
	return seen > 0
}

// paymentRefWellFormed is §7.1: type and value strings, every qualifier a
// string, and for a registered type exactly the qualifiers it defines.
func paymentRefWellFormed(raw any) bool {
	ref, isObject := raw.(map[string]any)
	if !isObject || str(ref["type"]) == "" {
		return false
	}
	for _, value := range ref {
		if _, isString := value.(string); !isString {
			return false
		}
	}
	if _, present := ref["value"]; !present {
		return false
	}
	registered, known := paymentRefTypes[ref["type"].(string)]
	if !known {
		return true
	}
	if len(ref) != 2+len(registered.qualifiers) {
		return false
	}
	for _, name := range registered.qualifiers {
		if _, present := ref[name]; !present {
			return false
		}
	}
	return true
}

// wrappedWellFormed is §8's entry table: type, digest_alg SHA-256, a digest,
// and an optional unpadded base64url content; nothing else.
func wrappedWellFormed(raw any) bool {
	entries, isArray := raw.([]any)
	if !isArray {
		return false
	}
	for _, item := range entries {
		w, isObject := item.(map[string]any)
		if !isObject || str(w["type"]) == "" || w["digest_alg"] != "SHA-256" || !hex64Pattern.MatchString(str(w["digest"])) {
			return false
		}
		for name, value := range w {
			switch name {
			case "type", "digest_alg", "digest":
			case "content":
				text, isString := value.(string)
				if !isString || !base64urlPattern.MatchString(text) || len(text)%4 == 1 {
					return false
				}
			default:
				return false
			}
		}
	}
	return true
}

// wrappedFailures is §8 rules 2 and 3 for one well-formed leg.
func wrappedFailures(s map[string]any, kid string, objects map[string][]byte) []string {
	var out []string
	entries, _ := s["wrapped"].([]any)
	for _, item := range entries {
		w := item.(map[string]any)
		digest := w["digest"].(string)
		octets := objects[digest]
		if content, present := w["content"]; present {
			decoded, _ := base64.RawURLEncoding.DecodeString(content.(string))
			if sum := sha256.Sum256(decoded); hex.EncodeToString(sum[:]) != digest {
				out = append(out, CodeWrappedDigestMismatch)
				continue
			}
			octets = decoded
		}
		issuer, known := wrappedTypes[w["type"].(string)]
		if known && issuer != s["sealer_role"] && octets != nil && kid != "" && jwsVerifies(octets, kid) {
			out = append(out, CodeWrappedResigned)
		}
	}
	return out
}

// jwsVerifies reports whether a JWS compact serialization (or the issuer JWT
// of an SD-JWT) carries an Ed25519 signature under the given key: the simplest
// re-signing case §8 rule 3 detects.
func jwsVerifies(octets []byte, kid string) bool {
	jwt, _, _ := bytes.Cut(octets, []byte("~"))
	parts := bytes.Split(jwt, []byte("."))
	if len(parts) != 3 {
		return false
	}
	key, err := hex.DecodeString(kid)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return false
	}
	signature, err := base64.RawURLEncoding.DecodeString(string(parts[2]))
	if err != nil {
		return false
	}
	signingInput := append(append(append([]byte{}, parts[0]...), '.'), parts[1]...)
	return ed25519.Verify(key, signingInput, signature)
}

// head returns the one observed leg of a side (§9.2): legs superseded by a
// later leg of the same side are set aside. ok is false when more than one
// leg remains.
func head(side []leg) (leg, bool) {
	var heads []leg
	for _, candidate := range side {
		superseded := false
		for _, later := range side {
			if later.chain != nil && later.chain["relation"] == "supersedes" && later.chain["parent_capsule_id"] == candidate.id {
				superseded = true
			}
		}
		if !superseded {
			heads = append(heads, candidate)
		}
	}
	if len(heads) != 1 {
		return leg{}, false
	}
	return heads[0], true
}

// derivePayment is §9.2 for one terms leg.
func derivePayment(out *Settlement, terms leg, answering []leg, conflated bool, objects map[string][]byte, find func(string, ...string)) {
	var payerSide, payeeSide []leg
	for _, l := range answering {
		switch legOf(l) {
		case LegPayerObserved:
			payerSide = append(payerSide, l)
		case LegPayeeObserved:
			payeeSide = append(payeeSide, l)
		}
	}
	switch {
	case len(payerSide) == 0 && len(payeeSide) == 0:
		out.PaymentState = PaymentTermsOnly
		return
	case len(payeeSide) == 0:
		out.PaymentState = PaymentPayerStated
		return
	case len(payerSide) == 0:
		out.PaymentState = PaymentPayeeStated
		return
	}
	payer, payerOne := head(payerSide)
	payee, payeeOne := head(payeeSide)
	if !payerOne || !payeeOne {
		var labels []string
		for _, l := range append(payerSide, payeeSide...) {
			labels = append(labels, l.label)
		}
		find(CodeObservedLegNotUnique, labels...)
		out.PaymentState = PaymentUnjoined
		return
	}
	a, b := payer.s, payee.s
	payerRef, payeeRef := a["payment_ref"].(map[string]any), b["payment_ref"].(map[string]any)
	payerType, payeeType := payerRef["type"].(string), payeeRef["type"].(string)
	_, payerKnown := paymentRefTypes[payerType]
	registered, payeeKnown := paymentRefTypes[payeeType]
	if !payerKnown || !payeeKnown || payerType != payeeType {
		out.PaymentState = PaymentUnjoined
		return
	}
	sent, _ := parseAmount(a["amount"])
	received, _ := parseAmount(b["received"])
	receiveFee, hasFee := parseAmount(b["receive_fee"])
	if !hasFee {
		notApplicable := registered.receiveFeeNotApplicable &&
			(payeeType != "x402.transaction" || x402Scheme([]leg{terms, payer}, objects) == "exact")
		if !notApplicable {
			find(CodeFeeUnstated, payee.label)
			out.PaymentState = PaymentUnjoined
			return
		}
		receiveFee = Amount{Value: new(big.Int), Asset: received.Asset, Scale: received.Scale} // read as zero (§6.1 rule 4)
	}
	if receiveFee.Asset != received.Asset {
		find(CodeFeeAssetDiffers, payee.label)
		out.PaymentState = PaymentUnjoined
		return
	}
	if routingFee, hasRouting := parseAmount(a["routing_fee"]); hasRouting && routingFee.Asset != sent.Asset {
		find(CodeFeeAssetDiffers, payer.label)
		out.PaymentState = PaymentUnjoined
		return
	}
	var differs []string
	if !amountRuleHolds(sent, received, receiveFee) {
		differs = append(differs, "amount")
	}
	if a["status"] != b["status"] {
		differs = append(differs, "status")
		if (a["status"] == "pending" && b["status"] == "settled") || (a["status"] == "settled" && b["status"] == "pending") {
			find(CodeInProgress, payer.label, payee.label)
		}
	}
	if !sameReference(payerRef, payeeRef) {
		differs = append(differs, "payment_ref")
	}
	switch {
	case len(differs) > 0:
		out.PaymentState = PaymentMismatch
		out.Differs = differs
	case conflated:
		// §9.2: never agreed for a pair that fails the distinct-key rule; the
		// sealer_conflation failure is already reported on the pair.
		out.PaymentState = PaymentUnjoined
	default:
		out.PaymentState = PaymentAgreed
		out.AgreedStatus = a["status"].(string)
		agreedTerms, _ := parseAmount(terms.s["amount"])
		out.TermsAmount = "differs"
		if sent.Equal(agreedTerms) {
			out.TermsAmount = "equal"
		}
	}
}

// sameReference is §7.1's join rule for two references of one registered
// type: every member identical after the type's normal form.
func sameReference(a, b map[string]any) bool {
	return maps2string(normalized(a)) == maps2string(normalized(b))
}

func normalized(ref map[string]any) map[string]string {
	out := make(map[string]string, len(ref))
	for name, value := range ref {
		out[name] = value.(string)
	}
	if registered := paymentRefTypes[out["type"]]; registered.lowercase != nil && registered.lowercase(ref) {
		out["value"] = strings.ToLower(out["value"])
	}
	return out
}

func maps2string(m map[string]string) string {
	encoded, _ := json.Marshal(m) // map keys are sorted: a stable form
	return string(encoded)
}

// x402Scheme is §7.2: the x402 scheme, from a wrapped x402.offer (in the terms
// leg) or x402.payment-payload (in the payer-observed leg) whose octets the
// verifier holds. Empty when it cannot be established.
func x402Scheme(from []leg, objects map[string][]byte) string {
	for _, l := range from {
		entries, _ := l.s["wrapped"].([]any)
		for _, item := range entries {
			w := item.(map[string]any)
			octets := objects[w["digest"].(string)]
			if content, present := w["content"]; present {
				octets, _ = base64.RawURLEncoding.DecodeString(content.(string))
			}
			if octets == nil {
				continue
			}
			switch w["type"] {
			case "x402.offer":
				return offerScheme(octets)
			case "x402.payment-payload":
				var payload struct {
					Accepted struct {
						Scheme string `json:"scheme"`
					} `json:"accepted"`
				}
				if json.Unmarshal(octets, &payload) == nil {
					return payload.Accepted.Scheme
				}
				return ""
			}
		}
	}
	return ""
}

// offerScheme reads scheme from an x402 signed offer in either format: a JWS
// compact serialization, or the EIP-712 envelope object whose payload holds
// the offer.
func offerScheme(octets []byte) string {
	var offer struct {
		Scheme string `json:"scheme"`
	}
	if parts := bytes.Split(octets, []byte(".")); len(parts) == 3 {
		payload, err := base64.RawURLEncoding.DecodeString(string(parts[1]))
		if err == nil && json.Unmarshal(payload, &offer) == nil {
			return offer.Scheme
		}
		return ""
	}
	var envelope struct {
		Payload json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(octets, &envelope) == nil && json.Unmarshal(envelope.Payload, &offer) == nil {
		return offer.Scheme
	}
	return ""
}

// deliveryState is §9.3 for one terms leg, with the sources behind it.
func deliveryState(terms leg, answering []leg) (string, []string) {
	deliverable, _ := terms.s["deliverable"].(map[string]any)
	pinned := str(deliverable["content_digest"])
	delivered, digests, directions, sources := 0, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, l := range answering {
		if legOf(l) != LegDelivered {
			continue
		}
		delivered++
		d := l.s["delivery"].(map[string]any)
		switch d["direction"] {
		case "sent":
			sources[EvidenceSellerHandoff] = true
		case "received":
			sources[EvidenceBuyerReceipt] = true
		}
		entries, _ := l.s["wrapped"].([]any)
		for _, item := range entries {
			if item.(map[string]any)["type"] == "delivery.proof" {
				sources[EvidenceCarrierConfirmation] = true
			}
		}
		if digest := str(d["content_digest"]); digest != "" {
			digests[digest] = true
			directions[d["direction"].(string)] = true
		}
	}
	var evidence []string
	for _, source := range []string{EvidenceSellerHandoff, EvidenceCarrierConfirmation, EvidenceBuyerReceipt} {
		if sources[source] {
			evidence = append(evidence, source)
		}
	}
	switch {
	case delivered == 0:
		return DeliveryNone, nil
	case len(digests) > 1 || (pinned != "" && len(digests) == 1 && !digests[pinned]):
		return DeliveryMismatch, evidence
	case directions["sent"] && directions["received"]:
		return DeliveryMatched, evidence
	}
	return DeliveryStated, evidence
}
