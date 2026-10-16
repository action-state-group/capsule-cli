package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/fxamacker/cbor/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/veraison/go-cose"
)

// The receipt check is agent-action-capsule's witness.VerifyCOSEReceipt.
// These tests pin where it differs from the check this CLI carried before.

var testCountersignStatement = map[string]interface{}{"checks": []interface{}{"range membership"}, "recomputed_at": "2026-10-01T12:00:00Z"}

// countersignEntry is the log entry a countersigner registers for statement:
// SHA-256 of SHA-256(JCS(statement)).
func countersignEntry(t *testing.T, statement interface{}) []byte {
	t.Helper()
	encoded, err := canonical.JCS(statement)
	require.NoError(t, err)
	registered := sha256.Sum256(encoded)
	entry := sha256.Sum256(registered[:])
	return entry[:]
}

// mintCountersignReceipt is a one-leaf RFC 9162 receipt for entry, signed by
// key, with extra (well-formed, unverifiable) proofs after the real one and,
// when crit is set, an unknown protected header marked critical.
func mintCountersignReceipt(t *testing.T, entry []byte, key ed25519.PrivateKey, extra int, crit bool) json.RawMessage {
	t.Helper()
	proof, err := cbor.Marshal([]any{int64(1), int64(0), [][]byte{}})
	require.NoError(t, err)
	proofs := []any{proof}
	for range extra {
		other, err := cbor.Marshal([]any{int64(2), int64(1), [][]byte{make([]byte, 32)}})
		require.NoError(t, err)
		proofs = append(proofs, other)
	}
	leaf := sha256.Sum256(append([]byte{0}, entry...))
	message := cose.NewSign1Message()
	message.Headers.Protected.SetAlgorithm(cose.AlgorithmEdDSA)
	message.Headers.Protected[int64(395)] = int64(1)
	if crit {
		message.Headers.Protected[int64(-70000)] = "must be understood"
		message.Headers.Protected[int64(2)] = []any{int64(-70000)}
	}
	message.Headers.Unprotected[int64(396)] = map[any]any{int64(-1): proofs}
	message.Payload = leaf[:]
	signer, err := cose.NewSigner(cose.AlgorithmEdDSA, key)
	require.NoError(t, err)
	require.NoError(t, message.Sign(rand.Reader, nil, signer))
	message.Payload = nil
	encoded, err := message.MarshalCBOR()
	require.NoError(t, err)
	raw, err := json.Marshal(map[string]any{"receipt_b64": base64.StdEncoding.EncodeToString(encoded), "leaf_index": 0, "tree_size": 1})
	require.NoError(t, err)
	return raw
}

func TestCountersignReceiptVerifiesUnderTheSignerKey(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	entry := countersignEntry(t, testCountersignStatement)
	state, err := verifyCountersignReceipt(public, testCountersignStatement, mintCountersignReceipt(t, entry, private, 0, false))
	require.NoError(t, err)
	assert.Equal(t, receiptVerified, state)

	other, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	state, err = verifyCountersignReceipt(other, testCountersignStatement, mintCountersignReceipt(t, entry, private, 0, false))
	assert.Equal(t, receiptUnverified, state)
	assert.ErrorContains(t, err, "does not verify under signer key_id")

	var receipt map[string]any
	require.NoError(t, json.Unmarshal(mintCountersignReceipt(t, entry, private, 0, false), &receipt))
	receipt["tree_size"] = 2
	moved, err := json.Marshal(receipt)
	require.NoError(t, err)
	state, err = verifyCountersignReceipt(public, testCountersignStatement, moved)
	assert.Equal(t, receiptUnverified, state)
	assert.EqualError(t, err, "receipt position mismatch")

	state, err = verifyCountersignReceipt(public, testCountersignStatement, json.RawMessage(`{"receipt_b64":"`+strings.Repeat("A", 4*(receiptMaxBytes/3+1))+`","leaf_index":0,"tree_size":1}`))
	assert.Equal(t, receiptUnverified, state)
	assert.ErrorContains(t, err, "bounded")
}

// A receipt may carry more than one inclusion proof: it verifies when one of
// them rebuilds the signed root. (This CLI once required exactly one.)
func TestCountersignReceiptWithSeveralProofsVerifies(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	entry := countersignEntry(t, testCountersignStatement)
	state, err := verifyCountersignReceipt(public, testCountersignStatement, mintCountersignReceipt(t, entry, private, 1, false))
	require.NoError(t, err)
	assert.Equal(t, receiptVerified, state)
}

// A receipt that marks a protected header this verifier does not understand
// as critical is refused (RFC 9052 section 3.1).
func TestCountersignReceiptWithAnUnknownCriticalHeaderIsUnverified(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	entry := countersignEntry(t, testCountersignStatement)
	state, err := verifyCountersignReceipt(public, testCountersignStatement, mintCountersignReceipt(t, entry, private, 0, true))
	assert.Equal(t, receiptUnverified, state)
	assert.ErrorContains(t, err, "crit")
}

// A receipt whose CBOR is not in its deterministic encoding is refused, even
// where the bytes are not signed: here the unprotected vdp label 396 is
// written with a 4-byte argument instead of 2.
func TestCountersignReceiptNotStrictlyEncodedIsUnverified(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	entry := countersignEntry(t, testCountersignStatement)
	var receipt map[string]any
	require.NoError(t, json.Unmarshal(mintCountersignReceipt(t, entry, private, 0, false), &receipt))
	blob, err := base64.StdEncoding.DecodeString(receipt["receipt_b64"].(string))
	require.NoError(t, err)
	minimal := []byte{0x19, 0x01, 0x8c}
	require.Equal(t, 1, bytes.Count(blob, minimal))
	blob = bytes.Replace(blob, minimal, []byte{0x1a, 0x00, 0x00, 0x01, 0x8c}, 1)
	receipt["receipt_b64"] = base64.StdEncoding.EncodeToString(blob)
	raw, err := json.Marshal(receipt)
	require.NoError(t, err)
	state, err := verifyCountersignReceipt(public, testCountersignStatement, raw)
	assert.Equal(t, receiptUnverified, state)
	assert.ErrorContains(t, err, "receipt rejected")
}
