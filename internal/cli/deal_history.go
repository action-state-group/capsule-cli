package cli

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/disclosure"
	"github.com/action-state-group/agent-action-capsule/go/envelope"
	"github.com/action-state-group/agent-action-capsule/go/witness"
)

// historySelectionExtension carries a History: deals the user picked, each
// as its own deal bundle, in the Evidence Bundle of a history log of the
// History's own (historyLogPrefix). Each deal has its own log, so the picked
// deals cannot share one checkpoint; the History's log instead seals one
// selection record naming who it is for, the picked deals and their bundles'
// digests, signed by the user's key, and the extension carries the deals:
//
//	{"selection_record": <the selection record's capsule id>,
//	 "selection": [{"deal_id": ..., "bundle_digest": <sha256 of the bundle's JCS>,
//	                "steps_after_checkpoint": <steps on the deal's log after the copy's checkpoint>}],
//	 "deals": {"<deal id>": <the deal's copy for the selected audience (history, or keep), an evidence-bundle/v2>}}
//
// It is a private kind, like x-deal-sale/v0, read by this verifier only. A
// History shows that the holder of one key picked these copies and that
// every record in them is signed by that key. A record's signature is
// outside its Capsule ID, so that key is not bound to who sealed the deals.
const HistorySelectionExtension = "x-history-selection/v0"

// historyLogPrefix names a History's own log, historyLogPrefix + the
// selection record's nonce: one log and one selection record per History,
// so a History carries nothing of any other (how many came before, when,
// under which key). No deal's log is ever written by `deal history`.
const HistoryLogPrefix = "deal-history/"

// historyActionID is the action_id of a selection record's capsule.
const HistoryActionID = "capsulectl-deal-history"

// dealAudienceHistory is the copy of a deal for someone who was not in it,
// the only copy a History is sent with: every record's payload is withheld,
// so each record shows only what its capsule states (its time, and for an
// act its effect's status). A deal's outcome and its acts' action classes
// are in payloads, beside the item, amounts and the other party's details,
// and a payload is disclosed whole or not at all: a History does not prove
// them (historyNotProven).
const DealAudienceHistory = "history"

// historyHolderStated are the reads of a History that are the holder's own
// word, signed with the selection and checked against no deal: who it is
// for, and how many steps each deal has after its copy's checkpoint.
var historyHolderStated = []string{"recipient", "steps_after_checkpoint"}

// historyNotProven are the reads a history copy does not prove, as verify
// reports them on every History for the history audience.
var historyNotProven = []string{"outcome", "action_class"}

var hexDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// historyEntry is `verify --bundle`'s reading of a History's extension. It
// passes only when the selection record is disclosed and matches, signed,
// on the History's own log, and names a recipient when sent (history);
// the carried selection is the sealed one; each selected deal is carried,
// is the bundle its digest names, is of that deal's own log, is the copy
// for the sealed audience and verifies VALID as its own Evidence Bundle;
// nothing unselected is carried; and every nested record is signed by the
// selection record's key (holder_key), the one key the deals share. A
// finding names the deal it is about.
// signatures is each record's producer-signature verdict by capsule id, as
// AAC strict gives it (verify --bundle's record_signatures).
func historyEntry(value map[string]interface{}, result aacbundle.VerificationResult, signatures map[string]string, directory *witness.Directory) (map[string]any, aacbundle.ClaimResult) {
	var failed, notShown []string
	exts, _ := value["extensions"].(map[string]interface{})
	ext, _ := exts[HistorySelectionExtension].(map[string]interface{})
	recordID, _ := ext["selection_record"].(string)
	carried, _ := ext["deals"].(map[string]interface{})
	entry := map[string]any{"kind": HistorySelectionExtension}

	// The selection record: disclosed, matching its seal, signed.
	matched := false
	for _, d := range result.Disclosures {
		if d.CapsuleID == recordID && d.Member == "agent_input" && d.Status == disclosure.Match {
			matched = true
		}
	}
	disclosures, _ := value["disclosures"].(map[string]interface{})
	d, _ := disclosures[recordID].(map[string]interface{})
	sealed, _ := d["agent_input"].(map[string]interface{})
	rawRecords, _ := value["records"].([]interface{})
	holder := ""
	for _, raw := range rawRecords {
		r, _ := raw.(map[string]interface{})
		if r["capsule_id"] == recordID && r["action_id"] == HistoryActionID {
			holder, _ = r["key_id"].(string)
		}
	}
	audience, _ := sealed["audience"].(string)
	switch {
	case !matched || sealed["type"] != "history_selection" || sealed["profile"] != HistorySelectionExtension:
		failed = append(failed, "selection_record_unverified")
	case holder == "" || signatures[recordID] != envelope.Authored:
		failed = append(failed, "selection_record_unsigned")
	}
	if value["root"] != recordID {
		failed = append(failed, "selection_record_not_root")
	}
	nonce, _ := sealed["nonce"].(string)
	if cert, _ := value["completeness_certificate"].(map[string]interface{}); !hexDigestPattern.MatchString(nonce) || cert["log_id"] != HistoryLogPrefix+nonce {
		failed = append(failed, "not_a_history_log")
	}
	if !sameJSONValue(ext["selection"], sealed["selection"]) {
		failed = append(failed, "selection_not_sealed")
	}
	recipient, _ := sealed["recipient"].(string)
	entry["holder_key"], entry["audience"], entry["holder_stated"] = holder, audience, historyHolderStated
	if recipient != "" {
		entry["recipient"] = recipient
	}
	switch audience {
	case DealAudienceHistory:
		entry["not_proven"] = historyNotProven
		if strings.TrimSpace(recipient) == "" {
			failed = append(failed, "selection_recipient_missing")
		}
	case DealAudienceKeep:
	default:
		failed = append(failed, "selection_audience_not_history")
	}

	listed, _ := sealed["selection"].([]interface{})
	if len(listed) == 0 {
		failed = append(failed, "selection_missing")
	}
	picked := map[string]string{} // deal id -> bundle digest
	later := map[string]int64{}   // deal id -> steps after its checkpoint, as the holder sealed it
	var order []string
	for _, raw := range listed {
		e, _ := raw.(map[string]interface{})
		id, _ := e["deal_id"].(string)
		digest, _ := e["bundle_digest"].(string)
		steps, _ := e["steps_after_checkpoint"].(json.Number)
		count, ok := stepCount(steps)
		if _, dup := picked[id]; dup || !ok || !DealIDPattern.MatchString(id) || !hexDigestPattern.MatchString(digest) {
			failed = append(failed, "selection_entry_malformed:"+id)
			continue
		}
		picked[id], later[id] = digest, count
		order = append(order, id)
	}
	extra := make([]string, 0)
	for id := range carried {
		if _, ok := picked[id]; !ok {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	signers := map[string][]string{} // key -> deals carried with a record signed by it
	for _, id := range extra {
		failed = append(failed, "deal_not_in_selection:"+id)
		tb, _ := carried[id].(map[string]interface{})
		keys, _ := recordKeys(tb)
		for _, key := range keys {
			signers[key] = append(signers[key], id)
		}
	}
	deals := make([]map[string]any, 0, len(order))
	for _, id := range order {
		member := map[string]any{"deal_id": id, "steps_after_checkpoint": later[id]}
		deals = append(deals, member)
		tb, ok := carried[id].(map[string]interface{})
		if !ok {
			failed = append(failed, "deal_missing:"+id)
			member["verdict"] = "INVALID"
			continue
		}
		if tb["bundle_kind"] != "evidence-bundle/v2" || tb["bundle_version"] != "2" {
			failed = append(failed, "deal_not_a_bundle:"+id)
			member["verdict"] = "INVALID"
			continue
		}
		verdict, report := BundleVerdict(tb, directory)
		member["verdict"], member["report"] = verdict, report
		if JCSDigest(tb) != picked[id] {
			failed = append(failed, "deal_bundle_not_the_selected_one:"+id)
		}
		if _, statement := CheckpointClaim(tb); statement == nil {
			failed = append(failed, "deal_log_unbound:"+id)
		} else if signed, err := SignedCheckpoint(statement); err != nil || signed["log_id"] != DealLogID(id) {
			failed = append(failed, "deal_not_of_its_log:"+id)
		}
		copyFor, _ := sealedOrInlineReport(tb)["audience"].(string)
		member["audience"] = copyFor
		if copyFor != audience {
			failed = append(failed, "deal_audience_not_selected:"+id)
		}
		if audience == DealAudienceHistory && PayloadDisclosed(tb) {
			failed = append(failed, "deal_payload_disclosed:"+id)
		}
		keys, unsigned := recordKeys(tb)
		if unsigned {
			failed = append(failed, "deal_record_unsigned:"+id)
		}
		for _, key := range keys {
			signers[key] = append(signers[key], id)
			if key != holder {
				failed = append(failed, "deal_signed_by_other_key:"+id)
			}
		}
		switch verdict {
		case "VALID":
		case "INCOMPLETE":
			notShown = append(notShown, "deal_incomplete:"+id)
		default:
			failed = append(failed, "deal_invalid:"+id)
		}
	}
	if len(signers) > 1 {
		failed = append(failed, "deal_signers_differ")
	}
	for _, ids := range signers {
		sort.Strings(ids)
	}
	entry["signer_keys"], entry["deals"] = signers, deals
	status, claim := "pass", aacbundle.ClaimResult{Status: "pass"}
	all := nonNilStrings(append(failed, notShown...))
	switch {
	case len(failed) > 0:
		status, claim = "fail", aacbundle.ClaimResult{Status: "fail", Findings: all}
	case len(notShown) > 0:
		status, claim = "incomplete", aacbundle.ClaimResult{Status: "not_shown", Findings: all}
	}
	entry["status"], entry["findings"] = status, all
	return entry, claim
}

// stepCount reads a sealed steps_after_checkpoint, and whether it is a
// whole number of steps (not negative).
func stepCount(n json.Number) (int64, bool) {
	count, err := n.Int64()
	return count, err == nil && count >= 0
}

// recordKeys are the keys a bundle's records name (key_id), sorted, and
// whether any record names none.
func recordKeys(b map[string]interface{}) ([]string, bool) {
	seen := map[string]bool{}
	unsigned := false
	records, _ := b["records"].([]interface{})
	for _, raw := range records {
		r, _ := raw.(map[string]interface{})
		key, _ := r["key_id"].(string)
		if key == "" {
			unsigned = true
			continue
		}
		seen[key] = true
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, unsigned
}

// payloadDisclosed is whether a bundle discloses any record's payload.
func PayloadDisclosed(b map[string]interface{}) bool {
	disclosures, _ := b["disclosures"].(map[string]interface{})
	for _, raw := range disclosures {
		d, _ := raw.(map[string]interface{})
		if len(d) > 0 {
			return true
		}
	}
	return false
}
