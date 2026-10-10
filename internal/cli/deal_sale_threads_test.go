package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A sale registers each of its threads on its own log, before the thread
// opens, and the user's own sale bundle carries every registered thread
// with proof that the list is whole.

// saleBundle writes the user's own sale bundle and returns it, decoded
// with numbers kept exact, and the command's result.
func saleBundle(t *testing.T, saleID string) (map[string]any, map[string]any) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "sale.json")
	res := dealRun(t, "sale", "bundle", "--sale", saleID, "--out", out)
	return decodeExact(t, mustRead(t, out)), res
}

func decodeExact(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var b map[string]any
	require.NoError(t, d.Decode(&b))
	return b
}

// verifySale writes b and verifies it, returning the verdict and the
// x-deal-sale/v0 entry's findings.
func verifySale(t *testing.T, b map[string]any) (string, []string) {
	t.Helper()
	result, _ := verifyBundleOutput(t, writeBundle(t, b))
	var findings []string
	for _, x := range result["extensions"].([]any) {
		if e := x.(map[string]any); e["kind"] == dealSaleExtension {
			for _, f := range e["findings"].([]any) {
				findings = append(findings, f.(string))
			}
		}
	}
	return result["verdict"].(string), findings
}

func saleSection(b map[string]any) (entries []any, threads map[string]any) {
	exts := b["extensions"].(map[string]any)
	entries = exts[dealProfile].(map[string]any)["sale_threads"].([]any)
	threads = exts[dealSaleExtension].(map[string]any)["threads"].(map[string]any)
	return entries, threads
}

// The thread record on the sale's log is sealed before its thread opens; it
// carries the thread's id only as a commitment, and the thread's task
// authority names it.
func TestAThreadIsRegisteredOnTheSalesLogBeforeItOpens(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	a := buyerThread(t, saleID, "buyer-a.example")

	recs, path := exportSale(t, saleID)
	checkProfile(t, path)
	require.Equal(t, []string{"sale", typeTaskAuthority, "thread", "thread_opened"}, saleRecordTypes(recs), "root, task authority, the registration, its opening")
	reg := recs[2]
	assert.Equal(t, "thread", reg["x-deal-v0"].(map[string]any)["record_type"])
	assert.Equal(t, []string{"thread_ref_commitment"}, keysOf(reg["body"].(map[string]any)))
	raw := mustJSONString(t, recs)
	assert.NotContains(t, raw, a, "the thread's id only as a commitment")
	opened, err := commitText(dealNonceOf(t, saleID, chainSteps(t, saleID)[2].CapsuleID, "thread_ref"), a)
	require.NoError(t, err)
	assert.Equal(t, reg["body"].(map[string]any)["thread_ref_commitment"], opened)

	thread := records(t, a)
	ta := thread[1]
	require.Equal(t, typeTaskAuthority, ta["type"])
	var names []string
	for _, r := range ta["refs"].([]any) {
		ref := r.(map[string]any)
		names = append(names, ref["rel"].(string)+" "+ref["digest"].(string))
	}
	assert.Contains(t, names, "registration "+recordDigest(t, reg))
	assert.LessOrEqual(t, reg["x-deal-v0"].(map[string]any)["at"], thread[0]["x-deal-v0"].(map[string]any)["at"], "registered first")
	_, export := exportRecords(t, a)
	checkProfile(t, export)
}

// A thread's task authority sealed after threads were registered, but
// naming no registration, is refused; one sealed before (no marker)
// re-derives unchanged, with no registration ref.
func TestAThreadTaskAuthorityWithoutItsRegistrationIsRefused(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	a := buyerThread(t, saleID, "buyer-a.example")
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.NoError(t, s.useDeal(t.Context(), a, false))
	events, err := s.load(t.Context(), a)
	require.NoError(t, err)
	ev := events[1].Event
	require.Equal(t, "task_authority", ev.Kind)
	require.NotEmpty(t, ev.SaleRegistration)

	ev.SaleRegistration = ""
	_, _, err = encodeDealRecord(ev, events[:1], s.dkey)
	require.ErrorIs(t, err, ErrInput, "registered era: a thread names its registration")

	ev.ThreadRegistration = ""
	raw, _, err := encodeDealRecord(ev, events[:1], s.dkey)
	require.NoError(t, err, "sealed before threads were registered")
	assert.NotContains(t, string(raw), `"registration"`)
}

// An open that failed after registering leaves a registration whose thread
// never opened; the next open reuses it rather than sealing another.
func TestARetriedOpenReusesAnUnopenedRegistration(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	orphan, _, err := s.registerThread(t.Context(), saleID, "")
	require.NoError(t, err)
	require.NoError(t, s.close())

	a := buyerThread(t, saleID, "buyer-a.example")
	assert.Equal(t, orphan, a, "the unopened registration is the thread's")
	recs, _ := exportSale(t, saleID)
	assert.Equal(t, []string{"sale", typeTaskAuthority, "thread", "thread_opened"}, saleRecordTypes(recs), "one registration, not two")
}

// Two threads: the sale bundle certifies the sale's whole log, carries both
// threads, and verifies with thread completeness.
func TestATwoThreadSaleBundleVerifiesWithThreadCompleteness(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	a := buyerThread(t, saleID, "buyer-a.example")
	b := buyerThread(t, saleID, "buyer-b.example")
	// A: an offer the buyer accepts, and the commit to it. B: an offer
	// accepted and committed to after A's.
	acc, err := acceptOffer(t, a, makeOffer(t, a, "190000"))
	require.NoError(t, err)
	require.NotEmpty(t, acc)
	require.Equal(t, false, commitNow(t, a, "190000")["unchecked"])
	_, err = acceptOffer(t, b, makeOffer(t, b, "185000"))
	require.NoError(t, err)
	commitNow(t, b, "185000")

	bundle, res := saleBundle(t, saleID)
	assert.Equal(t, "VALID", res["verdict"])
	assert.EqualValues(t, 2, res["present"])
	verdict, findings := verifySale(t, bundle)
	assert.Equal(t, "VALID", verdict)
	assert.Empty(t, findings)

	entries, threads := saleSection(bundle)
	require.Len(t, entries, 2)
	assert.ElementsMatch(t, []string{a, b}, keysOf(threads))
	for i, id := range []string{a, b} {
		e := entries[i].(map[string]any)
		assert.Equal(t, id, e["thread_id"], "in seal order")
		assert.Equal(t, saleThreadPresent, e["member"])
	}
	// The certificate covers the whole sale log: root, task authority and
	// both registrations.
	cert := bundle["completeness_certificate"].(map[string]any)
	assert.Equal(t, dealLogID(saleID), cert["log_id"])
	assert.Len(t, bundle["records"], 7, "root, task authority, two registrations, their openings, the cut")
}

// Every way the binding can break: a thread not shown is INCOMPLETE; a
// carried set that is not the present one, an opening that does not match,
// a thread under another registration, a carried thread that fails, or the
// extension on a bundle that is not a sale's, is INVALID.
func TestASaleBundleThatDoesNotBindItsThreadsIsNotValid(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	a := buyerThread(t, saleID, "buyer-a.example")
	b := buyerThread(t, saleID, "buyer-b.example")
	pristine, _ := saleBundle(t, saleID)
	raw, err := json.Marshal(pristine)
	require.NoError(t, err)
	fresh := func() map[string]any { return decodeExact(t, raw) }

	for name, c := range map[string]struct {
		edit    func(map[string]any)
		verdict string
		finding string
	}{
		"B withheld (missing)": {func(x map[string]any) {
			entries, threads := saleSection(x)
			e := entries[1].(map[string]any)
			e["member"] = saleThreadMissing
			delete(e, "nonce")
			delete(e, "thread_id")
			delete(threads, b)
		}, "INCOMPLETE", "thread_not_shown:"},
		"B present but not carried": {func(x map[string]any) {
			_, threads := saleSection(x)
			delete(threads, b)
		}, "INVALID", "carried_threads_are_not_the_present_ones"},
		"B carried but listed never_opened": {func(x map[string]any) {
			entries, _ := saleSection(x)
			entries[1].(map[string]any)["member"] = saleThreadNeverOpened
		}, "INVALID", "carried_threads_are_not_the_present_ones"},
		"an opening that does not match": {func(x map[string]any) {
			entries, _ := saleSection(x)
			entries[0].(map[string]any)["thread_id"], entries[1].(map[string]any)["thread_id"] = b, a
		}, "INVALID", "registration_opening_does_not_match"},
		"a registration left out of sale_threads": {func(x map[string]any) {
			exts := x["extensions"].(map[string]any)
			sec := exts[dealProfile].(map[string]any)
			sec["sale_threads"] = sec["sale_threads"].([]any)[:1]
			delete(exts[dealSaleExtension].(map[string]any)["threads"].(map[string]any), b)
		}, "INVALID", "sale_threads_do_not_match_the_registrations"},
		"threads carried under each other's ids": {func(x map[string]any) {
			_, threads := saleSection(x)
			threads[a], threads[b] = threads[b], threads[a]
		}, "INVALID", "thread_has_no_task_authority_of_this_thread"},
		"a carried thread that fails": {func(x map[string]any) {
			_, threads := saleSection(x)
			tb := threads[a].(map[string]any)
			for _, d := range tb["disclosures"].(map[string]any) {
				if in, ok := d.(map[string]any)["agent_input"].(map[string]any); ok && in["type"] == typeTaskAuthority {
					in["body"].(map[string]any)["max_total_minor"] = json.Number("1")
				}
			}
		}, "INVALID", "thread_invalid:"},
		"threads that predate registration": {func(x map[string]any) {
			x["extensions"].(map[string]any)[dealProfile].(map[string]any)["threads_predate_registration"] = true
		}, "INCOMPLETE", "threads_predate_registration"},
	} {
		t.Run(name, func(t *testing.T) {
			x := fresh()
			c.edit(x)
			verdict, findings := verifySale(t, x)
			assert.Equal(t, c.verdict, verdict)
			assert.Contains(t, strings.Join(findings, " "), c.finding)
			if name == "B withheld (missing)" {
				out, err := json.Marshal(x)
				require.NoError(t, err)
				assert.NotContains(t, string(out), b, "a withheld thread's id is not in the file")
			}
		})
	}

	// A buyer's own thread bundle relabelled as a sale bundle is not one.
	own := filepath.Join(t.TempDir(), "own.json")
	dealRun(t, "report", "--deal", a, "--bundle", own)
	thread := decodeExact(t, mustRead(t, own))
	thread["extensions"].(map[string]any)[dealSaleExtension] = map[string]any{"threads": map[string]any{}}
	verdict, findings := verifySale(t, thread)
	assert.Equal(t, "INVALID", verdict)
	assert.Contains(t, findings, "not_a_sale_bundle")
}

// A sale whose thread was opened before threads were registered says so:
// its sale bundle is INCOMPLETE, never VALID, and the old thread still
// re-derives.
func TestASaleWithAThreadFromBeforeRegistrationIsIncomplete(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	dealRegistersThreads = false
	old := buyerThread(t, saleID, "buyer-old.example")
	dealRegistersThreads = true
	buyerThread(t, saleID, "buyer-new.example")

	bundle, res := saleBundle(t, saleID)
	assert.Equal(t, true, res["threads_predate_registration"])
	assert.Equal(t, "INCOMPLETE", res["verdict"])
	verdict, findings := verifySale(t, bundle)
	assert.Equal(t, "INCOMPLETE", verdict)
	assert.Contains(t, findings, "threads_predate_registration")
	entries, threads := saleSection(bundle)
	assert.Len(t, entries, 1, "only the registered thread is listed")
	assert.NotContains(t, threads, old)

	_, export := exportRecords(t, old)
	checkProfile(t, export)
	for _, r := range records(t, old) {
		assert.NotContains(t, mustJSONString(t, r["refs"]), "registration")
	}
}

// No buyer's copy carries anything of the sale's list of threads: no
// registration but its own, no opening, no sale bundle section.
func TestABuyersCopyCarriesNothingOfTheSalesThreads(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	a := buyerThread(t, saleID, "buyer-a.example")
	b := buyerThread(t, saleID, "buyer-b.example")
	recs, _ := exportSale(t, saleID)
	regA, regB := recordDigest(t, recs[2]), recordDigest(t, recs[3])
	copies := map[string]string{}
	for id, buyer := range map[string]string{a: "buyer A", b: "buyer B"} {
		_, shared := sharedCopy(t, id, dealAudienceCounterparty, buyer)
		copies[id] = shared
		for _, word := range []string{"sale_threads", dealSaleExtension, "sale_authority_opening", "thread_ref_commitment", saleID} {
			assert.NotContains(t, shared, word, id)
		}
	}
	assert.Contains(t, copies[a], regA, "its own registration ref")
	assert.NotContains(t, copies[a], regB)
	assert.NotContains(t, copies[b], regA)
	// That no value in two buyers' copies links them, the registration refs
	// included, is TestABuyersCopyShowsTheOfferAndDoesNotLinkTheSalesThreads.
}

// writeSaleFixture writes a two-thread sale bundle for the engine's replay
// when CAPSULECTL_SALE_FIXTURE names a directory.
func TestWriteTheSaleBundleFixture(t *testing.T) {
	dir := os.Getenv("CAPSULECTL_SALE_FIXTURE")
	if dir == "" {
		t.Skip("CAPSULECTL_SALE_FIXTURE=DIR writes a two-thread sale bundle there")
	}
	dealFixture(t)
	saleID, _ := newSale(t)
	a := buyerThread(t, saleID, "buyer-a.example")
	b := buyerThread(t, saleID, "buyer-b.example")
	_, err := acceptOffer(t, a, makeOffer(t, a, "190000"))
	require.NoError(t, err)
	commitNow(t, a, "190000")
	_, err = acceptOffer(t, b, makeOffer(t, b, "185000"))
	require.NoError(t, err)
	commitNow(t, b, "185000")
	out := filepath.Join(dir, "sale-two-threads.json")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	res := dealRun(t, "sale", "bundle", "--sale", saleID, "--out", out)
	require.Equal(t, "VALID", res["verdict"])
}

// openThreadStoppingAt runs `deal open --sale` and stops it, as a crash
// would, at stage; the open fails and the hook is restored.
func openThreadStoppingAt(t *testing.T, saleID, stage string) {
	t.Helper()
	saved := dealOpenStopsAt
	dealOpenStopsAt = func(s string) error {
		if s == stage {
			return errors.New("stopped at " + s)
		}
		return nil
	}
	defer func() { dealOpenStopsAt = saved }()
	_, err := invoke(t, "", "--profile", "deal", "deal", "open", "--sale", saleID, "--input", writeJSON(t,
		`{"channel": "marketplace", "who": {"name": "Example Buyer", "domain": "buyer-a.example"}, "terms": {"price_minor": 190000}}`))
	require.Error(t, err)
}

// A crash right after the thread's key row is written, before anything of
// the thread is sealed: the retry reuses the registration (no second one),
// and the sale bundle lists the thread present and verifies.
func TestAnOpenStoppedAfterItsKeyRowIsTakenUpAgain(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	openThreadStoppingAt(t, saleID, "keyed")
	registered := chainSteps(t, saleID)[2].Event.Thread.ThreadID

	a := buyerThread(t, saleID, "buyer-a.example")
	assert.Equal(t, registered, a, "the retry opens the registered thread")
	recs, path := exportSale(t, saleID)
	checkProfile(t, path)
	assert.Equal(t, []string{"sale", typeTaskAuthority, "thread", "thread_opened"}, saleRecordTypes(recs), "one registration, not two")

	bundle, res := saleBundle(t, saleID)
	assert.Equal(t, "VALID", res["verdict"])
	entries, threads := saleSection(bundle)
	require.Len(t, entries, 1)
	assert.Equal(t, saleThreadPresent, entries[0].(map[string]any)["member"])
	assert.Contains(t, threads, a)
	verdict, findings := verifySale(t, bundle)
	assert.Equal(t, "VALID", verdict)
	assert.Empty(t, findings)
}

// A crash after the thread's baseline, before its task authority: that
// thread never opened (no task authority), so its registration is not
// reused; the retry registers a new thread, and the bundle lists the first
// never_opened and the second present, and verifies.
func TestAnOpenStoppedAfterItsBaselineLeavesTheRegistrationNeverOpened(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	openThreadStoppingAt(t, saleID, "baseline")
	first := chainSteps(t, saleID)[2].Event.Thread.ThreadID

	a := buyerThread(t, saleID, "buyer-a.example")
	assert.NotEqual(t, first, a)
	recs, _ := exportSale(t, saleID)
	assert.Equal(t, []string{"sale", typeTaskAuthority, "thread", "thread", "thread_opened"}, saleRecordTypes(recs), "a second registration; only the second opened")

	bundle, res := saleBundle(t, saleID)
	assert.Equal(t, "VALID", res["verdict"])
	assert.EqualValues(t, 1, res["never_opened"])
	entries, threads := saleSection(bundle)
	require.Len(t, entries, 2)
	assert.Equal(t, saleThreadNeverOpened, entries[0].(map[string]any)["member"])
	assert.Equal(t, saleThreadPresent, entries[1].(map[string]any)["member"])
	assert.Equal(t, []string{a}, keysOf(threads))
	verdict, _ := verifySale(t, bundle)
	assert.Equal(t, "VALID", verdict)
}

// saleRecordTypes is each record's type on a sale's log, in order.
func saleRecordTypes(recs []map[string]any) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		if blk, ok := r["x-deal-v0"].(map[string]any); ok {
			out[i] = blk["record_type"].(string)
		} else {
			out[i] = r["type"].(string)
		}
	}
	return out
}
