package cli

import (
	"bytes"
	"encoding/json"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/witness"
)

// loadWitnessDirectory reads a witness directory file (capsule-emit's
// witnesses.json format): {"witnesses": [rows]} or a bare array of rows. An
// object without a witnesses array is refused rather than read as a
// directory that trusts no witness.
func loadWitnessDirectory(path string) (witness.Directory, error) {
	raw, err := readInput(path)
	if err != nil {
		return witness.Directory{}, err
	}
	directory, err := witness.ParseDirectory(raw)
	if err != nil {
		return witness.Directory{}, hint(ErrInput, "the witness directory is not a witnesses.json file")
	}
	if trimmed := bytes.TrimLeft(raw, " \t\r\n"); len(trimmed) > 0 && trimmed[0] == '{' {
		var wrapped struct {
			Witnesses []json.RawMessage `json:"witnesses"`
		}
		if json.Unmarshal(raw, &wrapped) != nil || wrapped.Witnesses == nil {
			return witness.Directory{}, hint(ErrInput, "the witness directory has no witnesses array")
		}
	}
	return directory, nil
}

// witnessClaim checks every receipt in checkpoint.witnesses against the
// signed checkpoint statement (nil when it did not verify), under the keys
// of directory's row for the receipt's witness.
func witnessClaim(value map[string]interface{}, statement []byte, directory *witness.Directory) (aacbundle.ClaimResult, []map[string]string) {
	stated, _ := value["checkpoint"].(map[string]interface{})
	return receiptsClaim(stated["witnesses"], statement, directory, "")
}

// receiptsClaim is agent-action-capsule's witness receipt check
// (witness.VerifyReceipts) for one checkpoint's receipts, in this CLI's
// report shape. statement is the checkpoint's COSE statement, nil when it did
// not verify. Per receipt: pass when it verifies under one of the row's keys;
// withheld when it is a stub or there is no row or no usable key; fail when
// it does not verify. Overall: fail if any entry is malformed or fails, else
// pass if any passes, else withheld. trusted is the witness directory, nil
// when none was given; via, when set, is added to each receipt.
func receiptsClaim(entries interface{}, statement []byte, trusted *witness.Directory, via string) (aacbundle.ClaimResult, []map[string]string) {
	// No directory trusts no witness.
	var directory witness.Directory
	if trusted != nil {
		directory = *trusted
	}
	var signed *witness.SignedCheckpoint
	if statement != nil {
		if parsed, err := witness.ParseSignedCheckpoint(statement); err == nil {
			signed = &parsed
		} else {
			statement = nil
		}
	}
	status, findings, results := witness.VerifyReceipts(entries, signed, statement, directory)
	receipts := make([]map[string]string, 0, len(results))
	for _, r := range results {
		reason := r.Reason
		if r.Status == witness.StatusPass {
			if row, found := directory.RowFor(r.TSURL); found {
				reason = "verified under " + row.Name + "'s key"
			}
		}
		receipt := map[string]string{"ts_url": r.TSURL, "binding": r.Binding, "status": r.Status, "reason": reason}
		if via != "" {
			receipt["via"] = via
		}
		receipts = append(receipts, receipt)
	}
	return aacbundle.ClaimResult{Status: status, Findings: findings}, receipts
}
