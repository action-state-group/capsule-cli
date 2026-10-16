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
	normalized, err := normalizeWitnessDirectory(raw)
	if err != nil {
		return witness.Directory{}, hint(ErrInput, "the witness directory is not a witnesses.json file")
	}
	directory, err := witness.ParseDirectory(normalized)
	if err != nil {
		return witness.Directory{}, hint(ErrInput, "the witness directory is not a witnesses.json file")
	}
	return directory, nil
}

// normalizeWitnessDirectory keeps the readings this CLI has always given a
// row before agent-action-capsule reads it strictly: an empty or null
// binding is the default (cll), a null public_keys is none, and a
// public_keys entry may be wrapped across lines (PEM style), so its
// whitespace is removed. Without this, such a row would match no receipt or
// lose its keys, and a receipt it should check would go unchecked.
func normalizeWitnessDirectory(raw []byte) ([]byte, error) {
	var doc interface{}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&doc); err != nil {
		return nil, err
	}
	rows, _ := doc.([]interface{})
	if wrapped, ok := doc.(map[string]interface{}); ok {
		canonicalFieldNames(wrapped, "witnesses")
		rows, _ = wrapped["witnesses"].([]interface{})
	}
	for _, item := range rows {
		row, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		canonicalFieldNames(row, "name", "endpoint", "binding", "key_ids", "public_keys")
		if binding, present := row["binding"]; present && (binding == nil || binding == "") {
			delete(row, "binding")
		}
		if keys, present := row["public_keys"]; present {
			list, ok := keys.([]interface{})
			if keys == nil {
				delete(row, "public_keys")
				continue
			}
			if ok {
				for i, entry := range list {
					if text, isText := entry.(string); isText {
						list[i] = strings.Join(strings.Fields(text), "")
					}
				}
			}
		}
	}
	return json.Marshal(doc)
}

// canonicalFieldNames renames each member of object whose name matches one
// of names case-insensitively, as Go's struct decoder read it, to that name,
// so agent-action-capsule's case-sensitive reader sees the field this CLI
// has always honoured. An exact name always wins, as it does for the
// decoder.
func canonicalFieldNames(object map[string]interface{}, names ...string) {
	for _, name := range names {
		if _, exact := object[name]; exact {
			continue
		}
		var match string
		for key := range object {
			if strings.EqualFold(key, name) && (match == "" || key > match) {
				match = key
			}
		}
		if match != "" {
			object[name] = object[match]
			delete(object, match)
		}
	}
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
			wellFormed = append(wellFormed, canonicalReceipt(entry.(map[string]interface{})))
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

// canonicalReceipt is a copy of a well-formed entry whose receipt_b64 is
// re-spelled in canonical standard base64 from the bytes this CLI has always
// read from it (Go's decoder ignores line breaks), so a receipt wrapped
// across lines is still checked rather than refused by
// agent-action-capsule's strict decoder. The receipt bytes are unchanged.
func canonicalReceipt(fields map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(fields))
	for k, v := range fields {
		out[k] = v
	}
	encoded, _ := fields["receipt_b64"].(string)
	if raw, err := base64.StdEncoding.DecodeString(encoded); err == nil {
		out["receipt_b64"] = base64.StdEncoding.EncodeToString(raw)
	}
	return out
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
