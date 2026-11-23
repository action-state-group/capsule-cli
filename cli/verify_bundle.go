package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
// bytes are returned when the claim passes, else nil, for the witness check against it.
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
	return VerifyBundleFileWith(c, path, nil)
}

// VerifyBundleFileWith is verifyBundleFile with an optional factory of readers
// for the extensions the core does not read (see ExtensionReader): a caller's
// own verb reuses the page-file extraction, the exit codes and the report.
// The core's verb passes nil.
func VerifyBundleFileWith(c *cobra.Command, path string, newReader ExtensionReaderFactory) error {
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
	verdict, pageVerdict, report, err := AssessBundle(value, directory, newReader)
	if err != nil {
		return err
	}
	if err := Output(c, report); err != nil {
		return err
	}
	if verdict == "INCOMPLETE" && pageVerdict == "VALID" {
		return errServedRefusalsOnly{}
	}
	return verdictError(verdict)
}

// errServedRefusalsOnly is ErrPartial for a bundle that is INCOMPLETE only
// because composed/v1 refusal members are served answers (pageVerdict VALID,
// capsule-cli#513): everything else checked, so the say line can tell it from
// a file that could not be checked. It reads and exits as ErrPartial.
type errServedRefusalsOnly struct{}

func (errServedRefusalsOnly) Error() string { return ErrPartial.Error() }
func (errServedRefusalsOnly) Unwrap() error { return ErrPartial }

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
// only when pageVerdict (bundleAssessment) is VALID, which differs from this
// verdict only for composed/v1 refusals served by the observer's own key (a
// refused case the page draws), so a page claims no more than verify
// --bundle does about the bundle it carries.
func BundleVerdict(value map[string]interface{}, directory *witness.Directory) (string, map[string]any) {
	verdict, _, report := bundleAssessment(value, &assessEnv{directory: directory})
	return verdict, report
}

// bundleAssessment is BundleVerdict plus pageVerdict: the verdict with every
// composed/v1 refusal member whose signature verifies read as the observer's
// served answer instead of INCOMPLETE. It is VALID exactly when the only
// thing keeping the bundle from VALID is such refusals, which a deal page
// draws as refused cases (PageGate); otherwise it equals verdict. It reads
// served refusals of the top-level composed block only: a composed member's
// own bundle is assessed for its verdict alone.
func bundleAssessment(value map[string]interface{}, env *assessEnv) (verdict, pageVerdict string, report map[string]any) {
	result := aacbundle.VerifyBundleWithOptions(value, bundleOptions)
	report, verdict, pageVerdict = assessBundle(value, result, env)
	return verdict, pageVerdict, report
}

// The reading a bundle verdict names in checked_against: parts of AAC's strict
// reading, which goes beyond Disclosure Bundle -01 (capsule-cli#456). The
// values are a closed set the README lists.
const (
	CheckedAgainstChecks = "aac-strict"
	CheckedAgainstDraft  = "draft-mih-zhang-agent-disclosure-bundle-01"
)

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
func assessBundle(value map[string]interface{}, result aacbundle.VerificationResult, env *assessEnv) (map[string]any, string, string) {
	directory := env.directory
	strict := aacbundle.VerifyStrict(value, aacbundle.StrictOptions{Options: bundleOptions, WitnessDirectory: directory})
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
	categoryCoverage, coverageClaim := categoryCoverageClaim(strict)
	servedRefusals := 0
	claims := []aacbundle.ClaimResult{result.GraphClosure, signedCheckpoint, result.IntervalCoverage, result.PerRecordMembership, signatures}
	// The bundle draft defines no witness member: only a failing receipt
	// changes the verdict, and a file without one is judged as before.
	if witnesses.Status == "fail" {
		claims = append(claims, witnesses)
	}
	if coverageClaim.Status == "fail" {
		claims = append(claims, coverageClaim)
	}
	extensions := make([]map[string]any, 0, len(result.Extensions))
	for _, x := range result.Extensions {
		entry := map[string]any{"kind": x.Kind, "status": x.Status}
		if x.Kind == aacbundle.ParentLogWitnessKind && strict.ParentWitness != nil {
			// The neutral witness chain (Evidence Bundle -03), as AAC's strict
			// reading computed it against the witness directory this run was
			// given: its claim and the extent it covers, never read from the
			// block (capsule-cli#608).
			parent := *strict.ParentWitness
			entry["status"] = parent.Status
			entry["parent_witness"] = parentWitnessReport(parent)
			// As for the checkpoint's own receipts, only a failing chain
			// changes the verdict (a withheld one is shown, not gating).
			if parent.Status == "fail" {
				claims = append(claims, aacbundle.ClaimResult{Status: parent.Status, Findings: parent.Findings})
			}
		}
		if x.Composed != nil {
			composed, memberVerdicts, served := composedOutput(value, x.Composed, env)
			entry["composed"] = composed
			claims = append(claims, aacbundle.ClaimResult{Status: x.Composed.Status, Findings: x.Composed.Findings})
			for _, memberVerdict := range memberVerdicts {
				claims = append(claims, verdictClaim(memberVerdict))
			}
			servedRefusals += served
		}
		extensions = append(extensions, entry)
	}
	// An optional reader of the extensions the core does not read runs once
	// the core has finished its own reading, so nothing it does can change
	// what the core found (see ExtensionReader).
	var readerWitnesses map[string]any
	var readerReport []map[string]any
	if reader := env.reader(); reader != nil {
		ctx := &ExtensionContext{Value: value, Result: result, SignatureStates: signatureStates, Directory: directory, Statement: statement, Witnesses: witnesses, Receipts: witnessReceipts}
		extensions, claims, readerWitnesses, readerReport = applyReader(env, reader, ctx, result.Extensions, extensions, claims)
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
	report := map[string]any{
		"verdict":               verdict,
		"checked_against":       map[string]string{"checks": CheckedAgainstChecks, "bundle_draft": CheckedAgainstDraft},
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
	}
	// Printed only for a bundle that carries the member, so no other
	// bundle's output changes.
	if categoryCoverage != nil {
		report["category_coverage"] = categoryCoverage
	}
	if readerWitnesses != nil {
		report["reader_witnesses"] = readerWitnesses
	}
	if len(readerReport) > 0 {
		report["reader_claims"] = readerReport
	}
	return report, verdict, pageVerdict
}

// categoryCoverageClaim is AAC strict's category_coverage reading
// (capsule-cli#556): the five properties as printed (nil when the bundle has
// no such member), and a claim that fails when a property is FAIL and strict's
// verdict is INVALID. AAC does not export its gating set, so the CLI keeps no
// copy that could drift from it (capsule-cli#558). The claim only ever adds
// the INVALID strict already reached, so it can never make the verdict
// stricter than AAC's; it is needed because the CLI keeps its own claim list
// and a failure strict reached through the member must not depend on the others matching.
func categoryCoverageClaim(strict aacbundle.StrictResult) (map[string]string, aacbundle.ClaimResult) {
	if strict.CategoryCoverage == nil {
		return nil, aacbundle.ClaimResult{Status: "pass"}
	}
	properties := map[string]string{}
	var findings []string
	for _, property := range strict.CategoryCoverage {
		properties[property.Name] = property.Value
		if property.Value == aacbundle.CoverageFail {
			findings = append(findings, property.Name+" is FAIL")
		}
	}
	if findings == nil || strict.Verdict != aacbundle.VerdictInvalid {
		return properties, aacbundle.ClaimResult{Status: "pass"}
	}
	return properties, aacbundle.ClaimResult{Status: "fail", Findings: findings}
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

// parentWitnessReport is a parent-log-witness/v1 result in the report's
// shape: its status, findings and receipts, and the covered extent when it
// passed (whole when entries equals of).
func parentWitnessReport(r aacbundle.ParentWitnessResult) map[string]any {
	out := map[string]any{"status": r.Status, "findings": nonNilStrings(r.Findings), "receipts": receiptEntries(r.Receipts)}
	if r.Extent != nil {
		out["extent"] = map[string]any{"entries": r.Extent.Entries, "of": r.Extent.Of, "whole": r.Extent.Entries == r.Extent.Of}
	}
	return out
}
