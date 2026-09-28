package cli

// Countersign receipt verification is an attributed Go adaptation of
// action-state-group/scitt-cose/scitt-cose-go-verify (Apache-2.0), by way of
// cll-go's witness.ReceiptVerifier, which is bound to checkpoint statements
// and so cannot be reused for a countersign statement directly.

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/fxamacker/cbor/v2"
	"github.com/veraison/go-cose"
)

const (
	receiptHeaderVDS    = int64(395)
	receiptHeaderVDP    = int64(396)
	receiptVDSRFC9162   = int64(1)
	receiptVDPInclusion = int64(-1)
	receiptMaxTreeSize  = int64(1 << 62)
	receiptMaxBytes     = 1 << 16
)

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
	var message cose.Sign1Message
	if err := message.UnmarshalCBOR(blob); err != nil {
		return receiptUnverified, fmt.Errorf("decode COSE receipt: %w", err)
	}
	if vds, ok := receiptIntegerHeader(message.Headers.Protected, receiptHeaderVDS); !ok || vds != receiptVDSRFC9162 {
		return receiptUnverified, errors.New("receipt protected header requires RFC 9162 VDS")
	}
	if algorithm, err := message.Headers.Protected.Algorithm(); err != nil || algorithm != cose.AlgorithmEdDSA {
		return receiptUnverified, errors.New("receipt must use EdDSA")
	}
	vdpAny, ok := receiptLookup(message.Headers.Unprotected, receiptHeaderVDP)
	if !ok {
		return receiptUnverified, errors.New("receipt missing VDP")
	}
	vdp, ok := vdpAny.(map[any]any)
	if !ok {
		return receiptUnverified, errors.New("receipt VDP is not a map")
	}
	proofsAny, ok := receiptLookup(vdp, receiptVDPInclusion)
	if !ok {
		return receiptUnverified, errors.New("receipt missing inclusion proof")
	}
	proofs, ok := proofsAny.([]any)
	if !ok || len(proofs) != 1 {
		return receiptUnverified, errors.New("receipt requires one inclusion proof")
	}
	proofBlob, ok := proofs[0].([]byte)
	if !ok {
		return receiptUnverified, errors.New("receipt proof is not bytes")
	}
	treeSize, leafIndex, path, err := decodeReceiptProof(proofBlob)
	if err != nil {
		return receiptUnverified, err
	}
	if treeSize != *receipt.TreeSize || leafIndex != *receipt.LeafIndex {
		return receiptUnverified, errors.New("receipt position mismatch")
	}
	root, ok := receiptRootFromProof(entry[:], leafIndex, treeSize, path)
	if !ok {
		return receiptUnverified, errors.New("receipt inclusion proof is invalid")
	}
	message.Payload = root
	verifier, err := cose.NewVerifier(cose.AlgorithmEdDSA, signerKey)
	if err != nil {
		return receiptUnverified, err
	}
	if err := message.Verify(nil, verifier); err != nil {
		return receiptUnverified, fmt.Errorf("receipt signature does not verify under signer key_id: %w", err)
	}
	return receiptVerified, nil
}

func decodeReceiptProof(blob []byte) (int64, int64, [][]byte, error) {
	var values []cbor.RawMessage
	if err := cbor.Unmarshal(blob, &values); err != nil || len(values) != 3 {
		return 0, 0, nil, errors.New("invalid inclusion proof")
	}
	var size, index int64
	var path [][]byte
	if err := cbor.Unmarshal(values[0], &size); err != nil {
		return 0, 0, nil, errors.New("invalid inclusion proof tree size")
	}
	if err := cbor.Unmarshal(values[1], &index); err != nil {
		return 0, 0, nil, errors.New("invalid inclusion proof leaf index")
	}
	if err := cbor.Unmarshal(values[2], &path); err != nil {
		return 0, 0, nil, errors.New("invalid inclusion proof path")
	}
	for _, node := range path {
		if len(node) != sha256.Size {
			return 0, 0, nil, errors.New("invalid proof hash length")
		}
	}
	return size, index, path, nil
}

// receiptRootFromProof folds an RFC 9162 inclusion proof for entry at index
// in a tree of size, returning the root it reconstructs.
func receiptRootFromProof(entry []byte, index, size int64, path [][]byte) ([]byte, bool) {
	if index < 0 || index >= size || size > receiptMaxTreeSize || int64(len(path)) != receiptExpectedPath(size, index) {
		return nil, false
	}
	leaf := sha256.Sum256(append([]byte{0}, entry...))
	siblings := append([][]byte(nil), path...)
	var fold func(int64, int64) ([]byte, bool)
	fold = func(n, m int64) ([]byte, bool) {
		if n == 1 {
			return leaf[:], true
		}
		if len(siblings) == 0 {
			return nil, false
		}
		k := receiptLargestPowerBelow(n)
		sibling := siblings[len(siblings)-1]
		siblings = siblings[:len(siblings)-1]
		if m < k {
			child, ok := fold(k, m)
			if !ok {
				return nil, false
			}
			return receiptNodeHash(child, sibling), true
		}
		child, ok := fold(n-k, m-k)
		if !ok {
			return nil, false
		}
		return receiptNodeHash(sibling, child), true
	}
	root, ok := fold(size, index)
	return root, ok && len(siblings) == 0
}

func receiptNodeHash(left, right []byte) []byte {
	input := make([]byte, 1, 1+len(left)+len(right))
	input[0] = 1
	input = append(input, left...)
	input = append(input, right...)
	sum := sha256.Sum256(input)
	return sum[:]
}

func receiptLargestPowerBelow(n int64) int64 {
	k := int64(1)
	for k <= (n-1)/2 {
		k *= 2
	}
	return k
}

func receiptExpectedPath(size, index int64) int64 {
	var count int64
	for size > 1 {
		k := receiptLargestPowerBelow(size)
		if index < k {
			size = k
		} else {
			size -= k
			index -= k
		}
		count++
	}
	return count
}

func receiptLookup(values map[any]any, key int64) (any, bool) {
	for candidate, value := range values {
		switch typed := candidate.(type) {
		case int64:
			if typed == key {
				return value, true
			}
		case uint64:
			if key >= 0 && typed == uint64(key) {
				return value, true
			}
		case int:
			if int64(typed) == key {
				return value, true
			}
		}
	}
	return nil, false
}

func receiptIntegerHeader(values map[any]any, key int64) (int64, bool) {
	value, ok := receiptLookup(values, key)
	if !ok {
		return 0, false
	}
	switch typed := value.(type) {
	case int64:
		return typed, true
	case uint64:
		if typed <= uint64(^uint64(0)>>1) {
			return int64(typed), true
		}
	case int:
		return int64(typed), true
	}
	return 0, false
}
