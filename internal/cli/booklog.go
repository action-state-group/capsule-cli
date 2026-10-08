package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/action-state-group/capsule-emit-go/artifact"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/action-state-group/checkpointed-local-log/go/mmr"
	clljsonl "github.com/action-state-group/checkpointed-local-log/go/store/jsonl"
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

// publishedRecord returns the book record that carries capsuleID's capsule,
// if any: one published into the book, or one backfilled from the retired
// log with the capsule as its payloads. A backfilled entry without payloads
// is a retired value only (it may be a disclosure digest), never a capsule.
func publishedRecord(ctx context.Context, book *evidencebook.Book, capsuleID string) (evidencebook.Record, bool, error) {
	records, err := book.Query(ctx, evidencebook.Filter{SubjectRef: capsuleID})
	if err != nil {
		return evidencebook.Record{}, false, err
	}
	for _, r := range records {
		if carriesCapsule(r.Header) {
			return r, true, nil
		}
	}
	return evidencebook.Record{}, false, nil
}

func carriesCapsule(h evidencebook.Header) bool {
	return h.RecordType == recordTypePublished || (h.RecordType == recordTypeBackfilled && len(h.PayloadCommitments) > 0)
}

// publishedCommitWarning is reported when a capsule's record committed but a
// later step of the append (indexing, an automatic checkpoint) failed. The
// position stands and must not be retried; the next open of the book
// rebuilds its indexes from the log.
const publishedCommitWarning = "committed; a later indexing or checkpoint step failed and is redone when the book is next opened"

// appendPublished commits a verified, sealed capsule to the book once and
// returns its position; publishing the same capsule again returns the
// position it already has. warning is non-empty only when the record
// committed but a later step failed.
func appendPublished(ctx context.Context, book *evidencebook.Book, record artifact.Record) (seq uint64, warning string, err error) {
	if existing, ok, err := publishedRecord(ctx, book, record.CapsuleID); err != nil || ok {
		return existing.Seq, "", err
	}
	committed, err := book.Append(ctx, evidencebook.Entry{
		RecordType: recordTypePublished, EpistemicType: evidencebook.ProducerClaim,
		SubjectRef: record.CapsuleID,
		Payloads:   [][]byte{record.Capsule, record.ProducerEnvelope},
	})
	switch {
	case err == nil:
		return committed.Seq, "", nil
	case committed.RecordID != "":
		return committed.Seq, publishedCommitWarning, nil
	case errors.Is(err, evidencebook.ErrInvalid) || errors.Is(err, evidencebook.ErrNotFound):
		return 0, "", bookError(err)
	default:
		// Stored but not yet committed: the book retries it first on its
		// next open, so the same publish can be retried.
		return 0, "", errors.Join(ErrPending, err)
	}
}

// retiredLog is what a jsonl profile's pre-book cll.jsonl holds.
type retiredLog struct {
	entries    []cll.Entry
	checkpoint *cll.CheckpointState
}

func retiredLogPath(p Profile) string {
	return filepath.Join(p.Connection.Database, jsonlLogFile)
}

// fingerprint commits to every retired entry (position, value, time) and
// to the retired log's last signed checkpoint, so any later change to the
// file -- an entry added or rewritten, or a new checkpoint cut by an older
// binary -- is detected, not only a change in its length.
func (r retiredLog) fingerprint() (entries, checkpoint string) {
	h := sha256.New()
	for _, e := range r.entries {
		fmt.Fprintf(h, "%d:%x:%s\n", e.Seq, e.Value, e.AppendedAt.UTC().Format(time.RFC3339Nano))
	}
	entries = hex.EncodeToString(h.Sum(nil))
	if r.checkpoint != nil {
		digest := sha256.Sum256(r.checkpoint.Bytes)
		checkpoint = hex.EncodeToString(digest[:])
	}
	return entries, checkpoint
}

// openRetiredLog reads the profile's pre-book cll.jsonl (nothing when there
// is none) and holds that file's lock until release, so no older binary
// writes it meanwhile.
func openRetiredLog(ctx context.Context, p Profile) (out retiredLog, release func() error, err error) {
	release = func() error { return nil }
	path := retiredLogPath(p)
	if _, err = os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return retiredLog{}, release, nil
	}
	log, err := clljsonl.Open(path)
	if err != nil {
		return retiredLog{}, release, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, log.Close())
		}
	}()
	for after := uint64(0); ; {
		page, err := log.ScanEntries(ctx, after, cll.MaxScanLimit)
		if err != nil {
			return retiredLog{}, release, err
		}
		if len(page) == 0 {
			break
		}
		out.entries = append(out.entries, page...)
		after = page[len(page)-1].Seq
	}
	state, err := log.LoadCLL(ctx)
	if err != nil {
		return retiredLog{}, release, err
	}
	out.checkpoint = state.Checkpoint
	return out, log.Close, nil
}

// scanRetiredLog reads the retired cll.jsonl the way readBookFiles reads the
// book: read-only, no lock, an incomplete final line ignored. Every command
// that only needs to check the retired log uses it, so a read-only file, an
// older binary holding the file's lock, or another reader never stands in a
// command's way (and a check never stands in theirs). Only `store migrate`,
// which must keep an older binary out while it copies, takes the lock.
func scanRetiredLog(p Profile) (retiredLog, error) {
	lines, err := completeLines(retiredLogPath(p))
	if errors.Is(err, os.ErrNotExist) {
		return retiredLog{}, nil
	}
	if err != nil {
		return retiredLog{}, err
	}
	var out retiredLog
	for _, line := range lines {
		var event struct {
			Type  string `json:"type"`
			Entry *struct {
				Seq        string `json:"seq"`
				Value      string `json:"value"`
				AppendedAt string `json:"appendedAt"`
			} `json:"entry"`
			State *struct {
				Checkpoint     *string `json:"checkpoint"`
				CheckpointSize *string `json:"checkpointSize"`
			} `json:"state"`
		}
		if err = json.Unmarshal(line, &event); err != nil {
			return retiredLog{}, errors.Join(cll.ErrCorrupt, err)
		}
		switch {
		case event.Type == "entry.append" && event.Entry != nil:
			seq, err := strconv.ParseUint(event.Entry.Seq, 10, 64)
			value, valueErr := base64.StdEncoding.DecodeString(event.Entry.Value)
			at, timeErr := time.Parse(time.RFC3339Nano, event.Entry.AppendedAt)
			if err != nil || valueErr != nil || timeErr != nil || seq != uint64(len(out.entries)+1) {
				return retiredLog{}, errors.Join(cll.ErrCorrupt, errors.New("the retired cll.jsonl is not a dense sequence of entries"))
			}
			out.entries = append(out.entries, cll.Entry{Seq: seq, Value: value, AppendedAt: at.UTC()})
		case event.Type == "cll.commit" && event.State != nil && event.State.Checkpoint != nil && event.State.CheckpointSize != nil:
			statement, err := base64.StdEncoding.DecodeString(*event.State.Checkpoint)
			size, sizeErr := strconv.ParseUint(*event.State.CheckpointSize, 10, 64)
			if err != nil || sizeErr != nil {
				return retiredLog{}, errors.Join(cll.ErrCorrupt, errors.New("the retired cll.jsonl holds a malformed checkpoint"))
			}
			out.checkpoint = &cll.CheckpointState{Bytes: statement, Size: size}
		}
	}
	return out, nil
}

// migrationStatement is the body of the record `store migrate` seals once
// every retired entry is in the book. The two fingerprints are
// retiredLog.fingerprint of the retired log as migrated.
type migrationStatement struct {
	RetiredLogID      string `json:"retired_log_id"`
	RetiredEntries    uint64 `json:"retired_entries"`
	EntriesDigest     string `json:"retired_entries_digest"`
	CheckpointDigest  string `json:"retired_checkpoint_digest,omitempty"`
	RetiredCheckpoint uint64 `json:"retired_checkpoint_size,omitempty"`
	// AnchoredEntries is how many retired entries, from the first, the
	// retired log's verified checkpoint commits.
	AnchoredEntries uint64 `json:"anchored_entries"`
}

func (st migrationStatement) matches(r retiredLog) bool {
	entries, checkpoint := r.fingerprint()
	return st.RetiredEntries == uint64(len(r.entries)) && st.EntriesDigest == entries && st.CheckpointDigest == checkpoint
}

var errRetiredLogChanged = hint(ErrConflict, "the retired cll.jsonl changed after it was migrated: something wrote it after the migration (an older capsulectl?). Restore it to what was migrated, from a backup, before using this profile again")

// backfillStatement is the body of one backfilled entry. provenance_mode
// has the AAC shape: the retired log asserted the entry at appended_at, and
// the book imported it later. source_ref.digest is the retired entry's
// value, itself a SHA-256 digest: a capsule id, or a disclosure digest.
type backfillStatement struct {
	RetiredLogID string `json:"retired_log_id"`
	RetiredSeq   uint64 `json:"retired_seq"`
	// Anchored is true when the retired log's checkpoint, verified under
	// the profile's trusted checkpoint key with its root recomputed from the
	// retired entries, commits this entry's value at its position; false for
	// an entry past that checkpoint or from a log that was never
	// checkpointed. It never covers the entry's time: the MMR commits values
	// only, so source_asserted_at is the retired log's own unsigned claim.
	Anchored       bool               `json:"anchored"`
	ProvenanceMode backfillProvenance `json:"provenance_mode"`
	// CapsuleOmitted says why a capsule the artifact store holds was not
	// carried: it no longer verifies under the profile's trusted keys.
	CapsuleOmitted string `json:"capsule_omitted,omitempty"`
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
// once, in order, and resumably, holding the retired log's lock throughout.
// The book always gets a new log id: any checkpoint the retired log ever
// signed names its own tree under (log_id, checkpoint key), and the book's
// different tree under the same pair would read as a fork of it.
func migrateStore(ctx context.Context, p Profile, newLogID string, now time.Time) (_ migrateResult, err error) {
	retired, release, err := openRetiredLog(ctx, p)
	if err != nil {
		return migrateResult{}, err
	}
	defer func() { err = errors.Join(err, release()) }()
	if len(retired.entries) == 0 {
		return migrateResult{}, hint(ErrInput, "this profile's cll.jsonl holds no entries to migrate")
	}
	book := p
	if newLogID != "" {
		if !logName.MatchString(newLogID) {
			return migrateResult{}, hint(ErrInput, "--log-id must be lowercase letters, digits and ._:/- (starting with a letter or digit, at most 191 characters)")
		}
		book.LogID = newLogID
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
	result := migrateResult{LogID: book.LogID, RetiredLogID: p.LogID, Backfilled: uint64(len(retired.entries))}
	var opened openedBook
	if _, statErr := os.Stat(filepath.Join(p.Connection.Database, "book", "log.jsonl")); statErr == nil {
		if opened, err = openBookUnguarded(ctx, book, false); err != nil {
			if errors.Is(err, evidencebook.ErrCorrupt) {
				// The book's records name another log id.
				advice := "this book may belong to a migration to a new log id that was interrupted; re-run 'store migrate' with that --log-id"
				if newLogID != "" {
					advice = "this profile's book/ holds records of another log id: if a migration to a different --log-id was interrupted, re-run it with that one; otherwise move the book/ directory aside (keep it) and re-run"
				}
				err = errors.Join(hint(ErrInput, advice), err)
			}
			return migrateResult{}, err
		}
		defer func() { err = errors.Join(err, opened.release()) }()
		st, id, ok, err := migrationRecord(ctx, opened.book)
		if err != nil {
			return migrateResult{}, err
		}
		if ok {
			// Already migrated: from this profile (an interrupted run whose
			// profile save is finished below), or into it (a repeat). A book
			// migrated under any other log id does not get this far: Open
			// refuses records that name another book.
			if !st.matches(retired) {
				return migrateResult{}, errRetiredLogChanged
			}
			result.Migration, result.RetiredLogID = id, st.RetiredLogID
			return result, finishMigration(p, book)
		}
	}
	// Always a new log id: the retired log's tree and the book's tree must
	// never share (log_id, checkpoint key). Whether the retired log was ever
	// checkpointed or witnessed cannot be read reliably from the file itself
	// (a stripped commit line reads as "never checkpointed").
	if book.LogID == p.LogID {
		return migrateResult{}, hint(ErrInput, "store migrate needs --log-id with a new log id: the book's log must never share the retired log's")
	}
	// Nothing is appended until the retired log's own checkpoint is shown to
	// commit the entries being carried over.
	anchored, err := anchorRetiredLog(p, retired)
	if err != nil {
		return migrateResult{}, err
	}
	if opened.book == nil {
		if opened, err = openBookUnguarded(ctx, book, true); err != nil {
			return migrateResult{}, err
		}
		defer func() { err = errors.Join(err, opened.release()) }()
	}
	done, err := backfilledSoFar(ctx, opened.book, p.LogID, retired.entries)
	if err != nil {
		return migrateResult{}, err
	}
	for i, entry := range retired.entries[done:] {
		if err = appendBackfilled(ctx, opened.book, artifacts, p.LogID, entry, done+i < anchored, len(retired.entries), now); err != nil {
			return migrateResult{}, err
		}
	}
	entries, checkpoint := retired.fingerprint()
	st := migrationStatement{RetiredLogID: p.LogID, RetiredEntries: result.Backfilled, EntriesDigest: entries, CheckpointDigest: checkpoint, AnchoredEntries: uint64(anchored)}
	if retired.checkpoint != nil {
		st.RetiredCheckpoint = retired.checkpoint.Size
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
	return result, finishMigration(p, book)
}

// finishMigration points the profile at the book's log, last. A migration
// interrupted after its record but before this save finishes here when it
// is re-run with the same --log-id.
func finishMigration(p, book Profile) error {
	if book.LogID == p.LogID {
		return nil
	}
	return saveProfile(book, true)
}

// anchorRetiredLog checks the retired log's last checkpoint before anything
// from that log is carried into the book: the statement must verify under
// the profile's trusted checkpoint key and name the retired log_id, and the
// retired entries must rebuild exactly the root it signed at its size. It
// returns how many entries, from the first, that checkpoint commits. A log
// that was never checkpointed anchors none.
func anchorRetiredLog(p Profile, retired retiredLog) (int, error) {
	if retired.checkpoint == nil {
		return 0, nil
	}
	record, err := verifyCheckpoint(p, retired.checkpoint.Bytes)
	if err != nil {
		// Fixed text: the verifier's own error may name more than the operator
		// needs, and the hint is shown verbatim. err stays in the chain.
		return 0, errors.Join(hint(ErrConflict, "the retired log's checkpoint does not verify: its signature, its log_id, or its signer (which must be one of the profile's checkpoint trusted_keys); nothing was migrated"), err)
	}
	tree, err := mmr.New(nil)
	if err != nil {
		return 0, err
	}
	anchored := 0
	for anchored < len(retired.entries) && tree.Size() < record.MMRSize {
		if _, err = tree.Append(retired.entries[anchored].Value); err != nil {
			return 0, err
		}
		anchored++
	}
	root, err := tree.Root()
	if err != nil || tree.Size() != record.MMRSize || hex.EncodeToString(root) != record.Root {
		return 0, hint(ErrConflict, "the retired entries do not rebuild the root the retired log's checkpoint signed: cll.jsonl was altered after it was checkpointed; nothing was migrated")
	}
	return anchored, nil
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
			return 0, hint(ErrInput, "this profile's book already holds records; migration must come first: move the book/ directory aside (keep it), run 'store migrate --log-id NEW' into a fresh book, then decide what to do with the set-aside records")
		}
		var st backfillStatement
		if err = json.Unmarshal(r.Header.Statement, &st); err != nil {
			return 0, err
		}
		if done >= len(entries) || st.RetiredLogID != retiredLogID || st.RetiredSeq != entries[done].Seq || r.Header.SubjectRef != hex.EncodeToString(entries[done].Value) {
			return 0, hint(ErrConflict, "the book's backfilled entries do not match the retired log: move the book/ directory aside (keep it) and re-run store migrate into a fresh book")
		}
		done++
	}
	return done, nil
}

func appendBackfilled(ctx context.Context, book *evidencebook.Book, artifacts artifactStore, retiredLogID string, entry cll.Entry, anchored bool, total int, now time.Time) error {
	value := hex.EncodeToString(entry.Value)
	statement := backfillStatement{
		RetiredLogID: retiredLogID, RetiredSeq: entry.Seq, Anchored: anchored,
		ProvenanceMode: backfillProvenance{
			Mode:             "backfilled",
			SourceRef:        map[string]string{"type": "x-retired-cll-entry-value", "digest_alg": "SHA-256", "digest": value},
			SourceAssertedAt: entry.AppendedAt.UTC().Format(time.RFC3339Nano),
			ImportBatch:      fmt.Sprintf("%s@%d", retiredLogID, total),
			ImportedAt:       now.UTC().Format(time.RFC3339Nano),
		},
	}
	e := evidencebook.Entry{
		// The retired entry is this producer's own earlier claim, carried over;
		// only `anchored` says whether a verified checkpoint commits it.
		RecordType: recordTypeBackfilled, EpistemicType: evidencebook.ProducerClaim,
		SubjectRef: value, EventTimeClaim: entry.AppendedAt,
	}
	// A retired entry is a capsule this profile published, or (for a
	// disclose act) a disclosure digest with no capsule behind it.
	if artifacts != nil {
		record, err := artifacts.Get(ctx, value)
		switch {
		case err == nil:
			e.Payloads = [][]byte{record.Capsule, record.ProducerEnvelope}
		case errors.Is(err, artifact.ErrUntrustedSigner):
			statement.CapsuleOmitted = "untrusted_signer"
		case !errors.Is(err, artifact.ErrNotFound):
			return err
		}
	}
	body, err := json.Marshal(statement)
	if err != nil {
		return err
	}
	e.Statement = body
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
// when the profile's pre-book cll.jsonl holds nothing, or is exactly the log
// `store migrate` moved into this book -- same entries, same last checkpoint.
func requireNoRetiredEntries(ctx context.Context, p Profile, book *evidencebook.Book) error {
	return requireRetiredMigrated(ctx, p, func() (migrationStatement, bool, error) {
		st, _, ok, err := migrationRecord(ctx, book)
		return st, ok, err
	})
}

// requireRetiredMigrated is requireNoRetiredEntries over any source of the
// book's migration record, so the read-only listing applies the same rule
// without opening the book.
func requireRetiredMigrated(ctx context.Context, p Profile, migration func() (migrationStatement, bool, error)) error {
	retired, err := scanRetiredLog(p)
	if err != nil {
		return err
	}
	if len(retired.entries) == 0 && retired.checkpoint == nil {
		return nil
	}
	migrated, ok, err := migration()
	if err != nil {
		return err
	}
	if !ok {
		return hint(ErrInput, "this profile's cll.jsonl holds entries not yet in its book; run 'store migrate --log-id <new-log-id>'")
	}
	if !migrated.matches(retired) {
		return errRetiredLogChanged
	}
	return nil
}

// bookEntry is one `cll list` item for a jsonl profile, in the same
// contract as a mysql or sqlite profile's: capsule_id is the capsule the log
// entry records (a published capsule, or a retired entry's value), and
// appended_at is when the book committed it. record_type, record_id (the
// book record's own id) and capsule_carried (whether the record carries the
// capsule's bytes) are additions. Book-internal records (checkpoint index
// roots, Closes, disclosures, requests, the migration record) are listed
// only with --all, and carry no capsule_id.
type bookEntry struct {
	Sequence       uint64 `json:"sequence"`
	CapsuleID      string `json:"capsule_id,omitempty"`
	AppendedAt     string `json:"appended_at"`
	RecordType     string `json:"record_type"`
	RecordID       string `json:"record_id"`
	CapsuleCarried bool   `json:"capsule_carried"`
}

func listEntries(records []evidencebook.Record, after, through uint64, limit int, all bool) ([]bookEntry, uint64) {
	items := make([]bookEntry, 0, min(len(records), limit))
	next := after
	for _, r := range records {
		if r.Seq <= after || (through != 0 && r.Seq > through) {
			continue
		}
		if len(items) == limit {
			break
		}
		next = r.Seq
		published := r.Header.RecordType == recordTypePublished || r.Header.RecordType == recordTypeBackfilled
		if !published && !all {
			continue
		}
		item := bookEntry{Sequence: r.Seq, AppendedAt: r.Header.CommittedAt, RecordType: r.Header.RecordType, RecordID: r.RecordID, CapsuleCarried: carriesCapsule(r.Header)}
		if published {
			item.CapsuleID = r.Header.SubjectRef
		}
		items = append(items, item)
	}
	return items, next
}

// readBookFiles reads a jsonl profile's committed book records without
// opening the book: no signing key, no lock, and no repair of a torn line,
// so it is safe beside a live writer and on a read-only profile. The order
// and membership come from the log's own entry.append events (the records
// the book committed, in position order); each record's header comes from
// the record journal. An incomplete final line in either file (a write in
// progress) is ignored. It is a local listing, not a proof.
func readBookFiles(p Profile) ([]evidencebook.Record, error) {
	dir := filepath.Join(p.Connection.Database, "book")
	logLines, err := completeLines(filepath.Join(dir, "log.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, noBookError(p)
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, line := range logLines {
		var event struct {
			Type  string `json:"type"`
			Entry *struct {
				Seq   string `json:"seq"`
				Value string `json:"value"`
			} `json:"entry"`
		}
		if err = json.Unmarshal(line, &event); err != nil {
			return nil, errors.Join(cll.ErrCorrupt, err)
		}
		if event.Type != "entry.append" || event.Entry == nil {
			continue
		}
		value, err := base64.StdEncoding.DecodeString(event.Entry.Value)
		if err != nil || event.Entry.Seq != strconv.Itoa(len(ids)+1) {
			return nil, errors.Join(cll.ErrCorrupt, errors.New("the book's log is not a dense sequence of record ids"))
		}
		ids = append(ids, hex.EncodeToString(value))
	}
	recordLines, err := completeLines(filepath.Join(dir, "records", "records.jsonl"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	headers := make(map[string]evidencebook.Header, len(recordLines))
	for _, line := range recordLines {
		var stored evidencebook.StoredRecord
		if err = json.Unmarshal(line, &stored); err != nil {
			return nil, errors.Join(cll.ErrCorrupt, err)
		}
		var header evidencebook.Header
		if err = json.Unmarshal(stored.Header, &header); err != nil {
			return nil, errors.Join(cll.ErrCorrupt, err)
		}
		headers[stored.RecordID] = header
	}
	records := make([]evidencebook.Record, len(ids))
	for i, id := range ids {
		header, ok := headers[id]
		if !ok || header.Seq != uint64(i+1) {
			return nil, errors.Join(cll.ErrCorrupt, fmt.Errorf("the book's log commits record %s at position %d, which its record journal does not hold there", id, i+1))
		}
		if header.BookID != p.LogID {
			return nil, hint(ErrInput, fmt.Sprintf("this book belongs to log id %q, not the profile's log_id; if a 'store migrate --log-id %s' was interrupted, re-run it", header.BookID, header.BookID))
		}
		records[i] = evidencebook.Record{RecordID: id, Seq: uint64(i + 1), Header: header}
	}
	return records, nil
}

// noBookError says what a profile without a book needs: `store migrate` when
// its pre-book cll.jsonl holds entries (store init would refuse it), and
// `store init` otherwise.
func noBookError(p Profile) error {
	if retired, err := scanRetiredLog(p); err == nil && (len(retired.entries) > 0 || retired.checkpoint != nil) {
		return hint(ErrInput, "this profile's cll.jsonl holds entries not yet in its book; run 'store migrate --log-id <new-log-id>'")
	}
	return hint(ErrInput, "the profile has no book yet; run 'store init'")
}

// completeLines returns a journal's newline-terminated lines.
func completeLines(path string) ([][]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if i := bytes.LastIndexByte(raw, '\n'); i >= 0 {
		raw = raw[:i]
	} else {
		return nil, nil
	}
	return bytes.Split(raw, []byte("\n")), nil
}

// initBookFiles is `store init` on a jsonl profile: it creates the book's
// directory and an empty log journal, which is all a book is before its
// first record. It opens no book, so it needs no key: a profile that only
// verifies other producers' capsules can initialize its store. The first
// command that writes opens the book and checks its keys. A pre-book
// cll.jsonl that is not migrated is refused here too.
func initBookFiles(ctx context.Context, p Profile) error {
	dir := filepath.Join(p.Connection.Database, "book")
	path := filepath.Join(dir, "log.jsonl")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err = os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err = clljsonl.Init(path); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	_, _, err := listBookFiles(ctx, p, 0, 0, 1, true)
	return err
}

// listBookFiles is `cll list` on a jsonl profile.
func listBookFiles(ctx context.Context, p Profile, after, through uint64, limit int, all bool) ([]bookEntry, uint64, error) {
	records, err := readBookFiles(p)
	if err != nil {
		return nil, 0, err
	}
	err = requireRetiredMigrated(ctx, p, func() (migrationStatement, bool, error) {
		for _, r := range records {
			if r.Header.RecordType == recordTypeMigration {
				var st migrationStatement
				return st, true, json.Unmarshal(r.Header.Statement, &st)
			}
		}
		return migrationStatement{}, false, nil
	})
	if err != nil {
		return nil, 0, err
	}
	items, next := listEntries(records, after, through, limit, all)
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
func bookBundle(ctx context.Context, book *evidencebook.Book, root string, depth int, payloads string, suppress map[string]bool, disclose bool, producerKey ed25519.PublicKey) (evidencebook.Bundle, error) {
	if depth < 1 {
		// The book reads a zero depth as its default; it has no root-only
		// closure, so a zero is refused rather than silently widened.
		return evidencebook.Bundle{}, hint(ErrInput, "--closure-depth must be at least 1 on a jsonl profile")
	}
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
			return evidencebook.Bundle{}, hint(ErrInput, "--payloads must be all or selected")
		}
		if suppress["agent_output"] {
			return evidencebook.Bundle{}, hint(ErrInput, "a book record has no agent_output member to suppress")
		}
		if suppress["agent_input"] {
			// An evidence_result record carries the Result as its header's
			// statement: suppressing the header would withhold the very
			// document the bundle is rooted on.
			if root, err := book.Get(ctx, request.Root); err == nil && root.Header.RecordType == resultRecordType {
				return evidencebook.Bundle{}, hint(ErrInput, "the root is an evidence_result record, whose agent_input carries the Result itself; it cannot be suppressed")
			}
			request.Suppress = []string{evidencebook.HeaderMember}
		}
	}
	if producerKey != nil {
		// Passed to the book, not added afterwards: the book digests the
		// bundle (extensions included) into its disclosure record.
		extensions, err := producerKeyExtension(producerKey)
		if err != nil {
			return evidencebook.Bundle{}, err
		}
		request.Extensions = make(map[string]json.RawMessage, len(extensions))
		for kind, block := range extensions {
			if request.Extensions[kind], err = json.Marshal(block); err != nil {
				return evidencebook.Bundle{}, err
			}
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
