package cli

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
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
func loadWitnessDirectory(path string) (witness.Directory, error) {
	raw, err := readInput(path)
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
			return witness.Directory{}, hint(ErrInput, "the witness directory has no witnesses array")
		}
	}
	if err != nil {
		return witness.Directory{}, hint(ErrInput, "the witness directory is not a witnesses.json file")
	}
	directory, err := witness.ParseDirectory(raw)
	if err != nil {
		return witness.Directory{}, hint(ErrInput, "the witness directory is not a witnesses.json file")
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

// receiptsClaim checks one checkpoint's receipts in this CLI's report shape.
// statement is the checkpoint's COSE statement, nil when it did not verify;
// trusted is the witness directory, nil when none was given; via, when set,
// is added to each receipt.
//
// An entry that is not a receipt this CLI reads (wellFormedReceipt) is
// malformed. Every other entry is checked by agent-action-capsule's
// witness.VerifyReceipts: pass when it verifies under one of its row's keys;
// withheld when it is a stub or there is no row or no usable key; fail when
// it does not verify. Overall: fail if any entry is malformed or fails, else
// pass if any passes, else withheld. Findings are in entry order.
func receiptsClaim(entries interface{}, statement []byte, trusted *witness.Directory, via string) (aacbundle.ClaimResult, []map[string]string) {
	list, _ := entries.([]interface{})
	if len(list) == 0 {
		return aacbundle.ClaimResult{Status: witness.StatusWithheld, Findings: []string{"witness_receipt_absent"}}, []map[string]string{}
	}
	var signed *witness.SignedCheckpoint
	if statement != nil {
		if parsed, err := witness.ParseSignedCheckpoint(statement); err == nil {
			signed = &parsed
		}
	}
	if signed == nil {
		return aacbundle.ClaimResult{Status: witness.StatusWithheld, Findings: []string{"checkpoint_unverified"}}, []map[string]string{}
	}
	var directory witness.Directory // no directory trusts no witness
	if trusted != nil {
		directory = *trusted
	}
	var wellFormed []interface{}
	for _, entry := range list {
		if wellFormedReceipt(entry) {
			wellFormed = append(wellFormed, entry)
		}
	}
	var results []witness.ReceiptResult
	if len(wellFormed) > 0 {
		_, _, results = witness.VerifyReceipts(wellFormed, signed, statement, directory)
	}

	var findings []string
	receipts := make([]map[string]string, 0, len(results))
	sawPass, sawFail := false, false
	next := 0
	for index, entry := range list {
		if !wellFormedReceipt(entry) {
			sawFail = true
			findings = append(findings, "witness_receipt_malformed:"+strconv.Itoa(index))
			continue
		}
		r := results[next]
		next++
		reason := r.Reason
		switch r.Status {
		case witness.StatusPass:
			sawPass = true
			if row, found := directory.RowFor(r.TSURL); found {
				reason = "verified under " + row.Name + "'s key"
			}
		case witness.StatusWithheld:
			findings = append(findings, "witness_unverified:"+r.TSURL)
		default:
			sawFail = true
			findings = append(findings, "witness_receipt_invalid:"+r.TSURL)
		}
		receipt := map[string]string{"ts_url": r.TSURL, "binding": r.Binding, "status": r.Status, "reason": reason}
		if via != "" {
			receipt["via"] = via
		}
		receipts = append(receipts, receipt)
	}
	switch {
	case sawFail:
		return aacbundle.ClaimResult{Status: witness.StatusFail, Findings: findings}, receipts
	case sawPass:
		return aacbundle.ClaimResult{Status: witness.StatusPass, Findings: findings}, receipts
	}
	return aacbundle.ClaimResult{Status: witness.StatusWithheld, Findings: findings}, receipts
}

// wellFormedReceipt is a checkpoint.witnesses entry this CLI reads: an object
// with non-empty string ts_url, entry_hash and receipt_b64 (standard base64),
// and leaf_index and tree_size present. It is stricter than
// witness.VerifyReceipts' own record check, which only requires the members
// to be present: an entry that is not a receipt is malformed, never merely
// not checked.
func wellFormedReceipt(entry interface{}) bool {
	fields, isObject := entry.(map[string]interface{})
	if !isObject {
		return false
	}
	url, _ := fields["ts_url"].(string)
	entryHash, _ := fields["entry_hash"].(string)
	encoded, _ := fields["receipt_b64"].(string)
	if url == "" || entryHash == "" || encoded == "" {
		return false
	}
	if _, err := base64.StdEncoding.DecodeString(encoded); err != nil {
		return false
	}
	_, hasLeaf := fields["leaf_index"]
	_, hasSize := fields["tree_size"]
	return hasLeaf && hasSize
}
