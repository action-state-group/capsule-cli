package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/disclosure"
	"github.com/action-state-group/agent-action-capsule/go/witness"
)

// dealSaleExtension carries a sale's threads in the user's own sale bundle:
// {"threads": {"<thread deal id>": <the thread's own evidence-bundle/v2>}}.
// It only transports them, each verified as its own Evidence Bundle; it is
// a private kind, never registered. Completeness comes from the sale log's
// own completeness certificate, which covers every thread registration, and
// the openings in the x-deal-v0 section's sale_threads, which bind each
// registration to its thread.
const DealSaleExtension = "x-deal-sale/v0"

// Each registered thread's state in a sale bundle (sale_threads[].member).
const (
	SaleThreadPresent     = "present"
	SaleThreadMissing     = "missing"
	SaleThreadNeverOpened = "never_opened"
)

// saleBundleEntry checks a sale bundle's threads: every thread registration
// on the sale log the bundle certifies has exactly one sale_threads entry
// that opens it; the threads x-deal-sale/v0 carries are exactly the present
// ones; and each carried thread verifies as its own Evidence Bundle, is that
// thread, names that registration and opens its sale_authority_commitment
// to this sale's task authority. A registration not shown (missing), or a
// sale whose threads predate registration, is not shown: INCOMPLETE. Any
// mismatch, or a carried thread that fails, fails the bundle. The
// extension on a bundle that is not a sale's is never read as completeness.
func saleBundleEntry(value map[string]interface{}, result aacbundle.VerificationResult, directory *witness.Directory) (map[string]any, aacbundle.ClaimResult) {
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
	openedBy := map[string]string{} // registration -> task_authority_commitment
	// The latest sale_cut this copy can read (disclosed, and matching what
	// its record sealed), and its heads. A cut whose disclosure does not
	// match is never read: it is no readable cut.
	cutRead := false
	cutHeads := map[string]string{} // registration -> head_commitment
	cutSeq := 0.0
	// Where each record sits on the sale's log: a record this copy cannot
	// read after the latest cut it can read may itself be a later cut, so
	// then no cut is read.
	cert, _ := value["completeness_certificate"].(map[string]interface{})
	memberships, _ := cert["memberships"].(map[string]interface{})
	logSeq := func(id string) float64 {
		m, _ := memberships[id].(map[string]interface{})
		coords, _ := m["log_coordinates"].(map[string]interface{})
		return RecordSeq(coords)
	}
	cutAt, lastUnread := 0.0, 0.0
	undisclosed := false
	records, _ := value["records"].([]interface{})
	for _, raw := range records {
		r, _ := raw.(map[string]interface{})
		id, _ := r["capsule_id"].(string)
		d, _ := disclosures[id].(map[string]interface{})
		in, _ := d["agent_input"].(map[string]interface{})
		if in == nil || !matched[id] {
			// A certified record this copy does not show: it could be any
			// thread's opening, so no never_opened can be shown.
			undisclosed = true
			if at := logSeq(id); at > lastUnread {
				lastUnread = at
			}
			continue
		}
		digest := JCSDigest(in)
		if in["type"] == TypeTaskAuthority {
			if _, sold := in["chain_id"].(string); sold && SaleIDPattern.MatchString(in["chain_id"].(string)) {
				saleKey = digest
			}
			continue
		}
		blk, _ := in[DealProfile].(map[string]interface{})
		switch blk["record_type"] {
		case "sale":
			isSale = true
		case "thread_opened":
			refs, _ := blk["refs"].([]interface{})
			if len(refs) == 1 {
				ref, _ := refs[0].(map[string]interface{})
				reg, _ := ref["digest"].(string)
				body, _ := in["body"].(map[string]interface{})
				openedBy[reg], _ = body["task_authority_commitment"].(string)
			}
		case "sale_cut":
			seq := RecordSeq(blk)
			if seq > cutSeq {
				cutRead, cutSeq, cutHeads, cutAt = true, seq, map[string]string{}, logSeq(id)
				body, _ := in["body"].(map[string]interface{})
				heads, _ := body["thread_heads"].([]interface{})
				for _, h := range heads {
					head, _ := h.(map[string]interface{})
					ref, _ := head["registration"].(map[string]interface{})
					reg, _ := ref["digest"].(string)
					cutHeads[reg], _ = head["head_commitment"].(string)
				}
			}
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
	if lastUnread > cutAt {
		cutRead, cutHeads = false, map[string]string{}
	}
	entry := map[string]any{"kind": DealSaleExtension}
	exts, _ := value["extensions"].(map[string]interface{})
	carrier, _ := exts[DealSaleExtension].(map[string]interface{})
	carried, _ := carrier["threads"].(map[string]interface{})
	if !isSale || saleKey == "" {
		entry["status"], entry["findings"] = "fail", []string{"not_a_sale_bundle"}
		return entry, aacbundle.ClaimResult{Status: "fail", Findings: []string{"not_a_sale_bundle"}}
	}
	sort.Slice(registrations, func(i, j int) bool { return registrations[i].seq < registrations[j].seq })
	section, _ := exts[DealProfile].(map[string]interface{})
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
	openings := map[string]map[string]interface{}{}
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
		case SaleThreadMissing:
			notShown = append(notShown, "thread_not_shown:"+regDigest)
		case SaleThreadPresent, SaleThreadNeverOpened:
			nonce, _ := e["nonce"].(string)
			got, err := CommitText(nonce, threadID)
			if err != nil || got != body["thread_ref_commitment"] || !DealIDPattern.MatchString(threadID) {
				fail("registration_opening_does_not_match:" + regDigest)
				continue
			}
			if member == SaleThreadPresent {
				present[threadID] = regDigest
				openings[threadID] = e
				break
			}
			// never_opened: no thread_opened for it anywhere in the
			// certified log, so none withheld, and no head in the cut.
			_, opened := openedBy[regDigest]
			_, inCut := cutHeads[regDigest]
			switch {
			case opened:
				fail("never_opened_but_opened:" + regDigest)
			case inCut:
				fail("never_opened_but_in_the_cut:" + regDigest)
			case undisclosed:
				notShown = append(notShown, "never_opened_unprovable:"+regDigest)
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
		verdict, _ := BundleVerdict(tb, directory)
		switch verdict {
		case "VALID":
		case "INCOMPLETE":
			notShown = append(notShown, "thread_incomplete:"+id)
		default:
			fail("thread_invalid:" + id)
			continue
		}
		why, authority, last := threadBinding(tb, id, regDigest, saleKey)
		if why != "" {
			fail(why + ":" + id)
			continue
		}
		// The sale's log says the thread opened, with this task authority,
		// and the latest cut names this thread's last record.
		e := openings[id]
		if commitment, ok := openedBy[regDigest]; !ok {
			notShown = append(notShown, "opened_not_evidenced:"+id)
		} else if o, _ := e["opened"].(map[string]interface{}); !SaleOpensTo(o, authority, commitment) {
			fail("opening_does_not_match_the_thread:" + id)
		}
		switch commitment, ok := cutHeads[regDigest]; {
		case !cutRead:
			// Said once, below: there is no cut for any thread to be in.
		case !ok:
			// Present, but the latest cut has no head for it: not shown.
			notShown = append(notShown, "thread_not_in_the_cut:"+id)
		default:
			h, _ := e["head"].(map[string]interface{})
			digest, _ := h["record_digest"].(string)
			if !SaleOpensTo(h, digest, commitment) || digest != last {
				fail("thread_not_whole_at_the_cut:" + id)
			}
		}
	}
	if !cutRead && len(present) > 0 {
		notShown = append(notShown, "no_sale_cut")
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
func threadBinding(tb map[string]interface{}, threadID, regDigest, saleKey string) (string, string, string) {
	disclosures, _ := tb["disclosures"].(map[string]interface{})
	var ta map[string]interface{}
	taDigest := ""
	for _, d := range disclosures {
		in, _ := d.(map[string]interface{})["agent_input"].(map[string]interface{})
		if in["type"] == TypeTaskAuthority && in["chain_id"] == threadID {
			if body, _ := in["body"].(map[string]interface{}); body["sale_authority_commitment"] != nil {
				ta, taDigest = in, JCSDigest(in)
			}
		}
	}
	if ta == nil {
		return "thread_has_no_task_authority_of_this_thread", "", ""
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
		return "thread_does_not_name_its_registration", "", ""
	}
	report := sealedOrInlineReport(tb)
	opening, _ := report["sale_authority_opening"].(map[string]interface{})
	nonce, _ := opening["nonce"].(string)
	text, _ := opening["text"].(string)
	body, _ := ta["body"].(map[string]interface{})
	got, err := CommitText(nonce, text)
	if err != nil || got != body["sale_authority_commitment"] || text != saleKey || opening["record_digest"] != taDigest {
		return "thread_is_not_under_this_sale", "", ""
	}
	return "", taDigest, LastRecordDigest(tb)
}

// lastRecordDigest is the record digest of a deal bundle's last record on
// its log: the capsule its completeness certificate places at the highest
// log seq (the copy's own sealed report, when the bundle step sealed one in
// the thread's log; its last step, when the thread's reports are off its log,
// capsule-cli#294), as its capsule commits to it (agent_input_digest),
// disclosed or not.
func LastRecordDigest(tb map[string]interface{}) string {
	cert, _ := tb["completeness_certificate"].(map[string]interface{})
	memberships, _ := cert["memberships"].(map[string]interface{})
	best, last := -1.0, ""
	for id, raw := range memberships {
		m, _ := raw.(map[string]interface{})
		coords, _ := m["log_coordinates"].(map[string]interface{})
		if seq := RecordSeq(coords); seq > best {
			best, last = seq, id
		}
	}
	records, _ := tb["records"].([]interface{})
	for _, raw := range records {
		r, _ := raw.(map[string]interface{})
		if r["capsule_id"] != last {
			continue
		}
		ma, _ := r["model_attestation"].(map[string]interface{})
		ca, _ := ma["compute_attestation"].(map[string]interface{})
		digest, _ := ca["agent_input_digest"].(string)
		return digest
	}
	return ""
}

// recordSeq is a record header's seq.
func RecordSeq(blk map[string]interface{}) float64 {
	switch n := blk["seq"].(type) {
	case json.Number:
		f, _ := n.Float64()
		return f
	case float64:
		return n
	}
	return 0
}

// saleOpensTo reports whether an opening's nonce, with text, recomputes to a
// commitment.
func SaleOpensTo(opening map[string]interface{}, text, commitment string) bool {
	nonce, _ := opening["nonce"].(string)
	got, err := CommitText(nonce, text)
	return err == nil && text != "" && got == commitment
}

// sealedOrInlineReport is a deal bundle's report: the sealed report its
// x-deal-v0 extension names, or the extension itself in a copy whose report
// was not sealed.
func sealedOrInlineReport(b map[string]interface{}) map[string]interface{} {
	report, _, _ := DealSealedReport(b)
	return report
}

// jcsDigest is the lowercase-hex SHA-256 of a value's JCS bytes, as deal
// records are digested; "" when it cannot be canonicalized.
func JCSDigest(v any) string {
	raw, err := JCSOf(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
