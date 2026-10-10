package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A sale bundle is cut fresh: it seals each thread's report, then a cut on
// the sale's log naming every thread's last record, then the checkpoint,
// so the certified interval covers every thread's acts up to the bundle.
// And the sale's log says which threads opened, so never_opened is
// checkable.

// twoThreadSale is a sale with two buyers' threads, each an offer the buyer
// accepted and the commit to it, B's after A's: every act sealed after both
// registrations.
func twoThreadSale(t *testing.T) (saleID, a, b string) {
	t.Helper()
	dealFixture(t)
	saleID, _ = newSale(t)
	a = buyerThread(t, saleID, "buyer-a.example")
	b = buyerThread(t, saleID, "buyer-b.example")
	for id, price := range map[string]string{a: "190000", b: "185000"} {
		_, err := acceptOffer(t, id, makeOffer(t, id, price))
		require.NoError(t, err)
		require.Equal(t, false, commitNow(t, id, price)["unchecked"])
	}
	return saleID, a, b
}

// saleBodies are the bodies of a sale bundle's records of one type, in order.
func saleBodies(t *testing.T, bundle map[string]any, recordType string) []map[string]any {
	t.Helper()
	disclosures := bundle["disclosures"].(map[string]any)
	var out []map[string]any
	for _, raw := range bundle["records"].([]any) {
		in, _ := disclosures[raw.(map[string]any)["capsule_id"].(string)].(map[string]any)["agent_input"].(map[string]any)
		if blk, ok := in[dealProfile].(map[string]any); ok && blk["record_type"] == recordType {
			out = append(out, in["body"].(map[string]any))
		}
	}
	return out
}

// The order is the threads' reports, the cut, the checkpoint: the cut is
// the sale log's last record and the certified checkpoint covers it, and
// each thread's head is its last record, which is the report the bundle
// step sealed in it. A cut before the reports would name an earlier
// record, and this test fails.
func TestASaleBundleSealsTheReportsThenTheCutThenTheCheckpoint(t *testing.T) {
	saleID, a, b := twoThreadSale(t)
	bundle, res := saleBundle(t, saleID)
	require.Equal(t, "VALID", res["verdict"])

	recs := bundle["records"].([]any)
	assert.Equal(t, []string{"sale", typeTaskAuthority, "thread", "thread_opened", "thread", "thread_opened", "sale_cut"},
		saleRecordTypes(exportedSale(t, saleID)), "the cut is the sale log's last record")
	cert := bundle["completeness_certificate"].(map[string]any)
	assert.Equal(t, dealLogID(saleID), cert["log_id"])
	lastID := recs[len(recs)-1].(map[string]any)["capsule_id"].(string)
	lastIn := bundle["disclosures"].(map[string]any)[lastID].(map[string]any)["agent_input"].(map[string]any)
	assert.Equal(t, "sale_cut", lastIn[dealProfile].(map[string]any)["record_type"], "the bundle's last certified record is the cut")
	result, _ := verifyBundleOutput(t, writeBundle(t, bundle))
	assert.Equal(t, "pass", status(result, "per_record_membership"), "under the checkpoint cut after it")

	entries, threads := saleSection(bundle)
	cut := saleBodies(t, bundle, "sale_cut")
	require.Len(t, cut, 1)
	heads := cut[0]["thread_heads"].([]any)
	require.Len(t, heads, 2)
	for i, id := range []string{a, b} {
		tb := threads[id].(map[string]any)
		last := lastRecordDigest(tb)
		assert.Equal(t, dealReportActionID, actionIDOf(t, tb, last), "%s: the head is the report the bundle step sealed", id)
		e := entries[i].(map[string]any)
		head := e["head"].(map[string]any)
		assert.Equal(t, last, head["record_digest"], id)
		assert.True(t, saleOpensTo(head, last, heads[i].(map[string]any)["head_commitment"].(string)), id)
	}
	verdict, findings := verifySale(t, bundle)
	assert.Equal(t, "VALID", verdict)
	assert.Empty(t, findings)
}

// exportedSale is the sale's log as `deal sale export` writes it.
func exportedSale(t *testing.T, saleID string) []map[string]any {
	t.Helper()
	recs, _ := exportSale(t, saleID)
	return recs
}

// actionIDOf is the action_id of the capsule in a bundle that commits to
// the record digest.
func actionIDOf(t *testing.T, tb map[string]any, digest string) any {
	t.Helper()
	for _, raw := range tb["records"].([]any) {
		r := raw.(map[string]any)
		ca := r["model_attestation"].(map[string]any)["compute_attestation"].(map[string]any)
		if ca["agent_input_digest"] == digest {
			return r["action_id"]
		}
	}
	return nil
}

// Each bundle cuts again: a later bundle's checkpoint covers acts sealed
// after the first, its cut names the threads' new last records, and a
// thread carried from the earlier bundle (a copy cut short) is refused.
func TestEachSaleBundleCutsAgainAndRefusesAThreadCutShort(t *testing.T) {
	saleID, a, _ := twoThreadSale(t)
	first, _ := saleBundle(t, saleID)
	dealRun(t, "note", "--deal", a, "--kind", "message", "--input", writeJSON(t, `{"from":"counterparty","channel":"app_chat","text":"Picking it up Saturday"}`))
	second, res := saleBundle(t, saleID)
	require.Equal(t, "VALID", res["verdict"])

	sizeOf := func(b map[string]any) int64 {
		n, err := b["checkpoint"].(map[string]any)["mmr_size"].(json.Number).Int64()
		require.NoError(t, err)
		return n
	}
	assert.Greater(t, sizeOf(second), sizeOf(first), "a fresh checkpoint, past the first cut")
	assert.Len(t, saleBodies(t, second, "sale_cut"), 2)
	_, firstThreads := saleSection(first)
	_, secondThreads := saleSection(second)
	assert.NotEqual(t, lastRecordDigest(firstThreads[a].(map[string]any)), lastRecordDigest(secondThreads[a].(map[string]any)))

	secondThreads[a] = firstThreads[a]
	verdict, findings := verifySale(t, second)
	assert.Equal(t, "INVALID", verdict)
	assert.Contains(t, strings.Join(findings, " "), "thread_not_whole_at_the_cut:"+a)
}

// never_opened is checkable: an entry listed never_opened whose thread the
// sale's log says opened is refused; an opening that does not match the
// carried thread is refused; a present thread the latest cut does not name
// is not shown.
func TestTheSalesLogMakesOpenedCheckable(t *testing.T) {
	saleID, a, b := twoThreadSale(t)
	pristine, _ := saleBundle(t, saleID)
	raw, err := json.Marshal(pristine)
	require.NoError(t, err)
	for name, c := range map[string]struct {
		edit    func(map[string]any)
		verdict string
		finding string
	}{
		"B listed never_opened, though its opening is sealed": {func(x map[string]any) {
			entries, threads := saleSection(x)
			e := entries[1].(map[string]any)
			e["member"] = saleThreadNeverOpened
			delete(e, "opened")
			delete(e, "head")
			delete(threads, b)
		}, "INVALID", "never_opened_but_opened:"},
		"an opening that is not A's task authority": {func(x map[string]any) {
			entries, _ := saleSection(x)
			entries[0].(map[string]any)["opened"] = entries[1].(map[string]any)["opened"]
		}, "INVALID", "opening_does_not_match_the_thread:" + a},
		"B's opening withheld, B listed never_opened and dropped": {func(x map[string]any) {
			hideB(x)
			withhold(t, x, "thread_opened", registrationOf(x, 1))
		}, "INVALID", "never_opened_but_in_the_cut:"},
		"B's opening and the cut withheld, B listed never_opened and dropped": {func(x map[string]any) {
			hideB(x)
			withhold(t, x, "thread_opened", registrationOf(x, 1))
			withhold(t, x, "sale_cut", "")
		}, "INCOMPLETE", "never_opened_unprovable:"},
		"a head that is not A's last record": {func(x map[string]any) {
			entries, _ := saleSection(x)
			entries[0].(map[string]any)["head"] = entries[1].(map[string]any)["head"]
		}, "INVALID", "thread_not_whole_at_the_cut:" + a},
	} {
		t.Run(name, func(t *testing.T) {
			x := decodeExact(t, raw)
			c.edit(x)
			verdict, findings := verifySale(t, x)
			assert.Equal(t, c.verdict, verdict)
			assert.Contains(t, strings.Join(findings, " "), c.finding)
		})
	}
}

// hideB lists the second thread never_opened and drops it from the bundle.
func hideB(x map[string]any) {
	entries, threads := saleSection(x)
	e := entries[1].(map[string]any)
	delete(threads, e["thread_id"].(string))
	e["member"] = saleThreadNeverOpened
	delete(e, "opened")
	delete(e, "head")
}

// registrationOf is the registration digest of a sale bundle's i-th entry.
func registrationOf(x map[string]any, i int) string {
	entries, _ := saleSection(x)
	return entries[i].(map[string]any)["registration"].(map[string]any)["digest"].(string)
}

// withhold drops the disclosure of the sale-log record of recordType (one
// naming registration, when given): the record stays certified, unread.
func withhold(t *testing.T, x map[string]any, recordType, registration string) {
	t.Helper()
	disclosures := x["disclosures"].(map[string]any)
	for id, d := range disclosures {
		in, _ := d.(map[string]any)["agent_input"].(map[string]any)
		blk, _ := in[dealProfile].(map[string]any)
		if blk["record_type"] != recordType {
			continue
		}
		if registration != "" && blk["refs"].([]any)[0].(map[string]any)["digest"] != registration {
			continue
		}
		delete(disclosures, id)
		return
	}
	t.Fatalf("no %s record to withhold", recordType)
}

// A crash between a thread's task authority and its opening on the sale's
// log: the thread opened, and the next sale bundle seals its opening before
// the cut, and verifies.
func TestASaleBundleSealsAnOpeningACrashLeftOut(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	openThreadStoppingAt(t, saleID, "authority")
	assert.Equal(t, []string{"sale", typeTaskAuthority, "thread"}, saleRecordTypes(exportedSale(t, saleID)), "the thread opened; its opening is not sealed yet")

	bundle, res := saleBundle(t, saleID)
	assert.Equal(t, "VALID", res["verdict"])
	assert.Equal(t, []string{"sale", typeTaskAuthority, "thread", "thread_opened", "sale_cut"}, saleRecordTypes(exportedSale(t, saleID)))
	entries, _ := saleSection(bundle)
	require.Len(t, entries, 1)
	assert.Equal(t, saleThreadPresent, entries[0].(map[string]any)["member"])
}

// A copy made before sales recorded openings and cuts reads INCOMPLETE,
// never INVALID: nothing in it is wrong, it just cannot show the threads
// opened or the cut.
func TestASaleCopyWithoutOpeningsOrACutIsIncomplete(t *testing.T) {
	dealSealsSaleEvidence = false
	defer func() { dealSealsSaleEvidence = true }()
	saleID, _, _ := twoThreadSale(t)
	bundle, res := saleBundle(t, saleID)
	assert.Equal(t, "INCOMPLETE", res["verdict"])
	verdict, findings := verifySale(t, bundle)
	assert.Equal(t, "INCOMPLETE", verdict)
	joined := strings.Join(findings, " ")
	assert.Contains(t, joined, "opened_not_evidenced:")
	assert.Contains(t, joined, "no_sale_cut")
}

// sealCutWith makes the sale bundle with a cut sealing only the heads keep
// returns: a readable, sealed cut that names fewer threads.
func sealCutWith(t *testing.T, saleID string, keep func([]dealThreadHead) []dealThreadHead) map[string]any {
	t.Helper()
	saved := dealSaleCutHeads
	dealSaleCutHeads = keep
	defer func() { dealSaleCutHeads = saved }()
	bundle, _ := saleBundle(t, saleID)
	return bundle
}

func findingsWith(findings []string, prefix string) []string {
	var out []string
	for _, f := range findings {
		if strings.HasPrefix(f, prefix) {
			out = append(out, f)
		}
	}
	return out
}

// One name per cut finding. A readable cut without a present thread's head
// says thread_not_in_the_cut for that thread; a cut with no heads says it
// for each; no readable cut (withheld, or one whose disclosure does not
// match what its record sealed, which is never read) says no_sale_cut,
// once.
func TestEachCutFindingHasOneName(t *testing.T) {
	saleID, a, b := twoThreadSale(t)

	one := sealCutWith(t, saleID, func(h []dealThreadHead) []dealThreadHead { return h[:1] })
	verdict, findings := verifySale(t, one)
	assert.Equal(t, "INCOMPLETE", verdict, "a sealed cut that names A only")
	assert.Equal(t, []string{"thread_not_in_the_cut:" + b}, findingsWith(findings, "thread_not_in_the_cut"))
	assert.Empty(t, findingsWith(findings, "no_sale_cut"))

	empty := sealCutWith(t, saleID, func([]dealThreadHead) []dealThreadHead { return []dealThreadHead{} })
	verdict, findings = verifySale(t, empty)
	assert.Equal(t, "INCOMPLETE", verdict, "a sealed cut with no heads")
	assert.ElementsMatch(t, []string{"thread_not_in_the_cut:" + a, "thread_not_in_the_cut:" + b}, findingsWith(findings, "thread_not_in_the_cut"))
	assert.Empty(t, findingsWith(findings, "no_sale_cut"))

	whole, _ := saleBundle(t, saleID)
	raw, err := json.Marshal(whole)
	require.NoError(t, err)

	withheld := decodeExact(t, raw)
	withholdLatestCut(t, withheld)
	verdict, findings = verifySale(t, withheld)
	assert.Equal(t, "INCOMPLETE", verdict, "the cut withheld")
	assert.Equal(t, []string{"no_sale_cut"}, findingsWith(findings, "no_sale_cut"), "once, not per thread")
	assert.Empty(t, findingsWith(findings, "thread_not_in_the_cut"))

	// Each latest cut's disclosed body edited: a head dropped, or a head
	// added. Neither matches what the cut's record sealed, so the bundle is
	// INVALID and the cut is never read.
	for name, edit := range map[string]func([]any) []any{
		"a head dropped": func(h []any) []any { return h[:1] },
		"a head added":   func(h []any) []any { return append(h, h[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			x := decodeExact(t, raw)
			cuts := saleBodies(t, x, "sale_cut")
			latest := cuts[len(cuts)-1]
			latest["thread_heads"] = edit(latest["thread_heads"].([]any))
			verdict, findings := verifySale(t, x)
			assert.Equal(t, "INVALID", verdict)
			assert.Equal(t, []string{"no_sale_cut"}, findingsWith(findings, "no_sale_cut"))
			assert.Empty(t, findingsWith(findings, "thread_not_in_the_cut"), "an unread cut has no threads to be absent from")
		})
	}
}

// withholdLatestCut drops the disclosure of the sale log's latest cut.
func withholdLatestCut(t *testing.T, x map[string]any) {
	t.Helper()
	disclosures := x["disclosures"].(map[string]any)
	best, id := -1.0, ""
	for cid, d := range disclosures {
		in, _ := d.(map[string]any)["agent_input"].(map[string]any)
		if blk, _ := in[dealProfile].(map[string]any); blk["record_type"] == "sale_cut" {
			if seq := recordSeq(blk); seq > best {
				best, id = seq, cid
			}
		}
	}
	require.NotEmpty(t, id)
	delete(disclosures, id)
}
