package cli

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/checkpointed-local-log/go/witness"
)

// witnessRow is one row of a witness directory (capsule-emit's
// witnesses.json format): the witness's endpoint, its binding (cll when
// absent) and its keys. A key id is a raw Ed25519 key in hex, or the SHA-256
// of a DER key listed in public_keys. Other row fields are read past.
type witnessRow struct {
	Name       string   `json:"name"`
	Endpoint   string   `json:"endpoint"`
	Binding    string   `json:"binding"`
	KeyIDs     []string `json:"key_ids"`
	PublicKeys []string `json:"public_keys"`
}

// loadWitnessDirectory reads a witness directory file: {"witnesses": [rows]}
// or a bare array of rows.
func loadWitnessDirectory(path string) ([]witnessRow, error) {
	raw, err := readInput(path)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimLeft(string(raw), " \t\r\n")
	var rows []witnessRow
	if strings.HasPrefix(trimmed, "[") {
		err = json.Unmarshal(raw, &rows)
	} else {
		var wrapped struct {
			Witnesses *[]witnessRow `json:"witnesses"`
		}
		err = json.Unmarshal(raw, &wrapped)
		if err == nil && wrapped.Witnesses == nil {
			return nil, hint(ErrInput, "the witness directory has no witnesses array")
		}
		if wrapped.Witnesses != nil {
			rows = *wrapped.Witnesses
		}
	}
	if err != nil {
		return nil, hint(ErrInput, "the witness directory is not a witnesses.json file")
	}
	return rows, nil
}

// witnessBinding and witnessEndpoint read a receipt's witness URL the way
// the directory format names it: "rekor+https://..." and "scrapi+https://..."
// name those bindings, anything else is cll; the endpoint drops the binding
// prefix and any trailing slash.
func witnessBinding(url string) string {
	scheme, _, found := strings.Cut(url, "://")
	if !found {
		return "cll"
	}
	scheme = strings.ToLower(scheme)
	for _, binding := range []string{"rekor", "scrapi"} {
		if strings.HasPrefix(scheme, binding+"+") {
			return binding
		}
	}
	return "cll"
}

func witnessEndpoint(url string) string {
	if witnessBinding(url) != "cll" {
		_, url, _ = strings.Cut(url, "+")
	}
	return strings.TrimRight(url, "/")
}

func (r witnessRow) binding() string {
	if r.Binding == "" {
		return "cll"
	}
	return r.Binding
}

// keys returns the row's Ed25519 keys in key_ids order; a key id that names
// no usable Ed25519 key is skipped.
func (r witnessRow) keys() []ed25519.PublicKey {
	byHash := map[string]ed25519.PublicKey{}
	for _, b64 := range r.PublicKeys {
		der, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			continue
		}
		parsed, err := x509.ParsePKIXPublicKey(der)
		if key, ok := parsed.(ed25519.PublicKey); ok && err == nil {
			sum := sha256.Sum256(der)
			byHash[hex.EncodeToString(sum[:])] = key
		}
	}
	var keys []ed25519.PublicKey
	for _, id := range r.KeyIDs {
		if key, ok := byHash[id]; ok {
			keys = append(keys, key)
		} else if raw, err := hex.DecodeString(id); err == nil && len(raw) == ed25519.PublicKeySize {
			keys = append(keys, ed25519.PublicKey(raw))
		}
	}
	return keys
}

// witnessClaim checks every receipt in checkpoint.witnesses against the
// signed checkpoint statement (nil when it did not verify), under the keys
// of directory's row for the receipt's witness. Per receipt: pass when it
// verifies under one of the row's keys; withheld when there is no row, no
// key, or a binding this CLI does not check; fail when it does not verify or
// is malformed. Overall: fail if any fails, else pass if any passes, else
// withheld. The file carrying none, or an unverified checkpoint, is withheld.
func witnessClaim(value map[string]interface{}, statement []byte, directory []witnessRow) (aacbundle.ClaimResult, []map[string]string) {
	stated, _ := value["checkpoint"].(map[string]interface{})
	entries, _ := stated["witnesses"].([]interface{})
	if len(entries) == 0 {
		return aacbundle.ClaimResult{Status: "withheld", Findings: []string{"witness_receipt_absent"}}, []map[string]string{}
	}
	if statement == nil {
		return aacbundle.ClaimResult{Status: "withheld", Findings: []string{"checkpoint_unverified"}}, []map[string]string{}
	}
	var findings []string
	receipts := make([]map[string]string, 0, len(entries))
	sawPass, sawFail := false, false
	for index, entry := range entries {
		receipt, url, err := decodeWitnessReceipt(entry)
		if err != nil {
			sawFail = true
			findings = append(findings, "witness_receipt_malformed:"+strconv.Itoa(index))
			continue
		}
		status, reason := checkReceipt(statement, receipt, url, directory)
		switch status {
		case "pass":
			sawPass = true
		case "withheld":
			findings = append(findings, "witness_unverified:"+url)
		default:
			sawFail = true
			findings = append(findings, "witness_receipt_invalid:"+url)
		}
		receipts = append(receipts, map[string]string{"ts_url": url, "binding": witnessBinding(url), "status": status, "reason": reason})
	}
	switch {
	case sawFail:
		return aacbundle.ClaimResult{Status: "fail", Findings: findings}, receipts
	case sawPass:
		return aacbundle.ClaimResult{Status: "pass", Findings: findings}, receipts
	default:
		return aacbundle.ClaimResult{Status: "withheld", Findings: findings}, receipts
	}
}

func checkReceipt(statement []byte, receipt witness.Receipt, url string, directory []witnessRow) (string, string) {
	binding, endpoint := witnessBinding(url), witnessEndpoint(url)
	var row *witnessRow
	for i := range directory {
		if directory[i].binding() == binding && strings.TrimRight(directory[i].Endpoint, "/") == endpoint {
			row = &directory[i]
			break
		}
	}
	switch {
	case row == nil:
		return "withheld", "not checked: no directory row for this witness"
	case binding != "cll":
		return "withheld", "not checked: capsulectl checks cll receipts only, not " + binding
	}
	keys := row.keys()
	if len(keys) == 0 {
		return "withheld", "not checked: no usable key in the directory row"
	}
	var last error
	for _, key := range keys {
		verifier, err := witness.NewReceiptVerifier(key)
		if err != nil {
			last = err
			continue
		}
		if last = verifier.Verify(statement, receipt); last == nil {
			return "pass", "verified under " + row.Name + "'s key"
		}
	}
	return "fail", last.Error()
}

// decodeWitnessReceipt reads one checkpoint.witnesses entry: ts_url,
// entry_hash, receipt_b64 (standard base64), and leaf_index and tree_size
// (numbers, or numbers as strings, as producers write them).
func decodeWitnessReceipt(entry interface{}) (witness.Receipt, string, error) {
	fields, ok := entry.(map[string]interface{})
	if !ok {
		return witness.Receipt{}, "", fmt.Errorf("not a receipt")
	}
	url, _ := fields["ts_url"].(string)
	entryHash, _ := fields["entry_hash"].(string)
	encoded, _ := fields["receipt_b64"].(string)
	if url == "" || entryHash == "" || encoded == "" {
		return witness.Receipt{}, "", fmt.Errorf("receipt fields missing")
	}
	bytes, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return witness.Receipt{}, "", err
	}
	leaf, err := receiptInteger(fields["leaf_index"])
	if err != nil {
		return witness.Receipt{}, "", err
	}
	size, err := receiptInteger(fields["tree_size"])
	if err != nil {
		return witness.Receipt{}, "", err
	}
	return witness.Receipt{Bytes: bytes, EntryHash: entryHash, EntryHashScheme: witness.EntryHashSchemeCheckpointDigest, LeafIndex: leaf, TreeSize: size}, url, nil
}

func receiptInteger(value interface{}) (int64, error) {
	switch v := value.(type) {
	case json.Number:
		return v.Int64()
	case string:
		return strconv.ParseInt(v, 10, 64)
	default:
		return 0, fmt.Errorf("receipt position is not an integer")
	}
}
