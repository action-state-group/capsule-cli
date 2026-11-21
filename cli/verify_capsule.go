package cli

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/action-state-group/agent-action-capsule/go/envelope"
	aacverify "github.com/action-state-group/agent-action-capsule/go/verify"
	"github.com/spf13/cobra"
)

// The two shapes `verify --capsule` reads.
const (
	// capsuleShapeRecord is the artifact.Record wrapper `seal --output` and
	// `get --raw` write: capsule_id, capsule (its bytes), producer_envelope
	// and artifacts.
	capsuleShapeRecord = "artifact-record"
	// capsuleShapeBare is a bare Agent Action Capsule, as capsule-emit seals
	// it: the capsule's own fields with an inline signature (the hex
	// COSE_Sign1 producer envelope over capsule_id) and key_id.
	capsuleShapeBare = "capsule"
)

const capsuleShapesAccepted = "an artifact.Record (capsule_id, capsule, producer_envelope, artifacts: what `seal --output` and `get --raw` write) or a bare Agent Action Capsule (spec_version, format_version, capsule_id and the capsule's other fields, with an inline signature and key_id: what capsule-emit seals)"

// capsuleFileShape reads which of the two shapes a --capsule file is from the
// members it has, and never guesses: a file with the members of both, or of
// neither, is refused with both shapes named.
func capsuleFileShape(path string, raw []byte) (string, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return "", InputError("--capsule " + path + " is not a JSON object; it must be " + capsuleShapesAccepted)
	}
	has := func(names ...string) bool {
		for _, n := range names {
			if _, ok := members[n]; !ok {
				return false
			}
		}
		return true
	}
	hasAny := func(names ...string) bool {
		for _, n := range names {
			if _, ok := members[n]; ok {
				return true
			}
		}
		return false
	}
	record := has("capsule_id", "capsule", "producer_envelope")
	bare := has("capsule_id", "spec_version", "format_version")
	switch {
	case record && !hasAny("spec_version", "format_version", "signature", "key_id"):
		return capsuleShapeRecord, nil
	case bare && !hasAny("capsule", "producer_envelope", "artifacts"):
		return capsuleShapeBare, nil
	case record || bare:
		return "", InputError("--capsule " + path + " has the members of both shapes; it must be exactly one: " + capsuleShapesAccepted)
	}
	return "", InputError("--capsule " + path + " is neither shape: it must be " + capsuleShapesAccepted)
}

// verifyBareCapsule checks a bare capsule as the wrapper is checked: its
// capsule_id recomputed (with every Class 1 check of the capsule), its inline
// producer signature under its key_id, and that key held to the profile's
// trusted keys. A bare capsule carries no retained originals, so its
// committed digests are not rehashed, and the output says so.
func verifyBareCapsule(c *cobra.Command, p Profile, path string, raw []byte) error {
	capsule, err := DecodeBundleJSON(raw)
	if err != nil {
		return InputError("--capsule " + path + ": " + err.Error())
	}
	id, _ := capsule["capsule_id"].(string)
	key, _ := capsule["key_id"].(string)
	out := map[string]any{
		"shape": capsuleShapeBare, "capsule_id": id,
		"originals":     "not carried by a bare capsule: its committed digests are not rehashed here",
		"cll_inclusion": "not_performed", "business_truth": "not_performed",
	}
	if key != "" {
		out["key_id"] = key
	}
	fail := func(field, why string) error {
		out[field] = "failed: " + why
		if outErr := Output(c, out); outErr != nil {
			return outErr
		}
		return errors.New("capsule verification failed: " + why)
	}

	identity := aacverify.Verify(capsule, nil, nil)
	if !identity.OK {
		var codes []string
		for _, f := range identity.Findings {
			if f.Severity == "error" {
				codes = append(codes, f.Code)
			}
		}
		return fail("capsule_identity", strings.Join(codes, ", "))
	}
	out["capsule_identity"] = "passed"

	_, hasSig := capsule["signature"].(string)
	_, hasKey := capsule["key_id"].(string)
	switch {
	case !hasSig && !hasKey:
		out["producer_signature"] = "absent: the capsule carries no signature and key_id"
		if outErr := Output(c, out); outErr != nil {
			return outErr
		}
		return ErrPartial
	case hasSig != hasKey:
		return fail("producer_signature", "the capsule carries one of signature and key_id without the other")
	}
	// The signature itself is AAC's record check, which recomputes the
	// Capsule ID from the record's own content; only trust is the CLI's.
	if verdict, _ := envelope.VerifyRecordSignature(capsule); verdict != envelope.Authored {
		return fail("producer_signature", "the signature does not verify under the capsule's key_id")
	}
	keyBytes, _ := hex.DecodeString(key)

	keys, err := ParseKeys(p.TrustedKeys)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		out["producer_signature"], out["producer_trust"] = "passed", "not_performed: no trusted key"
		if outErr := Output(c, out); outErr != nil {
			return outErr
		}
		return ErrPartial
	}
	for _, trusted := range keys {
		if bytes.Equal(trusted, keyBytes) {
			out["producer_signature_and_trust"] = "passed"
			return Output(c, out)
		}
	}
	return fail("producer_signature_and_trust", "the signature verifies, but its key_id is not among the profile's trusted keys")
}

// dealReceiptPointer names the one verifiable form of a deal: the receipt
// page `deal report` writes, checked as an Evidence Bundle.
const dealReceiptPointer = "to verify an x-deal-v0 deal's records, write its receipt with `capsulectl deal report --deal ID --html receipt.html` and run `capsulectl verify --bundle receipt.html`"

// dealExportHint refuses what `deal export` writes, with the command to use
// instead. The export is a JSON array of x-deal-v0 records (each an object
// with an `x-deal-v0` member and no capsule_id or signature), so there is
// nothing for --capsule to authenticate; the receipt is what verifies. Any
// other input returns nil and is read by capsuleFileShape as before.
func dealExportHint(path string, raw []byte) error {
	var list []map[string]json.RawMessage
	var object map[string]json.RawMessage
	isRecord := func(m map[string]json.RawMessage) bool {
		_, deal := m["x-deal-v0"]
		_, id := m["capsule_id"]
		return deal && !id
	}
	switch {
	case json.Unmarshal(raw, &list) == nil && len(list) > 0:
		for _, m := range list {
			if m == nil || !isRecord(m) {
				return nil
			}
		}
	case json.Unmarshal(raw, &object) == nil && object != nil && isRecord(object):
	default:
		return nil
	}
	return InputError("--capsule " + path + " is a deal export: the deal's records without the capsules that signed them (no capsule ID), not a capsule, so there is nothing to verify here; " + dealReceiptPointer)
}
