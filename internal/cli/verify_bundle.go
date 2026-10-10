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
	"github.com/action-state-group/agent-action-capsule/go/disclosure"
	"github.com/action-state-group/agent-action-capsule/go/envelope"
	"github.com/spf13/cobra"
)

// signedCheckpointFields are the checkpoint fields a COSE checkpoint signs;
// a bundle's JSON copy of any of them must equal the signed value.
var signedCheckpointFields = []string{"log_id", "mmr_size", "root", "key_id", "timestamp", "prev_size", "prev_root"}

// checkpointClaim holds the bundle's checkpoint to its signature: the COSE
// checkpoint must verify, and every signed field the JSON copy carries must
// equal the signed value (mmr_size and root must be carried). The log id
// may be stated in the completeness certificate, the checkpoint, or both:
// the bundle draft requires the signed log identifier to equal "the
// certificate and checkpoint values", so at least one copy must be stated
// and every stated copy must equal the signed one. A bundle with no
// checkpoint.cose is not shown ("withheld"). The verified statement's bytes
// are returned when the claim passes, else nil.
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
		if field == "log_id" {
			if finding := logIDFinding(value, stated, signed[field]); finding != "" {
				findings = append(findings, finding)
			}
			continue
		}
		got, carried := stated[field]
		if !carried {
			if field == "mmr_size" || field == "root" {
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

// logIDFinding holds every log id the bundle states (the completeness
// certificate's and the checkpoint's) to the signed one: at least one must be
// stated, and each stated copy must equal it.
func logIDFinding(value, stated map[string]interface{}, signed interface{}) string {
	certificate, _ := value["completeness_certificate"].(map[string]interface{})
	copies := 0
	for _, holder := range []map[string]interface{}{certificate, stated} {
		got, carried := holder["log_id"]
		if !carried {
			continue
		}
		copies++
		if !sameJSONValue(got, signed) {
			return "checkpoint_field_mismatch:log_id"
		}
	}
	if copies == 0 {
		return "checkpoint_field_missing:log_id"
	}
	return ""
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
// when the bundle carries checkpoint.cose), disclosures, a composed/v1
// extension (bundle -01 §7.2, with each member bundle assessed as its own
// Evidence Bundle), and which other extensions and countersignatures were
// carried but not verified here. No profile, no
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
	if embedded, ok := receiptBundleJSON(raw); ok {
		raw = embedded
	}
	value, err := decodeBundleJSON(raw)
	if err != nil {
		return err
	}
	if value["bundle_kind"] != "evidence-bundle/v2" || value["bundle_version"] != "2" {
		return inputError("--bundle is not an evidence-bundle/v2 file (bundle_kind \"evidence-bundle/v2\", bundle_version \"2\")")
	}
	verdict, report := bundleVerdict(value, directory)
	if err := output(c, report); err != nil {
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

// bundleVerdict is `verify --bundle`'s verdict on an Evidence Bundle (VALID,
// INCOMPLETE or INVALID) and the result it prints, from the bundle's value
// and an optional witness directory. A page that checks itself is written
// only after this same verdict is VALID (pageGate), so the page can never
// claim more than verify --bundle does about the bundle it carries.
func bundleVerdict(value map[string]interface{}, directory []witnessRow) (string, map[string]any) {
	result := aacbundle.VerifyBundleWithOptions(value, aacbundle.Options{RefusalSignature: verifyRefusalSignature})
	report, verdict := assessBundle(value, result, directory)
	return verdict, report
}

// assessBundle reports one Evidence Bundle's claims and its verdict. A
// composed/v1 block's member bundles are assessed the same way and reported
// on their member; their claims are never merged into the containing
// Bundle's, but a failed member or block fails the verdict, and one with
// something not shown makes it INCOMPLETE.
func assessBundle(value map[string]interface{}, result aacbundle.VerificationResult, directory []witnessRow) (map[string]any, string) {
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
	// A disclosed member either matches what its record sealed or is
	// withheld; any other result (a mismatch, a member that cannot be
	// disclosed, one with no sealed digest, or an unknown status) means the
	// bundle carries content its records do not vouch for.
	//
	// They are listed by capsule id, then member, then status. The
	// verifier lists a record's withheld members in map order, so without
	// this the same bundle could print differently on each run.
	disclosures := make([]map[string]string, 0, len(result.Disclosures))
	disclosuresOK := true
	for _, d := range result.Disclosures {
		disclosures = append(disclosures, map[string]string{"capsule_id": d.CapsuleID, "member": d.Member, "status": d.Status})
		if d.Status != disclosure.Match && d.Status != "withheld" {
			disclosuresOK = false
		}
	}
	sort.SliceStable(disclosures, func(i, j int) bool {
		a, b := disclosures[i], disclosures[j]
		if a["capsule_id"] != b["capsule_id"] {
			return a["capsule_id"] < b["capsule_id"]
		}
		if a["member"] != b["member"] {
			return a["member"] < b["member"]
		}
		return a["status"] < b["status"]
	})
	countersignatures := make([]string, 0, len(result.Countersignatures))
	for _, s := range result.Countersignatures {
		countersignatures = append(countersignatures, s.Status)
	}

	rawRecords, _ := value["records"].([]interface{})
	signatures, signatureStates := producerSignatureClaim(rawRecords)
	signedCheckpoint, statement := checkpointClaim(value)
	witnesses, witnessReceipts := witnessClaim(value, statement, directory)
	// A deal bundle's receipt is on the cadence checkpoint its own
	// checkpoint is anchored in; that chain is the bundle's witness claim.
	cadence, cadenceReceipts, anchored := cadenceClaim(value, statement, directory)
	if anchored {
		witnesses, witnessReceipts = cadence, cadenceReceipts
	}
	claims := []aacbundle.ClaimResult{result.GraphClosure, signedCheckpoint, result.IntervalCoverage, result.PerRecordMembership, signatures}
	// The bundle draft defines no witness member: only a failing receipt
	// changes the verdict, and a file without one is judged as before.
	if witnesses.Status == "fail" {
		claims = append(claims, witnesses)
	}
	extensions := make([]map[string]any, 0, len(result.Extensions))
	dealSeen := false
	for _, x := range result.Extensions {
		entry := map[string]any{"kind": x.Kind, "status": x.Status}
		if x.Kind == dealProfile && x.Status == "uninterpreted" {
			dealSeen = true
			entry = dealReportEntry(value, result)
			if entry["status"] == "fail" {
				claims = append(claims, aacbundle.ClaimResult{Status: "fail", Findings: entry["findings"].([]string)})
			}
		}
		if x.Kind == dealSaleExtension {
			sale, claim := saleBundleEntry(value, result, directory)
			entry = sale
			claims = append(claims, claim)
		}
		if x.Composed != nil {
			composed, memberVerdicts := composedOutput(value, x.Composed, directory)
			entry["composed"] = composed
			claims = append(claims, aacbundle.ClaimResult{Status: x.Composed.Status, Findings: x.Composed.Findings})
			for _, memberVerdict := range memberVerdicts {
				claims = append(claims, verdictClaim(memberVerdict))
			}
		}
		extensions = append(extensions, entry)
	}
	// A deal bundle that discloses a sealed report but carries no x-deal-v0
	// extension at all had it removed.
	if !dealSeen {
		if entry := dealReportEntry(value, result); entry["status"] == "fail" {
			extensions = append(extensions, entry)
			claims = append(claims, aacbundle.ClaimResult{Status: "fail", Findings: entry["findings"].([]string)})
		}
	}
	verdict := "VALID"
	for _, r := range claims {
		if r.Status == "fail" {
			verdict = "INVALID"
		} else if (r.Status != "pass" || len(r.Findings) != 0) && verdict == "VALID" {
			verdict = "INCOMPLETE"
		}
	}
	if !identityOK || !disclosuresOK {
		verdict = "INVALID"
	}

	digest := ""
	if result.BundleDigest != nil {
		digest = *result.BundleDigest
	}
	return map[string]any{
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
	}, verdict
}

// verdictClaim folds a member bundle's verdict into the containing verdict.
func verdictClaim(verdict string) aacbundle.ClaimResult {
	switch verdict {
	case "VALID":
		return aacbundle.ClaimResult{Status: "pass"}
	case "INCOMPLETE":
		return aacbundle.ClaimResult{Status: "withheld"}
	default:
		return aacbundle.ClaimResult{Status: "fail"}
	}
}

// receiptBundleJSON returns the Evidence Bundle a self-contained report page
// (a deal receipt.html) embeds, so the page itself can be handed to
// `verify --bundle`. The emitter writes it as `window.__BUNDLE__ = <json>;`
// with every "<" escaped, so the first ";</script>" after it ends the JSON.
func receiptBundleJSON(raw []byte) ([]byte, bool) {
	const start = "window.__BUNDLE__ = "
	i := bytes.Index(raw, []byte(start))
	if i < 0 || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("<!")) {
		return nil, false
	}
	rest := raw[i+len(start):]
	j := bytes.Index(rest, []byte(";</script>"))
	if j < 0 {
		return nil, false
	}
	return rest[:j], true
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

// dealReportEntry is verify --bundle's entry for a deal bundle's x-deal-v0
// extension. A deal report seals each copy's readable text (its summary
// lines, step lines, amounts and what was told) as a deal_report record on
// the deal's log, and the extension names it (sealed_report); its text is
// then that record's disclosed input, which the disclosure check binds:
//   - pass: the extension names a deal_report record whose input is
//     disclosed and matches, and no other deal_report's input is disclosed;
//   - fail: it names anything else, the bundle discloses a deal_report the
//     extension does not name, or the bundle holds a deal_report record at
//     all and the extension names none (it was replaced or removed, whether
//     or not the record's input was dropped);
//   - a bundle written before reports were sealed carries the text in the
//     extension itself, which no record seals: uninterpreted, with the
//     finding extension_unbound (editing it leaves the verdict VALID).
func dealReportEntry(value map[string]interface{}, result aacbundle.VerificationResult) map[string]any {
	reports := map[string]bool{}
	records, _ := value["records"].([]interface{})
	for _, raw := range records {
		r, _ := raw.(map[string]interface{})
		if id, _ := r["capsule_id"].(string); id != "" && r["action_id"] == dealReportActionID {
			reports[id] = true
		}
	}
	matched := map[string]bool{}
	var disclosed []string
	for _, d := range result.Disclosures {
		if d.Member != "agent_input" || !reports[d.CapsuleID] || d.Status == "withheld" {
			continue
		}
		disclosed = append(disclosed, d.CapsuleID)
		if d.Status == disclosure.Match {
			matched[d.CapsuleID] = true
		}
	}
	ext, _ := value["extensions"].(map[string]interface{})
	deal, _ := ext[dealProfile].(map[string]interface{})
	fail := func(finding string) map[string]any {
		return map[string]any{"kind": dealProfile, "status": "fail", "findings": []string{finding}}
	}
	id, sealed := deal[dealReportPointer].(string)
	switch {
	case sealed && (!matched[id] || len(disclosed) != 1):
		return fail("sealed_report_unverified")
	case sealed:
		return map[string]any{"kind": dealProfile, "status": "pass", dealReportPointer: id}
	case len(reports) > 0:
		// A bundle that holds a sealed report was written after reports were
		// sealed: text in the extension instead of a pointer is a downgrade
		// (the record kept, its disclosure dropped, edited text inline).
		return fail("sealed_report_not_named")
	}
	if _, sale := ext[dealSaleExtension]; sale {
		return map[string]any{
			"kind": dealProfile, "status": "uninterpreted", "findings": []string{"extension_unbound"},
			"note": "a sale's section: its sale_threads openings are checked against the sealed registrations under " + dealSaleExtension,
		}
	}
	return map[string]any{
		"kind": dealProfile, "status": "uninterpreted", "findings": []string{"extension_unbound"},
		"note": "written by the producer when the bundle was made and sealed by no record: its text is not verified, and editing it does not change the verdict",
	}
}
