package cli

// Countersign receipts are RFC 9162 COSE Receipts, checked by
// agent-action-capsule's witness.VerifyCOSEReceipt over the leaf this CLI
// recomputes from the statement.

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/agent-action-capsule/go/witness"
)

// receiptMaxBytes bounds a countersign or registration receipt; the COSE
// check itself accepts larger ones.
const receiptMaxBytes = 1 << 16

// Receipt states reported per countersignatures[] entry. A receipt is
// optional: its absence never invalidates an entry, and one this CLI cannot
// verify is reported "unverified", never silently treated as verified.
const (
	receiptVerified   = "verified"
	receiptUnverified = "unverified"
	receiptAbsent     = "absent"
)

// countersignReceipt is the receipt encoding capsule-anchor's countersign
// service attaches: an RFC 9162 COSE Receipt (COSE_Sign1, detached root)
// for one entry in the countersigner's own log.
type countersignReceipt struct {
	ReceiptB64 string `json:"receipt_b64"`
	EntryHash  string `json:"entry_hash"`
	LeafIndex  *int64 `json:"leaf_index"`
	TreeSize   *int64 `json:"tree_size"`
}

// verifyCountersignReceipt checks that receipt proves the countersigner
// registered this exact statement in its own log. The registered bytes are
// SHA-256(JCS(statement)); the log entry is SHA-256 of those 32 bytes; the
// receipt's root is signed by the countersigner's key (signer.key_id). The
// log entry is always recomputed from the statement, never taken from the
// receipt's own entry_hash.
func verifyCountersignReceipt(signerKey ed25519.PublicKey, statement interface{}, raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return receiptAbsent, nil
	}
	var receipt countersignReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil || receipt.ReceiptB64 == "" || receipt.LeafIndex == nil || receipt.TreeSize == nil {
		return receiptUnverified, errors.New("receipt is not in a receipt encoding this CLI verifies")
	}
	statementBytes, err := canonical.JCS(statement)
	if err != nil {
		return receiptUnverified, fmt.Errorf("statement cannot be canonicalized: %w", err)
	}
	registered := sha256.Sum256(statementBytes)
	entry := sha256.Sum256(registered[:])
	if receipt.EntryHash != "" && receipt.EntryHash != hex.EncodeToString(entry[:]) {
		return receiptUnverified, errors.New("receipt entry_hash does not match this statement")
	}
	blob, err := base64.StdEncoding.DecodeString(receipt.ReceiptB64)
	if err != nil || len(blob) == 0 || len(blob) > receiptMaxBytes {
		return receiptUnverified, errors.New("receipt_b64 is not a bounded base64 COSE receipt")
	}
	// The countersigner's key is Ed25519, so only an EdDSA receipt verifies.
	checked, err := witness.VerifyCOSEReceipt(blob, entry[:], signerKey)
	if err != nil {
		return receiptUnverified, fmt.Errorf("receipt does not verify under signer key_id: %w", err)
	}
	if checked.TreeSize != *receipt.TreeSize || checked.LeafIndex != *receipt.LeafIndex {
		return receiptUnverified, errors.New("receipt position mismatch")
	}
	return receiptVerified, nil
}
