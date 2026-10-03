package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/envelope"
	"github.com/spf13/cobra"
)

// signedCheckpointFields are the checkpoint fields a COSE checkpoint signs;
// a bundle's JSON copy of any of them must equal the signed value.
var signedCheckpointFields = []string{"log_id", "mmr_size", "root", "key_id", "timestamp", "prev_size", "prev_root"}

// checkpointClaim holds the bundle's checkpoint to its signature: the COSE
// checkpoint must verify, and every signed field the JSON copy carries must
// equal the signed value (log_id, mmr_size and root must be carried). A
// bundle with no checkpoint.cose is not shown ("withheld"). The verified
// statement's bytes are returned when the claim passes, else nil.
func checkpointClaim(value map[string]interface{}) (aacbundle.ClaimResult, []byte) {
	stated, _ := value["checkpoint"].(map[string]interface{})
	encoded, present := stated["cose"].(string)
	if !present {
		return aacbundle.ClaimResult{Status: "withheld", Findings: []string{"checkpoint_signature_absent"}}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return aacbundle.ClaimResult{Status: "fail", Findings: []string{"checkpoint_signature_malformed"}}, nil
	}
	signed, err := signedCheckpoint(raw)
	if err != nil {
		return aacbundle.ClaimResult{Status: "fail", Findings: []string{"checkpoint_signature_invalid"}}, nil
	}
	var findings []string
	for _, field := range signedCheckpointFields {
		got, carried := stated[field]
		if !carried {
			if field == "log_id" || field == "mmr_size" || field == "root" {
				findings = append(findings, "checkpoint_field_missing:"+field)
			}
			continue
		}
		if !sameJSONValue(got, signed[field]) {
			findings = append(findings, "checkpoint_field_mismatch:"+field)
		}
	}
	if len(findings) != 0 {
		return aacbundle.ClaimResult{Status: "fail", Findings: findings}, nil
	}
	return aacbundle.ClaimResult{Status: "pass"}, raw
}

// producerSignatureClaim checks each record's inline producer signature
// (signature: hex COSE_Sign1 producer envelope over the capsule_id; key_id:
// the signer's raw public key). A record with neither is unsigned (not
// shown); one with only one of them, or whose envelope does not verify under
// its key_id, fails. Returns the claim and each record's state.
func producerSignatureClaim(records []interface{}) (aacbundle.ClaimResult, map[string]string) {
	states := map[string]string{}
	var findings []string
	unsigned := false
	failed := false
	for _, raw := range records {
		record, _ := raw.(map[string]interface{})
		id, _ := record["capsule_id"].(string)
		sig, hasSig := record["signature"].(string)
		key, hasKey := record["key_id"].(string)
		switch {
		case !hasSig && !hasKey:
			states[id] = "unclaimed"
			unsigned = true
			findings = append(findings, "producer_signature_unclaimed:"+id)
			continue
		case hasSig != hasKey:
			states[id] = "invalid"
		default:
			envelopeBytes, sigErr := hex.DecodeString(sig)
			keyBytes, keyErr := hex.DecodeString(key)
			result := envelope.Verify(id, envelopeBytes)
			if sigErr == nil && keyErr == nil && result.OK && bytes.Equal(result.PublicKey, keyBytes) {
				states[id] = "authored"
				continue
			}
			states[id] = "invalid"
		}
		failed = true
		findings = append(findings, "producer_signature_invalid:"+id)
	}
	switch {
	case failed || len(records) == 0:
		return aacbundle.ClaimResult{Status: "fail", Findings: findings}, states
	case unsigned:
		return aacbundle.ClaimResult{Status: "withheld", Findings: findings}, states
	default:
		return aacbundle.ClaimResult{Status: "pass"}, states
	}
}

// ErrBundleInvalid is a failed Evidence Bundle check: the result was printed,
// and at least one claim failed.
var ErrBundleInvalid = errors.New("evidence bundle verification failed")

// claimOutput is one claim's status, as printed by `verify --bundle`.
type claimOutput struct {
	Status   string   `json:"status"`
	Findings []string `json:"findings"`
}

func claim(result aacbundle.ClaimResult) claimOutput {
	findings := result.Findings
	if findings == nil {
		findings = []string{}
	}
	return claimOutput{Status: result.Status, Findings: findings}
}

// verifyBundleFile checks an Evidence Bundle (evidence-bundle/v2) offline, from
// the file alone: every record's identity, citation closure, interval
// coverage and per-record membership under the checkpoint (authenticated
// when the bundle carries checkpoint.cose), disclosures, and which extensions
// and countersignatures were carried but not verified here. No profile, no
// network. Exit 0 when every claim passes, 3 (ErrPartial) when nothing failed
// but something is not shown, 1 (ErrBundleInvalid) when a claim failed.
func verifyBundleFile(c *cobra.Command, path string) error {
	var directory []witnessRow
	if directoryPath, _ := c.Flags().GetString("witness-directory"); directoryPath != "" {
		rows, err := loadWitnessDirectory(directoryPath)
		if err != nil {
			return err
		}
		directory = rows
	}
	raw, err := readInput(path)
	if err != nil {
		return err
	}
	value, err := decodeBundleJSON(raw)
	if err != nil {
		return err
	}
	if value["bundle_kind"] != "evidence-bundle/v2" || value["bundle_version"] != "2" {
		return inputError("not an evidence-bundle/v2 file")
	}
	result := aacbundle.VerifyBundle(value)

	records := map[string]string{}
	ids := make([]string, 0, len(result.CapsuleResults))
	for id := range result.CapsuleResults {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	identityOK := len(ids) > 0
	for _, id := range ids {
		if result.CapsuleResults[id].OK {
			records[id] = "passed"
		} else {
			records[id] = "failed"
			identityOK = false
		}
	}
	disclosures := make([]map[string]string, 0, len(result.Disclosures))
	for _, d := range result.Disclosures {
		disclosures = append(disclosures, map[string]string{"capsule_id": d.CapsuleID, "member": d.Member, "status": d.Status})
	}
	extensions := make([]map[string]string, 0, len(result.Extensions))
	for _, x := range result.Extensions {
		extensions = append(extensions, map[string]string{"kind": x.Kind, "status": x.Status})
	}
	countersignatures := make([]string, 0, len(result.Countersignatures))
	for _, s := range result.Countersignatures {
		countersignatures = append(countersignatures, s.Status)
	}

	rawRecords, _ := value["records"].([]interface{})
	signatures, signatureStates := producerSignatureClaim(rawRecords)
	signedCheckpoint, statement := checkpointClaim(value)
	witnesses, witnessReceipts := witnessClaim(value, statement, directory)
	claims := []aacbundle.ClaimResult{result.GraphClosure, signedCheckpoint, result.IntervalCoverage, result.PerRecordMembership, signatures}
	// The bundle draft defines no witness member: only a failing receipt
	// changes the verdict, and a file without one is judged as before.
	if witnesses.Status == "fail" {
		claims = append(claims, witnesses)
	}
	verdict := "VALID"
	for _, r := range claims {
		if r.Status == "fail" {
			verdict = "INVALID"
		} else if (r.Status != "pass" || len(r.Findings) != 0) && verdict == "VALID" {
			verdict = "INCOMPLETE"
		}
	}
	if !identityOK {
		verdict = "INVALID"
	}

	digest := ""
	if result.BundleDigest != nil {
		digest = *result.BundleDigest
	}
	if err := output(c, map[string]any{
		"verdict":               verdict,
		"bundle_digest":         digest,
		"record_identity":       records,
		"graph_closure":         claim(result.GraphClosure),
		"interval_coverage":     claim(result.IntervalCoverage),
		"per_record_membership": claim(result.PerRecordMembership),
		"disclosures":           disclosures,
		"extensions":            extensions,
		"countersignatures":     countersignatures,
		"checkpoint":            claim(signedCheckpoint),
		"producer_signatures":   claim(signatures),
		"record_signatures":     signatureStates,
		"witnesses":             map[string]any{"status": witnesses.Status, "findings": nonNilStrings(witnesses.Findings), "receipts": witnessReceipts},
	}); err != nil {
		return err
	}
	switch verdict {
	case "VALID":
		return nil
	case "INCOMPLETE":
		return ErrPartial
	default:
		return ErrBundleInvalid
	}
}

// sameJSONValue is JSON equality with the type kept, as Python's == on the
// decoded values: a string never equals a number ("5" is not 5); two numbers
// are equal when their values are (5 equals 5.0); strings compare exactly.
// Values come from decoders using UseNumber, so numbers are json.Number.
func sameJSONValue(a, b interface{}) bool {
	switch x := a.(type) {
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		if xi, err := x.Int64(); err == nil {
			if yi, err := y.Int64(); err == nil {
				return xi == yi
			}
		}
		xf, errX := x.Float64()
		yf, errY := y.Float64()
		return errX == nil && errY == nil && xf == yf
	case string:
		y, ok := b.(string)
		return ok && x == y
	default:
		return fmt.Sprintf("%T:%v", a, a) == fmt.Sprintf("%T:%v", b, b)
	}
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
