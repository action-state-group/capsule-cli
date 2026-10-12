package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"sort"

	"github.com/spf13/cobra"
)

// dealHistoryKind is a history container: deals the user picked, each as its
// own deal bundle, carried in one file. Each deal has its own log, so the
// picked deals cannot share one checkpoint; the container instead carries
// each deal's own Evidence Bundle and a selection naming exactly the picked
// deals, and `verify --bundle` checks every one. It is a private kind, like
// x-deal-sale/v0, read by this verifier only:
//
//	{"kind": "x-deal-history/v0",
//	 "selection": [{"deal_id": ..., "bundle_digest": <sha256 of the bundle's JCS>}],
//	 "deals": {"<deal id>": <the deal's own evidence-bundle/v2>}}
//
// The selection is not signed: it says which bundles the file carries, not
// who picked them. Each nested bundle is the deal producer's own evidence.
const dealHistoryKind = "x-deal-history/v0"

var hexDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func dealHistoryCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "history", Short: "Write one file holding the deals you pick, each as its own bundle, checked by `verify --bundle`; read-only: never seals a record or cuts a checkpoint on a deal", Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		ids, _ := c.Flags().GetStringArray("deal")
		out, _ := c.Flags().GetString("out")
		if len(ids) == 0 || out == "" {
			return inputError("--deal (one per picked deal, ids as `deal list` prints them) and --out are required")
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
		s, err := openDealStoreRead(p)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, s.close()) }()
		if s.keys, err = parseKeys(p.TrustedKeys); err != nil {
			return err
		}
		container, summary, err := s.dealHistory(c.Context(), ids, dealVerifyCommand(out))
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(container)
		if err != nil {
			return err
		}
		if err = atomicFile(out, encoded, false); err != nil {
			return err
		}
		return output(c, map[string]any{"out": out, "deals": summary})
	}}
	cmd.Flags().StringArray("deal", nil, "A picked deal id from `deal list` (repeat for each deal)")
	cmd.Flags().String("out", "", "New file for the history container")
	return cmd
}

// dealHistory builds the container over the picked deals, in deal id order,
// and one summary line per deal: how many of its steps the copy shows and
// how many came after its latest checkpoint (not shown: the copy cuts none).
func (s *dealSession) dealHistory(ctx context.Context, ids []string, verify string) (map[string]interface{}, []map[string]any, error) {
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
		b, shown, after, err := s.dealHistoryMember(ctx, id, verify)
		if err != nil {
			return nil, nil, err
		}
		// The bundle is decoded as the verifier decodes the file, so the
		// digest it recomputes is this one.
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
	return map[string]interface{}{"kind": dealHistoryKind, "selection": selection, "deals": deals}, summary, nil
}

// dealHistoryMember is one picked deal's own bundle, read from its log as it
// stands: through its latest checkpoint, with nothing sealed and no
// checkpoint cut. A deal step indexed but not yet on the log (an
// interrupted write) is refused rather than recovered, since recovery
// writes. The copy's deal section is the deal's newest report sealed within
// the checkpoint, disclosed by pointer, when it has one (a bundle holding a
// sealed report must name one), else the report read now, unsealed. A
// sealed report older than the last step shown is refused: its text would
// describe the deal before those steps.
func (s *dealSession) dealHistoryMember(ctx context.Context, dealID, verify string) (_ map[string]interface{}, shown, after int, err error) {
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
	withhold, err := s.reportIDs(ctx, dealID)
	if err != nil {
		return nil, 0, 0, err
	}
	var pointer string
	var reportSeq uint64
	err = s.db.QueryRowContext(ctx, `SELECT capsule_id, cll_sequence FROM deal_reports WHERE deal_id=? AND cll_sequence>0 AND cll_sequence<=? ORDER BY cll_sequence DESC LIMIT 1`, dealID, state.Checkpoint.IndexedSeq).Scan(&pointer, &reportSeq)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, 0, 0, err
	}
	// A bundle holding a sealed report must name one, and its text would
	// then describe the deal as it stood before the later steps: refused
	// rather than shown as current.
	if pointer != "" && reportSeq < covered[len(covered)-1].Sequence {
		return nil, 0, 0, inputError("deal " + dealID + "'s newest sealed report predates its later steps, so a copy would show the deal as it stood then; seal a current one with `deal report --deal " + dealID + " --bundle FILE`, then pick it again")
	}
	delete(withhold, pointer)
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

// verifyHistory is `verify --bundle` on a history container: VALID only when
// the selection names each carried deal exactly once, each carried bundle is
// the one the selection's digest names, is of that deal's own log (its
// signed checkpoint's log id) and verifies VALID as its own Evidence Bundle.
// A finding names the deal it is about.
func verifyHistory(value map[string]interface{}, directory []witnessRow) (string, map[string]any) {
	var failed, notShown []string
	carried, _ := value["deals"].(map[string]interface{})
	listed, ok := value["selection"].([]interface{})
	if !ok || len(listed) == 0 {
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
	for _, id := range extra {
		failed = append(failed, "deal_not_in_selection:"+id)
	}
	deals := make([]map[string]any, 0, len(order))
	for _, id := range order {
		entry := map[string]any{"deal_id": id}
		deals = append(deals, entry)
		tb, ok := carried[id].(map[string]interface{})
		if !ok {
			failed = append(failed, "deal_missing:"+id)
			entry["verdict"] = "INVALID"
			continue
		}
		if tb["bundle_kind"] != "evidence-bundle/v2" || tb["bundle_version"] != "2" {
			failed = append(failed, "deal_not_a_bundle:"+id)
			entry["verdict"] = "INVALID"
			continue
		}
		verdict, report := bundleVerdict(tb, directory)
		entry["verdict"], entry["report"] = verdict, report
		if jcsDigest(tb) != picked[id] {
			failed = append(failed, "deal_bundle_not_the_selected_one:"+id)
		}
		if _, statement := checkpointClaim(tb); statement == nil {
			failed = append(failed, "deal_log_unbound:"+id)
		} else if signed, err := signedCheckpoint(statement); err != nil || signed["log_id"] != dealLogID(id) {
			failed = append(failed, "deal_not_of_its_log:"+id)
		}
		switch verdict {
		case "VALID":
		case "INCOMPLETE":
			notShown = append(notShown, "deal_incomplete:"+id)
		default:
			failed = append(failed, "deal_invalid:"+id)
		}
	}
	verdict := "VALID"
	switch {
	case len(failed) > 0:
		verdict = "INVALID"
	case len(notShown) > 0:
		verdict = "INCOMPLETE"
	}
	return verdict, map[string]any{
		"verdict": verdict, "kind": dealHistoryKind, "deals": deals,
		"findings": nonNilStrings(append(failed, notShown...)),
	}
}
