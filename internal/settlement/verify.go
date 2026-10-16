// Package settlement builds the settlement leg records the payee seals and
// reports the verifier's reading of a set of legs, both for
// draft-mih-agent-settlement-records-00.
//
// Verification and state derivation are agent-action-capsule's
// (go/settlement.Verify). This package maps capsule-cli's leg files onto that
// verifier's input and its capsule-ID-keyed result back onto the labels the
// caller gave, and adds which sources back each delivery state.
package settlement

import (
	"encoding/hex"

	"github.com/action-state-group/agent-action-capsule/go/envelope"
	aacsettlement "github.com/action-state-group/agent-action-capsule/go/settlement"
)

// Version is the settlement member version the legs this package builds carry.
const Version = aacsettlement.Version

// The four legs (§4.2).
const (
	LegTerms         = "terms"
	LegPayerObserved = "payer_observed"
	LegPayeeObserved = "payee_observed"
	LegDelivered     = "delivered"
)

// The two sealer roles (§3).
const (
	RolePayer = "payer"
	RolePayee = "payee"
)

// Payment states (§9.2).
const (
	PaymentTermsOnly   = "terms_only"
	PaymentPayerStated = "payer_stated"
	PaymentPayeeStated = "payee_stated"
	PaymentAgreed      = "agreed"
	PaymentMismatch    = "mismatch"
	PaymentUnjoined    = "unjoined"
)

// Delivery states (§9.3).
const (
	DeliveryNone     = "none"
	DeliveryStated   = "stated"
	DeliveryMatched  = "matched"
	DeliveryMismatch = "mismatch"
)

// The three delivery evidence sources, each a distinct kind of claim.
const (
	EvidenceSellerHandoff       = "seller_handoff"
	EvidenceCarrierConfirmation = "carrier_confirmation"
	EvidenceBuyerReceipt        = "buyer_receipt"
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

// Verify runs agent-action-capsule's settlement verifier over the legs and
// reports its answer by the caller's labels.
func Verify(in Input) Result {
	capsules := make([]interface{}, 0, len(in.Legs))
	keys := make([]string, len(in.Legs))
	for i, l := range in.Legs {
		capsules = append(capsules, wireCapsule(l, &keys[i]))
	}
	aac := aacsettlement.Verify(aacsettlement.Input{Capsules: capsules, WrappedObjects: in.Objects, KeyPolicy: in.Policy})

	labels := newLabels(in.Legs)
	result := Result{
		Conforming:  aac.Conforming,
		Failures:    labels.reports(aac.Failures),
		Findings:    labels.reports(aac.Findings),
		Settlements: make([]Settlement, 0, len(aac.Settlements)),
		Legs:        make([]LegKey, 0, len(in.Legs)),
		KeyPolicy:   "not_applied",
	}
	if aac.KeyPolicyApplied {
		result.KeyPolicy = "applied"
	}
	for i, l := range in.Legs {
		entry := LegKey{Record: l.Label}
		id, _ := l.Capsule["capsule_id"].(string)
		// The verifier keys each authenticated record by "<capsule_id>:<key>";
		// a leg without that entry failed the Capsule or envelope checks.
		if keys[i] != "" && aac.Keys[id+":"+keys[i]] == keys[i] {
			entry.CapsuleID, entry.AuthenticatedKey = id, keys[i]
		}
		result.Legs = append(result.Legs, entry)
	}
	excluded := labels.excluded(aac.Failures)
	for _, s := range aac.Settlements {
		out := Settlement{
			Terms:         labels.first(s.Terms),
			PaymentState:  PaymentUnjoined,
			AgreedStatus:  s.AgreedStatus,
			TermsAmount:   s.TermsAmount,
			Differs:       s.Differs,
			DeliveryState: s.DeliveryState,
		}
		// No state of the draft applies to a conflated pair that otherwise
		// matches (sealer_conflation is listed as a failure); capsule-cli
		// reports it as legs that cannot be joined, never as agreed.
		if s.PaymentState != nil {
			out.PaymentState = *s.PaymentState
		}
		if s.DeliveryState != DeliveryNone {
			out.DeliveryEvidence = deliveryEvidence(s.Terms, in.Legs, excluded)
		}
		result.Settlements = append(result.Settlements, out)
	}
	return result
}

// wireCapsule is the leg as the verifier reads it: the Capsule carrying its
// Producer Envelope as signature (hex) and, when the envelope verifies over
// the leg's capsule_id, the key it names as key_id (written to *key).
func wireCapsule(l Leg, key *string) interface{} {
	if l.Capsule == nil {
		return nil
	}
	out := make(map[string]any, len(l.Capsule)+2)
	for name, value := range l.Capsule {
		out[name] = value
	}
	delete(out, "signature")
	delete(out, "key_id")
	if l.Envelope == nil {
		return out
	}
	out["signature"] = hex.EncodeToString(l.Envelope)
	if id, isString := l.Capsule["capsule_id"].(string); isString {
		if signed := envelope.Verify(id, l.Envelope); signed.OK {
			*key = hex.EncodeToString(signed.PublicKey)
			out["key_id"] = *key
		}
	}
	return out
}

// labels maps the verifier's record ids (capsule_id, or nil when a leg's
// capsule_id is not a string) back to the caller's labels, in input order.
type labels struct {
	byID map[string][]string
	noID []string
}

func newLabels(legs []Leg) labels {
	out := labels{byID: map[string][]string{}}
	for _, l := range legs {
		id, isString := l.Capsule["capsule_id"].(string)
		if !isString {
			out.noID = append(out.noID, l.Label)
			continue
		}
		out.byID[id] = append(out.byID[id], l.Label)
	}
	return out
}

func (m labels) of(id *string) []string {
	if id == nil {
		return m.noID
	}
	return m.byID[*id]
}

func (m labels) first(id string) string {
	if names := m.byID[id]; len(names) > 0 {
		return names[0]
	}
	return id
}

func (m labels) reports(entries []aacsettlement.Entry) []Report {
	out := make([]Report, 0, len(entries))
	for _, e := range entries {
		records := []string{}
		for _, id := range e.Records {
			records = append(records, m.of(id)...)
		}
		out = append(out, Report{Records: records, Code: e.Code})
	}
	return out
}

// excluded is the capsule ids a failure of their own names: those legs take
// no part in any state. sealer_conflation, a failure of a pair, excludes
// neither leg.
func (m labels) excluded(failures []aacsettlement.Entry) map[string]bool {
	out := map[string]bool{}
	for _, f := range failures {
		if len(f.Records) == 1 && f.Records[0] != nil {
			out[*f.Records[0]] = true
		}
	}
	return out
}

// deliveryEvidence names the sources behind a delivery state from the
// delivered legs that answer the terms leg termsID and took part in it. The
// verifier derives the state; which sources back it is capsule-cli's reading.
func deliveryEvidence(termsID string, legs []Leg, excluded map[string]bool) []string {
	sources := map[string]bool{}
	for _, l := range legs {
		id, _ := l.Capsule["capsule_id"].(string)
		s, _ := l.Capsule["settlement"].(map[string]any)
		if id == "" || excluded[id] || s["leg"] != LegDelivered || s["terms_ref"] != termsID {
			continue
		}
		d, _ := s["delivery"].(map[string]any)
		switch d["direction"] {
		case "sent":
			sources[EvidenceSellerHandoff] = true
		case "received":
			sources[EvidenceBuyerReceipt] = true
		}
		entries, _ := s["wrapped"].([]any)
		for _, item := range entries {
			if w, _ := item.(map[string]any); w["type"] == "delivery.proof" {
				sources[EvidenceCarrierConfirmation] = true
			}
		}
	}
	var evidence []string
	for _, source := range []string{EvidenceSellerHandoff, EvidenceCarrierConfirmation, EvidenceBuyerReceipt} {
		if sources[source] {
			evidence = append(evidence, source)
		}
	}
	return evidence
}
