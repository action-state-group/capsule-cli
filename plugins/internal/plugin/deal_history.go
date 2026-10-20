package plugin

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/capsule-cli/internal/cli"
	emit "github.com/action-state-group/capsule-emit/go"
	"github.com/spf13/cobra"
)

func dealHistoryCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "history", Short: "Write one file holding the deals you pick, each with every record's payload withheld (only its time and an act's effect status show), checked by `verify --bundle`; seals the pick on your history log and never writes to a deal", Args: cli.NoArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		ids, _ := c.Flags().GetStringArray("deal")
		out, _ := c.Flags().GetString("out")
		audience, _ := c.Flags().GetString("audience")
		recipient, _ := c.Flags().GetString("to")
		recipient = strings.TrimSpace(recipient)
		if len(ids) == 0 || out == "" {
			return cli.InputError("--deal (one per picked deal, ids as `deal list` prints them) and --out are required")
		}
		switch audience {
		case cli.DealAudienceHistory, cli.DealAudienceKeep:
		case cli.DealAudienceCounterparty, cli.DealAudienceAdjudicator:
			return cli.InputError("--audience " + audience + " is a copy of one deal for someone in it (`deal report --share`); a History may be sent to anyone, so its copies are --audience history (the default), or keep for your own file")
		default:
			return cli.InputError("--audience must be history (the default: every record's payload withheld) or keep (your own copy, every private value in it: never one to send)")
		}
		switch {
		case audience == cli.DealAudienceHistory && recipient == "":
			return cli.InputError("deal history needs --to: who the History is for, as sealed in its selection record")
		case audience == cli.DealAudienceKeep && recipient != "":
			return cli.InputError("--to names who a History is for; --audience keep is your own copy, never one to send")
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if !cli.DealIDPattern.MatchString(id) {
				return cli.InputError("--deal must be a deal id as printed by `deal list`: deal- followed by 16 hex characters (a sale has its own copy: `deal sale bundle`)")
			}
			if seen[id] {
				return cli.InputError("--deal " + id + " is picked twice")
			}
			seen[id] = true
		}
		p, err := cli.Selected(c)
		if err != nil {
			return err
		}
		s, err := openDealSession(c.Context(), p)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, s.close()) }()
		b, summary, err := s.dealHistory(c.Context(), ids, audience, recipient, dealVerifyCommand(out))
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(b)
		if err != nil {
			return err
		}
		if err = cli.AtomicFile(out, encoded, false); err != nil {
			return err
		}
		result := map[string]any{"out": out, "audience": audience, "deals": summary}
		if recipient != "" {
			result["recipient"] = recipient
		}
		return cli.Output(c, result)
	}}
	cmd.Flags().StringArray("deal", nil, "A picked deal id from `deal list` (repeat for each deal)")
	cmd.Flags().String("out", "", "New file for the History")
	cmd.Flags().String("audience", cli.DealAudienceHistory, "Which copy of each deal: history (default: every record's payload withheld, for anyone) or keep (your own copy, never one to send)")
	cmd.Flags().String("to", "", "Who the History is for, as sealed in its selection record (required, except with --audience keep)")
	return cmd
}

// dealHistory builds the History over the picked deals, in deal id order,
// and one summary line per deal: how many of its steps the copy shows and
// how many came after its latest checkpoint (not shown: no deal's log is
// written, so none is cut). That count is sealed in the deal's selection
// entry too, so a step left out (a refund, a dispute) is stated in the file,
// not only on the holder's screen. Every deal copy is built and checked before the
// selection is sealed; the selection record and its checkpoint are the only
// writes, and only to the History's own log.
func (s *dealSession) dealHistory(ctx context.Context, ids []string, audience, recipient, verify string) (map[string]interface{}, []map[string]any, error) {
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
			return nil, nil, cli.InputError("unknown deal: " + id + " (pick from `deal list`)")
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
		value, err := cli.DecodeBundleJSON(encoded)
		if err != nil {
			return nil, nil, err
		}
		if err = cli.VerifyProducedBundle(value, true); err != nil {
			return nil, nil, err
		}
		deals[id] = value
		selection = append(selection, map[string]interface{}{"deal_id": id, "bundle_digest": cli.JCSDigest(value), "steps_after_checkpoint": after})
		summary = append(summary, map[string]any{"deal_id": id, "steps_shown": shown, "steps_after_checkpoint": after})
	}
	b, recordID, err := s.historyBundle(ctx, audience, recipient, selection)
	if err != nil {
		return nil, nil, err
	}
	b["extensions"] = map[string]interface{}{cli.HistorySelectionExtension: map[string]interface{}{
		"selection_record": recordID, "selection": selection, "deals": deals,
	}}
	return b, summary, nil
}

// historyBundle opens the History's own log, named by a fresh 256-bit nonce
// the selection record also holds, seals the record (with recipient, when
// given) as its only entry, cuts its checkpoint and assembles its Evidence
// Bundle around the record, disclosed.
func (s *dealSession) historyBundle(ctx context.Context, audience, recipient string, selection []interface{}) (_ map[string]interface{}, _ string, err error) {
	if s.p.Checkpoint.Signing == (cli.Secret{}) {
		return nil, "", cli.InputError("a History needs the profile's checkpoint key (see `deal init`)")
	}
	nonce := make([]byte, 32)
	if _, err = rand.Read(nonce); err != nil {
		return nil, "", err
	}
	record := map[string]interface{}{
		"type": "history_selection", "profile": cli.HistorySelectionExtension, "audience": audience,
		"nonce": hex.EncodeToString(nonce), "selection": selection,
	}
	if recipient != "" {
		record["recipient"] = recipient
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, "", err
	}
	generic, err := cli.DecodeBundleJSON(raw)
	if err != nil {
		return nil, "", err
	}
	payload, err := canonical.JCS(generic)
	if err != nil {
		return nil, "", err
	}
	request := cli.Request{
		Version: "capsule-seal-request/v1",
		Capsule: emit.Input{
			ActionID: cli.HistoryActionID, ActionType: emit.ActionTypeFYI, Operator: s.p.Name, Developer: "capsulectl-deal",
			Timestamp: cli.DealClock().UTC().Truncate(time.Second),
		},
		Payload: payload,
	}
	s.dp = s.p
	s.dp.LogID = cli.HistoryLogPrefix + hex.EncodeToString(nonce)
	if s.t, err = cli.OpenDealTarget(ctx, s.dp, cli.UseInitialization, s); err != nil {
		return nil, "", err
	}
	defer func() {
		err = errors.Join(err, s.t.Close())
		s.t = nil
	}()
	pub, err := s.t.Publish(ctx, request, s.key)
	if err != nil {
		return nil, "", err
	}
	if _, err = cli.CutCheckpoint(ctx, localOnly(s.dp), s.t.Log); err != nil {
		return nil, "", err
	}
	b, err := cli.AssembleBundle(ctx, s.t.Artifacts, s.t.Log, s.dp.LogID, cli.BundleOptions{
		Root: pub.CapsuleID, Payloads: "selected", WithDisclosure: true,
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
	s.dp.LogID = cli.DealLogID(dealID)
	// Whether the deal's reports are off its log is read from the store, as
	// a deal command reads it (reportsOffLog).
	s.onBook = false
	if x, ok := s.index.(shelfDealIndex); ok {
		legacy, err := x.isLegacy(ctx, dealID)
		if err != nil {
			return nil, 0, 0, err
		}
		s.onBook = !legacy
	}
	defer func() { s.onBook = false }()
	if s.t, err = cli.OpenTarget(ctx, s.dp, cli.UseRead); err != nil {
		return nil, 0, 0, err
	}
	defer func() {
		err = errors.Join(err, s.t.Close())
		s.t = nil
	}()
	indexed, err := s.index.stepCount(ctx, dealID)
	if err != nil {
		return nil, 0, 0, err
	}
	entries, err := s.stepEntries(ctx, dealID)
	if err != nil {
		return nil, 0, 0, err
	}
	if indexed != len(entries) {
		return nil, 0, 0, cli.InputError("deal " + dealID + " has a step not yet on its log (an interrupted write, which the deal's next write such as `deal note` recovers): pick it again after that")
	}
	events, err := s.load(ctx, dealID)
	if err != nil {
		return nil, 0, 0, err
	}
	state, err := s.t.Log.LoadCLL(ctx)
	if err != nil {
		return nil, 0, 0, err
	}
	if state.Checkpoint == nil {
		return nil, 0, 0, cli.InputError("deal " + dealID + " has no checkpoint yet, so no copy can prove its steps")
	}
	covered := events
	for len(covered) > 0 && covered[len(covered)-1].Sequence > state.Checkpoint.IndexedSeq {
		covered = covered[:len(covered)-1]
	}
	// A deal whose reports are off its log has a checkpoint at every step;
	// a step without one is a write a crash interrupted, which the deal's
	// next command finishes (coverSteps). History writes nothing, so it
	// refuses rather than show the deal before that step.
	if offLog, err := s.reportsOffLog(ctx, dealID); err != nil {
		return nil, 0, 0, err
	} else if offLog && len(covered) < len(events) {
		return nil, 0, 0, cli.InputError("deal " + dealID + " has a step its checkpoint does not cover yet (an interrupted write, which the deal's next command, such as `deal note`, finishes): pick it again after that")
	}
	if len(covered) == 0 {
		return nil, 0, 0, cli.InputError("deal " + dealID + " has no step under its latest checkpoint")
	}
	if audience == cli.DealAudienceHistory {
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
		extensions := map[string]interface{}{cli.DealCadenceExtension: cadence, cli.DealProfile: map[string]interface{}{"audience": cli.DealAudienceHistory, "deal_id": dealID}}
		if dealRole(covered) == dealRoleSeller {
			extensions[dealSellerExtension] = map[string]interface{}{}
		}
		b["extensions"] = extensions
		encoded, err := json.Marshal(b)
		if err != nil {
			return nil, 0, 0, err
		}
		for _, gate := range []string{cli.DealAudienceCounterparty, cli.DealAudienceAdjudicator} {
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
	sealedReports, err := s.index.reportsThrough(ctx, dealID, state.Checkpoint.IndexedSeq)
	if err != nil {
		return nil, 0, 0, err
	}
	var pointer string
	if sealedReports > 0 {
		var reportSeq uint64
		if pointer, reportSeq, _, err = s.index.latestReport(ctx, dealID, cli.DealAudienceKeep, state.Checkpoint.IndexedSeq); err != nil {
			return nil, 0, 0, err
		}
		if pointer == "" || reportSeq < covered[len(covered)-1].Sequence {
			return nil, 0, 0, cli.InputError("deal " + dealID + " has a sealed report but none of your own that covers its later steps, so your copy would show the deal as it stood then or as another reader sees it; seal a current one with `deal report --deal " + dealID + " --bundle FILE`, then pick it again")
		}
		delete(withhold, pointer)
	}
	b, cadence, err := s.dealAssembleAtCheckpoint(ctx, covered, withhold)
	if err != nil {
		return nil, 0, 0, err
	}
	ext := map[string]interface{}{cli.DealReportPointer: pointer}
	var standalone map[string]interface{}
	if pointer == "" {
		// A deal whose reports are off its log (capsule-cli#294) gets a
		// fresh keep report bound to the checkpoint this copy is at, and
		// computed at its time.
		offLog, err := s.reportsOffLog(ctx, dealID)
		if err != nil {
			return nil, 0, 0, err
		}
		if offLog {
			if s.reportAt, err = bundleCheckpointTime(b); err != nil {
				return nil, 0, 0, err
			}
			defer func() { s.reportAt = time.Time{} }()
		}
		report, err := s.dealReportFor(ctx, covered)
		if err != nil {
			return nil, 0, 0, err
		}
		if ext, err = s.dealReportExtension(covered, report, cli.DealAudienceKeep, verify, dealPrivate{}, withhold, witnessCoverage(covered, cadence)); err != nil {
			return nil, 0, 0, err
		}
		if offLog {
			var id string
			if id, standalone, err = s.dealStandaloneReport(dealID, cli.DealAudienceKeep, ext, b); err != nil {
				return nil, 0, 0, err
			}
			ext = map[string]interface{}{cli.DealReportPointer: id}
		}
	}
	extensions := map[string]interface{}{cli.DealCadenceExtension: cadence, cli.DealProfile: ext}
	if standalone != nil {
		extensions[cli.DealReportExtension] = standalone
	}
	if dealRole(covered) == dealRoleSeller {
		extensions[dealSellerExtension] = map[string]interface{}{}
	}
	b["extensions"] = extensions
	return b, len(covered), len(events) - len(covered), nil
}
