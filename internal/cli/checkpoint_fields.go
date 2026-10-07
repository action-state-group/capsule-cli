package cli

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/action-state-group/checkpointed-local-log/go/checkpoint"
	"github.com/fxamacker/cbor/v2"
)

// signedCheckpoint returns the fields a COSE checkpoint statement signs
// (log_id, mmr_size, root, prev_size, prev_root, key_id, timestamp) once its
// signature verifies. timestamp is the statement's issued_at claim exactly as
// signed: the parsed checkpoint keeps only the instant, and re-rendering it
// can differ from the signed text ("…:00.000Z" becomes "…:00Z"), which other
// verifiers compare as signed.
func signedCheckpoint(statement []byte) (map[string]interface{}, error) {
	record, err := checkpoint.ParseRecord(statement)
	if err != nil {
		return nil, err
	}
	if err := record.VerifySignature(); err != nil {
		return nil, err
	}
	projection, err := record.Payload().CanonicalJSON()
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(projection))
	decoder.UseNumber()
	var fields map[string]interface{}
	if err := decoder.Decode(&fields); err != nil {
		return nil, err
	}
	issuedAt, err := signedIssuedAt(statement)
	if err != nil {
		return nil, err
	}
	fields["timestamp"] = issuedAt
	return fields, nil
}

// signedIssuedAt reads the issued_at claim from a COSE_Sign1 checkpoint's
// payload (the statement may carry COSE_Sign1 tag 18 or not).
func signedIssuedAt(statement []byte) (string, error) {
	var message struct {
		_           struct{} `cbor:",toarray"`
		Protected   cbor.RawMessage
		Unprotected cbor.RawMessage
		Payload     []byte
		Signature   cbor.RawMessage
	}
	body := statement
	var tagged cbor.RawTag
	if err := cbor.Unmarshal(statement, &tagged); err == nil && tagged.Number == 18 {
		body = tagged.Content
	}
	if err := cbor.Unmarshal(body, &message); err != nil {
		return "", err
	}
	var claims struct {
		IssuedAt string `cbor:"issued_at"`
	}
	if err := cbor.Unmarshal(message.Payload, &claims); err != nil {
		return "", err
	}
	if claims.IssuedAt == "" {
		return "", errors.New("checkpoint payload has no issued_at")
	}
	return claims.IssuedAt, nil
}
