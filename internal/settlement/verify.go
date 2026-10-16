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
	"slices"

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
	views := make([]legView, len(in.Legs))
	for i, l := range in.Legs {
		views[i] = newLegView(l, capsules[i], keys[i], in)
	}

	result := Result{
		Conforming:  aac.Conforming,
		Failures:    reports(aac.Failures, views),
		Findings:    reports(aac.Findings, views),
		Settlements: []Settlement{},
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
	// The verifier derives one settlement per terms capsule_id; each terms
	// leg that took part is reported, in input order, as the legs were before.
	for i, v := range views {
		if !v.live() || v.leg() != LegTerms {
			continue
		}
		for _, s := range aac.Settlements {
			if s.Terms != *v.id {
				continue
			}
			out := Settlement{
				Terms:         in.Legs[i].Label,
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
				out.DeliveryEvidence = deliveryEvidence(s.Terms, views)
			}
			result.Settlements = append(result.Settlements, out)
		}
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

// legView is one input leg as the mapping reads it: its label, its record id
// as the verifier names it (nil when capsule_id is not a string), and the
// codes the verifier reports for this leg on its own.
type legView struct {
	label string
	id    *string
	// key is the hex key its envelope authenticates, "" when none.
	key string
	s   map[string]any
	// failures are its record-level failure codes and findings its
	// record-level finding codes, from verifying the leg alone: everything
	// that does not depend on another leg. terms_ref_unresolved, which a leg
	// alone always reports, is left out.
	failures, findings map[string]bool
}

// newLegView verifies the leg alone, with the same wrapped objects and key
// policy, to learn which record-level codes are its own. The verifier names
// a record by capsule_id only, so this is what tells apart two legs that
// share one (a valid copy and a re-signed or broken copy).
func newLegView(l Leg, wire interface{}, key string, in Input) legView {
	v := legView{label: l.Label, key: key, failures: map[string]bool{}, findings: map[string]bool{}}
	if id, isString := l.Capsule["capsule_id"].(string); isString {
		v.id = &id
	}
	v.s, _ = l.Capsule["settlement"].(map[string]any)
	alone := aacsettlement.Verify(aacsettlement.Input{Capsules: []interface{}{wire}, WrappedObjects: in.Objects, KeyPolicy: in.Policy})
	for _, f := range alone.Failures {
		if f.Code != "terms_ref_unresolved" {
			v.failures[f.Code] = true
		}
	}
	for _, f := range alone.Findings {
		v.findings[f.Code] = true
	}
	return v
}

// live is whether the leg took part in the derived states: it has no failure
// of its own.
func (v legView) live() bool { return v.id != nil && len(v.failures) == 0 }

func (v legView) leg() string { text, _ := v.s["leg"].(string); return text }

func (v legView) names(id *string) bool {
	return (id == nil && v.id == nil) || (id != nil && v.id != nil && *id == *v.id)
}

// reports maps the verifier's entries back to labels. A record-level code
// names only the legs with that capsule_id whose own codes include it, one
// leg per entry in input order (the verifier reports them in that order); a
// leg passed twice under one key is reported once by the verifier and keeps
// every label. Any other entry is about legs together: each record names its
// live legs, or every leg with that capsule_id when none is live.
func reports(entries []aacsettlement.Entry, views []legView) []Report {
	remaining := map[string]int{}
	for _, e := range entries {
		if len(e.Records) == 1 {
			remaining[entryKey(e)]++
		}
	}
	used := map[string]int{}
	out := make([]Report, 0, len(entries))
	for _, e := range entries {
		records := []string{}
		var own []legView
		if len(e.Records) == 1 {
			for _, v := range views {
				if v.names(e.Records[0]) && (v.failures[e.Code] || v.findings[e.Code]) {
					own = append(own, v)
				}
			}
		}
		switch {
		case len(own) > 0:
			key := entryKey(e)
			remaining[key]--
			from := min(used[key], len(own)-1)
			to := from + 1
			if remaining[key] == 0 {
				to = len(own)
			}
			used[key] = to
			for _, v := range own[from:to] {
				records = append(records, v.label)
			}
		case e.Code == "fee_asset_differs":
			records = feeAssetDiffers(e.Records[0], views)
		case e.Code == "sealer_conflation" && len(e.Records) == 2:
			records = conflated(e.Records[0], e.Records[1], views)
		default:
			for _, id := range e.Records {
				records = append(records, together(id, views)...)
			}
		}
		out = append(out, Report{Records: records, Code: e.Code})
	}
	return out
}

func entryKey(e aacsettlement.Entry) string {
	if e.Records[0] == nil {
		return e.Code + "\x00"
	}
	return e.Code + "\x00:" + *e.Records[0]
}

// together is the labels a record of an entry about several legs names: its
// live legs, or every leg with that capsule_id when none is live.
func together(id *string, views []legView) []string {
	var live, all []string
	for _, v := range views {
		if v.names(id) {
			all = append(all, v.label)
			if v.live() {
				live = append(live, v.label)
			}
		}
	}
	if len(live) > 0 {
		return live
	}
	return all
}

// conflated is the labels of a sealer_conflation pair: the payer-observed
// legs with capsule_id payer and the payee-observed legs with capsule_id
// payee that share an authenticated key.
func conflated(payer, payee *string, views []legView) []string {
	var payers, payees []string
	for _, p := range views {
		if !p.names(payer) || p.key == "" || p.leg() != LegPayerObserved {
			continue
		}
		for _, q := range views {
			if q.names(payee) && q.key == p.key && q.leg() == LegPayeeObserved {
				if !slices.Contains(payers, p.label) {
					payers = append(payers, p.label)
				}
				if !slices.Contains(payees, q.label) {
					payees = append(payees, q.label)
				}
			}
		}
	}
	if len(payers) == 0 {
		return append(together(payer, views), together(payee, views)...)
	}
	return append(payers, payees...)
}

// feeAssetDiffers names the leg whose fee is in another asset. The verifier
// names the payee-observed leg for either fee; when the payee's receive fee
// is in its received asset, it is the payer's routing_fee that differs from
// the payer's amount, so the payer-observed leg is named.
func feeAssetDiffers(payee *string, views []legView) []string {
	named := together(payee, views)
	for _, v := range views {
		if !v.names(payee) || !v.live() || v.leg() != LegPayeeObserved {
			continue
		}
		received := assetOf(v.s["received"])
		fee := received
		if _, stated := v.s["receive_fee"]; stated {
			fee = assetOf(v.s["receive_fee"])
		}
		if fee != received {
			return named
		}
		var payers []string
		for _, p := range views {
			_, routed := p.s["routing_fee"]
			if p.live() && p.leg() == LegPayerObserved && p.s["terms_ref"] == v.s["terms_ref"] && routed && assetOf(p.s["routing_fee"]) != assetOf(p.s["amount"]) {
				payers = append(payers, p.label)
			}
		}
		if len(payers) > 0 {
			return payers
		}
	}
	return named
}

func assetOf(amount any) any {
	object, _ := amount.(map[string]any)
	return object["assetCode"]
}

// deliveryEvidence names the sources behind a delivery state from the
// delivered legs that answer the terms leg termsID and took part in it. The
// verifier derives the state; which sources back it is capsule-cli's reading.
func deliveryEvidence(termsID string, views []legView) []string {
	sources := map[string]bool{}
	for _, v := range views {
		if !v.live() || v.leg() != LegDelivered || v.s["terms_ref"] != termsID {
			continue
		}
		d, _ := v.s["delivery"].(map[string]any)
		switch d["direction"] {
		case "sent":
			sources[EvidenceSellerHandoff] = true
		case "received":
			sources[EvidenceBuyerReceipt] = true
		}
		entries, _ := v.s["wrapped"].([]any)
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
