package cli

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	emit "github.com/action-state-group/capsule-emit-go"
	"github.com/action-state-group/capsule-emit-go/artifact"
)

// Request is an application-neutral v1 sealing request. Capsule metadata follows
// emit.Input's exported field names. Identity is always supplied by the profile.
// RawMessage retains originals exactly, separately from signed JCS digests.
type Request struct {
	Version     string              `json:"spec_version"`
	Capsule     emit.Input          `json:"capsule"`
	Payload     json.RawMessage     `json:"payload,omitempty"`
	AgentOutput json.RawMessage     `json:"agent_output,omitempty"`
	Model       *emit.Model         `json:"model,omitempty"`
	Runtime     string              `json:"runtime,omitempty"`
	Artifacts   []artifact.Artifact `json:"artifacts,omitempty"`
}

const maxInput = 12 << 20

func decodeJSON(raw []byte, v any) (err error) {
	defer func() {
		if err != nil {
			err = errors.Join(ErrInput, err)
		}
	}()
	if len(raw) > maxInput {
		return inputError("input exceeds size limit")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return inputError("invalid JSON input or unknown field")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return inputError("input must contain one JSON value")
	}
	return nil
}
func readInput(path string) ([]byte, error) {
	if path == "" {
		return nil, inputError("input file is required")
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, inputError("cannot open input file")
	}
	b, e := io.ReadAll(io.LimitReader(f, maxInput+1))
	e = errors.Join(e, f.Close())
	if e != nil {
		return nil, e
	}
	if len(b) > maxInput {
		return nil, inputError("input exceeds size limit")
	}
	return b, nil
}
func parseRequest(raw []byte) (Request, error) {
	var r Request
	e := decodeJSON(raw, &r)
	if e != nil {
		return r, e
	}
	if r.Version != "capsule-seal-request/v1" {
		return r, inputError("expected capsule-seal-request/v1")
	}
	return r, nil
}
func seal(r Request, key ed25519.PrivateKey) (artifact.Record, error) {
	identity, e := emit.NewEd25519SigningIdentity(key)
	if e != nil {
		return artifact.Record{}, e
	}
	input := emit.SealInput{Capsule: r.Capsule, Model: r.Model, Runtime: r.Runtime, Identity: identity}
	if r.Payload != nil {
		input.Payload = r.Payload
	}
	if r.AgentOutput != nil {
		input.AgentOutput = r.AgentOutput
	}
	result, e := emit.Seal(input)
	if e != nil {
		return artifact.Record{}, inputError("emit rejected sealing input")
	}
	record := artifact.Record{CapsuleID: result.CapsuleID, Capsule: result.Payload, ProducerEnvelope: result.Envelope, Artifacts: append([]artifact.Artifact(nil), r.Artifacts...)}
	if r.Payload != nil {
		record.Artifacts = append(record.Artifacts, artifact.Artifact{Name: "payload", Binding: artifact.PayloadDigest, State: artifact.Present, Content: r.Payload})
	}
	if r.AgentOutput != nil {
		record.Artifacts = append(record.Artifacts, artifact.Artifact{Name: "agent_output", Binding: artifact.AgentOutputDigest, State: artifact.Present, Content: r.AgentOutput})
	}
	record, e = artifact.Prepare(record)
	if e != nil {
		return record, e
	}
	public, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return record, inputError("invalid signing key")
	}
	_, e = artifact.Verify(record, []ed25519.PublicKey{public})
	return record, e
}
func readRecord(path string) (artifact.Record, error) {
	var r artifact.Record
	b, e := readInput(path)
	if e != nil {
		return r, e
	}
	e = decodeJSON(b, &r)
	return r, e
}

// Missing originals are coverage gaps, not failed signatures. The SDK validates
// supplied bindings; the CLI additionally reports committed fields for which
// no retained preimage was supplied in an imported artifact file.
func missingBindings(r artifact.Record) []string {
	payload, err := emit.DecodePayload(r.Capsule)
	if err != nil {
		return []string{"capsule could not be decoded"}
	}
	covered := map[string]bool{}
	for _, a := range r.Artifacts {
		if a.State == artifact.Present {
			covered[string(a.Binding)] = true
		}
	}
	var missing []string
	for _, field := range []artifact.DigestField{artifact.PayloadDigest, artifact.AgentOutputDigest, artifact.EffectRequestDigest, artifact.EffectResponseDigest} {
		var value any = payload
		for _, part := range strings.Split(string(field), ".") {
			object, ok := value.(map[string]any)
			if !ok {
				value = nil
				break
			}
			value = object[part]
		}
		if text, ok := value.(string); ok && text != "" && !covered[string(field)] {
			missing = append(missing, string(field))
		}
	}
	return missing
}
