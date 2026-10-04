package cli

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/cll-go/checkpoint"
	"github.com/action-state-group/cll-go/mmr"
)

// cadenceClaim checks a deal bundle's x-deal-cadence-v0 chain: the bundle's
// own (deal) checkpoint is a leaf, at the stated position and with the
// stated salt, of the tree whose root is an entry of the cadence log; that
// entry is included in the cadence checkpoint; the cadence checkpoint is
// signed by the same key as the deal checkpoint; and its witness receipts
// verify under the directory's keys. present is false when the bundle claims
// no witnessed chain. dealStatement is nil when the bundle's checkpoint did
// not verify.
func cadenceClaim(value map[string]interface{}, dealStatement []byte, directory []witnessRow) (result aacbundle.ClaimResult, receipts []map[string]string, present bool) {
	ext, _ := value["extensions"].(map[string]interface{})
	chain, _ := ext[dealCadenceExtension].(map[string]interface{})
	if chain["state"] != "witnessed" {
		return aacbundle.ClaimResult{}, nil, false
	}
	fail := func(finding string) (aacbundle.ClaimResult, []map[string]string, bool) {
		return aacbundle.ClaimResult{Status: "fail", Findings: []string{finding}}, []map[string]string{}, true
	}
	if dealStatement == nil {
		return aacbundle.ClaimResult{Status: "withheld", Findings: []string{"checkpoint_unverified"}}, []map[string]string{}, true
	}
	deal, err := checkpoint.ParseRecord(dealStatement)
	if err != nil {
		return fail("cadence_deal_checkpoint_malformed")
	}
	logID, _ := chain["deal_log_id"].(string)
	salt, _ := chain["salt"].(string)
	size, sizeErr := jsonUint(chain["size"])
	index, indexErr := jsonUint(chain["index"])
	if logID != deal.LogID || sizeErr != nil || size != deal.MMRSize {
		return fail("cadence_leaf_does_not_name_this_checkpoint")
	}
	if indexErr != nil || index >= 1<<dealCadenceDepth || salt == "" {
		return fail("cadence_leaf_malformed")
	}
	rawPath, _ := chain["path"].([]interface{})
	if len(rawPath) != dealCadenceDepth {
		return fail("cadence_path_malformed")
	}
	path := make([][]byte, len(rawPath))
	for i, v := range rawPath {
		s, _ := v.(string)
		if path[i], err = hex.DecodeString(s); err != nil || len(path[i]) != sha256.Size {
			return fail("cadence_path_malformed")
		}
	}
	sum := sha256.Sum256(dealStatement)
	leaf, err := cadenceLeaf(logID, size, hex.EncodeToString(sum[:]), salt)
	if err != nil {
		return fail("cadence_leaf_malformed")
	}
	root := cadenceFold(leaf, index, path)

	inner, _ := chain["cadence"].(map[string]interface{})
	cp, _ := inner["checkpoint"].(map[string]interface{})
	encoded, _ := cp["cose"].(string)
	statement, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return fail("cadence_checkpoint_malformed")
	}
	if _, err = signedCheckpoint(statement); err != nil {
		return fail("cadence_checkpoint_signature_invalid")
	}
	cadence, err := checkpoint.ParseRecord(statement)
	if err != nil {
		return fail("cadence_checkpoint_malformed")
	}
	if cadence.KeyID != deal.KeyID {
		return fail("cadence_checkpoint_signer_differs")
	}
	if inner["log_id"] != cadence.LogID {
		return fail("cadence_log_id_mismatch")
	}
	entry, entryErr := jsonUint(inner["entry_index"])
	proof, proofErr := parseProofJSON(inner["inclusion_proof"])
	cadenceRoot, rootErr := hex.DecodeString(cadence.Root)
	if entryErr != nil || proofErr != nil || rootErr != nil || !mmr.VerifyHexInclusion(cadenceRoot, cadence.MMRSize, entry, hex.EncodeToString(root), proof) {
		return fail("cadence_entry_not_included")
	}

	entries, _ := inner["witnesses"].([]interface{})
	if len(entries) == 0 {
		return aacbundle.ClaimResult{Status: "withheld", Findings: []string{"witness_receipt_absent"}}, []map[string]string{}, true
	}
	var findings []string
	receipts = make([]map[string]string, 0, len(entries))
	sawPass, sawFail := false, false
	for i, e := range entries {
		receipt, url, err := decodeWitnessReceipt(e)
		if err != nil {
			sawFail = true
			findings = append(findings, "witness_receipt_malformed:"+strconv.Itoa(i))
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
		receipts = append(receipts, map[string]string{"ts_url": url, "binding": witnessBinding(url), "status": status, "reason": reason, "via": "cadence"})
	}
	switch {
	case sawFail:
		return aacbundle.ClaimResult{Status: "fail", Findings: findings}, receipts, true
	case sawPass:
		return aacbundle.ClaimResult{Status: "pass", Findings: findings}, receipts, true
	default:
		return aacbundle.ClaimResult{Status: "withheld", Findings: findings}, receipts, true
	}
}

func jsonUint(v interface{}) (uint64, error) {
	switch n := v.(type) {
	case json.Number:
		return strconv.ParseUint(n.String(), 10, 64)
	case float64:
		if n < 0 || n != float64(uint64(n)) {
			return 0, fmt.Errorf("not an unsigned integer")
		}
		return uint64(n), nil
	default:
		return 0, fmt.Errorf("not a number")
	}
}

// parseProofJSON reads an inclusion proof as proofJSON writes it.
func parseProofJSON(v interface{}) (mmr.InclusionProof, error) {
	m, ok := v.(map[string]interface{})
	if !ok {
		return mmr.InclusionProof{}, fmt.Errorf("not a proof")
	}
	hashes := func(key string) ([][]byte, error) {
		list, _ := m[key].([]interface{})
		out := make([][]byte, len(list))
		for i, item := range list {
			s, _ := item.(string)
			b, err := hex.DecodeString(s)
			if err != nil {
				return nil, err
			}
			out[i] = b
		}
		return out, nil
	}
	var p mmr.InclusionProof
	var err error
	kind, _ := m["kind"].(string)
	p.Kind = kind
	if p.V, err = jsonUint(m["v"]); err != nil {
		return p, err
	}
	if p.Size, err = jsonUint(m["size"]); err != nil {
		return p, err
	}
	if p.LeafIndex, err = jsonUint(m["leaf_index"]); err != nil {
		return p, err
	}
	if p.Witness, err = hashes("witness"); err != nil {
		return p, err
	}
	if p.PeaksLeft, err = hashes("peaks_left"); err != nil {
		return p, err
	}
	p.PeaksRight, err = hashes("peaks_right")
	return p, err
}
