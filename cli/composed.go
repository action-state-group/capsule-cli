package cli

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/agent-action-capsule/go/evidencerequest"
	"github.com/action-state-group/agent-action-capsule/go/witness"
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

// EvidenceBookRefusalProfile names the second refusal form this CLI checks:
// the one `respond` sends (evidencebook's Refusal). It carries `key_id` and
// `sig` (an Ed25519 signature, 128 lowercase hex) over the UTF-8 JCS
// serialization of {request_digest, reason, issued_at} only.
const EvidenceBookRefusalProfile = "evidencebook-refusal"

// verifyRefusalSignature checks a carried refusal under RefusalSignatureProfile,
// or, when it carries `sig` and no `signature`, under
// EvidenceBookRefusalProfile. A refusal carrying no signature member is not
// in either form (unverified); one carrying a key without its signature, or
// whose signature does not verify, is invalid.
func verifyRefusalSignature(refusal map[string]interface{}) string {
	rawKey, hasKey := refusal["key_id"]
	rawSignature, hasSignature := refusal["signature"]
	if refusalProfile(refusal) == EvidenceBookRefusalProfile {
		return verifyEvidenceBookRefusal(refusal, rawKey, refusal["sig"])
	}
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

// refusalProfile is the profile a carried refusal is checked under: a
// refusal carrying `sig` and no `signature` is in EvidenceBookRefusalProfile.
func refusalProfile(refusal map[string]interface{}) string {
	_, hasSig := refusal["sig"]
	if _, hasSignature := refusal["signature"]; hasSig && !hasSignature {
		return EvidenceBookRefusalProfile
	}
	return RefusalSignatureProfile
}

// verifyEvidenceBookRefusal checks a refusal under EvidenceBookRefusalProfile.
func verifyEvidenceBookRefusal(refusal map[string]interface{}, rawKey, rawSig interface{}) string {
	key, keyOK := rawKey.(string)
	sig, sigOK := rawSig.(string)
	if !keyOK || !sigOK || !refusalKeyRE.MatchString(key) || !refusalSignatureRE.MatchString(sig) {
		return aacbundle.RefusalSignatureInvalid
	}
	signed := map[string]interface{}{}
	for _, name := range []string{"request_digest", "reason", "issued_at"} {
		member, ok := refusal[name].(string)
		if !ok {
			return aacbundle.RefusalSignatureInvalid
		}
		signed[name] = member
	}
	message, err := canonical.JCS(signed)
	if err != nil {
		return aacbundle.RefusalSignatureInvalid
	}
	keyBytes, _ := hex.DecodeString(key)
	sigBytes, _ := hex.DecodeString(sig)
	if !ed25519.Verify(ed25519.PublicKey(keyBytes), message, sigBytes) {
		return aacbundle.RefusalSignatureInvalid
	}
	return aacbundle.RefusalSignatureVerified
}

// ResponseSignatureProfile names the first form this CLI checks an artifact
// member's carried response in: evidencebook's signed artifact response
// without its artifact bytes. It carries `key_id` and `sig` (an Ed25519
// signature, 128 lowercase hex) over the response's signing body, which
// names the artifact by artifact_digest only, so it verifies without the
// artifact.
const ResponseSignatureProfile = "evidencebook-artifact-response"

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
	return ResponseSignatureProfile
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

// verifyResponseSignature checks a carried response in its form. A response
// carrying none of `key_id`, `responder` and `sig` is in neither form
// (unverified); any other that does not verify is invalid.
func verifyResponseSignature(response map[string]interface{}) string {
	if responseProfile(response) == InteropResponseProfile {
		return verifyInteropResponse(response)
	}
	rawKey, hasKey := response["key_id"]
	rawSig, hasSig := response["sig"]
	if !hasKey && !hasSig {
		return aacbundle.RefusalSignatureUnverified
	}
	key, keyOK := rawKey.(string)
	sig, sigOK := rawSig.(string)
	if !keyOK || !sigOK || !refusalKeyRE.MatchString(key) || !refusalSignatureRE.MatchString(sig) {
		return aacbundle.RefusalSignatureInvalid
	}
	fields := map[string]string{}
	for _, name := range []string{"request_digest", "anchor", "artifact_kind", "artifact_digest", "issued_at"} {
		member, ok := response[name].(string)
		if !ok {
			return aacbundle.RefusalSignatureInvalid
		}
		fields[name] = member
	}
	body, err := ArtifactResponseSigningBody(fields["request_digest"], fields["anchor"], fields["artifact_kind"], fields["artifact_digest"], fields["issued_at"])
	if err != nil {
		return aacbundle.RefusalSignatureInvalid
	}
	keyBytes, _ := hex.DecodeString(key)
	sigBytes, _ := hex.DecodeString(sig)
	if !ed25519.Verify(ed25519.PublicKey(keyBytes), body, sigBytes) {
		return aacbundle.RefusalSignatureInvalid
	}
	return aacbundle.RefusalSignatureVerified
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
	if !ok || !slices.Contains(evidencerequest.Profiles, envelope.Profile) || !refusalSignatureRE.MatchString(envelope.Sig) {
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
// never makes the verdict INVALID. A carried response signed by a key other
// than its member's observer fails (INVALID).
func composedOutput(container map[string]interface{}, result *aacbundle.ComposedResult, directory *witness.Directory) (map[string]any, []string) {
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
			member["reads_as"] = "INCOMPLETE"
			verdicts = append(verdicts, "INCOMPLETE")
		}
		if m.RefusalSignature != "" {
			profile := RefusalSignatureProfile
			if refusal, ok := composedMemberRefusal(container, m.ID); ok && refusalProfile(refusal) == EvidenceBookRefusalProfile {
				profile = EvidenceBookRefusalProfile
			}
			member["refusal_signature"] = map[string]any{"status": m.RefusalSignature, "profile": profile}
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
	return composedMemberBody(container, id, "bundle")
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

// artifactResponseType is the context member of an artifact response's
// signing body, so its signature cannot be read as any other statement.
const artifactResponseType = "evidence-artifact-response/v1"

// ArtifactResponseSigningBody is the exact bytes a signed artifact response
// (ResponseSignatureProfile) covers: its context and the five members that
// name the request, the anchor and the artifact by digest. The deal plugin
// signs this body; verify --bundle checks a composed member's carried
// response over it.
func ArtifactResponseSigningBody(requestDigest, anchor, artifactKind, artifactDigest, issuedAt string) ([]byte, error) {
	return canonical.JCS(map[string]interface{}{
		"type": artifactResponseType, "request_digest": requestDigest, "anchor": anchor,
		"artifact_kind": artifactKind, "artifact_digest": artifactDigest, "issued_at": issuedAt,
	})
}

// BytesDigest is the lowercase hex SHA-256 of data.
func BytesDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
