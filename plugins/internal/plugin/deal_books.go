package plugin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/capsule-cli/internal/cli"
	emit "github.com/action-state-group/capsule-emit/go"
	"github.com/action-state-group/capsule-emit/go/artifact"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/action-state-group/evidencebook"
)

// A deal opened since capsule-cli#242 PR3 is a book on the store's shelf
// (design note §2.2): each step and sealed report is a record the deal
// profile seals itself (Book.AppendCapsule), with one constant record type
// per kind, so a withheld step's header never says which step it is.
const (
	dealStepRecordType   = "deal_step"
	dealReportRecordType = "deal_report"
	// dealStepCompleted and dealStepFailed are the Personal-Agent History
	// Profile's record kinds (capsule-cli#264). A later writer seals steps
	// with them, mapped at seal time; every reader takes them as steps now,
	// so a counterparty on this build admits that writer's halves.
	dealStepCompleted = "step_completed"
	dealStepFailed    = "step_failed"
	// dealLocalField commits a step's local event (its raw values and the
	// nonces of its x-deal-v0 commitments); its opening never leaves the
	// device.
	dealLocalField = "deal_local"
	// dealAmountField and dealCurrencyField are a final close's money terms
	// (note §2.9): set on a received close, empty on every other step, each
	// under its own nonce, so every step's header has one shape.
	dealAmountField   = "amount"
	dealCurrencyField = "currency"
)

// dealNewDealsOnBooks is whether a deal opened now is kept on a book; a test
// turns it off to stand in for a release from before books, whose new deals
// were legacy deals (note §3.1).
var dealNewDealsOnBooks = true

// shelfDealIndex is the step source over a store that holds both kinds of
// deal (note §2.8, §3.1): a deal with indexed rows in deal_steps is legacy
// and answered by the SQLite index; every other deal is a book and answered
// from the shelf's committed records and their deal_local openings, without
// opening a book. A deal is never both.
type shelfDealIndex struct {
	legacy    sqliteDealIndex
	shelf     *evidencebook.Shelf
	originals cli.ArtifactStore
	db        *sql.DB
}

// errBookStep is a write meant for the SQLite index on a deal kept on a
// book, whose steps and reports its book appends.
var errBookStep = fmt.Errorf("%w: a deal kept on a book takes its steps and reports through its book", cli.ErrConflict)

func (x shelfDealIndex) isLegacy(ctx context.Context, dealID string) (bool, error) {
	n, err := x.legacy.stepCount(ctx, dealID)
	return n > 0, err
}

func (x shelfDealIndex) stepCount(ctx context.Context, dealID string) (int, error) {
	if legacy, err := x.isLegacy(ctx, dealID); err != nil || legacy {
		return x.legacy.stepCount(ctx, dealID)
	}
	steps, err := x.bookSteps(ctx, dealID)
	return len(steps), err
}

// steps answers a filter from both kinds of deal. With Indexed, the order is
// the order the steps' leaves were appended across the file (see stepFilter),
// which a store holding books has only from the CLL entries; a store with no
// book keeps the SQLite index's own order, its rowid, unchanged.
func (x shelfDealIndex) steps(ctx context.Context, f stepFilter) ([]indexedStep, error) {
	if f.DealID != "" {
		if legacy, err := x.isLegacy(ctx, f.DealID); err != nil || legacy {
			return x.legacy.steps(ctx, f)
		}
	}
	legacy, err := x.legacy.steps(ctx, f)
	if err != nil {
		return nil, err
	}
	books, err := x.bookSteps(ctx, f.DealID)
	if err != nil {
		return nil, err
	}
	books = slices.DeleteFunc(books, func(st indexedStep) bool { return !f.selects(st) })
	if len(books) == 0 {
		return legacy, nil
	}
	out := append(legacy, books...)
	if !f.Indexed {
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].DealID != out[j].DealID {
				return out[i].DealID < out[j].DealID
			}
			return out[i].N < out[j].N
		})
		return out, nil
	}
	order, err := x.appendOrder(ctx, out)
	if err != nil {
		return nil, err
	}
	// Each step is sorted with its own key: the keys never move apart from
	// the steps they rank.
	ranked := make([]struct {
		key  int64
		step indexedStep
	}, len(out))
	for i := range out {
		ranked[i].key, ranked[i].step = order[i], out[i]
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].key < ranked[j].key })
	for i := range ranked {
		out[i] = ranked[i].step
	}
	return out, nil
}

// selects is the filter's test for one step, as the SQLite index applies it
// in SQL.
func (f stepFilter) selects(st indexedStep) bool {
	return (f.DealID == "" || st.DealID == f.DealID) && (f.ExceptDeal == "" || st.DealID != f.ExceptDeal) &&
		(f.N == 0 || st.N == f.N) && (f.CapsuleID == "" || st.CapsuleID == f.CapsuleID) &&
		(len(f.Kinds) == 0 || slices.Contains(f.Kinds, st.Kind))
}

// appendOrder is each step's place in the file's append order: the rowid of
// its log entry. A legacy step prepared but not yet logged comes after every
// logged one, in the SQLite index's order.
func (x shelfDealIndex) appendOrder(ctx context.Context, steps []indexedStep) ([]int64, error) {
	order := make([]int64, len(steps))
	for i, st := range steps {
		if st.Sequence == 0 {
			order[i] = int64(1)<<62 + int64(i)
			continue
		}
		err := x.db.QueryRowContext(ctx, `SELECT rowid FROM cll_entries WHERE log_id = ? AND seq = ?`, cli.DealLogID(st.DealID), st.Sequence).Scan(&order[i])
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: step %d of %s names log entry %d, which its log does not hold", cli.ErrConflict, st.N, st.DealID, st.Sequence)
		}
		if err != nil {
			return nil, err
		}
	}
	return order, nil
}

func (x shelfDealIndex) dealIDs(ctx context.Context) ([]string, error) {
	ids, err := x.legacy.dealIDs(ctx)
	if err != nil {
		return nil, err
	}
	books, err := x.bookDealIDs(ctx)
	if err != nil {
		return nil, err
	}
	ids = append(ids, books...)
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

func (x shelfDealIndex) prepareStep(ctx context.Context, st indexedStep) error {
	if legacy, err := x.legacyWrite(ctx, st.DealID); err != nil || !legacy {
		return err
	}
	return x.legacy.prepareStep(ctx, st)
}

func (x shelfDealIndex) publishedStep(ctx context.Context, dealID string, n int64, sequence uint64) error {
	if legacy, err := x.legacyWrite(ctx, dealID); err != nil || !legacy {
		return err
	}
	return x.legacy.publishedStep(ctx, dealID, n, sequence)
}

func (x shelfDealIndex) withdrawStep(ctx context.Context, dealID string, n int64) error {
	if legacy, err := x.legacyWrite(ctx, dealID); err != nil || !legacy {
		return err
	}
	return x.legacy.withdrawStep(ctx, dealID, n)
}

func (x shelfDealIndex) prepareReport(ctx context.Context, dealID, capsuleID, audience string) error {
	if legacy, err := x.legacyWrite(ctx, dealID); err != nil || !legacy {
		return err
	}
	return x.legacy.prepareReport(ctx, dealID, capsuleID, audience)
}

func (x shelfDealIndex) publishedReport(ctx context.Context, dealID, capsuleID string, sequence uint64) error {
	if legacy, err := x.legacyWrite(ctx, dealID); err != nil || !legacy {
		return err
	}
	return x.legacy.publishedReport(ctx, dealID, capsuleID, sequence)
}

func (x shelfDealIndex) withdrawReport(ctx context.Context, dealID, capsuleID string) error {
	if legacy, err := x.legacyWrite(ctx, dealID); err != nil || !legacy {
		return err
	}
	return x.legacy.withdrawReport(ctx, dealID, capsuleID)
}

// legacyWrite is whether a write to the SQLite index for dealID is for a
// legacy deal, which it is; a book deal's is refused (errBookStep). A deal
// with no step yet is legacy when its book holds no record either: its first
// step, prepared here, makes it one.
func (x shelfDealIndex) legacyWrite(ctx context.Context, dealID string) (bool, error) {
	legacy, err := x.isLegacy(ctx, dealID)
	if err != nil || legacy {
		return legacy, err
	}
	records, err := x.records(ctx, dealID)
	if err != nil || len(records) > 0 {
		return false, errors.Join(err, errBookStep)
	}
	return true, nil
}

func (x shelfDealIndex) reportIDs(ctx context.Context, dealID string) (map[string]bool, error) {
	if legacy, err := x.isLegacy(ctx, dealID); err != nil || legacy {
		return x.legacy.reportIDs(ctx, dealID)
	}
	reports, err := x.bookReports(ctx, dealID)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]bool, len(reports))
	for _, r := range reports {
		ids[r.capsuleID] = true
	}
	return ids, nil
}

func (x shelfDealIndex) reportsThrough(ctx context.Context, dealID string, leaves uint64) (int, error) {
	if legacy, err := x.isLegacy(ctx, dealID); err != nil || legacy {
		return x.legacy.reportsThrough(ctx, dealID, leaves)
	}
	reports, err := x.bookReports(ctx, dealID)
	n := 0
	for _, r := range reports {
		if r.seq <= leaves {
			n++
		}
	}
	return n, err
}

func (x shelfDealIndex) latestReport(ctx context.Context, dealID, audience string, leaves uint64) (string, uint64, bool, error) {
	if legacy, err := x.isLegacy(ctx, dealID); err != nil || legacy {
		return x.legacy.latestReport(ctx, dealID, audience, leaves)
	}
	reports, err := x.bookReports(ctx, dealID)
	if err != nil {
		return "", 0, false, err
	}
	for i := len(reports) - 1; i >= 0; i-- {
		if r := reports[i]; r.seq <= leaves && r.audience == audience {
			return r.capsuleID, r.seq, true, nil
		}
	}
	return "", 0, false, nil
}

func (x shelfDealIndex) reportAt(ctx context.Context, dealID, audience string, sequence uint64) (string, bool, error) {
	if legacy, err := x.isLegacy(ctx, dealID); err != nil || legacy {
		return x.legacy.reportAt(ctx, dealID, audience, sequence)
	}
	reports, err := x.bookReports(ctx, dealID)
	if err != nil {
		return "", false, err
	}
	for _, r := range reports {
		if r.seq == sequence && r.audience == audience {
			return r.capsuleID, true, nil
		}
	}
	return "", false, nil
}

// bookDealIDs are the deals and sales kept on books: the shelf's books named
// deal/<id>, one per deal (note §2.1).
func (x shelfDealIndex) bookDealIDs(ctx context.Context) ([]string, error) {
	books, err := x.shelf.Books(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, book := range books {
		if id, ok := dealOfBook(book); ok {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// dealResultsLogID names a deal's results book, where `deal terms-result`
// seals its Results (note §2.10 v7, Decision 7.1): a book of its own,
// never the deal's book deal/<id>, so no copy of the deal carries one.
func dealResultsLogID(dealID string) string { return cli.DealLogID(dealID) + "/results" }

// dealOfResultsBook is the deal a results book belongs to, from its id
// deal/<id>/results.
func dealOfResultsBook(bookID string) (string, bool) {
	rest, ok := strings.CutPrefix(bookID, "deal/")
	if !ok {
		return "", false
	}
	id, ok := strings.CutSuffix(rest, "/results")
	return id, ok && cli.DealIDPattern.MatchString(id)
}

// dealOfBook is the deal or sale a book holds, from its id deal/<id>.
func dealOfBook(bookID string) (string, bool) {
	id, ok := strings.CutPrefix(bookID, "deal/")
	return id, ok && (cli.DealIDPattern.MatchString(id) || cli.SaleIDPattern.MatchString(id))
}

// records are the committed records of one deal's book, or of every deal's
// when dealID is empty, read from the shelf without opening a book. Each is
// bound to the header its Capsule carries inline (held) before any reader
// uses its type or a commitment: a stored header changed outside capsulectl
// is a conflict, never a record silently left out or read as another kind.
// A pending tail, stored above its book's entry count, is not listed: the
// book's next open commits it (note §2.8).
func (x shelfDealIndex) records(ctx context.Context, dealID string) ([]evidencebook.ShelfRecord, error) {
	var ids []string
	if dealID != "" {
		ids = []string{cli.DealLogID(dealID)}
	}
	read, err := x.shelf.Records(ctx, ids...)
	if err != nil {
		return nil, err
	}
	records := slices.DeleteFunc(read.Records, func(r evidencebook.ShelfRecord) bool {
		_, ok := dealOfBook(r.BookID)
		return !ok
	})
	for _, r := range records {
		if _, err := x.held(ctx, r); err != nil {
			return nil, err
		}
	}
	return records, nil
}

// isDealStep is whether a book record's header type is a step's: the
// constant this build writes, or a History profile kind a later build
// writes (capsule-cli#264).
func isDealStep(recordType string) bool {
	switch recordType {
	case dealStepRecordType, dealStepCompleted, dealStepFailed:
		return true
	}
	return false
}

// stepOfDeal is whether a step's header belongs to the deal: its book is the
// deal's, and a correlation identifier it carries, if any, is the deal id
// (capsule-cli#264: a later build sets correlation.exchange_id on steps).
func stepOfDeal(h evidencebook.Header, dealID string) bool {
	if !isDealStep(h.RecordType) || h.BookID != cli.DealLogID(dealID) {
		return false
	}
	return h.Correlation == nil || h.Correlation.ExchangeID == "" || h.Correlation.ExchangeID == dealID
}

// dealStepHeader is the record type and correlation a step is sealed with.
// This build seals the constant type and no correlation; tests replace it
// to seal steps as the History profile's later writer will.
var dealStepHeader = func(dealID string, ev dealEvent) (string, evidencebook.Correlation) {
	return dealStepRecordType, evidencebook.Correlation{}
}

// bookSteps are one deal's steps on its book, or every book deal's when
// dealID is empty, each read back from its deal_local opening.
func (x shelfDealIndex) bookSteps(ctx context.Context, dealID string) ([]indexedStep, error) {
	records, err := x.records(ctx, dealID)
	if err != nil {
		return nil, err
	}
	var out []indexedStep
	for _, r := range records {
		if !isDealStep(r.Header.RecordType) {
			continue
		}
		st, err := x.bookStep(ctx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// bookStep is one step record as the index gives it: its local event from
// the deal_local opening, checked against the header's commitment, and the
// step it names checked against the record's book and position.
func (x shelfDealIndex) bookStep(ctx context.Context, r evidencebook.ShelfRecord) (indexedStep, error) {
	dealID, _ := dealOfBook(r.BookID)
	if !stepOfDeal(r.Header, dealID) {
		// A step correlated to another deal is no step of this one.
		return indexedStep{}, fmt.Errorf("%w: step record %s of %s is correlated to another deal", cli.ErrConflict, r.RecordID, dealID)
	}
	if r.Retention == evidencebook.Withheld || r.Retention == evidencebook.Deleted {
		// No deal verb sets a retention in #242 (note §2.8).
		return indexedStep{}, fmt.Errorf("%w: step record %s of %s is %s", cli.ErrConflict, r.RecordID, dealID, r.Retention)
	}
	local, err := x.opening(ctx, r, dealLocalField)
	if err != nil {
		return indexedStep{}, err
	}
	var ev dealEvent
	if err := cli.DecodeJSON([]byte(local), &ev); err != nil || ev.DealID != dealID {
		return indexedStep{}, fmt.Errorf("%w: step record %s's local event is not a step of %s", cli.ErrConflict, r.RecordID, dealID)
	}
	digest, err := bookStepDigest(r.Header)
	if err != nil {
		return indexedStep{}, err
	}
	return indexedStep{DealID: dealID, N: ev.N, Kind: ev.Kind, CapsuleID: r.RecordID, Sequence: r.Seq, Digest: digest, Local: local}, nil
}

// bookStepDigest is a step record's record digest, as the next step's prev
// names it: its one payload commitment, the SHA-256 of its x-deal-v0 record
// bytes, which is what encodeDealRecord gives.
func bookStepDigest(h evidencebook.Header) (string, error) {
	if len(h.PayloadCommitments) != 1 {
		return "", fmt.Errorf("%w: a step record commits exactly one payload, its x-deal-v0 record", cli.ErrConflict)
	}
	return h.PayloadCommitments[0], nil
}

// dealOpeningPrefix names a record's field openings among its originals
// (evidencebook commitment.go openingPrefix: "opening.<field>").
const dealOpeningPrefix = "opening."

// opening is the text of a record's opening of field (openingOf).
func (x shelfDealIndex) opening(ctx context.Context, r evidencebook.ShelfRecord, field string) (string, error) {
	o, err := x.openingOf(ctx, r, field)
	return o.Text, err
}

// openingOf reads a record's opening of field from the artifact store and
// checks it opens the commitment the record's header carries.
func (x shelfDealIndex) openingOf(ctx context.Context, r evidencebook.ShelfRecord, field string) (evidencebook.Opening, error) {
	commitment, ok := r.Header.FieldCommitments[field]
	if !ok {
		return evidencebook.Opening{}, fmt.Errorf("%w: record %s commits no %s", cli.ErrConflict, r.RecordID, field)
	}
	held, err := x.held(ctx, r)
	if err != nil {
		return evidencebook.Opening{}, err
	}
	for _, a := range held.Artifacts {
		if a.Name != dealOpeningPrefix+field {
			continue
		}
		var o evidencebook.Opening
		if json.Unmarshal(a.Content, &o) != nil || !evidencebook.VerifyOpening(commitment, o) {
			return evidencebook.Opening{}, fmt.Errorf("%w: record %s's %s opening does not open its commitment", cli.ErrConflict, r.RecordID, field)
		}
		return o, nil
	}
	return evidencebook.Opening{}, fmt.Errorf("%w: the artifact store holds no %s opening of record %s", cli.ErrConflict, field, r.RecordID)
}

// held is a record's artifact record, once the header the shelf stores for
// it is the one its Capsule carries inline: Records reads headers as stored,
// without opening the book that checks them, so a reader binds them to the
// Capsule (whose signature load verifies) before trusting a commitment.
func (x shelfDealIndex) held(ctx context.Context, r evidencebook.ShelfRecord) (artifact.Record, error) {
	held, err := x.originals.Get(ctx, r.RecordID)
	if err != nil {
		return artifact.Record{}, err
	}
	var capsule map[string]json.RawMessage
	if err := json.Unmarshal(held.Capsule, &capsule); err != nil {
		return artifact.Record{}, fmt.Errorf("%w: record %s's Capsule: %v", cli.ErrConflict, r.RecordID, err)
	}
	inline, err := headerDigest(capsule[evidencebook.RecordHeaderMember])
	if err != nil {
		return artifact.Record{}, err
	}
	stored, err := json.Marshal(r.Header)
	if err != nil {
		return artifact.Record{}, err
	}
	if want, err := headerDigest(stored); err != nil || want != inline {
		return artifact.Record{}, errors.Join(err, fmt.Errorf("%w: record %s's stored header is not the one its Capsule carries", cli.ErrConflict, r.RecordID))
	}
	return held, nil
}

// headerDigest is the JCS digest of a header's JSON, so two encodings of one
// header compare equal.
func headerDigest(raw []byte) (string, error) {
	value, err := cli.DecodeBundleJSON(raw)
	if err != nil {
		return "", fmt.Errorf("%w: a record header: %v", cli.ErrConflict, err)
	}
	return canonical.JSONDigest(value)
}

// bookReport is one sealed report on a deal's book.
type bookReport struct {
	capsuleID, audience string
	seq                 uint64
}

// bookReports are a deal's sealed reports on its book, in log order, each
// with the audience its sealed record names.
func (x shelfDealIndex) bookReports(ctx context.Context, dealID string) ([]bookReport, error) {
	records, err := x.records(ctx, dealID)
	if err != nil {
		return nil, err
	}
	var out []bookReport
	for _, r := range records {
		if r.Header.RecordType != dealReportRecordType {
			continue
		}
		held, err := x.held(ctx, r)
		if err != nil {
			return nil, err
		}
		var report struct {
			Audience string `json:"audience"`
			DealID   string `json:"deal_id"`
		}
		found := false
		for _, a := range held.Artifacts {
			if len(r.Header.PayloadCommitments) == 1 && a.Content != nil && cli.BytesDigest(a.Content) == r.Header.PayloadCommitments[0] {
				found = json.Unmarshal(a.Content, &report) == nil
			}
		}
		if !found || report.DealID != dealID {
			return nil, fmt.Errorf("%w: report record %s is not a sealed report of %s", cli.ErrConflict, r.RecordID, dealID)
		}
		out = append(out, bookReport{capsuleID: r.RecordID, audience: report.Audience, seq: r.Seq})
	}
	return out, nil
}

// bookEntries is a book deal's log as its committed records give it, in
// order: each entry a record id, as the log holds it. steps leaves out the
// sealed reports.
func (x shelfDealIndex) bookEntries(ctx context.Context, dealID string, steps bool) ([]cll.Entry, error) {
	records, err := x.records(ctx, dealID)
	if err != nil {
		return nil, err
	}
	out := make([]cll.Entry, 0, len(records))
	for _, r := range records {
		if steps && !isDealStep(r.Header.RecordType) {
			continue
		}
		if dealOf, _ := dealOfBook(r.BookID); steps && !stepOfDeal(r.Header, dealOf) {
			return nil, fmt.Errorf("%w: step record %s of %s is correlated to another deal", cli.ErrConflict, r.RecordID, dealOf)
		}
		value, err := hex.DecodeString(r.RecordID)
		if err != nil {
			return nil, err
		}
		out = append(out, cll.Entry{Seq: r.Seq, Value: value})
	}
	return out, nil
}

// closedTerms are the money terms a step commits (note §2.9): a final close
// whose status is received, the one status that records a delivery, commits
// the delivered price and currency, or the agreed ones where the close names
// none; every other step, and a received close with no price or currency
// known, commits the empty text. The deposit is never the amount.
func closedTerms(events []sealedEvent, ev dealEvent) (amount, currency string, err error) {
	if ev.Kind != "close" || ev.Close == nil || ev.Close.Status != "received" {
		return "", "", nil
	}
	state, err := foldDeal(events)
	if err != nil {
		return "", "", err
	}
	price, cur := state.agreed.PriceMinor, state.agreed.Currency
	if d := ev.Close.Delivered; d != nil {
		if d.PriceMinor != nil {
			price = d.PriceMinor
		}
		if d.Currency != "" {
			cur = d.Currency
		}
	}
	if price == nil || cur == "" {
		return "", "", nil
	}
	return strconv.FormatInt(*price, 10), cur, nil
}

// sealBookStep seals a prepared step as a record on the deal's book (note
// §2.2): the step's Capsule, built as for a legacy deal, carries the record
// header the book assigns, and is sealed while the book holds the position,
// with the step's own time as the record's committed_at. The x-deal-v0
// record is its one payload; its local event, and its money terms, are
// salted field commitments whose openings stay in the artifact store.
func (s *dealSession) sealBookStep(ctx context.Context, dealID string, events []sealedEvent, ev dealEvent) (sealedEvent, error) {
	ev, err := s.fixStep(dealID, events, ev)
	if err != nil {
		return sealedEvent{}, err
	}
	request, digest, err := s.stepRequest(events, ev, dealCapsuleCurrent)
	if err != nil {
		return sealedEvent{}, err
	}
	local, err := json.Marshal(ev)
	if err != nil {
		return sealedEvent{}, err
	}
	amount, currency, err := closedTerms(events, ev)
	if err != nil {
		return sealedEvent{}, err
	}
	at, err := time.Parse("2006-01-02T15:04:05Z", ev.At)
	if err != nil {
		return sealedEvent{}, cli.ErrConflict
	}
	var links []evidencebook.Link
	if parent := request.Capsule.Chain; parent != nil {
		links = []evidencebook.Link{{Type: evidencebook.Cites, Target: parent.ParentCapsuleID}}
	}
	recordType, correlation := dealStepHeader(dealID, ev)
	record, err := s.appendBook(ctx, at, request, evidencebook.Entry{
		RecordType: recordType, EpistemicType: evidencebook.ProducerClaim, Correlation: correlation,
		SubjectRef: request.Capsule.ActionID, Links: links,
		Fields: map[string]string{dealLocalField: string(local), dealAmountField: amount, dealCurrencyField: currency},
	})
	if err != nil {
		return sealedEvent{}, err
	}
	if len(record.Header.PayloadCommitments) != 1 || record.Header.PayloadCommitments[0] != digest {
		return sealedEvent{}, cli.ErrConflict
	}
	return sealedEvent{CapsuleID: record.RecordID, Digest: digest, Sequence: record.Seq, Event: ev}, nil
}

// appendBook seals request as the next record of the deal's book, under the
// book's writer lock, with at as its committed_at: the shelf's clock gives
// at while the append runs (s.inFlight), so the record states one time, the
// Capsule's.
func (s *dealSession) appendBook(ctx context.Context, at time.Time, request cli.Request, entry evidencebook.Entry) (evidencebook.Record, error) {
	return s.appendTo(ctx, s.book, at, request, entry)
}

// appendTo is appendBook on any book of the profile's shelf: the deal's own,
// or its results book (note §2.10 v7).
func (s *dealSession) appendTo(ctx context.Context, book *evidencebook.Book, at time.Time, request cli.Request, entry evidencebook.Entry) (evidencebook.Record, error) {
	plain, err := cli.Seal(request, s.key)
	if err != nil {
		return evidencebook.Record{}, err
	}
	if len(cli.MissingBindings(plain)) != 0 {
		return evidencebook.Record{}, cli.ErrPartial
	}
	identity, err := emit.NewEd25519SigningIdentity(s.key)
	if err != nil {
		return evidencebook.Record{}, err
	}
	seal := func(_ evidencebook.Header, header []byte) (emit.Result, error) {
		if err := dealBookSealStops(); err != nil {
			return emit.Result{}, err
		}
		input := emit.SealInput{Capsule: request.Capsule, Identity: identity, Payload: request.Payload}
		input.Capsule.Extensions = append(append([]emit.Extension(nil), request.Capsule.Extensions...), emit.Extension{Name: evidencebook.RecordHeaderMember, Value: header})
		return emit.Seal(input)
	}
	s.inFlight = at.UTC()
	defer func() { s.inFlight = time.Time{} }()
	return book.AppendCapsule(ctx, entry, plain.Artifacts, seal)
}

// dealBookSealStops is a test hook: a seal that fails while the book holds
// the position, before anything is stored (a crash there).
var dealBookSealStops = func() error { return nil }

// sealBookReport seals a report record on the deal's book: the report's own
// Capsule, carrying its header, with no subject_ref (every report shares one
// action id) and no link (note §2.2).
func (s *dealSession) sealBookReport(ctx context.Context, at time.Time, request cli.Request) (string, error) {
	record, err := s.appendBook(ctx, at, request, evidencebook.Entry{RecordType: dealReportRecordType, EpistemicType: evidencebook.ProducerClaim})
	if err != nil {
		return "", err
	}
	return record.RecordID, nil
}

// loadBook reads a book deal's steps back and re-verifies each one, as load
// does a legacy deal's: the Capsule's signature and trust, the payload
// binding, the local step (its deal_local opening, checked against the
// header) re-derived into exactly the sealed record bytes, and the prev
// chain. Its book has already checked each record against its log on open.
func (s *dealSession) loadBook(ctx context.Context, dealID string) ([]sealedEvent, error) {
	index, err := s.index.steps(ctx, stepFilter{DealID: dealID})
	if err != nil {
		return nil, err
	}
	if len(index) == 0 {
		return nil, cli.InputError("unknown deal: " + dealID)
	}
	events := make([]sealedEvent, 0, len(index))
	for i, r := range index {
		var ev dealEvent
		if err = cli.DecodeJSON([]byte(r.Local), &ev); err != nil {
			return nil, cli.ErrConflict
		}
		prev := ""
		if i > 0 {
			prev = events[i-1].Digest
		}
		if ev.DealID != dealID || ev.N != int64(i+1) || ev.N != r.N || ev.Kind != r.Kind || ev.Prev != prev {
			return nil, cli.ErrConflict
		}
		content, err := s.verifiedPayload(ctx, r.CapsuleID)
		if err != nil {
			return nil, err
		}
		payload, digest, err := encodeDealRecord(ev, events, s.dkey)
		if err != nil || !bytes.Equal(payload, content) || digest != r.Digest {
			return nil, cli.ErrConflict
		}
		events = append(events, sealedEvent{CapsuleID: r.CapsuleID, Digest: digest, Sequence: r.Sequence, Event: ev})
	}
	return events, nil
}

// verifiedPayload is a step's sealed payload, once its Capsule verifies
// under the profile's trusted keys and binds it.
func (s *dealSession) verifiedPayload(ctx context.Context, capsuleID string) ([]byte, error) {
	record, err := s.t.Artifacts.Get(ctx, capsuleID)
	if err != nil {
		return nil, err
	}
	checks, err := artifact.Verify(record, s.keys)
	if err != nil {
		return nil, err
	}
	var content []byte
	for _, a := range record.Artifacts {
		if a.Binding == artifact.PayloadDigest && a.State == artifact.Present {
			content = a.Content
		}
	}
	if content == nil || !checks["payload"].Verified {
		return nil, cli.ErrConflict
	}
	return content, nil
}

// bookIndex is the step source's book side, when the current deal is kept
// on a book: a book deal's log is read from its committed records, never
// from its log directly (note §2.8).
func (s *dealSession) bookIndex() (shelfDealIndex, bool) {
	x, ok := s.index.(shelfDealIndex)
	return x, ok && s.onBook
}

// dealMilestoneStops is a test hook: a milestone that fails after its step
// is sealed (a crash between the two), leaving the step uncovered.
var dealMilestoneStops = func() error { return nil }

// cutMilestone cuts the deal's local checkpoint and returns its MMR size:
// its book's checkpoint for a book deal, the log's own otherwise.
func (s *dealSession) cutMilestone(ctx context.Context) (uint64, error) {
	if err := dealMilestoneStops(); err != nil {
		return 0, err
	}
	if s.book != nil {
		cp, err := s.book.Checkpoint(ctx)
		return cp.TreeSize, err
	}
	cp, err := cli.CutCheckpoint(ctx, localOnly(s.dp), s.t.Log)
	if err != nil {
		return 0, err
	}
	return cp.Size, nil
}

// books is the session's step source over both kinds of deal.
func (s *dealSession) books() shelfDealIndex {
	return shelfDealIndex{legacy: sqliteDealIndex{s.db}, shelf: s.shelf, originals: s.originals, db: s.db}
}

// settleEnded reads which live book deals have ended (a final close on their
// book), for the cadence's Ended predicate, which runs under the shelf's lock
// and so cannot read the shelf itself. It runs before a receipt is reported.
// Only the live set is read: a witnessed tick holds live logs only, and a log
// outside the live set has nothing for Ended to take it out of, so the work
// is per live deal, never per deal the profile ever kept (note §2.5). A live
// book sale is read too: it ends at its sale_close (capsule-cli#271).
func (s *dealSession) settleEnded(ctx context.Context, c *evidencebook.Cadence) error {
	x := s.books()
	live, err := c.Live(ctx)
	if err != nil {
		return err
	}
	ended := map[string]bool{}
	for _, logID := range live {
		// A live results book ends with its deal (note §2.10 v7, 7.1), so
		// its deal is settled even after the deal itself has left the live
		// set.
		id, ok := dealOfBook(logID)
		if !ok {
			id, ok = dealOfResultsBook(logID)
		}
		if _, done := ended[id]; !ok || done || !(cli.DealIDPattern.MatchString(id) || cli.SaleIDPattern.MatchString(id)) {
			continue
		}
		if legacy, err := x.isLegacy(ctx, id); err != nil || legacy {
			if err != nil {
				return err
			}
			continue
		}
		steps, err := x.bookSteps(ctx, id)
		if err != nil {
			return err
		}
		events := make([]sealedEvent, 0, len(steps))
		for _, st := range steps {
			var ev dealEvent
			if err := json.Unmarshal([]byte(st.Local), &ev); err != nil {
				return cli.ErrConflict
			}
			events = append(events, sealedEvent{Event: ev})
		}
		ended[id] = dealFinallyClosed(events)
	}
	s.bookEnded = ended
	return nil
}
