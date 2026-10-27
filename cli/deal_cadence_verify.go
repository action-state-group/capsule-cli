package cli

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/witness"
	"github.com/action-state-group/checkpointed-local-log/go/checkpoint"
	"github.com/action-state-group/checkpointed-local-log/go/mmr"
)

// cadenceClaim checks a bundle's x-cadence-witness/v0 chain: the bundle's
// own (deal) checkpoint is a leaf, at the stated position and with the
// stated salt, of the tree whose root is an entry of the cadence log; that
// entry is included in the cadence checkpoint; the cadence checkpoint is
// signed by the same key as the deal checkpoint; and its witness receipts
// verify under the directory's keys. present is false when the bundle claims
// no witnessed chain. dealStatement is nil when the bundle's checkpoint did
// not verify.
func cadenceClaim(value map[string]interface{}, dealStatement []byte, directory *witness.Directory) (result aacbundle.ClaimResult, receipts []map[string]any, present bool) {
	chain := CadenceChainOf(value)
	if chain["state"] != "witnessed" {
		return aacbundle.ClaimResult{}, nil, false
	}
	fail := func(finding string) (aacbundle.ClaimResult, []map[string]any, bool) {
		return aacbundle.ClaimResult{Status: "fail", Findings: []string{finding}}, []map[string]any{}, true
	}
	if dealStatement == nil {
		return aacbundle.ClaimResult{Status: "withheld", Findings: []string{"checkpoint_unverified"}}, []map[string]any{}, true
	}
	deal, err := checkpoint.ParseRecord(dealStatement)
	if err != nil {
		return fail("cadence_deal_checkpoint_malformed")
	}
	// Witnessed in part: the leaf holds an earlier checkpoint of this deal,
	// signed by the same key, and a consistency proof shows the bundle's
	// checkpoint extends it. The rest of the chain is checked for it.
	if chain["extent"] == "part" {
		earlier, _ := chain["earlier"].(map[string]interface{})
		cp, _ := earlier["checkpoint"].(map[string]interface{})
		encoded, _ := cp["cose"].(string)
		statement, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			return fail("cadence_earlier_checkpoint_malformed")
		}
		if _, err = SignedCheckpoint(statement); err != nil {
			return fail("cadence_earlier_checkpoint_signature_invalid")
		}
		prior, err := checkpoint.ParseRecord(statement)
		if err != nil {
			return fail("cadence_earlier_checkpoint_malformed")
		}
		if prior.KeyID != deal.KeyID || prior.LogID != deal.LogID || prior.MMRSize >= deal.MMRSize {
			return fail("cadence_earlier_checkpoint_not_this_deal")
		}
		proof, err := parseConsistencyJSON(earlier["consistency_proof"])
		oldRoot, oldErr := hex.DecodeString(prior.Root)
		newRoot, newErr := hex.DecodeString(deal.Root)
		if err != nil || oldErr != nil || newErr != nil || proof.OldSize != prior.MMRSize || proof.NewSize != deal.MMRSize || !mmr.VerifyConsistency(oldRoot, newRoot, proof) {
			return fail("cadence_earlier_checkpoint_not_a_prefix")
		}
		dealStatement, deal = statement, prior
	}
	logID, _ := chain["log_id"].(string)
	if logID == "" {
		logID, _ = chain["deal_log_id"].(string) // bundles written before the rename
	}
	salt, _ := chain["salt"].(string)
	size, sizeErr := JSONUint(chain["size"])
	index, indexErr := JSONUint(chain["index"])
	if logID != deal.LogID || sizeErr != nil || size != deal.MMRSize {
		return fail("cadence_leaf_does_not_name_this_checkpoint")
	}
	if indexErr != nil || index >= 1<<DealCadenceDepth || salt == "" {
		return fail("cadence_leaf_malformed")
	}
	rawPath, _ := chain["path"].([]interface{})
	if len(rawPath) != DealCadenceDepth {
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
	leaf, err := CadenceLeaf(logID, size, hex.EncodeToString(sum[:]), salt)
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
	if _, err = SignedCheckpoint(statement); err != nil {
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
	entry, entryErr := JSONUint(inner["entry_index"])
	proof, proofErr := parseProofJSON(inner["inclusion_proof"])
	cadenceRoot, rootErr := hex.DecodeString(cadence.Root)
	if entryErr != nil || proofErr != nil || rootErr != nil || !mmr.VerifyHexInclusion(cadenceRoot, cadence.MMRSize, entry, hex.EncodeToString(root), proof) {
		return fail("cadence_entry_not_included")
	}

	result, receipts = receiptsClaim(inner["witnesses"], statement, directory, "cadence")
	return result, receipts, true
}

func JSONUint(v interface{}) (uint64, error) {
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
	if p.V, err = JSONUint(m["v"]); err != nil {
		return p, err
	}
	if p.Size, err = JSONUint(m["size"]); err != nil {
		return p, err
	}
	if p.LeafIndex, err = JSONUint(m["leaf_index"]); err != nil {
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

func parseConsistencyJSON(v interface{}) (mmr.ConsistencyProof, error) {
	m, ok := v.(map[string]interface{})
	if !ok {
		return mmr.ConsistencyProof{}, fmt.Errorf("not a proof")
	}
	hashes := func(v interface{}) ([][]byte, error) {
		list, _ := v.([]interface{})
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
	var p mmr.ConsistencyProof
	var err error
	p.Kind, _ = m["kind"].(string)
	if p.V, err = JSONUint(m["v"]); err != nil {
		return p, err
	}
	if p.OldSize, err = JSONUint(m["old_size"]); err != nil {
		return p, err
	}
	if p.NewSize, err = JSONUint(m["new_size"]); err != nil {
		return p, err
	}
	if p.OldPeaks, err = hashes(m["old_peaks"]); err != nil {
		return p, err
	}
	if p.NewPeaks, err = hashes(m["new_peaks"]); err != nil {
		return p, err
	}
	steps, _ := m["witness"].([]interface{})
	p.Witness = make([][][]byte, len(steps))
	for i, step := range steps {
		if p.Witness[i], err = hashes(step); err != nil {
			return p, err
		}
	}
	return p, nil
}
