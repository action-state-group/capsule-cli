package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/disclosure"
	"github.com/action-state-group/agent-action-capsule/go/witness"
	"github.com/spf13/cobra"
)

// CheckpointClaim is AAC strict's checkpoint claim (bundle.VerifyCheckpoint):
// checkpoint.cose must verify, and the JSON copy, and the completeness
// certificate's log_id when stated, must equal the signed statement. A bundle
// with no checkpoint.cose is not shown ("withheld"). The verified statement's
// bytes are returned when the claim passes, else nil, for the witness and
// cadence checks against it.
func CheckpointClaim(value map[string]interface{}) (aacbundle.ClaimResult, []byte) {
	claim, _, statement := aacbundle.VerifyCheckpoint(value)
	return claim, statement
}

// signaturesClaim is AAC strict's producer-signature claim and each record's
// verdict (authored, unclaimed or invalid) by capsule id. A value strict
// refuses before reading its records (not an evidence-bundle/v2 object)
// carries its refusal here too.
func signaturesClaim(strict aacbundle.StrictResult) (aacbundle.ClaimResult, map[string]string) {
	states := map[string]string{}
	for _, record := range strict.Records {
		states[record.CapsuleID] = record.Signature
	}
	if signatures := strict.Claim("signatures"); signatures != nil {
		return *signatures, states
	}
	// Strict always reports records first; a refused value has only that.
	return *strict.Claim("records"), states
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
	var directory *witness.Directory
	if directoryPath, _ := c.Flags().GetString("witness-directory"); directoryPath != "" {
		rows, err := LoadWitnessDirectory(directoryPath)
		if err != nil {
			return err
		}
		directory = &rows
	}
	raw, err := ReadBundleInput(path)
	if err != nil {
		return err
	}
	embedded, ok, err := receiptBundleJSON(raw)
	if err != nil {
		return err
	}
	if ok {
		raw = embedded
	}
	value, err := DecodeBundleJSON(raw)
	if err != nil {
		return err
	}
	if value["bundle_kind"] != "evidence-bundle/v2" || value["bundle_version"] != "2" {
		return InputError("--bundle is not an evidence-bundle/v2 file (bundle_kind \"evidence-bundle/v2\", bundle_version \"2\")")
	}
	verdict, report := BundleVerdict(value, directory)
	if err := Output(c, report); err != nil {
		return err
	}
	return verdictError(verdict)
}

// verdictError is the exit for a verdict: none for VALID, ErrPartial for
// INCOMPLETE, ErrBundleInvalid otherwise.
func verdictError(verdict string) error {
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
func BundleVerdict(value map[string]interface{}, directory *witness.Directory) (string, map[string]any) {
	verdict, _, report := bundleAssessment(value, directory)
	return verdict, report
}

// bundleAssessment is BundleVerdict plus pageVerdict: the verdict with every
// composed/v1 refusal member whose signature verifies read as the observer's
// served answer instead of INCOMPLETE. It is VALID exactly when the only
// thing keeping the bundle from VALID is such refusals, which a deal page
// draws as refused cases (PageGate); otherwise it equals verdict.
func bundleAssessment(value map[string]interface{}, directory *witness.Directory) (verdict, pageVerdict string, report map[string]any) {
	result := aacbundle.VerifyBundleWithOptions(value, bundleOptions)
	report, verdict, pageVerdict = assessBundle(value, result, directory)
	return verdict, pageVerdict, report
}

// bundleOptions are the AAC verifier options verify --bundle uses.
var bundleOptions = aacbundle.Options{RefusalSignature: verifyRefusalSignature, ResponseSignature: verifyResponseSignature}

// withSelection takes a producer-selected bundle's interval coverage and
// per-record membership from AAC's strict reading (capsule-cli#301): its
// certificate binds the producer's pick, not an interval, so coverage is
// withheld (selection_not_checkable) and each record's own inclusion proof
// is checked against the signed checkpoint. Every other bundle keeps the
// plain verifier's reading, so no other verdict moves.
func withSelection(value map[string]interface{}, result aacbundle.VerificationResult, strict aacbundle.StrictResult) aacbundle.VerificationResult {
	if completeness, _ := value["completeness"].(map[string]interface{}); completeness["selection"] != aacbundle.SelectionProducerSelected {
		return result
	}
	result.IntervalCoverage, result.PerRecordMembership = strict.IntervalCoverage, strict.PerRecordMembership
	return result
}

// assessBundle reports one Evidence Bundle's claims and its verdict. A
// composed/v1 block's member bundles are assessed the same way and reported
// on their member; their claims are never merged into the containing
// Bundle's, but a failed member or block fails the verdict, and one with
// something not shown makes it INCOMPLETE.
func assessBundle(value map[string]interface{}, result aacbundle.VerificationResult, directory *witness.Directory) (map[string]any, string, string) {
	strict := aacbundle.VerifyStrict(value, aacbundle.StrictOptions{Options: bundleOptions})
	result = withSelection(value, result, strict)
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

	signatures, signatureStates := signaturesClaim(strict)
	signedCheckpoint, statement := CheckpointClaim(value)
	witnesses, witnessReceipts := witnessClaim(value, statement, directory)
	// A deal bundle's receipt is on the cadence checkpoint its own
	// checkpoint is anchored in; that chain is the bundle's witness claim.
	cadence, cadenceReceipts, anchored := cadenceClaim(value, statement, directory)
	if anchored {
		witnesses, witnessReceipts = cadence, cadenceReceipts
	}
	servedRefusals := 0
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
		if x.Kind == DealProfile && x.Status == "uninterpreted" {
			dealSeen = true
			entry = DealReportEntry(value, result)
			if entry["report"] == "standalone" {
				entry["as_of"] = dealReportAsOf(value, witnesses, anchored)
			}
			if entry["status"] == "fail" {
				claims = append(claims, aacbundle.ClaimResult{Status: "fail", Findings: entry["findings"].([]string)})
			}
		}
		if x.Kind == DealSaleExtension {
			sale, claim := saleBundleEntry(value, result, directory)
			entry = sale
			claims = append(claims, claim)
		}
		if x.Kind == HistorySelectionExtension {
			history, claim := historyEntry(value, result, signatureStates, directory)
			entry = history
			claims = append(claims, claim)
		}
		if x.Composed != nil {
			composed, memberVerdicts, served := composedOutput(value, x.Composed, directory)
			entry["composed"] = composed
			claims = append(claims, aacbundle.ClaimResult{Status: x.Composed.Status, Findings: x.Composed.Findings})
			for _, memberVerdict := range memberVerdicts {
				claims = append(claims, verdictClaim(memberVerdict))
			}
			servedRefusals += served
		}
		extensions = append(extensions, entry)
	}
	// A deal bundle that discloses a sealed report but carries no x-deal-v0
	// extension at all had it removed. A copy whose report is standalone
	// (capsule-cli#294) and that has lost both sections is its steps only:
	// it shows no report text, and nothing here can tell it from a book's
	// own bundle, so it is judged as one (a stated limit).
	if !dealSeen {
		if entry := DealReportEntry(value, result); entry["status"] == "fail" {
			extensions = append(extensions, entry)
			claims = append(claims, aacbundle.ClaimResult{Status: "fail", Findings: entry["findings"].([]string)})
		}
	}
	// The page verdict leaves out the served refusals: each is a withheld
	// claim for the verdict (what was asked for was not served) that PageGate
	// does not hold a page back for.
	pageVerdict := claimsVerdict(claims)
	for range servedRefusals {
		claims = append(claims, aacbundle.ClaimResult{Status: "withheld"})
	}
	verdict := claimsVerdict(claims)
	if !identityOK || !disclosuresOK {
		verdict, pageVerdict = "INVALID", "INVALID"
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
	}, verdict, pageVerdict
}

// claimsVerdict is the verdict of a set of claims: INVALID if any fails,
// INCOMPLETE if any is not a clean pass, else VALID.
func claimsVerdict(claims []aacbundle.ClaimResult) string {
	verdict := "VALID"
	for _, r := range claims {
		if r.Status == "fail" {
			return "INVALID"
		}
		if r.Status != "pass" || len(r.Findings) != 0 {
			verdict = "INCOMPLETE"
		}
	}
	return verdict
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

// bundleElementStart is the start tag of the JSON data element a page built by
// capsule-viewer 0.4.0 or later carries its bundle in (capsule-viewer#109):
// the JSON is data, parsed by a fixed script, so a "__proto__" member stays a
// member. The emitter escapes every "<" in it, so the first "</script>" ends it.
const bundleElementStart = `<script type="application/json">`

// oldBundleScript is where a page built before capsule-viewer 0.4.0 carried
// its bundle: a script assigning an object literal to window.__BUNDLE__.
const oldBundleScript = "window.__BUNDLE__ = "

// receiptBundleJSON returns the Evidence Bundle a self-contained report page
// (a deal receipt.html) embeds, so the page itself can be handed to
// `verify --bundle`. It reads the page's one JSON data element and refuses
// rather than guess: a page with two such elements, or one that has none but
// carries the old window.__BUNDLE__ script (which this no longer reads), is
// an error that says which. A file that is not an HTML page is not one of
// these, and returns (nil, false, nil).
func receiptBundleJSON(raw []byte) ([]byte, bool, error) {
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("<!")) {
		return nil, false, nil
	}
	start := []byte(bundleElementStart)
	i := bytes.Index(raw, start)
	if i < 0 {
		if bytes.Contains(raw, []byte(oldBundleScript)) {
			return nil, false, InputError("--bundle is a page in the old format, with its bundle in a window.__BUNDLE__ script, " +
				"written by capsulectl v0.1.0-rc24 or earlier, or capsule-viewer 0.3.0 or earlier; " +
				"this capsulectl reads the bundle from the page's JSON element: " +
				"verify the bundle file the page was made from, or build the page again")
		}
		return nil, false, nil
	}
	rest := raw[i+len(start):]
	j := bytes.Index(rest, []byte("</script>"))
	if j < 0 {
		return nil, false, InputError("--bundle is a page whose bundle element is never closed")
	}
	if bytes.Contains(rest[j:], start) {
		return nil, false, InputError("--bundle is a page with more than one bundle element: not guessing which one is the bundle")
	}
	return rest[:j], true, nil
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
//
// CheckDealStandaloneReport runs a copy's standalone report checks
// (capsule-cli#294) as verify --bundle does, for a producer or a reader that
// admits a copy without the full verify: a standalone report must pass, and
// a copy that carries report text instead of naming the sealed report it
// should (sealed_report_not_named, the report stripped and text put inline)
// fails. An in-log report's disclosure is checked by the full verify only.
func CheckDealStandaloneReport(value map[string]interface{}) error {
	exts, _ := value["extensions"].(map[string]interface{})
	entry := DealReportEntry(value, aacbundle.VerificationResult{})
	if _, ok := exts[DealReportExtension]; ok && entry["status"] != "pass" {
		return errors.Join(ErrBundleInvalid, errors.New("the copy's standalone report does not verify (sealed_report_unverified)"))
	}
	if findings, _ := entry["findings"].([]string); slices.Contains(findings, "sealed_report_not_named") {
		return errors.Join(ErrBundleInvalid, errors.New("the copy carries report text instead of naming its sealed report (sealed_report_not_named)"))
	}
	return nil
}

func DealReportEntry(value map[string]interface{}, result aacbundle.VerificationResult) map[string]any {
	reports := map[string]bool{}
	records, _ := value["records"].([]interface{})
	for _, raw := range records {
		r, _ := raw.(map[string]interface{})
		if id, _ := r["capsule_id"].(string); id != "" && r["action_id"] == DealReportActionID {
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
	deal, _ := ext[DealProfile].(map[string]interface{})
	fail := func(finding string) map[string]any {
		return map[string]any{"kind": DealProfile, "status": "fail", "findings": []string{finding}}
	}
	id, sealed := deal[DealReportPointer].(string)
	// A standalone report (capsule-cli#294) is named by a pointer-only
	// section; the extension without one, or beside any other section,
	// fails.
	if raw, standalone := ext[DealReportExtension]; standalone {
		x, _ := raw.(map[string]interface{})
		if !sealed || len(deal) != 1 || x == nil {
			return fail("sealed_report_unverified")
		}
		return standaloneReportEntry(value, x, id)
	}
	switch {
	case sealed && (!matched[id] || len(disclosed) != 1):
		return fail("sealed_report_unverified")
	case sealed:
		return map[string]any{"kind": DealProfile, "status": "pass", DealReportPointer: id}
	case len(deal) == 2 && deal["audience"] == DealAudienceHistory && deal["deal_id"] != nil && len(disclosed) == 0:
		// A history copy (deal history) withholds every payload and carries
		// no report text, so naming no sealed report is not a downgrade.
		return map[string]any{
			"kind": DealProfile, "status": "uninterpreted", "findings": []string{"extension_unbound"},
			"note": "a history copy: every record's payload is withheld and the section carries no report text",
		}
	case len(reports) > 0:
		// A bundle that holds a sealed report was written after reports were
		// sealed: text in the extension instead of a pointer is a downgrade
		// (the record kept, its disclosure dropped, edited text inline).
		return fail("sealed_report_not_named")
	}
	if _, sale := ext[DealSaleExtension]; sale {
		return map[string]any{
			"kind": DealProfile, "status": "uninterpreted", "findings": []string{"extension_unbound"},
			"note": "a sale's section: its sale_threads openings are checked against the sealed registrations under " + DealSaleExtension,
		}
	}
	// A deal on a book sealed its reports in its log until reports moved off
	// it (capsule-cli#294), so a copy of a book deal whose log holds none
	// names a standalone report: text in the section instead is a stripped
	// copy. Without an x-deal-v0 section at all, a copy is steps only and
	// shows no report text, which a book's own bundle may be.
	if deal != nil && holdsDealSteps(value) {
		return fail("sealed_report_not_named")
	}
	return map[string]any{
		"kind": DealProfile, "status": "uninterpreted", "findings": []string{"extension_unbound"},
		"note": "written by the producer when the bundle was made and sealed by no record: its text is not verified, and editing it does not change the verdict",
	}
}
