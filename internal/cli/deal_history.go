package cli

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/agent-action-capsule/go/disclosure"
	emit "github.com/action-state-group/capsule-emit-go"
	"github.com/spf13/cobra"
)

// historySelectionExtension carries a History: deals the user picked, each
// as its own deal bundle, in the Evidence Bundle of the user's own history
// log (historyLogID). Each deal has its own log, so the picked deals cannot
// share one checkpoint; the history log instead seals one selection record
// naming the picked deals and their bundles' digests, signed by the user's
// key, and the extension carries the deals:
//
//	{"selection_record": <the selection record's capsule id>,
//	 "selection": [{"deal_id": ..., "bundle_digest": <sha256 of the bundle's JCS>}],
//	 "deals": {"<deal id>": <the deal's copy for the selected audience (history, or keep), an evidence-bundle/v2>}}
//
// It is a private kind, like x-deal-sale/v0, read by this verifier only. A
// History shows that the holder of one key picked these copies and that
// every record in them is signed by that key. A record's signature is
// outside its Capsule ID, so that key is not bound to who sealed the deals.
const historySelectionExtension = "x-history-selection/v0"

// historyLogID is the user's own history log: one selection record per
// History written. No deal's log is ever written by `deal history`.
const historyLogID = "deal-history"

// historyActionID is the action_id of a selection record's capsule.
const historyActionID = "capsulectl-deal-history"

// dealAudienceHistory is the copy of a deal for someone who was not in it,
// the only copy a History is sent with: every record's payload is withheld,
// so each record shows only what its capsule states (its time, and for an
// act its effect's status). A deal's outcome and its acts' action classes
// are in payloads, beside the item, amounts and the other party's details,
// and a payload is disclosed whole or not at all: a History does not prove
// them (historyNotProven).
const dealAudienceHistory = "history"

// historyNotProven are the reads a history copy does not prove, as verify
// reports them on every History for the history audience.
var historyNotProven = []string{"outcome", "action_class"}

var hexDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func dealHistoryCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "history", Short: "Write one file holding the deals you pick, each with every record's payload withheld (only its time and an act's effect status show), checked by `verify --bundle`; seals the pick on your history log and never writes to a deal", Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		ids, _ := c.Flags().GetStringArray("deal")
		out, _ := c.Flags().GetString("out")
		audience, _ := c.Flags().GetString("audience")
		if len(ids) == 0 || out == "" {
			return inputError("--deal (one per picked deal, ids as `deal list` prints them) and --out are required")
		}
		switch audience {
		case dealAudienceHistory, dealAudienceKeep:
		case dealAudienceCounterparty, dealAudienceAdjudicator:
			return inputError("--audience " + audience + " is a copy of one deal for someone in it (`deal report --share`); a History may be sent to anyone, so its copies are --audience history (the default), or keep for your own file")
		default:
			return inputError("--audience must be history (the default: every record's payload withheld) or keep (your own copy, every private value in it: never one to send)")
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if !dealIDPattern.MatchString(id) {
				return inputError("--deal must be a deal id as printed by `deal list`: deal- followed by 16 hex characters (a sale has its own copy: `deal sale bundle`)")
			}
			if seen[id] {
				return inputError("--deal " + id + " is picked twice")
			}
			seen[id] = true
		}
		p, err := selected(c)
		if err != nil {
			return err
		}
		s, err := openDealSession(c.Context(), p)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, s.close()) }()
		b, summary, err := s.dealHistory(c.Context(), ids, audience, dealVerifyCommand(out))
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(b)
		if err != nil {
			return err
		}
		if err = atomicFile(out, encoded, false); err != nil {
			return err
		}
		return output(c, map[string]any{"out": out, "audience": audience, "deals": summary})
	}}
	cmd.Flags().StringArray("deal", nil, "A picked deal id from `deal list` (repeat for each deal)")
	cmd.Flags().String("out", "", "New file for the History")
	cmd.Flags().String("audience", dealAudienceHistory, "Which copy of each deal: history (default: every record's payload withheld, for anyone) or keep (your own copy, never one to send)")
	return cmd
}

// dealHistory builds the History over the picked deals, in deal id order,
// and one summary line per deal: how many of its steps the copy shows and
// how many came after its latest checkpoint (not shown: no deal's log is
// written, so none is cut). Every deal copy is built and checked before the
// selection is sealed; the selection record and its checkpoint are the only
// writes, and only to the history log.
func (s *dealSession) dealHistory(ctx context.Context, ids []string, audience, verify string) (map[string]interface{}, []map[string]any, error) {
	listed, err := s.dealListings(ctx)
	if err != nil {
		return nil, nil, err
	}
	known := map[string]bool{}
	for _, l := range listed {
		known[l.DealID] = true
	}
	picked := append([]string{}, ids...)
	sort.Strings(picked)
	selection := make([]interface{}, 0, len(picked))
	deals := map[string]interface{}{}
	summary := make([]map[string]any, 0, len(picked))
	for _, id := range picked {
		if !known[id] {
			return nil, nil, inputError("unknown deal: " + id + " (pick from `deal list`)")
		}
		b, shown, after, err := s.dealHistoryMember(ctx, id, audience, verify)
		if err != nil {
			return nil, nil, err
		}
		// The bundle is decoded as the verifier decodes the file, so the
		// digest it recomputes, and the bytes checked here, are the file's.
		encoded, err := json.Marshal(b)
		if err != nil {
			return nil, nil, err
		}
		value, err := decodeBundleJSON(encoded)
		if err != nil {
			return nil, nil, err
		}
		if err = verifyProducedBundle(value, true); err != nil {
			return nil, nil, err
		}
		deals[id] = value
		selection = append(selection, map[string]interface{}{"deal_id": id, "bundle_digest": jcsDigest(value)})
		summary = append(summary, map[string]any{"deal_id": id, "steps_shown": shown, "steps_after_checkpoint": after})
	}
	b, recordID, err := s.historyBundle(ctx, audience, selection)
	if err != nil {
		return nil, nil, err
	}
	b["extensions"] = map[string]interface{}{historySelectionExtension: map[string]interface{}{
		"selection_record": recordID, "selection": selection, "deals": deals,
	}}
	return b, summary, nil
}

// historyBundle seals the selection record on the history log, cuts that
// log's checkpoint and assembles its Evidence Bundle around the record. The
// record is disclosed; every earlier selection on the log is withheld (it
// names other deals). The record holds a fresh 256-bit nonce, so a later
// bundle that withholds it confirms no guess at what it names.
func (s *dealSession) historyBundle(ctx context.Context, audience string, selection []interface{}) (_ map[string]interface{}, _ string, err error) {
	if s.p.Checkpoint.Signing == (Secret{}) {
		return nil, "", inputError("a History needs the profile's checkpoint key (see `deal init`)")
	}
	nonce := make([]byte, 32)
	if _, err = rand.Read(nonce); err != nil {
		return nil, "", err
	}
	raw, err := json.Marshal(map[string]interface{}{
		"type": "history_selection", "profile": historySelectionExtension, "audience": audience,
		"nonce": hex.EncodeToString(nonce), "selection": selection,
	})
	if err != nil {
		return nil, "", err
	}
	generic, err := decodeBundleJSON(raw)
	if err != nil {
		return nil, "", err
	}
	payload, err := canonical.JCS(generic)
	if err != nil {
		return nil, "", err
	}
	request := Request{
		Version: "capsule-seal-request/v1",
		Capsule: emit.Input{
			ActionID: historyActionID, ActionType: emit.ActionTypeFYI, Operator: s.p.Name, Developer: "capsulectl-deal",
			Timestamp: dealClock().UTC().Truncate(time.Second),
		},
		Payload: payload,
	}
	s.dp = s.p
	s.dp.LogID = historyLogID
	if s.t, err = openTarget(ctx, s.dp, useInitialization); err != nil {
		return nil, "", err
	}
	defer func() {
		err = errors.Join(err, s.t.close())
		s.t = nil
	}()
	pub, err := s.t.publish(ctx, request, s.key)
	if err != nil {
		return nil, "", err
	}
	if _, err = cutCheckpoint(ctx, localOnly(s.dp), s.t.log); err != nil {
		return nil, "", err
	}
	entries, err := s.logEntries(ctx)
	if err != nil {
		return nil, "", err
	}
	withhold := map[string]bool{}
	for _, e := range entries {
		if id := hex.EncodeToString(e.Value); id != pub.CapsuleID {
			withhold[id] = true
		}
	}
	b, err := AssembleBundle(ctx, s.t.artifacts, s.t.log, s.dp.LogID, BundleOptions{
		Root: pub.CapsuleID, Payloads: "selected", WithDisclosure: true, Withhold: withhold,
	})
	if err != nil {
		return nil, "", err
	}
	cp, _ := b["checkpoint"].(map[string]interface{})
	statement, err := base64.StdEncoding.DecodeString(fmt.Sprint(cp["statement"]))
	if err != nil {
		return nil, "", err
	}
	cp["cose"] = base64.RawURLEncoding.EncodeToString(statement)
	return b, pub.CapsuleID, nil
}

// dealHistoryMember is one picked deal's copy for audience, read from its log
// as it stands: through its latest checkpoint, with nothing sealed and no
// checkpoint cut. A step indexed but not yet on the log (an interrupted
// write) is refused rather than recovered, since recovery writes.
//
// A history copy withholds the payload of every record on the deal's log
// through the checkpoint (each step and each sealed report) through the
// bundle builder's withholding, the one shared copies use, carries a deal section with no report
// text ({"audience": "history", "deal_id": ...}), and passes the share gate
// for both shared audiences on its bytes.
//
// A keep copy is the user's own: it must name the deal's newest report
// sealed for keep within the checkpoint when it has any (a bundle holding a
// sealed report must), and only when that report covers every step shown,
// else the deal is refused, since its text would describe the deal before
// those steps.
func (s *dealSession) dealHistoryMember(ctx context.Context, dealID, audience, verify string) (_ map[string]interface{}, shown, after int, err error) {
	if err = s.db.QueryRowContext(ctx, `SELECT deal_key FROM deal_keys WHERE deal_id=?`, dealID).Scan(&s.dkey); err != nil {
		return nil, 0, 0, err
	}
	s.dp = s.p
	s.dp.LogID = dealLogID(dealID)
	if s.t, err = openTarget(ctx, s.dp, useRead); err != nil {
		return nil, 0, 0, err
	}
	defer func() {
		err = errors.Join(err, s.t.close())
		s.t = nil
	}()
	var indexed int
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deal_steps WHERE deal_id=?`, dealID).Scan(&indexed); err != nil {
		return nil, 0, 0, err
	}
	entries, err := s.stepEntries(ctx, dealID)
	if err != nil {
		return nil, 0, 0, err
	}
	if indexed != len(entries) {
		return nil, 0, 0, inputError("deal " + dealID + " has a step not yet on its log (an interrupted write, which the deal's next write such as `deal note` recovers): pick it again after that")
	}
	events, err := s.load(ctx, dealID)
	if err != nil {
		return nil, 0, 0, err
	}
	state, err := s.t.log.LoadCLL(ctx)
	if err != nil {
		return nil, 0, 0, err
	}
	if state.Checkpoint == nil {
		return nil, 0, 0, inputError("deal " + dealID + " has no checkpoint yet, so no copy can prove its steps")
	}
	covered := events
	for len(covered) > 0 && covered[len(covered)-1].Sequence > state.Checkpoint.IndexedSeq {
		covered = covered[:len(covered)-1]
	}
	if len(covered) == 0 {
		return nil, 0, 0, inputError("deal " + dealID + " has no step under its latest checkpoint")
	}
	if audience == dealAudienceHistory {
		all, err := s.logEntries(ctx)
		if err != nil {
			return nil, 0, 0, err
		}
		withhold := map[string]bool{}
		for _, e := range all {
			if e.Seq <= state.Checkpoint.IndexedSeq {
				withhold[hex.EncodeToString(e.Value)] = true
			}
		}
		b, cadence, err := s.dealAssembleAtCheckpoint(ctx, covered, withhold)
		if err != nil {
			return nil, 0, 0, err
		}
		extensions := map[string]interface{}{dealCadenceExtension: cadence, dealProfile: map[string]interface{}{"audience": dealAudienceHistory, "deal_id": dealID}}
		if dealRole(covered) == dealRoleSeller {
			extensions[dealSellerExtension] = map[string]interface{}{}
		}
		b["extensions"] = extensions
		encoded, err := json.Marshal(b)
		if err != nil {
			return nil, 0, 0, err
		}
		for _, gate := range []string{dealAudienceCounterparty, dealAudienceAdjudicator} {
			if err = dealShareGate(encoded, covered, gate); err != nil {
				return nil, 0, 0, err
			}
		}
		return b, len(covered), len(events) - len(covered), nil
	}
	withhold, err := s.reportIDs(ctx, dealID)
	if err != nil {
		return nil, 0, 0, err
	}
	var sealedReports int
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deal_reports WHERE deal_id=? AND cll_sequence>0 AND cll_sequence<=?`, dealID, state.Checkpoint.IndexedSeq).Scan(&sealedReports); err != nil {
		return nil, 0, 0, err
	}
	var pointer string
	if sealedReports > 0 {
		var reportSeq uint64
		err = s.db.QueryRowContext(ctx, `SELECT capsule_id, cll_sequence FROM deal_reports WHERE deal_id=? AND cll_sequence>0 AND cll_sequence<=? AND audience=? ORDER BY cll_sequence DESC LIMIT 1`,
			dealID, state.Checkpoint.IndexedSeq, dealAudienceKeep).Scan(&pointer, &reportSeq)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, 0, 0, err
		}
		if pointer == "" || reportSeq < covered[len(covered)-1].Sequence {
			return nil, 0, 0, inputError("deal " + dealID + " has a sealed report but none of your own that covers its later steps, so your copy would show the deal as it stood then or as another reader sees it; seal a current one with `deal report --deal " + dealID + " --bundle FILE`, then pick it again")
		}
		delete(withhold, pointer)
	}
	b, cadence, err := s.dealAssembleAtCheckpoint(ctx, covered, withhold)
	if err != nil {
		return nil, 0, 0, err
	}
	ext := map[string]interface{}{dealReportPointer: pointer}
	if pointer == "" {
		report, err := s.dealReportFor(ctx, covered)
		if err != nil {
			return nil, 0, 0, err
		}
		if ext, err = s.dealReportExtension(covered, report, dealAudienceKeep, verify, dealPrivate{}, withhold, witnessCoverage(covered, cadence)); err != nil {
			return nil, 0, 0, err
		}
	}
	extensions := map[string]interface{}{dealCadenceExtension: cadence, dealProfile: ext}
	if dealRole(covered) == dealRoleSeller {
		extensions[dealSellerExtension] = map[string]interface{}{}
	}
	b["extensions"] = extensions
	return b, len(covered), len(events) - len(covered), nil
}

// historyEntry is `verify --bundle`'s reading of a History's extension. It
// passes only when the selection record is disclosed and matches, signed;
// the carried selection is the sealed one; each selected deal is carried,
// is the bundle its digest names, is of that deal's own log, is the copy
// for the sealed audience and verifies VALID as its own Evidence Bundle;
// nothing unselected is carried; and every nested record is signed by the
// selection record's key (holder_key), the one key the deals share. A
// finding names the deal it is about.
func historyEntry(value map[string]interface{}, result aacbundle.VerificationResult, directory []witnessRow) (map[string]any, aacbundle.ClaimResult) {
	var failed, notShown []string
	exts, _ := value["extensions"].(map[string]interface{})
	ext, _ := exts[historySelectionExtension].(map[string]interface{})
	recordID, _ := ext["selection_record"].(string)
	carried, _ := ext["deals"].(map[string]interface{})
	entry := map[string]any{"kind": historySelectionExtension}

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
	_, signatureStates := producerSignatureClaim(rawRecords)
	holder := ""
	for _, raw := range rawRecords {
		r, _ := raw.(map[string]interface{})
		if r["capsule_id"] == recordID && r["action_id"] == historyActionID {
			holder, _ = r["key_id"].(string)
		}
	}
	audience, _ := sealed["audience"].(string)
	switch {
	case !matched || sealed["type"] != "history_selection" || sealed["profile"] != historySelectionExtension:
		failed = append(failed, "selection_record_unverified")
	case holder == "" || signatureStates[recordID] != "authored":
		failed = append(failed, "selection_record_unsigned")
	}
	if value["root"] != recordID {
		failed = append(failed, "selection_record_not_root")
	}
	if cert, _ := value["completeness_certificate"].(map[string]interface{}); cert["log_id"] != historyLogID {
		failed = append(failed, "not_a_history_log")
	}
	if !sameJSONValue(ext["selection"], sealed["selection"]) {
		failed = append(failed, "selection_not_sealed")
	}
	entry["holder_key"], entry["audience"] = holder, audience
	switch audience {
	case dealAudienceHistory:
		entry["not_proven"] = historyNotProven
	case dealAudienceKeep:
	default:
		failed = append(failed, "selection_audience_not_history")
	}

	listed, _ := sealed["selection"].([]interface{})
	if len(listed) == 0 {
		failed = append(failed, "selection_missing")
	}
	picked := map[string]string{} // deal id -> bundle digest
	var order []string
	for _, raw := range listed {
		e, _ := raw.(map[string]interface{})
		id, _ := e["deal_id"].(string)
		digest, _ := e["bundle_digest"].(string)
		if _, dup := picked[id]; dup || !dealIDPattern.MatchString(id) || !hexDigestPattern.MatchString(digest) {
			failed = append(failed, "selection_entry_malformed:"+id)
			continue
		}
		picked[id] = digest
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
		member := map[string]any{"deal_id": id}
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
		verdict, report := bundleVerdict(tb, directory)
		member["verdict"], member["report"] = verdict, report
		if jcsDigest(tb) != picked[id] {
			failed = append(failed, "deal_bundle_not_the_selected_one:"+id)
		}
		if _, statement := checkpointClaim(tb); statement == nil {
			failed = append(failed, "deal_log_unbound:"+id)
		} else if signed, err := signedCheckpoint(statement); err != nil || signed["log_id"] != dealLogID(id) {
			failed = append(failed, "deal_not_of_its_log:"+id)
		}
		copyFor, _ := sealedOrInlineReport(tb)["audience"].(string)
		member["audience"] = copyFor
		if copyFor != audience {
			failed = append(failed, "deal_audience_not_selected:"+id)
		}
		if audience == dealAudienceHistory && payloadDisclosed(tb) {
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
func payloadDisclosed(b map[string]interface{}) bool {
	disclosures, _ := b["disclosures"].(map[string]interface{})
	for _, raw := range disclosures {
		d, _ := raw.(map[string]interface{})
		if len(d) > 0 {
			return true
		}
	}
	return false
}
