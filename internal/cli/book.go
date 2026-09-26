// Package cli: the book verbs -- `close`, `reconcile`, `request`, `respond`
// -- over a local evidencebook.Book. The book is opened from a jsonl
// profile and lives in a `book/` directory beside that profile's
// artifacts.jsonl and cll.jsonl; it keeps its own commitment log there and
// never shares the profile's. Everything here goes through the evidencebook
// public API: no commitment-substrate type appears in a flag, an output, or
// this file. The book composes every proof; these verbs choose windows,
// read files, and write results.
package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/action-state-group/capsule-emit-go/artifact"
	"github.com/action-state-group/evidencebook"
	"github.com/spf13/cobra"
)

// bookNow is the clock the book verbs use for record commit times and for
// deciding which period has ended. Tests replace it.
var bookNow = time.Now

// openedBook is a book plus the record store it was opened over. The store
// is kept only so `close --capsule-out` can export a record's sealed capsule
// and envelope for `verify`; nothing else reads it directly.
type openedBook struct {
	book  *evidencebook.Book
	store *evidencebook.FileStore
}

func (o openedBook) release() error { return o.book.Release() }

// bookLogSuffix names the book's commitment log apart from the profile's
// own CLL. Both are checkpointed with the profile's checkpoint key, and two
// different trees under one (log_id, key) would read as equivocation.
const bookLogSuffix = "/book"

// openBook is the whole signing path of the book verbs. Records are signed
// with the profile's signing key, which must be one of the profile's own
// trusted_keys (so `verify` under that profile accepts them); the book's
// checkpoints are signed with the profile's checkpoint signing key, which
// must be one of its checkpoint trusted_keys. create is false for verbs that
// only read: they refuse a book that does not exist rather than making one.
func openBook(ctx context.Context, p Profile, create bool) (openedBook, error) {
	if p.Type != "jsonl" || p.LogID == "" || p.Operator == "" {
		return openedBook{}, inputError("book verbs need a jsonl profile with log_id and operator set")
	}
	recordKey, err := privateKey(p.Signing)
	if err != nil {
		return openedBook{}, err
	}
	if err = requirePublisherKey(p, recordKey); err != nil {
		return openedBook{}, err
	}
	checkpointKey, err := privateKey(p.Checkpoint.Signing)
	if err != nil {
		return openedBook{}, err
	}
	if !checkpointSignerTrusted(p, hex.EncodeToString(checkpointKey.Public().(ed25519.PublicKey))) {
		return openedBook{}, inputError("checkpoint signer must be explicitly trusted")
	}
	signer, err := evidencebook.NewEd25519Signer(recordKey)
	if err != nil {
		return openedBook{}, err
	}
	// The log is opened first: it takes an exclusive lock that a second
	// process cannot, and the record store repairs a torn final line when it
	// opens, which must never happen under another process's write.
	dir := filepath.Join(p.Connection.Database, "book")
	if !create {
		if _, err = os.Stat(filepath.Join(dir, "log.jsonl")); err != nil {
			return openedBook{}, inputError("the profile has no book yet")
		}
	}
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return openedBook{}, err
	}
	substrate, err := evidencebook.OpenCLL(filepath.Join(dir, "log.jsonl"), p.LogID+bookLogSuffix, checkpointKey)
	if err != nil {
		return openedBook{}, err
	}
	store, err := evidencebook.OpenFileStore(filepath.Join(dir, "records"))
	if err != nil {
		return openedBook{}, errors.Join(err, substrate.Release())
	}
	payloads, err := evidencebook.OpenPayloadDir(filepath.Join(dir, "payloads"))
	if err != nil {
		return openedBook{}, errors.Join(err, store.Release(), substrate.Release())
	}
	book, err := evidencebook.Open(ctx, evidencebook.Config{
		BookID: p.LogID, Operator: p.Operator,
		Store: store, Substrate: substrate, Payloads: payloads, Signer: signer,
		Now: bookNow,
	})
	if err != nil {
		return openedBook{}, errors.Join(bookError(err), store.Release(), substrate.Release())
	}
	return openedBook{book: book, store: store}, nil
}

// bookError keeps the book's own input classes on exit code 2.
func bookError(err error) error {
	if errors.Is(err, evidencebook.ErrInvalid) || errors.Is(err, evidencebook.ErrNotFound) {
		return errors.Join(ErrInput, err)
	}
	return err
}

// -- periods and windows ---------------------------------------------------

// period is one UTC day or one ISO week (Monday start). Its key is what a
// Close is idempotent on.
type period struct {
	key        string
	start, end time.Time
}

// parsePeriod resolves --period/--date. With no --date it takes the most
// recent period that has ended. A period that has not ended is refused:
// closing it would freeze a window that is still receiving records.
func parsePeriod(kind, date string, now time.Time) (period, error) {
	now = now.UTC()
	var day time.Time
	if date == "" {
		switch kind {
		case "day":
			day = now.AddDate(0, 0, -1)
		case "week":
			day = now.AddDate(0, 0, -7)
		}
	} else {
		parsed, err := time.Parse(time.DateOnly, date)
		if err != nil {
			return period{}, inputError("--date must be YYYY-MM-DD")
		}
		day = parsed
	}
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	var p period
	switch kind {
	case "day":
		p = period{key: "day:" + day.Format(time.DateOnly), start: day, end: day.AddDate(0, 0, 1)}
	case "week":
		start := day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
		year, week := start.ISOWeek()
		p = period{key: fmt.Sprintf("week:%04d-W%02d", year, week), start: start, end: start.AddDate(0, 0, 7)}
	default:
		return period{}, inputError("--period must be day or week")
	}
	if p.end.After(now) {
		return period{}, inputError("the period has not ended yet")
	}
	return p, nil
}

// placed is one record whose commit time is known from a verified header.
type placed struct {
	seq uint64
	at  time.Time
}

// seqWindow maps a period onto one log's positions. records must be in log
// order. from is one past the last placed record committed before the
// period starts; to is one before the first placed record committed at or
// after it ends. A record that cannot be placed (a withheld or unverified
// header) between those bounds is therefore inside the window, where it
// makes the account incomplete instead of silently falling out. from > to
// is an empty window.
//
// ended reports whether a placed record shows the log continued past the
// period's end; when none does, to is last and the caller decides what an
// unproven end means. Commit times that go backwards in log order are
// refused: no position then separates the period from its neighbours.
// to == 0 would read as an open end to the log, so it is refused too.
func seqWindow(records []placed, p period, last uint64) (from, to uint64, ended bool, err error) {
	from, to = 1, last
	var prev placed
	for _, r := range records {
		if r.seq <= prev.seq || r.at.Before(prev.at) {
			return 0, 0, false, inputError(fmt.Sprintf("commit times go backwards at position %d; the period cannot be placed in this log", r.seq))
		}
		prev = r
		if r.at.Before(p.start) {
			from = r.seq + 1
		} else if !r.at.Before(p.end) && !ended {
			to, ended = r.seq-1, true
		}
	}
	if to == 0 {
		return 0, 0, false, inputError("the log holds nothing committed before the period ends")
	}
	return from, to, ended, nil
}

func committedAt(h evidencebook.Header) (time.Time, error) {
	at, err := time.Parse(time.RFC3339Nano, h.CommittedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("record %d: committed_at: %w", h.Seq, err)
	}
	return at, nil
}

func ownWindow(ctx context.Context, book *evidencebook.Book, p period) (from, to uint64, err error) {
	records, err := book.Query(ctx, evidencebook.Filter{})
	if err != nil {
		return 0, 0, err
	}
	all := make([]placed, 0, len(records))
	for _, r := range records {
		at, err := committedAt(r.Header)
		if err != nil {
			return 0, 0, err
		}
		all = append(all, placed{seq: r.Seq, at: at})
	}
	from, to, _, err = seqWindow(all, p, book.Size())
	return from, to, err
}

// peerWindow maps the period onto the peer's positions. When no record the
// peer disclosed shows its log continuing past the period's end, the bundle
// may have been cut mid-period: the window then runs one position past the
// bundle's interval, which the book reads as an incomplete account.
func peerWindow(peer evidencebook.VerifiedBundle, p period) (from, to uint64, err error) {
	var known []placed
	for _, r := range peer.Records {
		if r.Header == nil || !r.HeaderVerified || !r.CapsuleOK {
			continue
		}
		at, err := committedAt(*r.Header)
		if err != nil {
			return 0, 0, errors.Join(ErrInput, err)
		}
		known = append(known, placed{seq: r.Seq, at: at})
	}
	from, to, ended, err := seqWindow(known, p, peer.IntervalLast)
	if err != nil {
		return 0, 0, err
	}
	if !ended {
		to = peer.IntervalLast + 1
	}
	return from, to, nil
}

// readPeer verifies a held peer bundle and pins its checkpoint key. The
// bundle verifier authenticates the checkpoint under whatever key the bundle
// names; only the caller knows which key is the peer's, so an unpinned or
// mismatched key is refused rather than reconciled against.
//
// The bundle must also be the counterparty's: every record whose header
// verified names --counterparty as its book, and at least one does.
func readPeer(path, key, counterparty string) (evidencebook.VerifiedBundle, error) {
	pinned, err := parseKeys([]string{key})
	if err != nil || key == "" {
		return evidencebook.VerifiedBundle{}, inputError("--peer needs --peer-checkpoint-key, a 32-byte Ed25519 public key in hex")
	}
	raw, err := readInput(path)
	if err != nil {
		return evidencebook.VerifiedBundle{}, err
	}
	peer, err := evidencebook.VerifyBundle(raw)
	if err != nil {
		return evidencebook.VerifiedBundle{}, errors.Join(ErrInput, err)
	}
	if !peer.AnchorAuthenticated || peer.AnchorKeyID != hex.EncodeToString(pinned[0]) {
		return evidencebook.VerifiedBundle{}, inputError("the peer bundle's checkpoint is not signed by --peer-checkpoint-key")
	}
	named := false
	for _, r := range peer.Records {
		if r.Header == nil || !r.HeaderVerified {
			continue
		}
		if r.Header.BookID != counterparty {
			return evidencebook.VerifiedBundle{}, inputError("the peer bundle holds records of a book other than --counterparty")
		}
		named = true
	}
	if !named {
		return evidencebook.VerifiedBundle{}, inputError("the peer bundle discloses no header naming its book")
	}
	return peer, nil
}

// -- close and reconcile ---------------------------------------------------

const closeComparator = "same_payload_commitments"

// closeProfile is the value a Close states as the profile that produced its
// reconciliation: this verb's version, the period, and the comparator.
// A second close for the same counterparty and period finds it and returns
// the existing Close instead of signing another.
func closeProfile(p period) string {
	return "capsulectl-close/v1 " + p.key + " comparator=" + closeComparator
}

type reconcileResult struct {
	Period         string                      `json:"period"`
	Comparator     string                      `json:"comparator"`
	Reconciliation evidencebook.Reconciliation `json:"reconciliation"`
}

type closeResult struct {
	RecordID       string                      `json:"record_id"`
	Seq            uint64                      `json:"seq"`
	Period         string                      `json:"period"`
	Counterparty   string                      `json:"counterparty"`
	AlreadyClosed  bool                        `json:"already_closed"`
	PeerBundle     string                      `json:"peer_bundle,omitempty"`
	Reconciliation evidencebook.Reconciliation `json:"reconciliation"`
	Capsule        string                      `json:"capsule,omitempty"`
	Bundle         string                      `json:"bundle,omitempty"`
	BundleDigest   string                      `json:"bundle_digest,omitempty"`
}

// priorCloses returns this book's Closes for one counterparty, in log order.
func priorCloses(ctx context.Context, book *evidencebook.Book, counterparty string) ([]evidencebook.Record, []evidencebook.CloseStatement, error) {
	records, err := book.Query(ctx, evidencebook.Filter{RecordType: evidencebook.RecordTypeClose, CounterpartyRef: counterparty})
	if err != nil {
		return nil, nil, err
	}
	statements := make([]evidencebook.CloseStatement, len(records))
	for i, r := range records {
		if err := json.Unmarshal(r.Header.Statement, &statements[i]); err != nil {
			return nil, nil, fmt.Errorf("close %s: statement: %w", r.RecordID, err)
		}
	}
	return records, statements, nil
}

// reconcileInput builds the windows both verbs share. A zero peer (no
// --peer) is an account that proves nothing, so every own exchange in the
// window reads INSUFFICIENT: one half unavailable is never disagreement.
func reconcileInput(ctx context.Context, book *evidencebook.Book, p period, peer evidencebook.VerifiedBundle, hasPeer bool) (evidencebook.ReconcileInput, error) {
	from, to, err := ownWindow(ctx, book, p)
	if err != nil {
		return evidencebook.ReconcileInput{}, err
	}
	in := evidencebook.ReconcileInput{Peer: peer, FromSeq: from, ToSeq: to, Compare: evidencebook.SamePayloadCommitments}
	if hasPeer {
		if in.PeerFromSeq, in.PeerToSeq, err = peerWindow(peer, p); err != nil {
			return evidencebook.ReconcileInput{}, err
		}
	}
	return in, nil
}

func periodFlags(c *cobra.Command) {
	c.Flags().String("period", "", "day or week (UTC; weeks start Monday)")
	c.Flags().String("date", "", "Any date inside the period, YYYY-MM-DD; default: the most recent period that has ended")
	c.Flags().String("counterparty", "", "The counterparty's book id; a peer bundle must be from that book")
	c.Flags().String("peer", "", "A held Evidence Bundle from the counterparty's book (never fetched)")
	c.Flags().String("peer-checkpoint-key", "", "The counterparty's checkpoint public key in hex, obtained independently")
}

type periodArgs struct {
	period       period
	counterparty string
	peer         evidencebook.VerifiedBundle
	hasPeer      bool
}

func periodAndPeer(c *cobra.Command) (periodArgs, error) {
	var a periodArgs
	a.counterparty, _ = c.Flags().GetString("counterparty")
	if a.counterparty == "" {
		return a, inputError("--counterparty is required")
	}
	kind, _ := c.Flags().GetString("period")
	date, _ := c.Flags().GetString("date")
	var err error
	if a.period, err = parsePeriod(kind, date, bookNow()); err != nil {
		return a, err
	}
	path, _ := c.Flags().GetString("peer")
	if path == "" {
		return a, nil
	}
	key, _ := c.Flags().GetString("peer-checkpoint-key")
	a.peer, err = readPeer(path, key, a.counterparty)
	a.hasPeer = err == nil
	return a, err
}

func reconcileCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "reconcile", Short: "Reconcile a period against a held peer bundle as of this book's latest checkpoint; adds no record", Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		p, e := selected(c)
		if e != nil {
			return e
		}
		a, e := periodAndPeer(c)
		if e != nil {
			return e
		}
		if !a.hasPeer {
			return inputError("--peer is required")
		}
		opened, e := openBook(c.Context(), p, false)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, opened.release()) }()
		in, e := reconcileInput(c.Context(), opened.book, a.period, a.peer, true)
		if e != nil {
			return e
		}
		recon, e := opened.book.Reconcile(c.Context(), in)
		if e != nil {
			return bookError(e)
		}
		return output(c, reconcileResult{Period: a.period.key, Comparator: closeComparator, Reconciliation: recon})
	}}
	periodFlags(cmd)
	return cmd
}

// sinceLastFrom moves a window's start back to just after the furthest
// position any earlier Close with this counterparty reached, so an unclosed
// day between two Closes is not skipped. A previous Close that already
// reached into this window (a period closed out of order) is refused.
func sinceLastFrom(statements []evidencebook.CloseStatement, from uint64) (uint64, error) {
	var reached uint64
	for _, st := range statements {
		reached = max(reached, st.Reconciliation.ToSeq)
	}
	if reached >= from {
		return 0, inputError("an earlier Close with this counterparty already reaches into this period; --since-last needs periods closed in order")
	}
	return reached + 1, nil
}

func closeCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "close", Short: "Seal the book's Close for one ended period and counterparty (signs; a repeat seals no second Close)", Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		p, e := selected(c)
		if e != nil {
			return e
		}
		a, e := periodAndPeer(c)
		if e != nil {
			return e
		}
		sinceLast, _ := c.Flags().GetBool("since-last")
		if sinceLast && a.hasPeer {
			return inputError("--since-last cannot be combined with --peer: the peer's window cannot be stretched to match")
		}
		opened, e := openBook(c.Context(), p, true)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, opened.release()) }()
		book := opened.book
		result := closeResult{Period: a.period.key, Counterparty: a.counterparty}
		records, statements, e := priorCloses(c.Context(), book, a.counterparty)
		if e != nil {
			return e
		}
		var sealed evidencebook.Record
		for i, st := range statements {
			if st.Profile == closeProfile(a.period) {
				sealed, result.AlreadyClosed, result.Reconciliation = records[i], true, st.Reconciliation
				break
			}
		}
		if !result.AlreadyClosed {
			// The window is fixed, and refused if it must be, before anything
			// is written. Then a checkpoint covers every own record in it, so
			// the book's own account of the period is complete.
			in, e := reconcileInput(c.Context(), book, a.period, a.peer, a.hasPeer)
			if e != nil {
				return e
			}
			if sinceLast && len(statements) > 0 {
				if in.FromSeq, e = sinceLastFrom(statements, in.FromSeq); e != nil {
					return e
				}
			}
			if _, e = book.Checkpoint(c.Context()); e != nil {
				return e
			}
			if sealed, e = book.Close(c.Context(), evidencebook.CloseInput{Reconcile: in, Counterparty: a.counterparty, Profile: closeProfile(a.period)}); e != nil {
				return bookError(e)
			}
			var st evidencebook.CloseStatement
			if e = json.Unmarshal(sealed.Header.Statement, &st); e != nil {
				return e
			}
			result.Reconciliation = st.Reconciliation
			if a.hasPeer {
				result.PeerBundle = a.peer.Digest
			}
		}
		result.RecordID, result.Seq = sealed.RecordID, sealed.Seq
		// The Close is sealed from here on: a failure writing either file
		// still reports its record_id, so the caller never loses it.
		if e = closeOutputs(c, opened, p, &result, sealed); e != nil {
			return errors.Join(e, output(c, result))
		}
		return output(c, result)
	}}
	periodFlags(cmd)
	cmd.Flags().Bool("since-last", false, "Start the window just after the furthest position an earlier Close with this counterparty reached")
	cmd.Flags().String("capsule-out", "", "Also write the Close as an artifact.Record for `verify --capsule`")
	cmd.Flags().String("bundle-out", "", "Also write an Evidence Bundle rooted at the Close (commits a disclosure record and a checkpoint)")
	return cmd
}

func closeOutputs(c *cobra.Command, opened openedBook, p Profile, result *closeResult, sealed evidencebook.Record) error {
	if path, _ := c.Flags().GetString("capsule-out"); path != "" {
		if err := exportRecord(c.Context(), opened.store, p, sealed.RecordID, path); err != nil {
			return err
		}
		result.Capsule = path
	}
	path, _ := c.Flags().GetString("bundle-out")
	if path == "" {
		return nil
	}
	include, err := closeBundleRecords(c.Context(), opened.book, result.Reconciliation.FromSeq, sealed.Seq)
	if err != nil {
		return err
	}
	bundle, err := opened.book.Bundle(c.Context(), evidencebook.BundleRequest{Root: sealed.RecordID, Include: include, WithholdPayloads: true})
	if err != nil {
		return bookError(err)
	}
	if _, err = evidencebook.VerifyBundle(bundle.JSON); err != nil {
		return err
	}
	if err = atomicFile(path, bundle.JSON, false); err != nil {
		return err
	}
	result.Bundle, result.BundleDigest = path, bundle.Digest
	return nil
}

// closeBundleRecords selects what a Close's bundle discloses so that the
// counterparty can reconcile against it: the header of every record from
// the one just before the Close's window through the Close itself. The
// record before the window places the window's start in time; the records
// between the window's end and the Close (the checkpoint's index roots)
// place its end. The bundle withholds every payload; reconciliation compares
// the payload commitments the headers carry.
func closeBundleRecords(ctx context.Context, book *evidencebook.Book, from, through uint64) ([]string, error) {
	records, err := book.Query(ctx, evidencebook.Filter{FromSeq: max(from, 2) - 1, ToSeq: through})
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(records))
	for i, r := range records {
		ids[i] = r.RecordID
	}
	return ids, nil
}

// exportRecord writes one book record as an artifact.Record: the sealed
// capsule, its producer envelope, and the canonical header it commits as
// its payload. It is verified under the profile's trusted keys before it is
// written, the same self-check `seal` applies.
func exportRecord(ctx context.Context, store *evidencebook.FileStore, p Profile, recordID, path string) error {
	stored, err := store.GetRecord(ctx, recordID)
	if err != nil {
		return err
	}
	record, err := artifact.Prepare(artifact.Record{
		CapsuleID: stored.RecordID, Capsule: stored.Capsule, ProducerEnvelope: stored.Envelope,
		Artifacts: []artifact.Artifact{{Name: "payload", Binding: artifact.PayloadDigest, State: artifact.Present, Content: stored.Header}},
	})
	if err != nil {
		return err
	}
	keys, err := parseKeys(p.TrustedKeys)
	if err != nil {
		return err
	}
	if _, err = artifact.Verify(record, keys); err != nil {
		return err
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return atomicFile(path, encoded, false)
}

// -- request and respond ---------------------------------------------------

type requestResult struct {
	RecordID      string `json:"record_id"`
	RequestDigest string `json:"request_digest,omitempty"`
	Output        string `json:"output,omitempty"`
	Outcome       string `json:"outcome,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// requestCommand has three modes, one per flag: ask (--request), record
// what came back (--response), or record that nothing came back
// (--absent-until). Each commits a record before it reports.
func requestCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "request", Short: "Ask a party for evidence, or record its answer or its absence (each is a signed book record)", Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		p, e := selected(c)
		if e != nil {
			return e
		}
		ask, _ := c.Flags().GetString("request")
		answer, _ := c.Flags().GetString("response")
		absentUntil, _ := c.Flags().GetString("absent-until")
		forID, _ := c.Flags().GetString("for")
		modes := 0
		for _, v := range []string{ask, answer, absentUntil} {
			if v != "" {
				modes++
			}
		}
		if modes != 1 || (ask == "") == (forID == "") {
			return inputError("give exactly one of --request, --response or --absent-until; --for names the request record for the last two")
		}
		var windowEnd time.Time
		if absentUntil != "" {
			if windowEnd, e = time.Parse(time.RFC3339Nano, absentUntil); e != nil {
				return inputError("--absent-until must be an RFC 3339 time")
			}
		}
		var req evidencebook.EvidenceRequest
		var resp evidencebook.Response
		if ask != "" {
			raw, e := readInput(ask)
			if e != nil {
				return e
			}
			if e = decodeJSON(raw, &req); e != nil {
				return e
			}
		}
		var responder evidencebook.ResponderKeys
		if answer != "" {
			// Both of the responder's keys are pinned by the caller, obtained
			// independently: a response that does not verify under them is
			// refused and never recorded, so it cannot foreclose the request.
			responder.Signer, _ = c.Flags().GetString("responder-key")
			responder.Checkpoint, _ = c.Flags().GetString("responder-checkpoint-key")
			if keys, e := parseKeys([]string{responder.Signer, responder.Checkpoint}); e != nil || len(keys) != 2 {
				return inputError("--response needs --responder-key and --responder-checkpoint-key, each a 32-byte Ed25519 public key in hex")
			}
			raw, e := readInput(answer)
			if e != nil {
				return e
			}
			if e = decodeJSON(raw, &resp); e != nil {
				return e
			}
		}
		opened, e := openBook(c.Context(), p, true)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, opened.release()) }()
		book := opened.book
		switch {
		case ask != "":
			responder, _ := c.Flags().GetString("responder")
			path, _ := c.Flags().GetString("output")
			if responder == "" || path == "" {
				return inputError("--request needs --responder and --output")
			}
			sent, e := book.Request(c.Context(), req, responder)
			if e != nil {
				return bookError(e)
			}
			if e = atomicFile(path, sent.Bytes, false); e != nil {
				return e
			}
			return output(c, requestResult{RecordID: sent.RecordID, RequestDigest: sent.Digest, Output: path})
		case answer != "":
			record, e := book.RecordResponse(c.Context(), forID, resp, responder)
			if e != nil {
				return bookError(e)
			}
			var st evidencebook.ResponseStatement
			if e = json.Unmarshal(record.Header.Statement, &st); e != nil {
				return e
			}
			return output(c, requestResult{RecordID: record.RecordID, RequestDigest: st.RequestDigest, Outcome: st.Outcome, Reason: st.Reason})
		default:
			route, _ := c.Flags().GetString("route")
			record, e := book.RecordAbsence(c.Context(), forID, windowEnd, route)
			if e != nil {
				return bookError(e)
			}
			var st evidencebook.AbsenceStatement
			if e = json.Unmarshal(record.Header.Statement, &st); e != nil {
				return e
			}
			return output(c, requestResult{RecordID: record.RecordID, RequestDigest: st.RequestDigest, Outcome: evidencebook.RecordTypeAbsence})
		}
	}}
	cmd.Flags().String("request", "", "Evidence request JSON to ask with (subject + exactly one coverage member)")
	cmd.Flags().String("responder", "", "Who is being asked (recorded in the request record)")
	cmd.Flags().String("output", "", "Write the canonical request bytes to transmit")
	cmd.Flags().String("for", "", "The request record id an answer or absence belongs to")
	cmd.Flags().String("response", "", "The response JSON that came back")
	cmd.Flags().String("responder-key", "", "Required with --response: the responder's signing public key in hex; a response under any other key is refused")
	cmd.Flags().String("responder-checkpoint-key", "", "Required with --response: the responder's checkpoint public key in hex; an artifact anchored under any other key is refused")
	cmd.Flags().String("absent-until", "", "RFC 3339 end of the window in which nothing arrived")
	cmd.Flags().String("route", "", "Route the request was sent over (recorded with an absence)")
	return cmd
}

type respondResult struct {
	RecordID      string `json:"record_id"`
	RequestDigest string `json:"request_digest"`
	Outcome       string `json:"outcome"`
	Reason        string `json:"reason,omitempty"`
	Relationship  string `json:"relationship"`
	Output        string `json:"output"`
}

func respondCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "respond", Short: "Answer a received evidence request with a signed artifact or refusal, and record it", Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		p, e := selected(c)
		if e != nil {
			return e
		}
		requestPath, _ := c.Flags().GetString("request")
		path, _ := c.Flags().GetString("output")
		if path == "" {
			return inputError("--output is required")
		}
		requestBytes, e := readInput(requestPath)
		if e != nil {
			return e
		}
		policy := evidencebook.DefaultSharePolicy()
		if policyPath, _ := c.Flags().GetString("policy"); policyPath != "" {
			raw, e := readInput(policyPath)
			if e != nil {
				return e
			}
			if e = decodeJSON(raw, &policy); e != nil {
				return e
			}
		}
		requester, _ := c.Flags().GetString("requester")
		opened, e := openBook(c.Context(), p, true)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, opened.release()) }()
		response, record, e := opened.book.Respond(c.Context(), requestBytes, evidencebook.RespondOptions{RequesterID: requester, Policy: policy})
		if e != nil {
			return bookError(e)
		}
		encoded, e := json.Marshal(response)
		if e != nil {
			return e
		}
		if e = atomicFile(path, encoded, false); e != nil {
			return e
		}
		var st evidencebook.AnsweredStatement
		if e = json.Unmarshal(record.Header.Statement, &st); e != nil {
			return e
		}
		return output(c, respondResult{RecordID: record.RecordID, RequestDigest: st.RequestDigest, Outcome: st.Outcome, Reason: st.Reason, Relationship: st.Relationship, Output: path})
	}}
	cmd.Flags().String("request", "", "The request bytes as received")
	cmd.Flags().String("requester", "", "The requester's identity as the transport supplied it (not authenticated here)")
	cmd.Flags().String("policy", "", "Share policy JSON (record_at_completion, history_segments, adjudications, witness); default: the documented defaults")
	cmd.Flags().String("output", "", "Write the signed response JSON to send back")
	return cmd
}
