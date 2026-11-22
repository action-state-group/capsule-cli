package cli

import (
	"encoding/hex"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/action-state-group/agent-action-capsule/go/evidencerequest"
	"github.com/action-state-group/agent-action-capsule/go/witness"
)

// card2Form checks the envelope-level form of a history_card/2 (ER -03
// §8.1.2): the card names its own subject, derivation, anchor and stream, and
// each history entry's checkpoint is hex. Everything about the window, the
// positions, the proofs and the receipts is AAC's CheckHistoryCard2 (see
// card2Checks). The card has the members a producer builds and no others
// (its vectors reject a card or an entry with another member), though AAC's
// check alone would ignore them.
func card2Form(e *evidencerequest.Envelope) ([][]byte, [][]byte, string, string) {
	decoded, err := evidencerequest.DecodeJSON(e.Artifact)
	card, ok := decoded.(map[string]any)
	if err != nil || !ok {
		return nil, nil, "", "the card is not a JSON object"
	}
	keys := make([]string, 0, len(card))
	for k := range card {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"anchor", "derivation", "evidence_stream", "history", "subject", "window"}) {
		return nil, nil, "", "the card has members other than the card's"
	}
	subject, _ := card["subject"].(map[string]any)
	if len(subject) != 1 || !reflect.DeepEqual(subject, e.Subject.Value()) {
		return nil, nil, "", "the card's subject is not the envelope's"
	}
	stream, _ := card["evidence_stream"].(string)
	if card["derivation"] != stapleCard2 || card["anchor"] != e.Anchor.Digest || stream == "" {
		return nil, nil, "", "the card does not name its derivation, anchor and stream"
	}
	history, _ := card["history"].([]any)
	if len(history) == 0 {
		return nil, nil, "", "the card has no history"
	}
	statements := make([][]byte, len(history))
	for i, item := range history {
		entry, _ := item.(map[string]any)
		if len(entry) != 4 {
			return nil, nil, "", fmt.Sprintf("history entry %d has members other than position, checkpoint, receipts and consistency_from_previous", i)
		}
		checkpoint, _ := entry["checkpoint"].(string)
		statement, derr := hex.DecodeString(checkpoint)
		if derr != nil || checkpoint == "" || strings.ToLower(checkpoint) != checkpoint {
			return nil, nil, "", fmt.Sprintf("history entry %d does not carry a hex checkpoint", i)
		}
		statements[i] = statement
	}
	return statements, nil, stream, ""
}

// card2Property is the staple property a CheckHistoryCard2 finding is reported
// under, so a /2 staple reads in the same rows as a /1.
var card2Property = map[string]string{
	evidencerequest.FindingCardMalformed:      "card_form",
	evidencerequest.FindingWindowOutOfRange:   "card_form",
	evidencerequest.FindingPositionsOrder:     "card_form",
	evidencerequest.FindingPositionMissing:    "card_form",
	evidencerequest.FindingPositionUnexpected: "card_form",
	evidencerequest.FindingReceiptInvalid:     "checkpoint_receipts",
	evidencerequest.FindingCheckpointInvalid:  "checkpoint_signature",
	evidencerequest.FindingNotFirstCheckpoint: "checkpoint_consistency",
	evidencerequest.FindingCheckpointShrinks:  "checkpoint_consistency",
	evidencerequest.FindingProofInvalid:       "checkpoint_consistency",
	evidencerequest.FindingAnchorMismatch:     "checkpoint_anchor",
}

// card2Checks is IP v1 §6.2.1 for a history_card/2: the chain, positions,
// receipts and anchor are AAC's CheckHistoryCard2, called once. Which log keys
// the reader accepts is the reader's own input and a separate row, so the
// library is given the keys the checkpoints name and checkpoint_key is judged
// against --checkpoint-key exactly as for a /1 card.
func card2Checks(row *cardRow, e *evidencerequest.Envelope, statements [][]byte, accepted []string) {
	decoded, err := evidencerequest.DecodeJSON(e.Artifact)
	if err != nil {
		row.set("card_form", statusFail, err.Error())
		return
	}
	named := make([]string, 0, len(statements))
	key := statusPass
	if len(accepted) == 0 {
		key = statusNotChecked
	}
	for i, s := range statements {
		signed, err := witness.ParseSignedCheckpoint(s)
		if err != nil {
			continue
		}
		named = append(named, signed.KeyID)
		if len(accepted) > 0 && !slices.Contains(accepted, signed.KeyID) {
			key = statusFail
			row.Findings = append(row.Findings, fmt.Sprintf("checkpoint_key FAIL: checkpoint %d is signed by %s, not a key you accept", i, signed.KeyID))
		}
	}
	row.set("checkpoint_key", key, "")
	finding := evidencerequest.CheckHistoryCard2(decoded, e.Anchor.Digest, named)
	failed := ""
	if finding != "" {
		failed = card2Property[finding]
		if failed == "" {
			failed = "card_form"
		}
	}
	// A finding is the first check that failed, so the rows the library
	// reached before it passed and those after it were not run.
	for i, p := range card2Order {
		switch {
		case failed == "":
			row.set(p, statusPass, "")
		case p == failed:
			row.set(p, statusFail, "history_card/2 "+finding)
		case i < slices.Index(card2Order, failed):
			row.set(p, statusPass, "")
		default:
			row.set(p, statusNotChecked, "")
		}
	}
}

// card2Order is the order CheckHistoryCard2 runs its checks in, by the
// property each reports under.
var card2Order = []string{"card_form", "checkpoint_signature", "checkpoint_consistency", "checkpoint_receipts", "checkpoint_anchor"}
