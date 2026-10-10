package cli

import (
	"context"
	"errors"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/spf13/cobra"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/disclosure"
)

// dealSaleExtension carries a sale's threads in the user's own sale bundle:
// {"threads": {"<thread deal id>": <the thread's own evidence-bundle/v2>}}.
// It only transports them, each verified as its own Evidence Bundle; it is
// a private kind, never registered. Completeness comes from the sale log's
// own completeness certificate, which covers every thread registration, and
// the openings in the x-deal-v0 section's sale_threads, which bind each
// registration to its thread.
const dealSaleExtension = "x-deal-sale/v0"

// Each registered thread's state in a sale bundle (sale_threads[].member).
const (
	saleThreadPresent     = "present"
	saleThreadMissing     = "missing"
	saleThreadNeverOpened = "never_opened"
)

func dealSaleBundleCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "bundle", Short: "Write your own copy of a sale: its log, every registered thread, and proof the list of threads is whole", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		saleID, _ := c.Flags().GetString("sale")
		out, _ := c.Flags().GetString("out")
		if !saleIDPattern.MatchString(saleID) || out == "" {
			return inputError("--sale (a sale id as printed by `deal sale new`) and --out are required")
		}
		return runDeal(c, false, func(ctx context.Context, s *dealSession, _ string, _ []sealedEvent) error {
			b, summary, err := s.dealSaleBundle(ctx, saleID, dealVerifyCommand(out))
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
			summary["sale_id"], summary["out"] = saleID, out
			return output(c, summary)
		})
	}}
	cmd.Flags().String("sale", "", "Sale ID from `deal sale new`")
	cmd.Flags().String("out", "", "New file for the sale bundle")
	return cmd
}

// dealSaleBundle builds the user's own sale bundle: the sale log's Evidence
// Bundle through a checkpoint cut now, so its completeness certificate
// covers every registration so far; each registered thread's own copy, as
// `bundle --deal` builds it, in x-deal-sale/v0; and the openings of the
// registrations in the x-deal-v0 section's sale_threads. A sale with a thread
// opened before threads were registered says so
// (threads_predate_registration): its list of threads is not whole.
func (s *dealSession) dealSaleBundle(ctx context.Context, saleID, verify string) (map[string]interface{}, map[string]interface{}, error) {
	if err := s.useDeal(ctx, saleID, false); err != nil {
		return nil, nil, err
	}
	events, err := s.load(ctx, saleID)
	if err != nil {
		return nil, nil, err
	}
	predate, err := s.unregisteredThreads(ctx, saleID)
	if err != nil {
		return nil, nil, err
	}
	var entries []interface{}
	threads := map[string]interface{}{}
	counts := map[string]int{}
	for _, se := range events {
		th := se.Event.Thread
		if th == nil {
			continue
		}
		entry := map[string]interface{}{"registration": digestRef(se.Digest), "nonce": se.Event.Nonces["thread_ref"], "thread_id": th.ThreadID, "member": saleThreadNeverOpened}
		var held int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deal_keys WHERE deal_id=?`, th.ThreadID).Scan(&held); err != nil {
			return nil, nil, err
		}
		if held > 0 {
			if err := s.t.close(); err != nil {
				return nil, nil, err
			}
			s.t = nil
			if err := s.useDeal(ctx, th.ThreadID, false); err != nil {
				return nil, nil, err
			}
			tevents, err := s.load(ctx, th.ThreadID)
			if err != nil {
				return nil, nil, err
			}
			report, err := s.dealReportFor(ctx, tevents)
			if err != nil {
				return nil, nil, err
			}
			tb, err := s.dealReportBundle(ctx, tevents, report, dealAudienceKeep, verify, true)
			if err != nil {
				return nil, nil, err
			}
			threads[th.ThreadID] = tb
			entry["member"] = saleThreadPresent
		}
		counts[entry["member"].(string)]++
		entries = append(entries, entry)
	}
	if s.t != nil {
		if err := s.t.close(); err != nil {
			return nil, nil, err
		}
		s.t = nil
	}
	if err := s.useDeal(ctx, saleID, false); err != nil {
		return nil, nil, err
	}
	b, cadence, err := s.dealAssemble(ctx, events, nil)
	if err != nil {
		return nil, nil, err
	}
	section := map[string]interface{}{"sale_threads": entries}
	if entries == nil {
		section["sale_threads"] = []interface{}{}
	}
	if predate {
		section["threads_predate_registration"] = true
	}
	b["extensions"] = map[string]interface{}{dealCadenceExtension: cadence, dealProfile: section, dealSaleExtension: map[string]interface{}{"threads": threads}}
	if err = verifyProducedBundle(b, true); err != nil {
		return nil, nil, err
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return nil, nil, err
	}
	value, err := decodeBundleJSON(raw)
	if err != nil {
		return nil, nil, err
	}
	verdict, _ := bundleVerdict(value, nil)
	summary := map[string]interface{}{"verdict": verdict, "threads": len(entries), "present": counts[saleThreadPresent], "never_opened": counts[saleThreadNeverOpened]}
	if predate {
		summary["threads_predate_registration"] = true
	}
	return b, summary, nil
}

// unregisteredThreads reports whether this device holds a thread of the
// sale opened before threads were registered (its task authority names no
// registration).
func (s *dealSession) unregisteredThreads(ctx context.Context, saleID string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT local FROM deal_steps WHERE kind='task_authority'`)
	if err != nil {
		return false, err
	}
	var unregistered []string
	for rows.Next() {
		var local string
		if err = rows.Scan(&local); err != nil {
			return false, errors.Join(err, rows.Close())
		}
		var ev dealEvent
		if json.Unmarshal([]byte(local), &ev) == nil && ev.SaleAuthority != "" && ev.SaleRegistration == "" {
			unregistered = append(unregistered, ev.DealID)
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return false, err
	}
	// The store holds one connection: read each thread's opening only after
	// the rows above are closed.
	for _, dealID := range unregistered {
		var open string
		if err = s.db.QueryRowContext(ctx, `SELECT local FROM deal_steps WHERE deal_id=? AND n=1`, dealID).Scan(&open); err != nil {
			return false, err
		}
		var first dealEvent
		if json.Unmarshal([]byte(open), &first) == nil && first.Open != nil && first.Open.Sale == saleID {
			return true, nil
		}
	}
	return false, nil
}

// saleBundleEntry checks a sale bundle's threads: every thread registration
// on the sale log the bundle certifies has exactly one sale_threads entry
// that opens it; the threads x-deal-sale/v0 carries are exactly the present
// ones; and each carried thread verifies as its own Evidence Bundle, is that
// thread, names that registration and opens its sale_authority_commitment
// to this sale's task authority. A registration not shown (missing), or a
// sale whose threads predate registration, is not shown: INCOMPLETE. Any
// mismatch, or a carried thread that fails, fails the bundle. The
// extension on a bundle that is not a sale's is never read as completeness.
func saleBundleEntry(value map[string]interface{}, result aacbundle.VerificationResult, directory []witnessRow) (map[string]any, aacbundle.ClaimResult) {
	var findings, notShown []string
	failed := false
	fail := func(f string) { findings, failed = append(findings, f), true }
	matched := map[string]bool{}
	for _, d := range result.Disclosures {
		if d.Member == "agent_input" && d.Status == disclosure.Match {
			matched[d.CapsuleID] = true
		}
	}
	disclosures, _ := value["disclosures"].(map[string]interface{})
	type held struct {
		seq    float64
		digest string
		record map[string]interface{}
	}
	var registrations []held
	saleKey, isSale := "", false
	records, _ := value["records"].([]interface{})
	for _, raw := range records {
		r, _ := raw.(map[string]interface{})
		id, _ := r["capsule_id"].(string)
		d, _ := disclosures[id].(map[string]interface{})
		in, _ := d["agent_input"].(map[string]interface{})
		if in == nil || !matched[id] {
			continue
		}
		digest := jcsDigest(in)
		if in["type"] == typeTaskAuthority {
			if _, sold := in["chain_id"].(string); sold && saleIDPattern.MatchString(in["chain_id"].(string)) {
				saleKey = digest
			}
			continue
		}
		blk, _ := in[dealProfile].(map[string]interface{})
		switch blk["record_type"] {
		case "sale":
			isSale = true
		case "thread":
			var seq float64
			switch n := blk["seq"].(type) {
			case json.Number:
				seq, _ = n.Float64()
			case float64:
				seq = n
			}
			registrations = append(registrations, held{seq, digest, in})
		}
	}
	entry := map[string]any{"kind": dealSaleExtension}
	exts, _ := value["extensions"].(map[string]interface{})
	carrier, _ := exts[dealSaleExtension].(map[string]interface{})
	carried, _ := carrier["threads"].(map[string]interface{})
	if !isSale || saleKey == "" {
		entry["status"], entry["findings"] = "fail", []string{"not_a_sale_bundle"}
		return entry, aacbundle.ClaimResult{Status: "fail", Findings: []string{"not_a_sale_bundle"}}
	}
	sort.Slice(registrations, func(i, j int) bool { return registrations[i].seq < registrations[j].seq })
	section, _ := exts[dealProfile].(map[string]interface{})
	listed, _ := section["sale_threads"].([]interface{})
	if section["threads_predate_registration"] == true {
		notShown = append(notShown, "threads_predate_registration")
	}
	if _, ok := section["sale_threads"]; !ok {
		notShown = append(notShown, "no_sale_threads")
	}
	if len(listed) != len(registrations) {
		fail("sale_threads_do_not_match_the_registrations")
	}
	present := map[string]string{} // thread id -> registration digest
	var shown []any
	for i, raw := range listed {
		e, _ := raw.(map[string]interface{})
		ref, _ := e["registration"].(map[string]interface{})
		regDigest, _ := ref["digest"].(string)
		member, _ := e["member"].(string)
		if i >= len(registrations) || regDigest != registrations[i].digest {
			fail("sale_threads_do_not_match_the_registrations")
			continue
		}
		body, _ := registrations[i].record["body"].(map[string]interface{})
		threadID, _ := e["thread_id"].(string)
		switch member {
		case saleThreadMissing:
			notShown = append(notShown, "thread_not_shown:"+regDigest)
		case saleThreadPresent, saleThreadNeverOpened:
			nonce, _ := e["nonce"].(string)
			got, err := commitText(nonce, threadID)
			if err != nil || got != body["thread_ref_commitment"] || !dealIDPattern.MatchString(threadID) {
				fail("registration_opening_does_not_match:" + regDigest)
				continue
			}
			if member == saleThreadPresent {
				present[threadID] = regDigest
			}
		default:
			fail("unknown_thread_state:" + regDigest)
		}
		shown = append(shown, map[string]any{"registration": regDigest, "member": member})
	}
	if len(carried) != len(present) {
		fail("carried_threads_are_not_the_present_ones")
	}
	ids := make([]string, 0, len(carried))
	for id := range carried {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		regDigest, ok := present[id]
		if !ok {
			fail("carried_threads_are_not_the_present_ones")
			continue
		}
		tb, _ := carried[id].(map[string]interface{})
		verdict, _ := bundleVerdict(tb, directory)
		switch verdict {
		case "VALID":
		case "INCOMPLETE":
			notShown = append(notShown, "thread_incomplete:"+id)
		default:
			fail("thread_invalid:" + id)
			continue
		}
		if why := threadBinding(tb, id, regDigest, saleKey); why != "" {
			fail(why + ":" + id)
		}
	}
	entry["threads"] = shown
	status := "pass"
	switch {
	case failed:
		status = "fail"
	case len(notShown) > 0:
		status = "incomplete"
	}
	all := append(append([]string{}, findings...), notShown...)
	entry["status"], entry["findings"] = status, nonNilStrings(all)
	claim := aacbundle.ClaimResult{Status: "pass", Findings: nil}
	if status != "pass" {
		claim = aacbundle.ClaimResult{Status: map[bool]string{true: "fail", false: "not_shown"}[failed], Findings: all}
	}
	return entry, claim
}

// threadBinding checks a carried thread is the registered one: its task
// authority is of that thread, names that registration, and its sale
// authority opening (the user's own copy carries it) recomputes to its
// sale_authority_commitment and names this sale's task authority. "" when
// it holds.
func threadBinding(tb map[string]interface{}, threadID, regDigest, saleKey string) string {
	disclosures, _ := tb["disclosures"].(map[string]interface{})
	var ta map[string]interface{}
	taDigest := ""
	for _, d := range disclosures {
		in, _ := d.(map[string]interface{})["agent_input"].(map[string]interface{})
		if in["type"] == typeTaskAuthority && in["chain_id"] == threadID {
			if body, _ := in["body"].(map[string]interface{}); body["sale_authority_commitment"] != nil {
				ta, taDigest = in, jcsDigest(in)
			}
		}
	}
	if ta == nil {
		return "thread_has_no_task_authority_of_this_thread"
	}
	names := false
	refs, _ := ta["refs"].([]interface{})
	for _, raw := range refs {
		r, _ := raw.(map[string]interface{})
		if r["rel"] == "registration" && r["digest"] == regDigest {
			names = true
		}
	}
	if !names {
		return "thread_does_not_name_its_registration"
	}
	report := sealedOrInlineReport(tb)
	opening, _ := report["sale_authority_opening"].(map[string]interface{})
	nonce, _ := opening["nonce"].(string)
	text, _ := opening["text"].(string)
	body, _ := ta["body"].(map[string]interface{})
	got, err := commitText(nonce, text)
	if err != nil || got != body["sale_authority_commitment"] || text != saleKey || opening["record_digest"] != taDigest {
		return "thread_is_not_under_this_sale"
	}
	return ""
}

// sealedOrInlineReport is a deal bundle's report: the sealed report its
// x-deal-v0 extension names, or the extension itself in a copy whose report
// was not sealed.
func sealedOrInlineReport(b map[string]interface{}) map[string]interface{} {
	exts, _ := b["extensions"].(map[string]interface{})
	report, _ := exts[dealProfile].(map[string]interface{})
	if id, ok := report[dealReportPointer].(string); ok {
		disclosures, _ := b["disclosures"].(map[string]interface{})
		d, _ := disclosures[id].(map[string]interface{})
		in, _ := d["agent_input"].(map[string]interface{})
		sealed, _ := in["report"].(map[string]interface{})
		return sealed
	}
	return report
}

// jcsDigest is the lowercase-hex SHA-256 of a value's JCS bytes, as deal
// records are digested; "" when it cannot be canonicalized.
func jcsDigest(v any) string {
	raw, err := jcsOf(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
