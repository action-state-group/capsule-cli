package cli

import (
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/action-state-group/capsule-emit/go/artifact"
)

// displayBytes is a presentation only: JSON reserialization must never be used
// to reconstruct signed bytes. Raw exports retain the SDK's exact-byte format.
func displayBytes(b []byte) any {
	if b == nil {
		return nil
	}
	if json.Valid(b) {
		return json.RawMessage(b)
	}
	if utf8.Valid(b) {
		return string(b)
	}
	return struct {
		Encoding string `json:"encoding"`
		Data     string `json:"data"`
	}{"base64", base64.StdEncoding.EncodeToString(b)}
}

// openingOriginalPrefix names an evidence book's field openings among a
// record's originals (evidencebook commitment.go: "opening.<field>"). An
// opening is a salted commitment's nonce and plain text, kept on the device
// to prove the field later; a capsule never binds it, and `get` never prints
// it (capsule-cli#242).
const openingOriginalPrefix = "opening."

// getRecordOutput preserves artifact metadata, including retention and binding
// information, while replacing only byte fields in the human-readable view.
// Field openings are left out of both views.
func getRecordOutput(r artifact.Record, raw bool) any {
	if slices.ContainsFunc(r.Artifacts, isOpening) {
		r.Artifacts = slices.DeleteFunc(slices.Clone(r.Artifacts), isOpening)
	}
	if raw {
		return r
	}
	type displayedArtifact struct {
		artifact.Artifact
		Content any `json:"content,omitempty"`
	}
	var artifacts []displayedArtifact
	if r.Artifacts != nil {
		artifacts = make([]displayedArtifact, 0, len(r.Artifacts))
	}
	for _, a := range r.Artifacts {
		artifacts = append(artifacts, displayedArtifact{a, displayBytes(a.Content)})
	}
	return struct {
		artifact.Record
		Capsule          any                 `json:"capsule"`
		ProducerEnvelope any                 `json:"producer_envelope"`
		Artifacts        []displayedArtifact `json:"artifacts"`
	}{r, displayBytes(r.Capsule), displayBytes(r.ProducerEnvelope), artifacts}
}

func isOpening(a artifact.Artifact) bool { return strings.HasPrefix(a.Name, openingOriginalPrefix) }
