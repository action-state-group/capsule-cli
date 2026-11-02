package cli

import (
	"bytes"
	"encoding/hex"
	"fmt"

	"github.com/action-state-group/agent-action-capsule/go/witness"
	"github.com/action-state-group/checkpointed-local-log/go/mmr"
	"github.com/fxamacker/cbor/v2"
	"github.com/veraison/go-cose"
)

// statementPeaks is the peak list a CLL checkpoint statement commits to:
// the deterministic CBOR array of 32-octet peaks in its `commitment` claim
// (IP v1 §6.2.1 step 7; CLL §3.1).
func statementPeaks(statement []byte) ([][]byte, error) {
	var message cose.Sign1Message
	if err := message.UnmarshalCBOR(statement); err != nil {
		return nil, err
	}
	var claims struct {
		Commitment []byte `cbor:"commitment"`
	}
	if err := cbor.Unmarshal(message.Payload, &claims); err != nil {
		return nil, err
	}
	var peaks [][]byte
	if err := cbor.Unmarshal(claims.Commitment, &peaks); err != nil || len(peaks) == 0 {
		return nil, fmt.Errorf("the commitment is not a list of peaks")
	}
	return peaks, nil
}

// chainFailure is why checkpoint next does not extend previous, or "" when
// it does (IP v1 §6.2.1 step 13; MMR §7.1, §7.1.1): same log, the signed
// prev_size and prev_root name the previous checkpoint, and the entry's
// consistency proof (the MMR consistency-proof array, deterministic CBOR)
// shows the previous peaks extend to the next ones. Both checkpoints have
// already been parsed under their own signatures.
func chainFailure(prev, next witness.SignedCheckpoint, prevStatement, nextStatement, proof []byte) string {
	switch {
	case prev.LogID != next.LogID:
		return "the checkpoints name different logs"
	case next.PrevSize != prev.MMRSize || next.PrevRoot != prev.Root:
		return "the checkpoint's signed prev_size and prev_root are not the previous checkpoint's"
	case next.MMRSize <= prev.MMRSize:
		return "the checkpoint does not grow the log"
	}
	oldPeaks, err1 := statementPeaks(prevStatement)
	newPeaks, err2 := statementPeaks(nextStatement)
	if err1 != nil || err2 != nil {
		return "a checkpoint's commitment is unreadable"
	}
	var parts []cbor.RawMessage
	var size1, size2 uint64
	var paths [][][]byte
	var right [][]byte
	if err := cbor.Unmarshal(proof, &parts); err != nil || len(parts) != 4 ||
		cbor.Unmarshal(parts[0], &size1) != nil || cbor.Unmarshal(parts[1], &size2) != nil ||
		cbor.Unmarshal(parts[2], &paths) != nil || cbor.Unmarshal(parts[3], &right) != nil {
		return "the consistency proof is not [tree-size-1, tree-size-2, consistency-paths, right-peaks]"
	}
	if size1 != prev.MMRSize || size2 != next.MMRSize {
		return "the consistency proof's sizes are not the checkpoints'"
	}
	if len(right) > len(newPeaks) || !equalPeaks(newPeaks[len(newPeaks)-len(right):], right) {
		return "the proof's right-peaks are not the tail of the checkpoint's peaks"
	}
	oldRoot, err1 := hex.DecodeString(prev.Root)
	newRoot, err2 := hex.DecodeString(next.Root)
	if err1 != nil || err2 != nil {
		return "a checkpoint root is not hex"
	}
	if !mmr.VerifyConsistency(oldRoot, newRoot, mmr.ConsistencyProof{V: 1, Kind: "consistency", OldSize: size1, NewSize: size2, OldPeaks: oldPeaks, Witness: paths, NewPeaks: newPeaks}) {
		return "the consistency proof does not show the checkpoint extends the previous one"
	}
	return ""
}

func equalPeaks(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}
