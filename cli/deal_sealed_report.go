package cli

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/disclosure"
	"github.com/action-state-group/agent-action-capsule/go/envelope"
)

// dealReportActionID is the action_id of the capsule a deal report seals its
// copy's readable text in: an fyi capsule on the deal's own log whose input
// is the deal_report record (dealSealReport). The bundle's x-deal-v0
// extension then only points at it (dealReportPointer), so the text a page
// shows is a disclosed record payload, checked like every other: an edited
// line makes the bundle INVALID.
const DealReportActionID = "capsulectl-deal-report"

// dealReportPointer is the x-deal-v0 extension member naming the copy's
// sealed report capsule.
const DealReportPointer = "sealed_report"

// DealReportExtension carries a copy's standalone report (capsule-cli#294):
// {"record": the report's Capsule as a bundle record carries one, with its
// signature and key_id; "agent_input": its payload}. The report is on no
// log; it binds the deal checkpoint the copy is made at (deal_checkpoint).
const DealReportExtension = "x-deal-report/v0"

// dealStepRecordTypes are the evidence record header types of a deal's
// steps on its book: deal_step, and the Personal-Agent History Profile's
// step_completed and step_failed (capsule-cli#264), which the plugin's
// readers take as steps too.
var dealStepRecordTypes = map[string]bool{"deal_step": true, "step_completed": true, "step_failed": true}

// DealSealedReport is a copy's report text and how the copy carries it:
// "in_log" (a deal_report record of the bundle, disclosed), "standalone"
// (x-deal-report/v0, named by the pointer; capsule-cli#294), or "inline"
// (the x-deal-v0 section itself, as before reports were sealed). The text
// is returned as the copy carries it; `verify --bundle` checks it
// (DealReportEntry). A standalone section the pointer does not name gives
// no text.
func DealSealedReport(b map[string]interface{}) (report map[string]interface{}, id, form string) {
	exts, _ := b["extensions"].(map[string]interface{})
	deal, _ := exts[DealProfile].(map[string]interface{})
	id, sealed := deal[DealReportPointer].(string)
	if !sealed {
		return deal, "", "inline"
	}
	if x, ok := exts[DealReportExtension].(map[string]interface{}); ok {
		record, _ := x["record"].(map[string]interface{})
		if record["capsule_id"] != id {
			return nil, id, "standalone"
		}
		input, _ := x["agent_input"].(map[string]interface{})
		report, _ = input["report"].(map[string]interface{})
		return report, id, "standalone"
	}
	disclosures, _ := b["disclosures"].(map[string]interface{})
	d, _ := disclosures[id].(map[string]interface{})
	input, _ := d["agent_input"].(map[string]interface{})
	report, _ = input["report"].(map[string]interface{})
	return report, id, "in_log"
}

// standaloneReportEntry checks a copy's standalone report (capsule-cli#294)
// for verify --bundle, against the outer bundle it rides in:
//  1. its record parses, its id is the pointer, it is a deal report, and
//     its producer signature verifies under its key_id;
//  2. that key is the verified key of the deal's last step: the record with
//     the highest log position whose header type is a step's;
//  3. its agent_input disclosure matches the Capsule's committed digest;
//  4. its payload is a deal_report of the bundle's deal, for a copy audience
//     (a v1 report names its book by rests_on.own.log_id, a v0 report by
//     its deal_id, which is the log id's suffix);
//  5. its deal_checkpoint is the bundle's checkpoint (log id, MMR size, root);
//  6. the section holds nothing else, and the bundle holds no deal_report;
//  7. its lifecycle's as_of is the checkpoint's signed time; a v1 report
//     (profile is x-deal-v1) has no lifecycle, so its own as_of member is
//     instead RFC 3339 UTC to the second, as the profile's BuildReport
//     writes it: the time the holder computed the report at, which no
//     witness attests and which is not the checkpoint's time.
//
// profile is the version the extension naming the report is: DealProfile or
// DealProfileV1. The payload's profile member must be the same.
func standaloneReportEntry(value map[string]interface{}, x map[string]interface{}, id, profile string, section map[string]interface{}) map[string]any {
	fail := map[string]any{"kind": profile, "status": "fail", "findings": []string{"sealed_report_unverified"}}
	record, _ := x["record"].(map[string]interface{})
	input, _ := x["agent_input"].(map[string]interface{})
	if len(x) != 2 || record == nil || input == nil || record["capsule_id"] != id || record["action_id"] != DealReportActionID {
		return fail
	}
	key, ok := verifiedProducerKey(record)
	if !ok || key != DealLastStepKey(value) {
		return fail
	}
	if checked := agentInputDisclosure(record, input); !checked.OK() || checked.DisclosuresMatched() != 1 {
		return fail
	}
	cert, _ := value["completeness_certificate"].(map[string]interface{})
	logID, _ := cert["log_id"].(string)
	dealID, isDeal := strings.CutPrefix(logID, "deal/")
	audience, _ := input["audience"].(string)
	if input["type"] != "deal_report" || input["profile"] != profile || !isDeal || !dealCopyAudiences[audience] {
		return fail
	}
	if profile == DealProfileV1 {
		// A v1 book's log id is random, never derived from the deal id
		// (design note 461 §3.1), so the report binds to the book by the log
		// it rests on; deal_id is only the report's own name for the deal.
		restsOn, _ := input["rests_on"].(map[string]interface{})
		own, _ := restsOn["own"].(map[string]interface{})
		if id, _ := input["deal_id"].(string); id == "" || own["log_id"] != logID {
			return fail
		}
	} else if input["deal_id"] != dealID {
		return fail
	}
	cp, _ := value["checkpoint"].(map[string]interface{})
	bound, _ := input["deal_checkpoint"].(map[string]interface{})
	boundSize, sizeErr := JSONUint(bound["mmr_size"])
	cpSize, cpErr := JSONUint(cp["mmr_size"])
	if len(bound) != 3 || bound["log_id"] != logID || sizeErr != nil || cpErr != nil || boundSize != cpSize || bound["root"] != cp["root"] {
		return fail
	}
	records, _ := value["records"].([]interface{})
	for _, raw := range records {
		if r, _ := raw.(map[string]interface{}); r["action_id"] == DealReportActionID {
			return fail
		}
	}
	if profile == DealProfileV1 {
		asOf, _ := input["as_of"].(string)
		t, err := time.Parse(time.RFC3339, asOf)
		if err != nil || t.UTC().Format(time.RFC3339) != asOf {
			return fail
		}
		if dealV1ReportVerifier != nil {
			// The deal plugin registered the profile's report rule
			// (SetDealV1ReportVerifier): the pass below rests on it.
			entry := map[string]any{"kind": profile, "status": "pass", DealReportPointer: id, "report": "standalone", "as_of": dealReportAsOfV1(asOf)}
			if err := runDealV1ReportRule(record, input, section); err != nil {
				entry["status"] = "fail"
				entry["findings"] = []string{DealV1ReportRuleFailed}
				entry["note"] = "the x-deal-v1 report rule failed: " + err.Error()
			}
			return entry
		}
		// What this verifier checked is the neutral part: the bindings, the
		// signature, the profile and the as_of form. The profile's own report
		// rule (xdealv1.VerifyReport) is EvidenceBook's, which the core may not
		// import, so it is reported as not checked and the bundle INCOMPLETE
		// until the deal plugin runs it.
		return map[string]any{
			"kind": profile, "status": "pass", DealReportPointer: id, "report": "standalone", "as_of": dealReportAsOfV1(asOf),
			"findings": []string{DealV1ReportRuleNotChecked},
			"note":     "NOT_CHECKED: the x-deal-v1 report rule (xdealv1.VerifyReport: the report's structure, its rows' rulings and its first record) is run by the deal plugin (capsulectl-deal), not by this verifier",
		}
	}
	report, _ := input["report"].(map[string]interface{})
	lifecycle, _ := report["lifecycle"].(map[string]interface{})
	asOf, asOfErr := time.Parse(time.RFC3339Nano, fmt.Sprint(lifecycle["as_of"]))
	signed, signedErr := time.Parse(time.RFC3339Nano, fmt.Sprint(cp["timestamp"]))
	if asOfErr != nil || signedErr != nil || !asOf.Equal(signed) {
		return fail
	}
	return map[string]any{"kind": profile, "status": "pass", DealReportPointer: id, "report": "standalone"}
}

// DealV1ReportRuleNotChecked is the finding a passing v1 report carries: the
// profile's report rule is not run by the core.
const DealV1ReportRuleNotChecked = "report_rule_not_checked"

// dealReportAsOfV1 words a v1 report's as_of: the time the holder computed
// the report at, which is the producer's own and which no witness attests
// (evidencebook design note 461-deal-profile.md §9, "as_of"). It is not the
// checkpoint's time, so no witness claim is read into it.
func dealReportAsOfV1(asOf string) string {
	return "as of " + asOf + " (the holder's time, when it computed the report; no witness attests it)"
}

// dealReportAsOf says when a standalone report's deal state is from, and what
// backs it (capsule-cli#294): the bound checkpoint's signed time, which is
// the deal's last step's when that step's record states the same time, and
// separately whether a witness saw that very checkpoint. No time is taken
// from a cadence tick or from the report's own Capsule.
func dealReportAsOf(value map[string]interface{}, witnesses aacbundle.ClaimResult, anchored bool) string {
	cp, _ := value["checkpoint"].(map[string]interface{})
	at, err := time.Parse(time.RFC3339Nano, fmt.Sprint(cp["timestamp"]))
	if err != nil {
		return "as of an unreadable checkpoint time"
	}
	basis := "the deal's checkpoint"
	if last := lastStepRecord(value); last != nil {
		header, _ := last["evidence_record_header"].(map[string]interface{})
		if stepAt, err := time.Parse(time.RFC3339Nano, fmt.Sprint(header["committed_at"])); err == nil && stepAt.Equal(at) {
			basis = "the deal's last step"
		}
	}
	witnessed := "not witnessed"
	chain := CadenceChainOf(value)
	switch {
	case witnesses.Status == "fail":
		witnessed = "its witness claim failed"
	case anchored && witnesses.Status == "pass" && chain["extent"] != "part":
		witnessed = "witnessed"
	}
	return fmt.Sprintf("as of %s at %s (the producer's time); %s", basis, at.UTC().Format("2006-01-02T15:04:05Z"), witnessed)
}

// dealCopyAudiences are the audiences a copy's report is made for.
var dealCopyAudiences = map[string]bool{DealAudienceCounterparty: true, DealAudienceAdjudicator: true, DealAudienceKeep: true}

// verifiedProducerKey is a record's producer key, hex, when its signature
// verifies under its own key_id.
func verifiedProducerKey(record map[string]interface{}) (string, bool) {
	id, _ := record["capsule_id"].(string)
	sig, _ := record["signature"].(string)
	key, _ := record["key_id"].(string)
	envelopeBytes, sigErr := hex.DecodeString(sig)
	keyBytes, keyErr := hex.DecodeString(key)
	if sigErr != nil || keyErr != nil || id == "" {
		return "", false
	}
	result := envelope.Verify(id, envelopeBytes)
	if !result.OK || !bytes.Equal(result.PublicKey, keyBytes) {
		return "", false
	}
	return key, true
}

// DealLastStepKey is the verified producer key (hex) of a deal's last step
// in a bundle: the record at the highest log position whose evidence record
// header is a step's. "" when there is none, or it does not verify. A
// standalone report must be signed by it (capsule-cli#294).
func DealLastStepKey(value map[string]interface{}) string {
	last := lastStepRecord(value)
	if last == nil {
		return ""
	}
	key, ok := verifiedProducerKey(last)
	if !ok {
		return ""
	}
	return key
}

// lastStepRecord is a deal's last step in a bundle: the record at the
// highest log position whose evidence record header is a step's.
func lastStepRecord(value map[string]interface{}) map[string]interface{} {
	cert, _ := value["completeness_certificate"].(map[string]interface{})
	memberships, _ := cert["memberships"].(map[string]interface{})
	records, _ := value["records"].([]interface{})
	var last map[string]interface{}
	var lastSeq uint64
	for _, raw := range records {
		r, _ := raw.(map[string]interface{})
		header, _ := r["evidence_record_header"].(map[string]interface{})
		rt, _ := header["record_type"].(string)
		if !dealStepRecordTypes[rt] {
			continue
		}
		id, _ := r["capsule_id"].(string)
		m, _ := memberships[id].(map[string]interface{})
		coords, _ := m["log_coordinates"].(map[string]interface{})
		seq, err := JSONUint(coords["seq"])
		if err == nil && seq > lastSeq {
			last, lastSeq = r, seq
		}
	}
	return last
}

// holdsDealSteps is whether a bundle's records include a deal step on a
// book (an evidence record header of a step's type).
func holdsDealSteps(value map[string]interface{}) bool {
	records, _ := value["records"].([]interface{})
	for _, raw := range records {
		r, _ := raw.(map[string]interface{})
		header, _ := r["evidence_record_header"].(map[string]interface{})
		if rt, _ := header["record_type"].(string); dealStepRecordTypes[rt] {
			return true
		}
	}
	return false
}

// agentInputDisclosure checks input against the agent_input the record's
// Capsule commits (its digest).
func agentInputDisclosure(record, input map[string]interface{}) disclosure.Result {
	capsule := make(map[string]interface{}, len(record))
	for k, v := range record {
		if k != "signature" && k != "key_id" {
			capsule[k] = v
		}
	}
	return disclosure.Verify(map[string]interface{}{"capsule": capsule, "disclosures": map[string]interface{}{"agent_input": input}}, nil)
}

// DealV1ReportRuleFailed is the finding of a v1 report the deal plugin's
// report rule refused.
const DealV1ReportRuleFailed = "report_rule_failed"

// dealV1ReportVerifier is the deal plugin's check of a v1 report (its
// registration is SetDealV1ReportVerifier); nil in a process without it.
var dealV1ReportVerifier func(report, ext []byte) error

// SetDealV1ReportVerifier registers the x-deal-v1 report rule for
// `verify --bundle` (capsule-cli#587). The core may not import EvidenceBook, so
// the deal plugin supplies it: report is the standalone report pair
// {"record": the report Capsule with signature and key_id, "agent_input": its
// payload}, and ext is the bundle's x-deal-v1 extension exactly as the bundle
// carries it ({sealed_report, book: {agreement, relied_on}}, design note
// 461-deal-profile.md §9 and PROFILE §10), which holds the seq 1 agreement the
// report is read against. The core reads none of the book: the verifier must
// verify the report and every pair in it (xdealv1.VerifyReportBundle), and a
// non-nil error fails the report. With none registered a passing v1 report
// stays report_rule_not_checked. A plugin calls it before building its tree.
func SetDealV1ReportVerifier(verify func(report, ext []byte) error) { dealV1ReportVerifier = verify }

// runDealV1ReportRule hands the registered verifier the report pair and the
// bundle's x-deal-v1 extension.
func runDealV1ReportRule(record, input, section map[string]interface{}) error {
	report, err := json.Marshal(map[string]interface{}{"record": record, "agent_input": input})
	if err != nil {
		return err
	}
	ext, err := json.Marshal(section)
	if err != nil {
		return err
	}
	return dealV1ReportVerifier(report, ext)
}
