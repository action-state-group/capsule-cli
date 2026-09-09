package cli

import (
	"encoding/base64"
	"encoding/json"
	"unicode/utf8"

	"github.com/action-state-group/capsule-emit-go/artifact"
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

// getRecordOutput preserves artifact metadata, including retention and binding
// information, while replacing only byte fields in the human-readable view.
func getRecordOutput(r artifact.Record, raw bool) any {
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
