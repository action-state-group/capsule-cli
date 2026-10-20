package cli

import (
	"bytes"
	"encoding/hex"
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
//  4. its payload is a deal_report of the bundle's deal, for a copy audience;
//  5. its deal_checkpoint is the bundle's checkpoint (log id, MMR size, root);
//  6. the section holds nothing else, and the bundle holds no deal_report;
//  7. its lifecycle's as_of is the checkpoint's signed time.
func standaloneReportEntry(value map[string]interface{}, x map[string]interface{}, id string) map[string]any {
	fail := map[string]any{"kind": DealProfile, "status": "fail", "findings": []string{"sealed_report_unverified"}}
	record, _ := x["record"].(map[string]interface{})
	input, _ := x["agent_input"].(map[string]interface{})
	if len(x) != 2 || record == nil || input == nil || record["capsule_id"] != id || record["action_id"] != DealReportActionID {
		return fail
	}
	key, ok := verifiedProducerKey(record)
	if !ok || key != DealLastStepKey(value) {
		return fail
	}
	capsule := make(map[string]interface{}, len(record))
	for k, v := range record {
		if k != "signature" && k != "key_id" {
			capsule[k] = v
		}
	}
	checked := disclosure.Verify(map[string]interface{}{"capsule": capsule, "disclosures": map[string]interface{}{"agent_input": input}}, nil)
	if !checked.OK() || checked.DisclosuresMatched() != 1 {
		return fail
	}
	cert, _ := value["completeness_certificate"].(map[string]interface{})
	logID, _ := cert["log_id"].(string)
	dealID, isDeal := strings.CutPrefix(logID, "deal/")
	audience, _ := input["audience"].(string)
	if input["type"] != "deal_report" || input["profile"] != DealProfile || !isDeal || input["deal_id"] != dealID || !dealCopyAudiences[audience] {
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
	report, _ := input["report"].(map[string]interface{})
	lifecycle, _ := report["lifecycle"].(map[string]interface{})
	asOf, asOfErr := time.Parse(time.RFC3339Nano, fmt.Sprint(lifecycle["as_of"]))
	signed, signedErr := time.Parse(time.RFC3339Nano, fmt.Sprint(cp["timestamp"]))
	if asOfErr != nil || signedErr != nil || !asOf.Equal(signed) {
		return fail
	}
	return map[string]any{"kind": DealProfile, "status": "pass", DealReportPointer: id, "report": "standalone"}
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
