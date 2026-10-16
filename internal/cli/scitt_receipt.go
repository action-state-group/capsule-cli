package cli

// Receipts for a SCITT Signed Statement registered at a witness's
// /transparency/register-statement, verified with agent-action-capsule's
// witness.VerifyCOSEReceipt. For a COSE_Sign1 statement the witness's entry
// hash is SHA-256 over its Sig_structure (["Signature1", protected, h'',
// payload]), and the receipt is a COSE_Sign1 by the witness's Ed25519
// authority key over the detached root that the proof folds to.

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/action-state-group/agent-action-capsule/go/witness"
	"github.com/fxamacker/cbor/v2"
	"github.com/veraison/go-cose"
)

const entryHashSigStructure = "sig_structure"

// statementEntryHash is the witness's sig_structure entry hash of a
// COSE_Sign1 statement.
func statementEntryHash(statement []byte) ([]byte, error) {
	var message cose.Sign1Message
	if err := message.UnmarshalCBOR(statement); err != nil {
		return nil, fmt.Errorf("decode statement: %w", err)
	}
	// The protected header is signed as the byte string it was sent as.
	var protected []byte
	if err := cbor.Unmarshal(message.Headers.RawProtected, &protected); err != nil {
		return nil, fmt.Errorf("decode protected header: %w", err)
	}
	structure, err := cbor.Marshal([]any{"Signature1", protected, []byte{}, message.Payload})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(structure)
	return sum[:], nil
}

// statementRegistration is the witness's answer to a registration.
type statementRegistration struct {
	ReceiptB64      string `json:"receipt_b64"`
	EntryHash       string `json:"entry_hash"`
	EntryHashScheme string `json:"entry_hash_scheme"`
	LeafIndex       int64  `json:"leaf_index"`
	TreeSize        int64  `json:"tree_size"`
}

// verifyStatementReceipt checks that receipt proves statement's inclusion in
// the witness's log, under the pinned authority key. The entry hash is
// recomputed from the statement, never taken from the registration.
func verifyStatementReceipt(statement, receipt []byte, reg statementRegistration, authority ed25519.PublicKey) error {
	if len(authority) != ed25519.PublicKeySize {
		return errors.New("a pinned Ed25519 witness key is required")
	}
	if len(receipt) == 0 || len(receipt) > receiptMaxBytes {
		return errors.New("receipt size is invalid")
	}
	if reg.EntryHashScheme != entryHashSigStructure {
		return fmt.Errorf("unsupported entry hash scheme %q", reg.EntryHashScheme)
	}
	entry, err := statementEntryHash(statement)
	if err != nil {
		return err
	}
	if hex.EncodeToString(entry) != reg.EntryHash {
		return errors.New("the registration's entry hash does not match this statement")
	}
	checked, err := witness.VerifyCOSEReceipt(receipt, entry, authority)
	if err != nil {
		return fmt.Errorf("verify receipt authority signature: %w", err)
	}
	if checked.TreeSize != reg.TreeSize || checked.LeafIndex != reg.LeafIndex {
		return errors.New("receipt position mismatch")
	}
	return nil
}
