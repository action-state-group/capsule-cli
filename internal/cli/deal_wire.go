package cli

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/evidencebook"
)

// The deal copy's request and artifact response, as a deal profile writes
// and reads them. evidencebook wrote this shape until it moved to the
// evidence-interop/0 profile envelope, which serves its own book only and
// never an Evidence Bundle. A deal copy is an Evidence Bundle, and a
// composed/v1 artifact member carries this response as its "response", so
// the deal path keeps the shape unchanged: the same request bytes and
// digest, the same signing body, the same files a composer reads. Refusals
// are evidencebook.Refusal, whose signing body did not change.

// dealArtifactBundle is the one artifact kind a deal copy is served as.
const dealArtifactBundle = "evidence-bundle/v2"

// dealOutcomeArtifactFailed is the outcome a deal request record carries for a
// genuinely signed copy that does not hold up. evidencebook since spells its
// own book outcome artifact_verification_failed; deal records keep theirs.
const dealOutcomeArtifactFailed = "artifact_failed_verification"

// dealArtifactResponseType is the context member of an artifact response's
// signing body, so its signature cannot be read as any other statement.
const dealArtifactResponseType = "evidence-artifact-response/v1"

// dealPin names a checkpoint the requester already holds.
type dealPin struct {
	Root    string `json:"root"`
	MMRSize uint64 `json:"mmr_size"`
}

// dealFreshness is a minimum coverage: a log size, a time, or both.
type dealFreshness struct {
	Size uint64 `json:"size,omitempty"`
	Time string `json:"time,omitempty"`
}

type dealCoverage struct {
	ExpectedPin  *dealPin       `json:"expected_pin,omitempty"`
	MinFreshness *dealFreshness `json:"min_freshness,omitempty"`
}

// dealSubjectRef names what is asked for. A deal is a correlation subject
// whose value is the deal id; the other members are kept so a request that
// sets them is read, and refused, rather than silently narrowed.
type dealSubjectRef struct {
	Kind   evidencebook.SubjectKind `json:"kind"`
	Digest string                   `json:"digest,omitempty"`
	First  uint64                   `json:"first,omitempty"`
	Last   uint64                   `json:"last,omitempty"`
	Value  string                   `json:"value,omitempty"`
}

// dealEvidenceRequest is the request map. Nonce and Route never influence
// the copy served.
type dealEvidenceRequest struct {
	Subject    dealSubjectRef `json:"subject"`
	Coverage   dealCoverage   `json:"coverage"`
	Derivation string         `json:"derivation,omitempty"`
	Deadline   string         `json:"deadline,omitempty"`
	Nonce      string         `json:"nonce,omitempty"`
	Route      string         `json:"route,omitempty"`
}

// dealArtifactResponse is the signed granting answer: the copy, the anchor
// it verifies against, and a signature binding both to the request.
type dealArtifactResponse struct {
	RequestDigest  string          `json:"request_digest"`
	Anchor         string          `json:"anchor"`
	ArtifactKind   string          `json:"artifact_kind"`
	Artifact       json.RawMessage `json:"artifact"`
	ArtifactDigest string          `json:"artifact_digest"`
	IssuedAt       string          `json:"issued_at"`
	KeyID          string          `json:"key_id"`
	Sig            string          `json:"sig"`
}

// SigningBody is the exact bytes an artifact response signature covers.
func (a dealArtifactResponse) SigningBody() ([]byte, error) {
	return canonical.JCS(map[string]interface{}{
		"type": dealArtifactResponseType, "request_digest": a.RequestDigest, "anchor": a.Anchor,
		"artifact_kind": a.ArtifactKind, "artifact_digest": a.ArtifactDigest, "issued_at": a.IssuedAt,
	})
}

// Verify checks that the artifact bytes hash to the signed digest and the
// signature verifies under the response's own key_id.
func (a dealArtifactResponse) Verify() error {
	if bytesDigest(a.Artifact) != a.ArtifactDigest {
		return fmt.Errorf("%w: artifact does not hash to its signed digest", evidencebook.ErrInvalid)
	}
	body, err := a.SigningBody()
	if err != nil {
		return err
	}
	key, keyErr := hex.DecodeString(a.KeyID)
	sig, sigErr := hex.DecodeString(a.Sig)
	switch {
	case sigErr != nil:
		return fmt.Errorf("%w: artifact signature is not hex", evidencebook.ErrInvalid)
	case keyErr != nil || len(key) != ed25519.PublicKeySize:
		return fmt.Errorf("%w: key_id is not an Ed25519 public key", evidencebook.ErrInvalid)
	case !ed25519.Verify(ed25519.PublicKey(key), body, sig):
		return fmt.Errorf("%w: signature does not verify", evidencebook.ErrInvalid)
	}
	return nil
}

// dealResponse is exactly one of the two answers a deal responder sends.
type dealResponse struct {
	Artifact *dealArtifactResponse `json:"artifact,omitempty"`
	Refusal  *evidencebook.Refusal `json:"refusal,omitempty"`
}
