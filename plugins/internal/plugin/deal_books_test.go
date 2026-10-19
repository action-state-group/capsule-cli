package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/action-state-group/evidencebook"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// legacyDeals makes the deals a test opens legacy deals, as a release from
// before books opened them, for the tests of the legacy step path (prepared
// steps, index rows, recovery): a store keeps both kinds (note §3.1).
func legacyDeals(t *testing.T) {
	t.Helper()
	old := dealNewDealsOnBooks
	dealNewDealsOnBooks = false
	t.Cleanup(func() { dealNewDealsOnBooks = old })
}

func dealProfile(t *testing.T) cli.Profile {
	t.Helper()
	p, err := cli.LoadProfile("deal")
	require.NoError(t, err)
	return p
}

// bookRecords are the committed records of a deal's book, read from the
// shelf as the step source reads them.
func bookRecords(t *testing.T, dealID string) []evidencebook.ShelfRecord {
	t.Helper()
	p, err := cli.LoadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	read, err := s.shelf.Records(t.Context(), cli.DealLogID(dealID))
	require.NoError(t, err)
	return read.Records
}

// bookOpening is a record's opening of field, read as the step source reads
// it.
func bookOpening(t *testing.T, r evidencebook.ShelfRecord, field string) string {
	t.Helper()
	p, err := cli.LoadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	text, err := s.index.(shelfDealIndex).opening(t.Context(), r, field)
	require.NoError(t, err)
	return text
}

// A deal opened now is a book: each step a record the deal profile sealed
// itself, with the header §2.2 sets field by field, and no deal_steps row.
func TestANewDealsStepsAreRecordsOnItsBook(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":38000}`))
	dealRun(t, "close", "--deal", dealID, "--input", writeJSON(t, `{"status":"received"}`))
	assert.Zero(t, storeCount(t, dealProfile(t), `SELECT COUNT(*) FROM deal_steps WHERE deal_id = ?`, dealID), "a book deal has no index row")

	records := bookRecords(t, dealID)
	require.NotEmpty(t, records)
	seen := map[string]bool{}
	var parent string
	for i, r := range records {
		h := r.Header
		assert.Equal(t, uint64(i+1), r.Seq)
		assert.Equal(t, dealStepRecordType, h.RecordType)
		assert.Equal(t, evidencebook.ProducerClaim, h.EpistemicType)
		assert.Equal(t, cli.DealLogID(dealID), h.BookID)
		assert.Equal(t, evidencebook.Available, h.RetentionState)
		assert.Equal(t, evidencebook.CommitAlgNonce256, h.CommitAlg)
		assert.Len(t, h.PayloadCommitments, 1, "the x-deal-v0 record, and nothing else")
		require.Len(t, h.FieldCommitments, 3, "every step commits one field-name set")
		for _, field := range []string{dealLocalField, dealAmountField, dealCurrencyField} {
			c, ok := h.FieldCommitments[field]
			require.True(t, ok, field)
			assert.Len(t, c, 64)
			assert.False(t, seen[c], "every commitment has its own nonce")
			seen[c] = true
		}
		var ev dealEvent
		require.NoError(t, json.Unmarshal([]byte(bookOpening(t, r, dealLocalField)), &ev))
		assert.Equal(t, int64(i+1), ev.N)
		assert.Equal(t, ev.At, h.CommittedAt, "one time per record: the step's own")
		assert.Equal(t, dealID+"/"+strconv.FormatInt(ev.N, 10), h.SubjectRef)
		if i == 0 {
			assert.Empty(t, h.Links)
		} else {
			assert.Equal(t, []evidencebook.Link{{Type: evidencebook.Cites, Target: parent}}, h.Links)
		}
		parent = r.RecordID
		amount, currency := bookOpening(t, r, dealAmountField), bookOpening(t, r, dealCurrencyField)
		if ev.Kind == "close" {
			assert.Equal(t, "38000", amount)
			assert.Equal(t, "USD", currency)
		} else {
			assert.Empty(t, amount, "only a received close carries money terms")
			assert.Empty(t, currency)
		}
	}
	// No authenticated index: the log holds the records and nothing else.
	assert.Equal(t, len(records), storeCount(t, dealProfile(t), `SELECT COUNT(*) FROM cll_entries WHERE log_id = ?`, cli.DealLogID(dealID)))
	assert.Equal(t, len(records), len(dealLogEntries(t, dealID)))
}

// A sealed report is a record too: a constant type, no subject_ref (every
// report shares one action id), no link and no field commitment.
func TestASealedReportIsARecordOnTheDealsBook(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	steps := len(bookRecords(t, dealID))
	for range 2 {
		sharedCopy(t, dealID, cli.DealAudienceCounterparty, "the shop")
	}
	records := bookRecords(t, dealID)
	require.Len(t, records, steps+2)
	for _, r := range records[steps:] {
		h := r.Header
		assert.Equal(t, dealReportRecordType, h.RecordType)
		assert.Empty(t, h.SubjectRef)
		assert.Empty(t, h.Links)
		assert.Empty(t, h.FieldCommitments)
		assert.Len(t, h.PayloadCommitments, 1)
	}
	p, err := cli.LoadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	ids, err := s.index.reportIDs(t.Context(), dealID)
	require.NoError(t, err)
	assert.Len(t, ids, 2)
	n, err := s.index.reportsThrough(t.Context(), dealID, uint64(steps+1))
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

// A record states one time, its step's own: committed_at equals the step's
// at even when the clock moves on while the step is sealed.
func TestCommittedAtIsTheStepsTimeWhenTheClockMovesOn(t *testing.T) {
	dealFixture(t)
	start := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	calls := 0
	cli.DealClock = func() time.Time { calls++; return start.Add(time.Duration(calls) * 1500 * time.Millisecond) }
	t.Cleanup(func() { cli.DealClock = func() time.Time { return time.Now().UTC() } })
	dealID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":38000}`))
	for _, r := range bookRecords(t, dealID) {
		var ev dealEvent
		require.NoError(t, json.Unmarshal([]byte(bookOpening(t, r, dealLocalField)), &ev))
		assert.Equal(t, ev.At, r.Header.CommittedAt)
	}
}

// A book's first append puts its log in the cadence, counted in H and live,
// in the same step.
func TestABooksFirstAppendEntersTheCadence(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	assert.Equal(t, 1, storeCount(t, dealProfile(t), `SELECT COUNT(*) FROM cadence_logs WHERE log_id = ?`, cli.DealLogID(dealID)))
	assert.Equal(t, 1, storeCount(t, dealProfile(t), `SELECT COUNT(*) FROM cadence_live WHERE log_id = ?`, cli.DealLogID(dealID)))
}

// A shared copy carries every step's capsule, withheld ones included, and
// each capsule its header inline (a record of the bundle is its capsule): the headers of steps of different kinds
// have one shape, the same field names with digests of one length, and no
// two digests alike, so a withheld step's header never says which step it
// is (note §2.2, §2.9).
func TestWithheldStepsHeadersHaveOneShape(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":38000}`))
	dealRun(t, "close", "--deal", dealID, "--input", writeJSON(t, `{"status":"received"}`))
	b, _ := sharedCopy(t, dealID, cli.DealAudienceCounterparty, "the shop")
	records, _ := b["records"].([]any)
	require.NotEmpty(t, records)
	digests := map[string]bool{}
	var names []string
	steps := 0
	for _, raw := range records {
		header, _ := raw.(map[string]any)[evidencebook.RecordHeaderMember].(map[string]any)
		require.NotNil(t, header, "every capsule carries its header inline")
		if header["record_type"] != dealStepRecordType {
			continue
		}
		steps++
		fields, _ := header["field_commitments"].(map[string]any)
		var these []string
		for name, digest := range fields {
			these = append(these, name)
			assert.Len(t, digest, 64)
			assert.False(t, digests[digest.(string)])
			digests[digest.(string)] = true
		}
		slices.Sort(these)
		if names == nil {
			names = these
		}
		assert.Equal(t, names, these, "every step's header has the same field names")
	}
	assert.GreaterOrEqual(t, steps, 3, "open, act and close are all in the copy")
	assert.Equal(t, []string{dealAmountField, dealCurrencyField, dealLocalField}, names)
}

// A store holds legacy deals and book deals together (note §3.1), and every
// reader across deals answers from both through one step source (§2.8): the
// cross-deal act history keeps the order the acts were appended in, whichever
// kind of deal each is on.
func TestReadersSpanLegacyAndBookDealsTogether(t *testing.T) {
	dealFixture(t)
	dealNewDealsOnBooks = false
	t.Cleanup(func() { dealNewDealsOnBooks = true })
	legacyID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	dealNewDealsOnBooks = true
	bookID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	pay := writeJSON(t, `{"action":"pay","amount_minor":38000}`)
	first := dealRun(t, "note", "--deal", legacyID, "--kind", "act", "--input", pay)
	second := dealRun(t, "note", "--deal", bookID, "--kind", "act", "--input", pay)
	third := dealRun(t, "note", "--deal", legacyID, "--kind", "act", "--input", pay)

	p := dealProfile(t)
	assert.Positive(t, storeCount(t, p, `SELECT COUNT(*) FROM deal_steps WHERE deal_id = ?`, legacyID))
	assert.Zero(t, storeCount(t, p, `SELECT COUNT(*) FROM deal_steps WHERE deal_id = ?`, bookID))
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	acts, err := s.index.steps(t.Context(), stepFilter{Kinds: []string{"act"}, Indexed: true})
	require.NoError(t, err)
	ids, err := s.index.dealIDs(t.Context())
	require.NoError(t, err)
	require.NoError(t, s.close())
	var order []string
	for _, st := range acts {
		order = append(order, st.CapsuleID)
	}
	assert.Equal(t, []string{first["capsule_id"].(string), second["capsule_id"].(string), third["capsule_id"].(string)}, order)
	assert.ElementsMatch(t, []string{legacyID, bookID}, ids)

	out, err := invoke(t, "", "--profile", "deal", "deal", "list", "--json")
	require.NoError(t, err, out)
	assert.Contains(t, out, legacyID)
	assert.Contains(t, out, bookID)
	open := dealRun(t, "deadlines")["open_deals"].([]any)
	assert.Len(t, open, 2)
	for _, id := range []string{legacyID, bookID} {
		dealRun(t, "export", "--deal", id, "--output", filepath.Join(t.TempDir(), "export.json"))
		_, raw := sharedCopy(t, id, cli.DealAudienceCounterparty, "the shop")
		path := filepath.Join(t.TempDir(), "copy.json")
		require.NoError(t, os.WriteFile(path, []byte(raw), 0o600))
		verified, err := invoke(t, "", "verify", "--bundle", path)
		require.NoError(t, err, verified)
		dealRun(t, "close", "--deal", id, "--input", writeJSON(t, `{"status":"received"}`))
	}
}

// A sale and each of its threads are books of their own (note §2.3), and
// nothing in one book's headers names or repeats anything in another's: no
// digest is shared and no link crosses books, so two threads are not
// linkable through the store's records.
func TestASalesThreadsAreBooksThatShareNothing(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	first := buyerThread(t, saleID, "buyer-a.example")
	second := buyerThread(t, saleID, "buyer-b.example")
	owner := map[string]string{}
	for _, id := range []string{saleID, first, second} {
		records := bookRecords(t, id)
		require.NotEmpty(t, records, id)
		for _, r := range records {
			owner[r.RecordID] = id
		}
	}
	digests := map[string]string{}
	for _, id := range []string{saleID, first, second} {
		for _, r := range bookRecords(t, id) {
			for _, link := range r.Header.Links {
				if book, ok := owner[link.Target]; ok {
					assert.Equal(t, id, book, "a link stays in its book")
				}
			}
			values := append([]string{}, r.Header.PayloadCommitments...)
			for _, c := range r.Header.FieldCommitments {
				values = append(values, c)
			}
			for _, v := range values {
				if other, ok := digests[v]; ok {
					assert.Equal(t, id, other, "a digest is in one book only")
				}
				digests[v] = id
			}
		}
	}
}

// A sale opened before books stays a legacy sale, and a thread opened under
// it now is a book: the sale's own steps stay on its log, the thread's on its
// book (note §2.3).
func TestALegacySaleGainsABookThread(t *testing.T) {
	dealFixture(t)
	dealNewDealsOnBooks = false
	t.Cleanup(func() { dealNewDealsOnBooks = true })
	saleID, _ := newSale(t)
	before := buyerThread(t, saleID, "buyer-a.example")
	dealNewDealsOnBooks = true
	after := buyerThread(t, saleID, "buyer-b.example")
	p := dealProfile(t)
	for _, id := range []string{saleID, before} {
		assert.Positive(t, storeCount(t, p, `SELECT COUNT(*) FROM deal_steps WHERE deal_id = ?`, id), id)
		assert.Empty(t, bookRecords(t, id), id)
	}
	assert.Zero(t, storeCount(t, p, `SELECT COUNT(*) FROM deal_steps WHERE deal_id = ?`, after))
	assert.NotEmpty(t, bookRecords(t, after))
	recs, _ := exportSale(t, saleID)
	assert.NotEmpty(t, recs)
	// The book thread loads and re-verifies as any deal does.
	dealRun(t, "export", "--deal", after, "--output", filepath.Join(t.TempDir(), "thread.json"))
}

// core get never prints a step's deal_local opening: an opening is an
// original the capsule does not bind (note §1).
func TestGetPrintsNoOpeningOfABookStep(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	r := bookRecords(t, dealID)[0]
	local := bookOpening(t, r, dealLocalField)
	for _, args := range [][]string{{}, {"--raw"}} {
		out, err := invoke(t, "", append([]string{"get", "--profile", "deal", "--capsule-id", r.RecordID}, args...)...)
		require.NoError(t, err, out)
		assert.NotContains(t, out, dealOpeningPrefix)
		assert.NotContains(t, out, local[:40])
	}
}

// A step whose seal fails while its book holds the position (a crash before
// the record is stored) leaves nothing: the next step takes the same
// position, and the deal loads as if the failed one was never tried.
func TestAStepThatFailsBeforeItIsStoredLeavesNothing(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	before := bookRecords(t, dealID)
	old := dealBookSealStops
	t.Cleanup(func() { dealBookSealStops = old })
	dealBookSealStops = func() error { return assert.AnError }
	_, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":38000}`))
	require.ErrorIs(t, err, assert.AnError)
	assert.Len(t, bookRecords(t, dealID), len(before))
	dealBookSealStops = old
	act := dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":38000}`))
	after := bookRecords(t, dealID)
	require.Len(t, after, len(before)+1)
	assert.Equal(t, uint64(len(before)+1), after[len(after)-1].Seq)
	assert.Equal(t, act["capsule_id"], after[len(after)-1].RecordID)
	dealRun(t, "export", "--deal", dealID, "--output", filepath.Join(t.TempDir(), "export.json"))
}

// The step source reads headers as the shelf stores them, without opening a
// book; it binds each to the header its signed Capsule carries inline before
// trusting a commitment, so a stored header changed outside capsulectl is a
// conflict, not a different step.
func TestAStoredHeaderThatIsNotTheCapsulesIsAConflict(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	p := dealProfile(t)
	db, _, err := cli.SqliteConnection(p)
	require.NoError(t, err)
	var raw []byte
	require.NoError(t, db.QueryRow(`SELECT header FROM book_records WHERE book_id = ? AND seq = 1`, cli.DealLogID(dealID)).Scan(&raw))
	var header map[string]any
	require.NoError(t, json.Unmarshal(raw, &header))
	header["committed_at"] = "2020-01-01T00:00:00Z"
	changed, err := json.Marshal(header)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE book_records SET header = ? WHERE book_id = ? AND seq = 1`, changed, cli.DealLogID(dealID))
	require.NoError(t, err)
	require.NoError(t, db.Close())

	s, err := openDealStoreRead(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	_, err = s.index.steps(t.Context(), stepFilter{DealID: dealID})
	require.ErrorIs(t, err, cli.ErrConflict)
}

// A step stored but not yet in its log (the log append fails after the
// record is stored: a crash there) is the book's pending tail: the next open
// commits it at its position, and the deal loads with the step (note §2.2).
func TestAStepStoredButNotLoggedIsCommittedOnTheNextOpen(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	before := bookRecords(t, dealID)
	p := dealProfile(t)
	db, _, err := cli.SqliteConnection(p)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`CREATE TRIGGER deal_book_crash BEFORE INSERT ON cll_entries WHEN NEW.log_id = '` + cli.DealLogID(dealID) + `' BEGIN SELECT RAISE(ABORT, 'crash before the log append'); END`)
	require.NoError(t, err)
	_, err = invoke(t, "", "--profile", "deal", "deal", "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":38000}`))
	require.Error(t, err)
	_, err = db.Exec(`DROP TRIGGER deal_book_crash`)
	require.NoError(t, err)
	assert.Equal(t, 1, storeCount(t, p, `SELECT COUNT(*) FROM book_records WHERE book_id = ? AND seq = ?`, cli.DealLogID(dealID), len(before)+1), "the record is stored")
	assert.Len(t, bookRecords(t, dealID), len(before), "and not yet committed: the step source lists committed records only")
	// The next deal command opens the book, which commits it.
	report := dealRun(t, "report", "--deal", dealID)
	assert.Contains(t, reportTexts(t, report, "anomalies"), "agent/skipped_check: Skipped the check: pay $380.00 (no check before this action)")
	assert.Len(t, bookRecords(t, dealID), len(before)+1, "the step is committed at its position")
}

// The milestone a book deal reports is the same MMR size a legacy deal's
// reports for the same steps: the book's checkpoint TreeSize is the CLL
// checkpoint's MMR size.
func TestABookDealsMilestoneIsTheLegacyMMRSize(t *testing.T) {
	dealFixture(t)
	sizes := map[bool]any{}
	for _, books := range []bool{false, true} {
		dealNewDealsOnBooks = books
		id := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
		dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":38000}`))
		closed := dealRun(t, "close", "--deal", id, "--input", writeJSON(t, `{"status":"received"}`))
		sizes[books] = closed["checkpoint"].(map[string]any)["checkpoint"]
	}
	dealNewDealsOnBooks = true
	assert.Equal(t, sizes[false], sizes[true])
}

// A book deal whose last committed record is gone from the store while its
// log still names it is refused, as a legacy deal with a dropped step is.
func TestABookDealMissingItsLastRecordIsRefused(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":38000}`))
	p := dealProfile(t)
	db, _, err := cli.SqliteConnection(p)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM book_records WHERE book_id = ? AND seq = 2`, cli.DealLogID(dealID))
	require.NoError(t, err)
	require.NoError(t, db.Close())
	_, err = invoke(t, "", "--profile", "deal", "deal", "report", "--deal", dealID)
	require.Error(t, err)
	_, err = invoke(t, "", "--profile", "deal", "deal", "close", "--deal", dealID, "--input", writeJSON(t, `{"status":"received"}`))
	require.Error(t, err)
}

// No command reaches the reconcile engine in PR3: there is no halves verb.
func TestNoCommandReachesTheHalvesEngine(t *testing.T) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		assert.NotEqual(t, "halves", c.Name())
		assert.NotContains(t, c.Short+c.Long, "halves")
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(testCommand())
	for _, sub := range DealCommands() {
		walk(sub)
	}
}

// A book deal's witness receipt is reported without re-entering the shelf:
// the cadence calls Ended under the shelf's lock, so a closed book deal's
// standing is read before (settleEnded). Its tick is witnessed and the
// ended deal leaves the live set, with no deadlock.
func TestABookDealsReceiptIsReportedAndItLeavesTheTick(t *testing.T) {
	p, _, witnessKey := rc16Store(t)
	cli.DealClock = func() time.Time { return time.Date(2026, 10, 19, 9, 28, 0, 0, time.UTC) }
	dealRun(t, "tick")
	bookID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	dealRun(t, "close", "--deal", bookID, "--input", writeJSON(t, `{"status":"received"}`))
	require.Contains(t, liveLogs(t, p), cli.DealLogID(bookID))
	cli.DealClock = func() time.Time { return time.Date(2026, 10, 19, 9, 30, 0, 0, time.UTC) }
	tick := dealRun(t, "tick")
	require.Equal(t, "ticked", tick["state"])
	// The receipt is reported in a session past its migration, holding the
	// step source over both kinds of deal, as deal tick's delivery reports
	// one: the reconcile on open would report it with the SQLite index only.
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	c, cp, err := s.cadenceOf(t.Context())
	require.NoError(t, err)
	storeReceipt(t, p, uint64(tick["checkpoint"].(float64)), witnessKey)
	done := make(chan error, 1)
	go func() { done <- s.witnessNativeTicks(t.Context(), c, cp) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("reporting a receipt for a tick holding a book deal did not return")
	}
	require.NoError(t, s.close())
	assert.NotContains(t, liveLogs(t, p), cli.DealLogID(bookID), "the ended, witnessed book deal leaves the tick")
}

// The cross-deal act history keeps the file's append order however the
// acts of legacy and book deals interleave: each step is sorted with its
// own key.
func TestInterleavedActsKeepTheirAppendOrder(t *testing.T) {
	dealFixture(t)
	dealNewDealsOnBooks = false
	t.Cleanup(func() { dealNewDealsOnBooks = true })
	legacyID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	dealNewDealsOnBooks = true
	bookID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	pay := writeJSON(t, `{"action":"pay","amount_minor":100}`)
	var want []string
	for _, id := range []string{legacyID, bookID, legacyID, bookID, legacyID} {
		want = append(want, dealRun(t, "note", "--deal", id, "--kind", "act", "--input", pay)["capsule_id"].(string))
	}
	var got []string
	withSession(t, func(s *dealSession) {
		acts, err := s.index.steps(t.Context(), stepFilter{Kinds: []string{"act"}, Indexed: true})
		require.NoError(t, err)
		for _, st := range acts {
			got = append(got, st.CapsuleID)
		}
	})
	assert.Equal(t, want, got)
}

// A stored header whose record_type was changed is a conflict, never a step
// silently left out: the binding runs before a reader filters by type.
func TestAStoredRecordTypeChangedIsAConflict(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	dealRun(t, "close", "--deal", dealID, "--input", writeJSON(t, `{"status":"received"}`))
	p := dealProfile(t)
	db, _, err := cli.SqliteConnection(p)
	require.NoError(t, err)
	var raw []byte
	var seq int
	require.NoError(t, db.QueryRow(`SELECT header, seq FROM book_records WHERE book_id = ? ORDER BY seq DESC LIMIT 1`, cli.DealLogID(dealID)).Scan(&raw, &seq))
	var header map[string]any
	require.NoError(t, json.Unmarshal(raw, &header))
	header["record_type"] = "something_else"
	changed, err := json.Marshal(header)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE book_records SET header = ? WHERE book_id = ? AND seq = ?`, changed, cli.DealLogID(dealID), seq)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	s, err := openDealStoreRead(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	_, err = s.index.steps(t.Context(), stepFilter{})
	require.ErrorIs(t, err, cli.ErrConflict)
}

// Cadence work is per live deal, never per deal the profile ever kept: once
// a closed book deal is witnessed and leaves the tick, a later tick and
// another deal's checkpoint status never read it, so even its originals
// gone from the store change nothing for them.
func TestARetiredBookIsNotReadByCadenceWork(t *testing.T) {
	p, _, witnessKey := rc16Store(t)
	cli.DealClock = func() time.Time { return time.Date(2026, 10, 19, 9, 28, 0, 0, time.UTC) }
	dealRun(t, "tick")
	retired := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	dealRun(t, "close", "--deal", retired, "--input", writeJSON(t, `{"status":"received"}`))
	cli.DealClock = func() time.Time { return time.Date(2026, 10, 19, 9, 30, 0, 0, time.UTC) }
	tick := dealRun(t, "tick")
	require.Equal(t, "ticked", tick["state"])
	deliver(t, p, uint64(tick["checkpoint"].(float64)), witnessKey)
	require.NotContains(t, liveLogs(t, p), cli.DealLogID(retired))
	active := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)

	// The retired book's originals are gone.
	db, _, err := cli.SqliteConnection(p)
	require.NoError(t, err)
	for _, r := range bookRecords(t, retired) {
		for _, table := range []string{"capsule_store_artifacts", "capsule_store_capsules"} {
			_, err = db.Exec(`DELETE FROM `+table+` WHERE capsule_id = ?`, r.RecordID)
			require.NoError(t, err)
		}
	}
	require.NoError(t, db.Close())

	cli.DealClock = func() time.Time { return time.Date(2026, 10, 19, 9, 30, 30, 0, time.UTC) }
	assert.Equal(t, "not_due", dealRun(t, "tick")["state"])
	out, err := invoke(t, "", "--profile", "deal", "deal", "checkpoint", "status", "--deal", active)
	require.NoError(t, err, out)
}

// A sale never ends, so it stays in the tick for good; settling which deals
// have ended never reads it, so even a live sale's originals gone from the
// store change nothing for a tick.
func TestALiveSaleIsNotReadToSettleEndedDeals(t *testing.T) {
	p, _, witnessKey := rc16Store(t)
	cli.DealClock = func() time.Time { return time.Date(2026, 10, 19, 9, 28, 0, 0, time.UTC) }
	dealRun(t, "tick")
	saleID, _ := newSale(t)
	require.Contains(t, liveLogs(t, p), cli.DealLogID(saleID))
	db, _, err := cli.SqliteConnection(p)
	require.NoError(t, err)
	for _, r := range bookRecords(t, saleID) {
		for _, table := range []string{"capsule_store_artifacts", "capsule_store_capsules"} {
			_, err = db.Exec(`DELETE FROM `+table+` WHERE capsule_id = ?`, r.RecordID)
			require.NoError(t, err)
		}
	}
	require.NoError(t, db.Close())
	cli.DealClock = func() time.Time { return time.Date(2026, 10, 19, 9, 30, 0, 0, time.UTC) }
	tick := dealRun(t, "tick")
	require.Equal(t, "ticked", tick["state"])
	deliver(t, p, uint64(tick["checkpoint"].(float64)), witnessKey)
	assert.Contains(t, liveLogs(t, p), cli.DealLogID(saleID), "a sale never ends")
}
