package cli

import (
	"encoding/json"
	"strings"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/witness"
)

// witnessRowShape is the shape every witness directory row must have
// (capsule-emit's witnesses.json format): string name, endpoint and binding,
// and lists of strings for key_ids and public_keys. Other row fields are read
// past. A row of another shape refuses the whole directory as an input
// error; the keys themselves are read by agent-action-capsule's witness
// package.
type witnessRowShape struct {
	Name       string   `json:"name"`
	Endpoint   string   `json:"endpoint"`
	Binding    string   `json:"binding"`
	KeyIDs     []string `json:"key_ids"`
	PublicKeys []string `json:"public_keys"`
}

// loadWitnessDirectory reads a witness directory file: {"witnesses": [rows]}
// or a bare array of rows. An object without a witnesses array is refused
// rather than read as a directory that trusts no witness.
func LoadWitnessDirectory(path string) (witness.Directory, error) {
	raw, err := ReadInput(path)
	if err != nil {
		return witness.Directory{}, err
	}
	trimmed := strings.TrimLeft(string(raw), " \t\r\n")
	var rows []witnessRowShape
	if strings.HasPrefix(trimmed, "[") {
		err = json.Unmarshal(raw, &rows)
	} else {
		var wrapped struct {
			Witnesses *[]witnessRowShape `json:"witnesses"`
		}
		err = json.Unmarshal(raw, &wrapped)
		if err == nil && wrapped.Witnesses == nil {
			return witness.Directory{}, Hint(ErrInput, "the witness directory has no witnesses array")
		}
	}
	if err != nil {
		return witness.Directory{}, Hint(ErrInput, "the witness directory is not a witnesses.json file")
	}
	directory, err := witness.ParseDirectory(raw)
	if err != nil {
		return witness.Directory{}, Hint(ErrInput, "the witness directory is not a witnesses.json file")
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

// receiptsClaim is agent-action-capsule's witness.VerifyReceipts on one
// checkpoint's receipts, in this CLI's report shape. statement is the
// checkpoint's COSE statement, nil when it did not verify; trusted is the
// witness directory, nil when none was given (no witness is trusted); via,
// when set, is added to each receipt. The claim, its findings (malformed
// entries first) and each receipt's status and reason are AAC's.
func receiptsClaim(entries interface{}, statement []byte, trusted *witness.Directory, via string) (aacbundle.ClaimResult, []map[string]string) {
	var signed *witness.SignedCheckpoint
	if statement != nil {
		if parsed, err := witness.ParseSignedCheckpoint(statement); err == nil {
			signed = &parsed
		}
	}
	var directory witness.Directory
	if trusted != nil {
		directory = *trusted
	}
	status, findings, results := witness.VerifyReceipts(entries, signed, statement, directory)
	receipts := make([]map[string]string, 0, len(results))
	for _, r := range results {
		receipt := map[string]string{"ts_url": r.TSURL, "binding": r.Binding, "status": r.Status, "reason": r.Reason}
		if via != "" {
			receipt["via"] = via
		}
		receipts = append(receipts, receipt)
	}
	return aacbundle.ClaimResult{Status: status, Findings: findings}, receipts
}
