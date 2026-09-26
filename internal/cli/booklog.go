package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/action-state-group/capsule-emit-go/artifact"
	"github.com/action-state-group/cll-go/cll"
	clljsonl "github.com/action-state-group/cll-go/store/jsonl"
	"github.com/action-state-group/evidencebook"
)

// A jsonl profile has one log: its evidence book. `publish` and `cll append`
// commit a published capsule as a book record, keyed by the capsule's id as
// its subject and carrying the capsule and its producer envelope as payloads,
// so a bundle can disclose the capsule itself. A profile whose earlier
// cll.jsonl holds entries is migrated once by `store migrate`, which
// backfills them into the book in order; from then on cll.jsonl is retired
// and nothing writes it.
const (
	recordTypePublished  = "published_capsule"
	recordTypeBackfilled = "backfilled_entry"
	recordTypeMigration  = "log_migration"
)

// publishedRecord returns the book record that holds capsuleID, if any: a
// capsule published into the book, or one backfilled from the retired log.
func publishedRecord(ctx context.Context, book *evidencebook.Book, capsuleID string) (evidencebook.Record, bool, error) {
	records, err := book.Query(ctx, evidencebook.Filter{SubjectRef: capsuleID})
	if err != nil {
		return evidencebook.Record{}, false, err
	}
	for _, r := range records {
		if r.Header.RecordType == recordTypePublished || r.Header.RecordType == recordTypeBackfilled {
			return r, true, nil
		}
	}
	return evidencebook.Record{}, false, nil
}

// appendPublished commits a verified, sealed capsule to the book once and
// returns its position; publishing the same capsule again returns the
// position it already has.
func appendPublished(ctx context.Context, book *evidencebook.Book, record artifact.Record) (uint64, error) {
	if existing, ok, err := publishedRecord(ctx, book, record.CapsuleID); err != nil || ok {
		return existing.Seq, err
	}
	committed, err := book.Append(ctx, evidencebook.Entry{
		RecordType: recordTypePublished, EpistemicType: evidencebook.ProducerClaim,
		SubjectRef: record.CapsuleID,
		Payloads:   [][]byte{record.Capsule, record.ProducerEnvelope},
	})
	if err != nil && committed.RecordID == "" {
		return 0, ErrPending
	}
	return committed.Seq, err
}

// retiredLog is what a jsonl profile's pre-book cll.jsonl holds.
type retiredLog struct {
	entries    []cll.Entry
	checkpoint *cll.CheckpointState
}

func retiredLogPath(p Profile) string {
	return filepath.Join(p.Connection.Database, jsonlLogFile)
}

// readRetiredLog reads the profile's pre-book cll.jsonl, or nothing when
// there is none. Opening it takes that file's lock, so no older binary is
// writing it while it is read.
func readRetiredLog(ctx context.Context, p Profile) (out retiredLog, err error) {
	path := retiredLogPath(p)
	if _, err = os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return retiredLog{}, nil
	}
	log, err := clljsonl.Open(path)
	if err != nil {
		return retiredLog{}, err
	}
	defer func() { err = errors.Join(err, log.Close()) }()
	for after := uint64(0); ; {
		page, err := log.ScanEntries(ctx, after, cll.MaxScanLimit)
		if err != nil {
			return retiredLog{}, err
		}
		if len(page) == 0 {
			break
		}
		out.entries = append(out.entries, page...)
		after = page[len(page)-1].Seq
	}
	state, err := log.LoadCLL(ctx)
	if err != nil {
		return retiredLog{}, err
	}
	out.checkpoint = state.Checkpoint
	return out, nil
}

// migrationStatement is the body of the record `store migrate` seals once
// every retired entry is in the book.
type migrationStatement struct {
	RetiredLogID   string `json:"retired_log_id"`
	RetiredEntries uint64 `json:"retired_entries"`
	// The retired log's last signed checkpoint, when it had one: its MMR
	// size and the SHA-256 of the signed statement.
	RetiredCheckpointSize   uint64 `json:"retired_checkpoint_size,omitempty"`
	RetiredCheckpointDigest string `json:"retired_checkpoint_digest,omitempty"`
}

// backfillStatement is the body of one backfilled entry. provenance_mode
// has the AAC shape: the retired log asserted the entry at appended_at, and
// the book imported it later.
type backfillStatement struct {
	RetiredLogID   string             `json:"retired_log_id"`
	RetiredSeq     uint64             `json:"retired_seq"`
	ProvenanceMode backfillProvenance `json:"provenance_mode"`
}

type backfillProvenance struct {
	Mode             string            `json:"mode"`
	SourceRef        map[string]string `json:"source_ref"`
	SourceAssertedAt string            `json:"source_asserted_at"`
	ImportBatch      string            `json:"import_batch"`
	ImportedAt       string            `json:"imported_at"`
}

type migrateResult struct {
	LogID        string `json:"log_id"`
	RetiredLogID string `json:"retired_log_id"`
	Backfilled   uint64 `json:"backfilled"`
	Migration    string `json:"migration_record_id"`
}

// migrateStore moves a jsonl profile's pre-book cll.jsonl into its book,
// once, in order, and resumably. A retired log that was ever checkpointed
// needs a new log id: its signed checkpoints name its own tree under
// (log_id, checkpoint key), and the book's different tree under the same
// pair would read as a fork of it.
func migrateStore(ctx context.Context, p Profile, newLogID string, now time.Time) (_ migrateResult, err error) {
	retired, err := readRetiredLog(ctx, p)
	if err != nil {
		return migrateResult{}, err
	}
	if len(retired.entries) == 0 {
		return migrateResult{}, inputError("this profile's cll.jsonl holds no entries to migrate")
	}
	book := p
	if newLogID != "" {
		if !logName.MatchString(newLogID) {
			return migrateResult{}, inputError("invalid --log-id")
		}
		book.LogID = newLogID
	}
	if retired.checkpoint != nil && book.LogID == p.LogID {
		return migrateResult{}, inputError("the retired log was checkpointed under this log_id; give a new --log-id")
	}
	var artifacts artifactStore
	if p.Namespace != "" {
		t, openErr := openTarget(ctx, p, useArtifacts)
		if openErr != nil {
			return migrateResult{}, openErr
		}
		defer func() { err = errors.Join(err, t.close()) }()
		artifacts = t.artifacts
	}
	opened, err := openBookUnguarded(ctx, book, true)
	if err != nil {
		return migrateResult{}, err
	}
	defer func() { err = errors.Join(err, opened.release()) }()
	result := migrateResult{LogID: book.LogID, RetiredLogID: p.LogID, Backfilled: uint64(len(retired.entries))}
	if st, id, ok, err := migrationRecord(ctx, opened.book); err != nil || ok {
		if err == nil && st.RetiredEntries != result.Backfilled {
			err = errors.Join(ErrConflict, errors.New("the retired cll.jsonl changed after it was migrated"))
		}
		result.Migration = id
		return result, err
	}
	done, err := backfilledSoFar(ctx, opened.book, p.LogID, retired.entries)
	if err != nil {
		return migrateResult{}, err
	}
	for _, entry := range retired.entries[done:] {
		if err = appendBackfilled(ctx, opened.book, artifacts, p.LogID, entry, len(retired.entries), now); err != nil {
			return migrateResult{}, err
		}
	}
	st := migrationStatement{RetiredLogID: p.LogID, RetiredEntries: result.Backfilled}
	if retired.checkpoint != nil {
		digest := sha256.Sum256(retired.checkpoint.Bytes)
		st.RetiredCheckpointSize, st.RetiredCheckpointDigest = retired.checkpoint.Size, hex.EncodeToString(digest[:])
	}
	body, err := json.Marshal(st)
	if err != nil {
		return migrateResult{}, err
	}
	record, err := opened.book.Append(ctx, evidencebook.Entry{RecordType: recordTypeMigration, EpistemicType: evidencebook.SystemOfRecordFact, Statement: body})
	if err != nil {
		return migrateResult{}, err
	}
	result.Migration = record.RecordID
	if book.LogID != p.LogID {
		if err = saveProfile(book, true); err != nil {
			return migrateResult{}, err
		}
	}
	return result, nil
}

// backfilledSoFar counts the retired entries an interrupted migration
// already moved, and refuses a book holding anything else: migration must
// be the start of the book's history.
func backfilledSoFar(ctx context.Context, book *evidencebook.Book, retiredLogID string, entries []cll.Entry) (int, error) {
	records, err := book.Query(ctx, evidencebook.Filter{})
	if err != nil {
		return 0, err
	}
	done := 0
	for _, r := range records {
		switch r.Header.RecordType {
		case evidencebook.RecordTypeIndexRoot:
			continue
		case recordTypeBackfilled:
		default:
			return 0, inputError("this profile's book already holds records; migration must come first")
		}
		var st backfillStatement
		if err = json.Unmarshal(r.Header.Statement, &st); err != nil {
			return 0, err
		}
		if done >= len(entries) || st.RetiredLogID != retiredLogID || st.RetiredSeq != entries[done].Seq || r.Header.SubjectRef != hex.EncodeToString(entries[done].Value) {
			return 0, errors.Join(ErrConflict, errors.New("the book's backfilled entries do not match the retired log"))
		}
		done++
	}
	return done, nil
}

func appendBackfilled(ctx context.Context, book *evidencebook.Book, artifacts artifactStore, retiredLogID string, entry cll.Entry, total int, now time.Time) error {
	value := hex.EncodeToString(entry.Value)
	body, err := json.Marshal(backfillStatement{
		RetiredLogID: retiredLogID, RetiredSeq: entry.Seq,
		ProvenanceMode: backfillProvenance{
			Mode:             "backfilled",
			SourceRef:        map[string]string{"type": "x-retired-cll-entry", "digest_alg": "SHA-256", "digest": value},
			SourceAssertedAt: entry.AppendedAt.UTC().Format(time.RFC3339Nano),
			ImportBatch:      fmt.Sprintf("%s@%d", retiredLogID, total),
			ImportedAt:       now.UTC().Format(time.RFC3339Nano),
		},
	})
	if err != nil {
		return err
	}
	e := evidencebook.Entry{
		RecordType: recordTypeBackfilled, EpistemicType: evidencebook.SystemOfRecordFact,
		SubjectRef: value, EventTimeClaim: entry.AppendedAt, Statement: body,
	}
	// A retired entry is a capsule this profile published, or (for a
	// disclose act) a disclosure digest with no capsule behind it.
	if artifacts != nil {
		record, err := artifacts.Get(ctx, value)
		switch {
		case err == nil:
			e.Payloads = [][]byte{record.Capsule, record.ProducerEnvelope}
		case !errors.Is(err, artifact.ErrNotFound):
			return err
		}
	}
	_, err = book.Append(ctx, e)
	return err
}

func migrationRecord(ctx context.Context, book *evidencebook.Book) (migrationStatement, string, bool, error) {
	records, err := book.Query(ctx, evidencebook.Filter{RecordType: recordTypeMigration})
	if err != nil || len(records) == 0 {
		return migrationStatement{}, "", false, err
	}
	var st migrationStatement
	if err = json.Unmarshal(records[0].Header.Statement, &st); err != nil {
		return st, "", false, fmt.Errorf("migration record: %w", err)
	}
	return st, records[0].RecordID, true, nil
}

// requireNoRetiredEntries keeps the profile to one log: a book opens only
// when the profile's pre-book cll.jsonl holds nothing, or holds exactly the
// entries `store migrate` moved into this book.
func requireNoRetiredEntries(ctx context.Context, p Profile, book *evidencebook.Book) error {
	retired, err := readRetiredLog(ctx, p)
	if err != nil {
		return err
	}
	if len(retired.entries) == 0 {
		return nil
	}
	migrated, _, ok, err := migrationRecord(ctx, book)
	if err != nil {
		return err
	}
	if !ok {
		return inputError("this profile's cll.jsonl holds entries not yet in its book; run 'store migrate'")
	}
	if migrated.RetiredEntries != uint64(len(retired.entries)) {
		return errors.Join(ErrConflict, errors.New("the retired cll.jsonl changed after it was migrated"))
	}
	return nil
}

// bookEntry is one `cll list` item for a jsonl profile. capsule_id is the
// log entry itself (the book record's capsule id); published_capsule_id is
// the capsule a published or backfilled record holds.
type bookEntry struct {
	Sequence           uint64 `json:"sequence"`
	CapsuleID          string `json:"capsule_id"`
	RecordType         string `json:"record_type"`
	PublishedCapsuleID string `json:"published_capsule_id,omitempty"`
	AppendedAt         string `json:"appended_at"`
}

func listBook(ctx context.Context, book *evidencebook.Book, after, through uint64, limit int) ([]bookEntry, uint64, error) {
	records, err := book.Query(ctx, evidencebook.Filter{FromSeq: after + 1, ToSeq: through})
	if err != nil {
		return nil, 0, err
	}
	items := make([]bookEntry, 0, min(len(records), limit))
	next := after
	for _, r := range records {
		if len(items) == limit {
			break
		}
		item := bookEntry{Sequence: r.Seq, CapsuleID: r.RecordID, RecordType: r.Header.RecordType, AppendedAt: r.Header.CommittedAt}
		if r.Header.RecordType == recordTypePublished || r.Header.RecordType == recordTypeBackfilled {
			item.PublishedCapsuleID = r.Header.SubjectRef
		}
		items = append(items, item)
		next = r.Seq
	}
	return items, next, nil
}

// bookWitnessStore keeps witness-delivery state for a book's checkpoints in
// <book>/witness.json, with the same compare-and-set rules as the CLL
// backends, so `cll checkpoint status|publish` deliver a book checkpoint the
// way they deliver a CLL one. The book's log lock, held while the target is
// open, makes this process its only writer.
type bookWitnessStore struct {
	path string
}

func newBookWitnessStore(p Profile) bookWitnessStore {
	return bookWitnessStore{path: filepath.Join(p.Connection.Database, "book", "witness.json")}
}

func (s bookWitnessStore) load() ([]cll.WitnessState, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var states []cll.WitnessState
	if err = json.Unmarshal(raw, &states); err != nil {
		return nil, errors.Join(cll.ErrCorrupt, err)
	}
	return states, nil
}

func (s bookWitnessStore) save(states []cll.WitnessState) error {
	raw, err := json.Marshal(states)
	if err != nil {
		return err
	}
	_, statErr := os.Stat(s.path)
	return atomicFile(s.path, raw, statErr == nil)
}

// seed records a checkpoint as due for delivery to witnessID, once.
func (s bookWitnessStore) seed(witnessID string, size uint64, statement []byte, now time.Time) error {
	states, err := s.load()
	if err != nil {
		return err
	}
	for _, w := range states {
		if w.WitnessID == witnessID && w.CheckpointSize == size {
			return nil
		}
	}
	return s.save(append(states, cll.WitnessState{WitnessID: witnessID, CheckpointSize: size, Checkpoint: statement, NextAttemptAt: now.UTC()}))
}

func (s bookWitnessStore) PendingWitnesses(_ context.Context, now time.Time, limit int) ([]cll.WitnessState, error) {
	states, err := s.load()
	if err != nil {
		return nil, err
	}
	var out []cll.WitnessState
	for _, w := range states {
		if w.Receipt == nil && !w.Permanent && !w.NextAttemptAt.After(now) && len(out) < limit {
			out = append(out, w)
		}
	}
	return out, nil
}

func (s bookWitnessStore) GetWitness(_ context.Context, witnessID string, size uint64) (cll.WitnessState, error) {
	states, err := s.load()
	if err != nil {
		return cll.WitnessState{}, err
	}
	for _, w := range states {
		if w.WitnessID == witnessID && w.CheckpointSize == size {
			return w, nil
		}
	}
	return cll.WitnessState{}, cll.ErrNotFound
}

func (s bookWitnessStore) CommitWitness(_ context.Context, expectedAttempts uint32, next cll.WitnessState) error {
	states, err := s.load()
	if err != nil {
		return err
	}
	for i, w := range states {
		if w.WitnessID != next.WitnessID || w.CheckpointSize != next.CheckpointSize {
			continue
		}
		if w.Attempts != expectedAttempts || !bytes.Equal(w.Checkpoint, next.Checkpoint) {
			return fmt.Errorf("%w: witness state changed", cll.ErrContention)
		}
		states[i] = next
		return s.save(states)
	}
	return fmt.Errorf("%w: witness state changed", cll.ErrContention)
}

// checkpointBook cuts a book checkpoint for `cll checkpoint create` and, when
// the profile names a witness service, records it as due for delivery.
func checkpointBook(ctx context.Context, p Profile, book *evidencebook.Book, service string) (map[string]any, error) {
	cp, err := book.Checkpoint(ctx)
	if err != nil {
		return nil, err
	}
	record, err := verifyCheckpoint(p, cp.Statement)
	if err != nil {
		return nil, err
	}
	if service != "" {
		if err = newBookWitnessStore(p).seed(service, record.MMRSize, cp.Statement, time.Now()); err != nil {
			return nil, err
		}
	}
	return map[string]any{"checkpoint": record.MMRSize, "indexed_sequence": cp.Entries, "statement": cp.Statement, "log_id": p.LogID}, nil
}

// bookBundle builds a jsonl profile's Evidence Bundle from its book. root is
// a published capsule's id or a book record id. `bundle` discloses no
// header and no payload; `disclose` and `permalink` disclose headers and the
// payloads --payloads names. A book record has no agent_output member, so
// suppressing it is refused rather than silently meaning nothing.
func bookBundle(ctx context.Context, book *evidencebook.Book, root string, depth int, payloads string, suppress map[string]bool, disclose bool) (evidencebook.Bundle, error) {
	request := evidencebook.BundleRequest{Root: root, ClosureDepth: depth}
	if record, ok, err := publishedRecord(ctx, book, root); err != nil {
		return evidencebook.Bundle{}, err
	} else if ok {
		request.Root = record.RecordID
	}
	if !disclose {
		request.Suppress, request.WithholdPayloads = []string{evidencebook.HeaderMember}, true
	} else {
		switch payloads {
		case "all":
			request.Payloads = evidencebook.PayloadsAll
		case "selected":
			request.Payloads = evidencebook.PayloadsSelected
		default:
			return evidencebook.Bundle{}, inputError("--payloads must be all or selected")
		}
		if suppress["agent_output"] {
			return evidencebook.Bundle{}, inputError("a book record has no agent_output member to suppress")
		}
		if suppress["agent_input"] {
			request.Suppress = []string{evidencebook.HeaderMember}
		}
	}
	bundle, err := book.Bundle(ctx, request)
	if err != nil {
		return evidencebook.Bundle{}, bookError(err)
	}
	if _, err = evidencebook.VerifyBundle(bundle.JSON); err != nil {
		return evidencebook.Bundle{}, err
	}
	return bundle, nil
}
