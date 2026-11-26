package cli

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/evidencerequest"
)

// The carried signature forms this CLI checks are agent-action-capsule's
// composed/v1 carried signature profiles (draft-mih-zhang-agent-disclosure-bundle-03,
// "Carried Signature Profiles"): the forms, their signing inputs and strict
// Ed25519 (a small-order key or R never verifies) are AAC's, and the CLI only
// names them in its report.
const (
	// WholeRefusalProfile signs the whole refusal without `signature`.
	WholeRefusalProfile = aacbundle.WholeRefusalProfile
	// ThreeMemberRefusalProfile signs a refusal's issued_at, reason and
	// request_digest with `sig`.
	ThreeMemberRefusalProfile = aacbundle.ThreeMemberRefusalProfile
	// DigestBoundResponseProfile signs an artifact response without its
	// artifact bytes, naming the artifact by artifact_digest.
	DigestBoundResponseProfile = aacbundle.DigestBoundArtifactResponseProfile
)

// interopSigRE is the form of an interop envelope's `sig`: an Ed25519
// signature as 128 lowercase hex.
var interopSigRE = regexp.MustCompile(`^[0-9a-f]{128}$`)

// verifyRefusalSignature checks a carried refusal in the profile AAC reads it
// in (aacbundle.VerifyCarriedRefusal).
func verifyRefusalSignature(refusal map[string]interface{}) string {
	return aacbundle.VerifyCarriedRefusal(refusal)
}

// refusalProfile is the profile a carried refusal is read in, "" for one AAC
// reads in none of its profiles (a three-member refusal that also carries
// policy_digest), whose signature stays unverified.
func refusalProfile(refusal map[string]interface{}) string {
	return aacbundle.CarriedRefusalProfile(refusal)
}

// InteropResponseProfile names the second form: an evidence-interop envelope
// (interop profile §6.2, evidence-interop/0 or /1) as received, minus its
// `artifact`. It carries `responder` and `sig` over the envelope's signing
// body, which names the artifact by artifact_digest only. Its `anchor` is
// bound to the member bundle's checkpoint in composedOutput.
const InteropResponseProfile = "evidence-interop-envelope"

// responseProfile is the form a carried response is checked in: one
// carrying `responder` is an interop envelope.
func responseProfile(response map[string]interface{}) string {
	if _, ok := response["responder"]; ok {
		return InteropResponseProfile
	}
	return DigestBoundResponseProfile
}

// responseKey is the key a carried response is signed under, in its form.
func responseKey(response map[string]interface{}) (string, bool) {
	if responseProfile(response) == InteropResponseProfile {
		key, ok := response["responder"].(string)
		return key, ok
	}
	key, ok := response["key_id"].(string)
	return key, ok
}

// verifyResponseSignature checks a carried response in its form: an interop
// envelope by the interop profile's reader, any other in AAC's
// DigestBoundResponseProfile (aacbundle.VerifyDigestBoundArtifactResponse,
// strict Ed25519).
func verifyResponseSignature(response map[string]interface{}) string {
	if responseProfile(response) == InteropResponseProfile {
		return verifyInteropResponse(response)
	}
	return aacbundle.VerifyDigestBoundArtifactResponse(response)
}

// verifyInteropResponse checks a carried response under
// InteropResponseProfile. The envelope is read by evidencerequest.ParseReply
// with an empty `artifact` put back (it is not signed), so its members,
// signing body and signature follow the profile's own reader. A carried
// `artifact` or `key_id` is not the envelope minus its artifact, so it is
// invalid.
func verifyInteropResponse(response map[string]interface{}) string {
	_, hasArtifact := response["artifact"]
	_, hasKeyID := response["key_id"]
	if hasArtifact || hasKeyID {
		return aacbundle.RefusalSignatureInvalid
	}
	envelope, ok := interopEnvelope(response)
	if !ok || !slices.Contains(evidencerequest.Profiles, envelope.Profile) || !interopSigRE.MatchString(envelope.Sig) {
		return aacbundle.RefusalSignatureInvalid
	}
	body, err := envelope.SigningBody()
	if err != nil {
		return aacbundle.RefusalSignatureInvalid
	}
	sig, _ := hex.DecodeString(envelope.Sig)
	if evidencerequest.VerifySignature(envelope.Responder, body, sig) != nil {
		return aacbundle.RefusalSignatureInvalid
	}
	// The verification items travel beside the signed members, as the
	// artifact does; each must hash to the digest signed at its position,
	// as Envelope.Verify requires, or it is unbound bytes.
	if len(envelope.VerificationContent) != len(envelope.Verification) {
		return aacbundle.RefusalSignatureInvalid
	}
	for i, item := range envelope.VerificationContent {
		if evidencerequest.Digest(item) != envelope.Verification[i].Digest {
			return aacbundle.RefusalSignatureInvalid
		}
	}
	return aacbundle.RefusalSignatureVerified
}

// interopEnvelope reads a carried interop response as an Envelope with no
// artifact bytes.
func interopEnvelope(response map[string]interface{}) (evidencerequest.Envelope, bool) {
	withArtifact := make(map[string]interface{}, len(response)+1)
	for name, member := range response {
		withArtifact[name] = member
	}
	withArtifact["artifact"] = ""
	octets, err := json.Marshal(withArtifact)
	if err != nil {
		return evidencerequest.Envelope{}, false
	}
	reply, err := evidencerequest.ParseReply(evidencerequest.Message{Octets: octets, MediaType: evidencerequest.MediaTypeJSON})
	if err != nil || reply.Envelope == nil {
		return evidencerequest.Envelope{}, false
	}
	return *reply.Envelope, true
}

// interopAnchorBound reports whether an interop response's anchor.digest is
// the SHA-256 of the member bundle's checkpoint COSE octets as carried
// (interop profile §6.2.1): the anchor is that checkpoint.
func interopAnchorBound(response, bundle map[string]interface{}) bool {
	envelope, ok := interopEnvelope(response)
	if !ok {
		return false
	}
	checkpoint, _ := bundle["checkpoint"].(map[string]interface{})
	encoded, ok := checkpoint["cose"].(string)
	if !ok {
		return false
	}
	cose, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(cose) == 0 {
		return false
	}
	return envelope.Anchor.Digest == evidencerequest.Digest(cose)
}

// composedOutput renders a verified composed/v1 block for `verify --bundle`:
// the recomputed composed digest, each member (with a carried member bundle
// assessed as its own Evidence Bundle), composition closure, each join's
// declared and derived state, and per join whether a derived agreement is
// redundant or corroborating on declared custody. It also returns each
// carried member bundle's verdict, and INCOMPLETE for each refusal member:
// what was asked for was not served, so capsulectl reads the composition as
// incomplete. A refusal is still no failed claim (Bundle -01), so it alone
// never makes the verdict INVALID. A refusal whose signature verifies also
// reports served_answer and is counted in the third result instead of the
// verdicts: the caller still reads it INCOMPLETE, but PageGate draws it as a
// refused case. A carried response signed by a key other
// than its member's observer fails (INVALID), as does an interop response
// whose anchor.digest is not the member bundle's checkpoint
// (response_anchor_not_checkpoint).
func composedOutput(container map[string]interface{}, result *aacbundle.ComposedResult, env *assessEnv) (map[string]any, []string, int) {
	out := map[string]any{
		"status":   result.Status,
		"findings": nonNilStrings(result.Findings),
		"scope":    "composition closure covers the declared members only; it is not completeness of participation",
	}
	if result.Malformed {
		return out, nil, 0
	}
	out["composed_digest"] = map[string]any{
		"declared":   result.ComposedDigest.Declared,
		"recomputed": result.ComposedDigest.Recomputed,
		"matches":    result.ComposedDigest.Matches,
	}

	var verdicts []string
	served := 0
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
			report, verdict, _ := assessBundle(raw, *m.Bundle, env)
			member["bundle"] = report
			verdicts = append(verdicts, verdict)
		}
		if m.ResponseSignature != "" {
			response, _ := composedMemberBody(container, m.ID, "response")
			profile := responseProfile(response)
			member["response_signature"] = map[string]any{"status": m.ResponseSignature, "profile": profile}
			findings := nonNilStrings(m.Findings)
			if key, ok := responseKey(response); ok && key != m.Observer {
				findings = append(findings, "response_key_not_observer")
				verdicts = append(verdicts, "INVALID")
			}
			if profile == InteropResponseProfile {
				raw, _ := composedMemberBundle(container, m.ID)
				if !interopAnchorBound(response, raw) {
					findings = append(findings, "response_anchor_not_checkpoint")
					verdicts = append(verdicts, "INVALID")
				}
			}
			member["findings"] = findings
		}
		if m.Outcome == "refusal" {
			// What was asked for was not served, so the member reads
			// INCOMPLETE, whatever the refusal's signature.
			// A refusal whose signature verifies under the observer's own key
			// is also the observer's answer: served_answer lets the deal page
			// draw the refused case (PageGate). The signature check binds the
			// refusal to the key it carries, so, as for a carried response,
			// any other key fails the member (INVALID). Under the
			// three-member profile key_id is outside the signed body, so the
			// attribution rests on the composed block as carried (as every
			// observer comparison here does), not on the signature alone;
			// trusted keys are deployment policy and are not pinned here.
			member["reads_as"] = "INCOMPLETE"
			key, _ := composedMemberRefusalKey(container, m.ID)
			switch {
			case m.RefusalSignature == aacbundle.RefusalSignatureVerified && key != m.Observer:
				member["findings"] = append(nonNilStrings(m.Findings), "refusal_key_not_observer")
				verdicts = append(verdicts, "INVALID")
			case m.RefusalSignature == aacbundle.RefusalSignatureVerified:
				member["served_answer"] = true
				served++
			default:
				verdicts = append(verdicts, "INCOMPLETE")
			}
		}
		if m.RefusalSignature != "" {
			// The profile AAC read the refusal in; a refusal in none of its
			// profiles stays unverified and names none.
			entry := map[string]any{"status": m.RefusalSignature}
			profile := WholeRefusalProfile
			if refusal, ok := composedMemberRefusal(container, m.ID); ok {
				profile = refusalProfile(refusal)
			}
			if profile != "" {
				entry["profile"] = profile
			}
			member["refusal_signature"] = entry
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
	return out, verdicts, served
}

// composedMemberBundle returns the carried bundle of the member with id.
func composedMemberBundle(container map[string]interface{}, id string) (map[string]interface{}, bool) {
	return composedMemberBody(container, id, "bundle")
}

// composedMemberRefusalKey returns the key_id a member's carried refusal
// names as its signer.
func composedMemberRefusalKey(container map[string]interface{}, id string) (string, bool) {
	refusal, _ := composedMemberRefusal(container, id)
	key, ok := refusal["key_id"].(string)
	return key, ok
}

// composedMemberRefusal returns the carried refusal of member id.
func composedMemberRefusal(container map[string]interface{}, id string) (map[string]interface{}, bool) {
	return composedMemberBody(container, id, "refusal")
}

func composedMemberBody(container map[string]interface{}, id, key string) (map[string]interface{}, bool) {
	extensions, _ := container["extensions"].(map[string]interface{})
	block, _ := extensions[aacbundle.ComposedKind].(map[string]interface{})
	members, _ := block["members"].([]interface{})
	for _, raw := range members {
		member, _ := raw.(map[string]interface{})
		if member["id"] == id {
			body, ok := member[key].(map[string]interface{})
			return body, ok
		}
	}
	return nil, false
}

// ArtifactResponseSigningBody is the exact bytes a signed artifact response
// (DigestBoundResponseProfile) covers: its context and the five members that
// name the request, the anchor and the artifact by digest: AAC's
// aacbundle.ArtifactResponseSigningInput, kept here for a plugin that signs it.
func ArtifactResponseSigningBody(requestDigest, anchor, artifactKind, artifactDigest, issuedAt string) ([]byte, error) {
	return aacbundle.ArtifactResponseSigningInput(requestDigest, anchor, artifactKind, artifactDigest, issuedAt)
}

// BytesDigest is the lowercase hex SHA-256 of data.
func BytesDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
