package cli

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
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

// inputFileError reports a user-provided input file (the value of --request,
// --proof, or --capsule) that could not be read. The path came from a flag the
// caller typed, so naming it in full discloses nothing sensitive — unlike the
// profile, driver, or config detail SafeError suppresses. It still carries
// ErrInput (exit code 2); only the SafeError message differs.
type inputFileError struct {
	path     string
	notFound bool
}

func (e *inputFileError) Error() string {
	if e.notFound {
		return "input file not found: " + e.path
	}
	return "cannot read input file: " + e.path
}

func readInput(path string) ([]byte, error) {
	if path == "" {
		return nil, inputError("input file is required")
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, errors.Join(ErrInput, &inputFileError{path: path, notFound: errors.Is(e, fs.ErrNotExist)})
	}
	b, e := io.ReadAll(io.LimitReader(f, maxInput+1))
	e = errors.Join(e, f.Close())
	if e != nil {
		// A read/close failure on the caller-supplied file (e.g. it is a
		// directory) is the same input-file class as an open failure: exit 2
		// and name the path, not the generic operational catch-all.
		return nil, errors.Join(ErrInput, &inputFileError{path: path})
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
	return record, nil
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
// TODO: this coverage check belongs in capsule-emit-go; move it there once an
// SDK release exposes it and bump the go.mod pin.
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
