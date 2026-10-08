package cli

import (
	"crypto/ed25519"
	"encoding/hex"
	"regexp"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/canonical"
)

// RefusalSignatureProfile names the one refusal signature form this CLI
// checks. The Evidence Request draft leaves the format to the deployment; a
// refusal in any other form is reported signature_unverified, never failed.
//
// Form: the refusal carries `key_id` (the responder's raw Ed25519 public key,
// 64 lowercase hex) and `signature` (an Ed25519 signature, 128 lowercase hex)
// over the UTF-8 JCS serialization of the refusal object without `signature`
// (so request_digest, reason, issued_at and key_id are all signed).
const RefusalSignatureProfile = "ed25519-jcs"

var (
	refusalKeyRE       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	refusalSignatureRE = regexp.MustCompile(`^[0-9a-f]{128}$`)
)

// verifyRefusalSignature checks a carried refusal under RefusalSignatureProfile.
// A refusal carrying neither member is not in this form (unverified); one
// carrying only one of them, or whose signature does not verify, is invalid.
func verifyRefusalSignature(refusal map[string]interface{}) string {
	rawKey, hasKey := refusal["key_id"]
	rawSignature, hasSignature := refusal["signature"]
	if !hasKey && !hasSignature {
		return aacbundle.RefusalSignatureUnverified
	}
	key, keyOK := rawKey.(string)
	signature, signatureOK := rawSignature.(string)
	if !keyOK || !signatureOK || !refusalKeyRE.MatchString(key) || !refusalSignatureRE.MatchString(signature) {
		return aacbundle.RefusalSignatureInvalid
	}
	unsigned := make(map[string]interface{}, len(refusal))
	for name, member := range refusal {
		if name != "signature" {
			unsigned[name] = member
		}
	}
	message, err := canonical.JCS(unsigned)
	if err != nil {
		return aacbundle.RefusalSignatureInvalid
	}
	keyBytes, _ := hex.DecodeString(key)
	signatureBytes, _ := hex.DecodeString(signature)
	if !ed25519.Verify(ed25519.PublicKey(keyBytes), message, signatureBytes) {
		return aacbundle.RefusalSignatureInvalid
	}
	return aacbundle.RefusalSignatureVerified
}

// composedOutput renders a verified composed/v1 block for `verify --bundle`:
// the recomputed composed digest, each member (with a carried member bundle
// assessed as its own Evidence Bundle), composition closure, each join's
// declared and derived state, and per join whether a derived agreement is
// redundant or corroborating on declared custody. It also returns each
// carried member bundle's verdict.
func composedOutput(container map[string]interface{}, result *aacbundle.ComposedResult, directory []witnessRow) (map[string]any, []string) {
	out := map[string]any{
		"status":   result.Status,
		"findings": nonNilStrings(result.Findings),
		"scope":    "composition closure covers the declared members only; it is not completeness of participation",
	}
	if result.Malformed {
		return out, nil
	}
	out["composed_digest"] = map[string]any{
		"declared":   result.ComposedDigest.Declared,
		"recomputed": result.ComposedDigest.Recomputed,
		"matches":    result.ComposedDigest.Matches,
	}

	var verdicts []string
	members := make([]map[string]any, 0, len(result.Members))
	for _, m := range result.Members {
		member := map[string]any{
			"id":       m.ID,
			"observer": m.Observer,
			"outcome":  m.Outcome,
			"body":     m.Body,
			"digest":   m.Digest,
			"findings": nonNilStrings(m.Findings),
		}
		if m.Bundle != nil {
			raw, _ := composedMemberBundle(container, m.ID)
			report, verdict := assessBundle(raw, *m.Bundle, directory)
			member["bundle"] = report
			verdicts = append(verdicts, verdict)
		}
		if m.RefusalSignature != "" {
			member["refusal_signature"] = map[string]any{"status": m.RefusalSignature, "profile": RefusalSignatureProfile}
		}
		members = append(members, member)
	}
	out["members"] = members
	out["composition_closure"] = map[string]any{
		"status":   result.CompositionClosure.Status,
		"findings": nonNilStrings(result.CompositionClosure.Findings),
		"missing":  nonNilStrings(result.CompositionClosure.Missing),
	}

	joins := make([]map[string]any, 0, len(result.Joins))
	for _, j := range result.Joins {
		var derived any
		if j.Derived != "" {
			derived = j.Derived
		}
		join := map[string]any{
			"members":  j.Members,
			"basis":    j.Basis,
			"declared": j.Declared,
			"derived":  derived,
			"result":   j.Result,
		}
		if len(j.Differences) != 0 {
			differences := make([]map[string]any, 0, len(j.Differences))
			for _, d := range j.Differences {
				values := make([]map[string]any, 0, len(d.Values))
				for _, v := range d.Values {
					value := map[string]any{"member": v.Member, "resolved": v.Resolved}
					if v.Resolved {
						value["value"] = v.Value
					}
					values = append(values, value)
				}
				differences = append(differences, map[string]any{"pointer": d.Pointer, "values": values})
			}
			join["differences"] = differences
		}
		joins = append(joins, join)
	}
	out["joins"] = joins

	corroboration := make([]map[string]any, 0, len(result.Corroboration))
	for _, c := range result.Corroboration {
		pair := map[string]any{"members": c.Members, "result": c.Result}
		if c.Result == "redundant" {
			pair["reason"] = c.Reason
			pair["reasons"] = c.Reasons
			pair["report"] = c.Report
		}
		if c.Qualifier != "" {
			pair["qualifier"] = c.Qualifier
		}
		corroboration = append(corroboration, pair)
	}
	out["corroboration"] = corroboration
	return out, verdicts
}

// composedMemberBundle returns the carried bundle of the member with id.
func composedMemberBundle(container map[string]interface{}, id string) (map[string]interface{}, bool) {
	extensions, _ := container["extensions"].(map[string]interface{})
	block, _ := extensions[aacbundle.ComposedKind].(map[string]interface{})
	members, _ := block["members"].([]interface{})
	for _, raw := range members {
		member, _ := raw.(map[string]interface{})
		if member["id"] == id {
			bundle, ok := member["bundle"].(map[string]interface{})
			return bundle, ok
		}
	}
	return nil, false
}
