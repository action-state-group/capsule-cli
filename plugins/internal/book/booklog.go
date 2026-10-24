package book

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
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/capsule-cli/cli"
	emit "github.com/action-state-group/capsule-emit/go"
	"github.com/action-state-group/capsule-emit/go/artifact"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/action-state-group/checkpointed-local-log/go/mmr"
	clljsonl "github.com/action-state-group/checkpointed-local-log/go/store/jsonl"
	"github.com/action-state-group/evidencebook"
)

// A book profile has one log: its evidence book. `publish` seals the
// request as a capsule of the book, carrying its record header and signed by
// the profile's key, and that capsule is the record itself (own_capsule): the
// log entry is the capsule's id, and the artifact store holds the capsule
// once, with its originals. Anything else is wrapped in a record of the
// book's own: `cll append` of a capsule sealed elsewhere (published_capsule,
// subject_ref naming it; the artifact store already holds it), and the
// entries `book migrate` carries over from a pre-book cll.jsonl
// (backfilled_entry). Before capsule-cli#236 a publish was wrapped too, with
// the capsule and its envelope as the record's payloads; such records stay
// readable as they are.
const (
	recordTypeOwn        = "own_capsule"
	recordTypePublished  = "published_capsule"
	recordTypeBackfilled = "backfilled_entry"
	recordTypeMigration  = "log_migration"
)

// capsuleIDOf is the capsule a record publishes: the record itself for an
// own_capsule, the capsule subject_ref names for a wrapper. A backfilled
// entry publishes one only when its value was a capsule; otherwise (a
// disclosure digest) it publishes none.
func capsuleIDOf(r evidencebook.Record) (string, bool) {
	switch r.Header.RecordType {
	case recordTypeOwn:
		return r.RecordID, true
	case recordTypePublished:
		return r.Header.SubjectRef, r.Header.SubjectRef != ""
	case recordTypeBackfilled:
		return r.Header.SubjectRef, backfilledCapsule(r.Header)
	}
	return "", false
}

// publishedRecord returns the record that publishes capsuleID, if any: the
// own_capsule that is it, or the record that wraps it. An own_capsule is
// also found by the id its request seals to on a plain profile (its
// subject_ref), so publishing the same request twice finds the first.
func publishedRecord(ctx context.Context, book *evidencebook.Book, capsuleID string) (evidencebook.Record, bool, error) {
	if r, err := book.Get(ctx, capsuleID); err == nil && r.Header.RecordType == recordTypeOwn {
		return r, true, nil
	} else if err != nil && !errors.Is(err, evidencebook.ErrNotFound) {
		return evidencebook.Record{}, false, err
	}
	records, err := book.Query(ctx, evidencebook.Filter{SubjectRef: capsuleID})
	if err != nil {
		return evidencebook.Record{}, false, err
	}
	for _, r := range records {
		if _, ok := capsuleIDOf(r); ok {
			return r, true, nil
		}
	}
	return evidencebook.Record{}, false, nil
}

// carriesCapsule reports whether a record's own bytes hold the capsule it
// publishes: an own_capsule is that capsule, and a wrapper carries it as its
// first payload. A backfilled disclosure digest carries none.
func carriesCapsule(h evidencebook.Header) bool {
	switch h.RecordType {
	case recordTypeOwn:
		return true
	case recordTypePublished, recordTypeBackfilled:
		return len(h.PayloadCommitments) > 0
	}
	return false
}

// backfilledCapsule reports whether a backfilled entry's value is a capsule:
// one carried as payloads, or (statement.capsule) one the artifact store held
// when it was backfilled.
func backfilledCapsule(h evidencebook.Header) bool {
	if len(h.PayloadCommitments) > 0 {
		return true
	}
	var st backfillStatement
	return json.Unmarshal(h.Statement, &st) == nil && st.Capsule
}

// publishedCommitWarning is reported when a capsule's record committed but a
// later step of the append (indexing, an automatic checkpoint) failed. The
// position stands and must not be retried; the next open of the book
// rebuilds its indexes from the log.
const publishedCommitWarning = "committed; a later indexing or checkpoint step failed and is redone when the book is next opened"

// commitOutcome turns the result of a book append into a position, a
// warning, or the error class `publish` and `cll append` report.
func commitOutcome(committed evidencebook.Record, err error) (seq uint64, warning string, _ error) {
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
		return 0, "", errors.Join(cli.ErrPending, err)
	}
}

// publishOwn is `publish` on a book profile. The request is sealed under the
// book's writer lock, once the book has given it its position, carrying the
// record header (evidencebook.RecordHeaderMember) and signed by the
// profile's key: the capsule is the record. The book then stores it and its
// originals in the artifact store in one Put and appends its id.
//
// The capsule's id depends on its position and commit time, so it is not
// the id the same request seals to elsewhere (`seal`, a plain profile).
// That id, the request's, is the record's subject_ref: publishing the same
// request again returns the record it already has.
func publishOwn(ctx context.Context, book *evidencebook.Book, artifacts cli.ArtifactStore, p cli.Profile, r cli.Request, key ed25519.PrivateKey) (cli.Publication, error) {
	plain, err := cli.Seal(r, key)
	if err != nil {
		return cli.Publication{}, err
	}
	if len(cli.MissingBindings(plain)) != 0 {
		return cli.Publication{}, cli.ErrPartial
	}
	result := cli.Publication{Namespace: p.Namespace, LogID: p.LogID, SealedID: plain.CapsuleID}
	if existing, ok, err := publishedRecord(ctx, book, plain.CapsuleID); err != nil {
		return cli.Publication{}, err
	} else if ok {
		if err = sameOriginals(ctx, artifacts, existing, plain, key); err != nil {
			return cli.Publication{}, err
		}
		result.CapsuleID, _ = capsuleIDOf(existing)
		result.Sequence, result.State = existing.Seq, "appended"
		if result.CapsuleID == result.SealedID {
			// A wrapper published it under the sealed id itself.
			result.SealedID = ""
		}
		return result, nil
	}
	identity, err := emit.NewEd25519SigningIdentity(key)
	if err != nil {
		return cli.Publication{}, err
	}
	seal := func(_ evidencebook.Header, header []byte) (emit.Result, error) {
		input := emit.SealInput{Capsule: r.Capsule, Model: r.Model, Runtime: r.Runtime, Identity: identity}
		input.Capsule.Extensions = append(append([]emit.Extension(nil), r.Capsule.Extensions...), emit.Extension{Name: evidencebook.RecordHeaderMember, Value: header})
		if r.Payload != nil {
			input.Payload = r.Payload
		}
		if r.AgentOutput != nil {
			input.AgentOutput = r.AgentOutput
		}
		return emit.Seal(input)
	}
	committed, err := book.AppendCapsule(ctx, evidencebook.Entry{
		RecordType: recordTypeOwn, EpistemicType: evidencebook.ProducerClaim, SubjectRef: plain.CapsuleID,
		Links: citedCapsules(r),
	}, plain.Artifacts, seal)
	seq, warning, err := commitOutcome(committed, err)
	if err != nil {
		return cli.Publication{}, err
	}
	result.CapsuleID, result.Sequence, result.State, result.Warning = committed.RecordID, seq, "appended", warning
	return result, nil
}

// citedCapsules are the capsules a request's capsule cites, as the record's
// cites links: its references to other capsules and its chain parent. A
// bundle then follows them as it follows any record's links, so the capsules
// the record cites ride in its bundle, or are declared missing when the book
// does not hold them, and the bundle's citations never dangle.
func citedCapsules(r cli.Request) []evidencebook.Link {
	var links []evidencebook.Link
	seen := map[string]bool{}
	add := func(digest string) {
		if cli.Hex64.MatchString(digest) && !seen[digest] {
			seen[digest] = true
			links = append(links, evidencebook.Link{Type: evidencebook.Cites, Target: digest})
		}
	}
	for _, ref := range r.Capsule.References {
		if ref.Type == "agent-action-capsule" {
			add(ref.Digest)
		}
	}
	if r.Capsule.Chain != nil {
		add(r.Capsule.Chain.ParentCapsuleID)
	}
	return links
}

// sameOriginals refuses a repeated publish whose originals differ from the
// ones the first stored: the request seals to the same id either way, so
// only its originals can tell, and a plain profile's store refuses the same
// conflict.
func sameOriginals(ctx context.Context, artifacts cli.ArtifactStore, existing evidencebook.Record, plain artifact.Record, key ed25519.PrivateKey) error {
	// The retry's originals are checked as a first publish's are (names,
	// bindings, digests) before they are compared.
	prepared, err := artifact.Prepare(plain)
	if err != nil {
		return err
	}
	if _, err = artifact.Verify(prepared, []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}); err != nil {
		return errors.Join(cli.Hint(cli.ErrInput, "the request's artifacts do not verify against its capsule (a name given twice, or a binding the capsule does not commit); fix them in the request"), err)
	}
	capsuleID, _ := capsuleIDOf(existing)
	held, err := artifacts.Get(ctx, capsuleID)
	if err != nil {
		return err
	}
	// Each original as name, binding, retention and content digest; a purged
	// one keeps its digest, so it compares as the present one it was.
	inventory := func(r artifact.Record) map[string]string {
		out := map[string]string{}
		for _, a := range r.Artifacts {
			digest := a.ContentSHA256
			state := a.State
			if state == artifact.Present {
				sum := sha256.Sum256(a.Content)
				digest = hex.EncodeToString(sum[:])
			} else if state == artifact.Purged {
				state = artifact.Present
			}
			out[a.Name] = string(a.Binding) + "|" + string(state) + "|" + digest
		}
		return out
	}
	if !maps.Equal(inventory(held), inventory(prepared)) {
		return cli.Hint(cli.ErrConflict, "this request was published before with other originals; its record keeps the first ones")
	}
	return nil
}

// appendWrapped is `cll append` on a book profile: the capsule, sealed
// elsewhere and already in the artifact store, does not carry this book's
// record header, so the book wraps it in a published_capsule record whose
// subject_ref names it and whose payloads are the capsule and its envelope,
// so a bundle carries what it wraps. Appending the same
// capsule again returns the position it already has.
func appendWrapped(ctx context.Context, book *evidencebook.Book, record artifact.Record) (seq uint64, warning string, err error) {
	if existing, ok, err := publishedRecord(ctx, book, record.CapsuleID); err != nil || ok {
		return existing.Seq, "", err
	}
	// The wrapper commits the capsule and its envelope as its payloads, so
	// a bundle carries the capsule it wraps and a reader (report build, the
	// neutral viewer) resolves the capsule's id through it.
	return commitOutcome(book.Append(ctx, evidencebook.Entry{
		RecordType: recordTypePublished, EpistemicType: evidencebook.ProducerClaim, SubjectRef: record.CapsuleID,
		Payloads: [][]byte{record.Capsule, record.ProducerEnvelope},
	}))
}

// retiredLog is what a jsonl profile's pre-book cll.jsonl holds.
type retiredLog struct {
	entries    []cll.Entry
	checkpoint *cll.CheckpointState
}

func retiredLogPath(p cli.Profile) string {
	return filepath.Join(p.Connection.Database, cli.JSONLLogFile)
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
func openRetiredLog(ctx context.Context, p cli.Profile) (out retiredLog, release func() error, err error) {
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
// command's way (and a check never stands in theirs). Only `book migrate`,
// which must keep an older binary out while it copies, takes the lock.
func scanRetiredLog(p cli.Profile) (retiredLog, error) {
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

// migrationStatement is the body of the record `book migrate` seals once
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

var errRetiredLogChanged = cli.Hint(cli.ErrConflict, "the retired cll.jsonl changed after it was migrated: something wrote it after the migration (an older capsulectl?). Restore it to what was migrated, from a backup, before using this profile again")

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
	// Capsule says the retired value is a capsule the artifact store holds
	// under that id; the record commits it and its envelope as its payloads.
	Capsule bool `json:"capsule,omitempty"`
	// CapsuleOmitted says why a capsule the artifact store holds was not
	// taken as one: it no longer verifies under the profile's trusted keys.
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
func migrateStore(ctx context.Context, p cli.Profile, newLogID string, now time.Time) (_ migrateResult, err error) {
	retired, release, err := openRetiredLog(ctx, p)
	if err != nil {
		return migrateResult{}, err
	}
	defer func() { err = errors.Join(err, release()) }()
	if len(retired.entries) == 0 {
		return migrateResult{}, cli.Hint(cli.ErrInput, "this profile's cll.jsonl holds no entries to migrate")
	}
	book := p
	if newLogID != "" {
		if !cli.LogName.MatchString(newLogID) {
			return migrateResult{}, cli.Hint(cli.ErrInput, "--log-id must be lowercase letters, digits and ._:/- (starting with a letter or digit, at most 191 characters)")
		}
		book.LogID = newLogID
	}
	var artifacts cli.ArtifactStore
	if p.Namespace != "" {
		t, openErr := cli.OpenTarget(ctx, p, cli.UseArtifacts)
		if openErr != nil {
			return migrateResult{}, openErr
		}
		defer func() { err = errors.Join(err, t.Close()) }()
		artifacts = t.Artifacts
	}
	result := migrateResult{LogID: book.LogID, RetiredLogID: p.LogID, Backfilled: uint64(len(retired.entries))}
	var opened openedBook
	if _, statErr := os.Stat(filepath.Join(p.Connection.Database, "book", "log.jsonl")); statErr == nil {
		if opened, err = openBookUnguarded(ctx, book, false, artifacts); err != nil {
			if errors.Is(err, evidencebook.ErrCorrupt) {
				// The book's records name another log id.
				advice := "this book may belong to a migration to a new log id that was interrupted; re-run 'book migrate' with that --log-id"
				if newLogID != "" {
					advice = "this profile's book/ holds records of another log id: if a migration to a different --log-id was interrupted, re-run it with that one; otherwise move the book/ directory aside (keep it) and re-run"
				}
				err = errors.Join(cli.Hint(cli.ErrInput, advice), err)
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
		return migrateResult{}, cli.Hint(cli.ErrInput, "book migrate needs --log-id with a new log id: the book's log must never share the retired log's")
	}
	// Nothing is appended until the retired log's own checkpoint is shown to
	// commit the entries being carried over.
	anchored, err := anchorRetiredLog(p, retired)
	if err != nil {
		return migrateResult{}, err
	}
	if opened.book == nil {
		if opened, err = openBookUnguarded(ctx, book, true, artifacts); err != nil {
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
func finishMigration(p, book cli.Profile) error {
	if book.LogID == p.LogID {
		return nil
	}
	return cli.SaveProfile(book, true)
}

// anchorRetiredLog checks the retired log's last checkpoint before anything
// from that log is carried into the book: the statement must verify under
// the profile's trusted checkpoint key and name the retired log_id, and the
// retired entries must rebuild exactly the root it signed at its size. It
// returns how many entries, from the first, that checkpoint commits. A log
// that was never checkpointed anchors none.
func anchorRetiredLog(p cli.Profile, retired retiredLog) (int, error) {
	if retired.checkpoint == nil {
		return 0, nil
	}
	record, err := cli.VerifyCheckpoint(p, retired.checkpoint.Bytes)
	if err != nil {
		// Fixed text: the verifier's own error may name more than the operator
		// needs, and the hint is shown verbatim. err stays in the chain.
		return 0, errors.Join(cli.Hint(cli.ErrConflict, "the retired log's checkpoint does not verify: its signature, its log_id, or its signer (which must be one of the profile's checkpoint trusted_keys); nothing was migrated"), err)
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
		return 0, cli.Hint(cli.ErrConflict, "the retired entries do not rebuild the root the retired log's checkpoint signed: cll.jsonl was altered after it was checkpointed; nothing was migrated")
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
			return 0, cli.Hint(cli.ErrInput, "this profile's book already holds records; migration must come first: move the book/ directory aside (keep it), run 'book migrate --log-id NEW' into a fresh book, then decide what to do with the set-aside records")
		}
		var st backfillStatement
		if err = json.Unmarshal(r.Header.Statement, &st); err != nil {
			return 0, err
		}
		if done >= len(entries) || st.RetiredLogID != retiredLogID || st.RetiredSeq != entries[done].Seq || r.Header.SubjectRef != hex.EncodeToString(entries[done].Value) {
			return 0, cli.Hint(cli.ErrConflict, "the book's backfilled entries do not match the retired log: move the book/ directory aside (keep it) and re-run book migrate into a fresh book")
		}
		done++
	}
	return done, nil
}

func appendBackfilled(ctx context.Context, book *evidencebook.Book, artifacts cli.ArtifactStore, retiredLogID string, entry cll.Entry, anchored bool, total int, now time.Time) error {
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
	// disclose act) a disclosure digest with no capsule behind it. A capsule
	// is committed as the record's payloads, as a wrapper commits it.
	if artifacts != nil {
		held, err := artifacts.Get(ctx, value)
		switch {
		case err == nil:
			statement.Capsule = true
			e.Payloads = [][]byte{held.Capsule, held.ProducerEnvelope}
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
// `book migrate` moved into this book -- same entries, same last checkpoint.
func requireNoRetiredEntries(ctx context.Context, p cli.Profile, book *evidencebook.Book) error {
	return requireRetiredMigrated(ctx, p, func() (migrationStatement, bool, error) {
		st, _, ok, err := migrationRecord(ctx, book)
		return st, ok, err
	})
}

// requireRetiredMigrated is requireNoRetiredEntries over any source of the
// book's migration record, so the read-only listing applies the same rule
// without opening the book.
func requireRetiredMigrated(ctx context.Context, p cli.Profile, migration func() (migrationStatement, bool, error)) error {
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
		return cli.Hint(cli.ErrInput, "this profile's cll.jsonl holds entries not yet in its book; run 'book migrate --log-id <new-log-id>'")
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
		published := r.Header.RecordType == recordTypeOwn || r.Header.RecordType == recordTypePublished || r.Header.RecordType == recordTypeBackfilled
		if !published && !all {
			continue
		}
		item := bookEntry{Sequence: r.Seq, AppendedAt: r.Header.CommittedAt, RecordType: r.Header.RecordType, RecordID: r.RecordID, CapsuleCarried: carriesCapsule(r.Header)}
		switch r.Header.RecordType {
		case recordTypeOwn:
			item.CapsuleID = r.RecordID
		case recordTypePublished, recordTypeBackfilled:
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
func readBookFiles(p cli.Profile) ([]evidencebook.Record, error) {
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
			return nil, cli.Hint(cli.ErrInput, fmt.Sprintf("this book belongs to log id %q, not the profile's log_id; if a 'book migrate --log-id %s' was interrupted, re-run it", header.BookID, header.BookID))
		}
		records[i] = evidencebook.Record{RecordID: id, Seq: uint64(i + 1), Header: header}
	}
	return records, nil
}

// noBookError says what a profile without a book needs: `book migrate` when
// its pre-book cll.jsonl holds entries (store init would refuse it), and
// `store init` otherwise.
func noBookError(p cli.Profile) error {
	if retired, err := scanRetiredLog(p); err == nil && (len(retired.entries) > 0 || retired.checkpoint != nil) {
		return cli.Hint(cli.ErrInput, "this profile's cll.jsonl holds entries not yet in its book; run 'book migrate --log-id <new-log-id>'")
	}
	return cli.Hint(cli.ErrInput, "the profile has no book yet; run 'store init'")
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
func initBookFiles(ctx context.Context, p cli.Profile) error {
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
func listBookFiles(ctx context.Context, p cli.Profile, after, through uint64, limit int, all bool) ([]bookEntry, uint64, error) {
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

func newBookWitnessStore(p cli.Profile) bookWitnessStore {
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
	return cli.AtomicFile(s.path, raw, statErr == nil)
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
func checkpointBook(ctx context.Context, p cli.Profile, book *evidencebook.Book, service string) (map[string]any, error) {
	cp, err := book.Checkpoint(ctx)
	if err != nil {
		return nil, err
	}
	record, err := cli.VerifyCheckpoint(p, cp.Statement)
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
func bookBundle(ctx context.Context, book *evidencebook.Book, artifacts cli.BundleArtifacts, root string, depth int, payloads string, suppress map[string]bool, disclose, attachOriginals bool, producerKey ed25519.PublicKey) (evidencebook.Bundle, error) {
	if depth < 1 {
		// The book reads a zero depth as its default; it has no root-only
		// closure, so a zero is refused rather than silently widened.
		return evidencebook.Bundle{}, cli.Hint(cli.ErrInput, "--closure-depth must be at least 1 on a jsonl profile")
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
			return evidencebook.Bundle{}, cli.Hint(cli.ErrInput, "--payloads must be all or selected")
		}
		if suppress["agent_input"] {
			// An evidence_result record carries the Result as its header's
			// statement: suppressing the header would withhold the very
			// document the bundle is rooted on.
			if root, err := book.Get(ctx, request.Root); err == nil && root.Header.RecordType == resultRecordType {
				return evidencebook.Bundle{}, cli.Hint(cli.ErrInput, "the root is an evidence_result record, whose agent_input carries the Result itself; it cannot be suppressed")
			}
			request.Suppress = []string{evidencebook.HeaderMember}
		}
		closure, err := bookClosure(ctx, book, request.Root, depth)
		if err != nil {
			return evidencebook.Bundle{}, err
		}
		if attachOriginals && suppress["agent_input"] {
			return evidencebook.Bundle{}, cli.Hint(cli.ErrInput, "--attach-input-originals and --suppress agent_input contradict each other; pass one")
		}
		withheld, err := suppressedOriginals(ctx, artifacts, closure, suppress)
		if err != nil {
			return evidencebook.Bundle{}, err
		}
		if len(withheld) > 0 && request.Payloads == evidencebook.PayloadsAll {
			return evidencebook.Bundle{}, cli.Hint(cli.ErrInput, "a record in the bundle commits an original that --suppress withholds, which needs --payloads selected")
		}
		request.Withhold = append(request.Withhold, withheld...)
		if attachOriginals {
			extension, err := inputOriginals(ctx, book, artifacts, closure)
			if err != nil {
				return evidencebook.Bundle{}, err
			}
			request.Extensions = map[string]json.RawMessage{cli.InputOriginalsExtension: extension}
		}
	}
	// The book declares it (producer-key/v1) inside the bundle it digests
	// into its disclosure record. Every carried record also carries the
	// Producer Envelope the book stored when it sealed it, so the bundle
	// verifies VALID as written (#148).
	request.ProducerKey = producerKey
	bundle, err := book.Bundle(ctx, request)
	if err != nil {
		return evidencebook.Bundle{}, bookError(err)
	}
	if _, err = evidencebook.VerifyBundle(bundle.JSON); err != nil {
		return evidencebook.Bundle{}, err
	}
	return bundle, nil
}

// suppressedOriginals are the payload digests --suppress withholds from the
// records a bundle discloses: an own capsule's original bound to the member
// suppressed (agent_input_digest or agent_output_digest), and the
// agent_input original a wrapper written before capsule-cli#236 committed
// as its third payload. Suppressing agent_output, which only an own capsule
// commits, is refused when no record in the bundle has one: it would mean
// nothing.
func suppressedOriginals(ctx context.Context, artifacts cli.BundleArtifacts, closure []evidencebook.Record, suppress map[string]bool) ([]string, error) {
	bound := map[artifact.DigestField]bool{artifact.PayloadDigest: suppress["agent_input"], artifact.AgentOutputDigest: suppress["agent_output"]}
	var withheld []string
	outputs := 0
	for _, record := range closure {
		switch record.Header.RecordType {
		case recordTypePublished:
			if suppress["agent_input"] && len(record.Header.PayloadCommitments) > 2 {
				withheld = append(withheld, record.Header.PayloadCommitments[2:]...)
			}
		case recordTypeOwn:
			if !suppress["agent_input"] && !suppress["agent_output"] {
				continue
			}
			held, err := artifacts.Get(ctx, record.RecordID)
			if err != nil {
				return nil, err
			}
			for _, a := range held.Artifacts {
				if a.Binding == artifact.AgentOutputDigest {
					outputs++
				}
				if bound[a.Binding] && a.ContentSHA256 != "" && slices.Contains(record.Header.PayloadCommitments, a.ContentSHA256) {
					withheld = append(withheld, a.ContentSHA256)
				}
			}
		}
	}
	if suppress["agent_output"] && outputs == 0 {
		return nil, cli.Hint(cli.ErrInput, "no record in this bundle commits an agent_output original, so there is none to suppress")
	}
	return withheld, nil
}

// bookClosure is the set of records a bundle on root carries disclosed: the
// root and every record its links reach within depth hops (the book's own
// closure walk; targets the book does not hold are skipped here, and the
// book declares them missing).
func bookClosure(ctx context.Context, book *evidencebook.Book, root string, depth int) ([]evidencebook.Record, error) {
	first, err := book.Get(ctx, root)
	if err != nil {
		return nil, bookError(err)
	}
	seen := map[string]bool{root: true}
	closure := []evidencebook.Record{first}
	frontier := []evidencebook.Record{first}
	for range depth {
		var next []evidencebook.Record
		for _, record := range frontier {
			for _, link := range record.Header.Links {
				if seen[link.Target] {
					continue
				}
				target, err := book.Get(ctx, link.Target)
				if errors.Is(err, evidencebook.ErrNotFound) {
					continue
				}
				if err != nil {
					return nil, err
				}
				seen[link.Target] = true
				closure = append(closure, target)
				next = append(next, target)
			}
		}
		frontier = next
	}
	return closure, nil
}

// inputOriginals builds the cli.InputOriginalsExtension for the records in
// closure that publish a capsule. A capsule whose original is not retained
// is left out (a reader shows it withheld), and so is an own capsule whose
// record's retention state yields no bytes (WITHHELD or DELETED): the
// extension never carries what the book itself would withhold. A retained
// original that does not hash to the capsule's agent_input_digest refuses
// the bundle.
func inputOriginals(ctx context.Context, book *evidencebook.Book, artifacts cli.BundleArtifacts, closure []evidencebook.Record) (json.RawMessage, error) {
	originals := make(map[string]string)
	for _, record := range closure {
		capsuleID, ok := capsuleIDOf(record)
		if !ok {
			continue
		}
		if record.Header.RecordType == recordTypeOwn && len(record.Header.PayloadCommitments) > 0 {
			state, err := book.Retention(ctx, record.RecordID)
			if err != nil {
				return nil, err
			}
			if state == evidencebook.Withheld || state == evidencebook.Deleted {
				continue
			}
		}
		stored, err := artifacts.Get(ctx, capsuleID)
		if err != nil {
			return nil, err
		}
		original, err := verifiedInputOriginal(stored)
		if err != nil {
			return nil, err
		}
		if original != nil {
			originals[stored.CapsuleID] = base64.RawURLEncoding.EncodeToString(original)
		}
	}
	return json.Marshal(originals)
}

// verifiedInputOriginal is the record's retained agent_input original -- the
// one artifact bound to the capsule's agent_input_digest -- after checking
// that its JSON-DIGEST is that digest; nil when none is retained (purged,
// never retained, or undeclared). An original that does not hash to the
// capsule's commitment is an error, never attached.
func verifiedInputOriginal(record artifact.Record) ([]byte, error) {
	var original []byte
	for _, a := range record.Artifacts {
		if a.Binding == artifact.PayloadDigest && a.State == artifact.Present && len(a.Content) > 0 {
			original = a.Content
			break
		}
	}
	if original == nil {
		return nil, nil
	}
	var capsule map[string]interface{}
	if err := cli.DecodeJSONPreserveNumbers("the stored capsule", record.Capsule, &capsule); err != nil {
		return nil, fmt.Errorf("capsule %s: %w", record.CapsuleID, err)
	}
	attestation, _ := capsule["model_attestation"].(map[string]interface{})
	compute, _ := attestation["compute_attestation"].(map[string]interface{})
	committed, _ := compute["agent_input_digest"].(string)
	var decoded interface{}
	if err := cli.DecodeJSONPreserveNumbers("the stored agent_input original", original, &decoded); err != nil {
		return nil, fmt.Errorf("capsule %s: stored agent_input original: %w", record.CapsuleID, err)
	}
	digest, err := canonical.JSONDigest(decoded)
	if err != nil {
		return nil, fmt.Errorf("capsule %s: stored agent_input original: %w", record.CapsuleID, err)
	}
	if committed == "" || digest != committed {
		return nil, fmt.Errorf("capsule %s: the stored agent_input original does not hash to the capsule's agent_input_digest; nothing was attached", record.CapsuleID)
	}
	return original, nil
}
