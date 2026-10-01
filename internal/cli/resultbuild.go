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

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/agent-action-capsule/go/envelope"
	"github.com/action-state-group/evidencebook"
	"github.com/spf13/cobra"
)

// `result build` seals a caller-supplied Evidence Result v0 into the
// profile's book as one record the report is rooted on (2026-09-26:
// the Result is sealed into the book as a record that cites the daily
// reports and closes by digest; `bundle --disclose` roots on it). The book
// has no judging engine, so v0 seals what it can check and refuses what it
// cannot: the document must satisfy the vendored schema; its headline values
// must recompute from its claims; every digest a claim cites must resolve to
// a record this book holds; a close claim's close_ref must be in its
// evidence and its close_state must be what the counterparty's links to that
// Close read (checkCloseClaim); a reconcile claim's tallies must be what
// the Close it cites recorded. The record's header carries the document as
// its statement -- the member the book seals as the record capsule's
// agent_input_digest -- and a `cites` link to every resolved record, which
// is what the book's closure walk follows when a bundle is built on it.

type resultBuildResult struct {
	RecordID        string   `json:"record_id"`
	Seq             uint64   `json:"seq"`
	ContractRef     string   `json:"contract_ref,omitempty"`
	ContractRefs    []string `json:"contract_refs"`
	Claims          int      `json:"claims"`
	Cites           []string `json:"cites"`
	StatementDigest string   `json:"statement_digest"`
	AlreadyBuilt    bool     `json:"already_built"`
	Checkpoint      uint64   `json:"checkpoint,omitempty"`
	Result          string   `json:"result,omitempty"`
	Capsule         string   `json:"capsule,omitempty"`
}

// resolveRecord finds the book record a cited digest names: the record that
// carries a published capsule of that id, or the record with that id.
func resolveRecord(ctx context.Context, book *evidencebook.Book, digest string) (evidencebook.Record, bool, error) {
	if record, ok, err := publishedRecord(ctx, book, digest); err != nil || ok {
		return record, ok, err
	}
	record, err := book.Get(ctx, digest)
	if errors.Is(err, evidencebook.ErrNotFound) {
		return evidencebook.Record{}, false, nil
	}
	return record, err == nil, err
}

// recordStore is the part of the book's store the close walk reads: a
// record's sealed capsule and its Producer Envelope.
type recordStore interface {
	GetRecord(ctx context.Context, recordID string) (evidencebook.StoredRecord, error)
}

// errUnverifiedSigner marks a record whose Producer Envelope does not verify
// under its key_id; the walk refuses rather than read the key as stated.
var errUnverifiedSigner = errors.New("producer envelope does not verify under its key_id")

// verifiedKeyID is the key a book record is signed under, VERIFIED
// (2026-09-29: "verify the signature under
// key_id ... and don't let it pass the check"). The CLI holds the record's
// Producer Envelope, so it checks it: the sealed capsule recomputes to the
// record id (canonical.ComputeCapsuleID drops the local-only signature and
// key_id), envelope.Verify -- the aac module's own verifier -- accepts the
// envelope over that id, and the key_id is the envelope's kid, the raw
// Ed25519 public key in hex (evidencebook.KeyID's convention). Anything
// else is errUnverifiedSigner.
func verifiedKeyID(ctx context.Context, store recordStore, recordID string) (string, error) {
	stored, err := store.GetRecord(ctx, recordID)
	if err != nil {
		return "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(stored.Capsule))
	decoder.UseNumber()
	var capsule map[string]interface{}
	if decoder.Decode(&capsule) != nil {
		return "", errUnverifiedSigner
	}
	if id, err := canonical.ComputeCapsuleID(capsule); err != nil || id != recordID || stored.RecordID != recordID {
		return "", errUnverifiedSigner
	}
	verified := envelope.Verify(recordID, stored.Envelope)
	if !verified.OK || len(verified.PublicKey) == 0 {
		return "", errUnverifiedSigner
	}
	return hex.EncodeToString(verified.PublicKey), nil
}

// counterpartyReason says why a record linking to a Close is NOT its
// counterparty, or "" when it is (2026-09-29:
// "neither book_id nor signer alone is enough, since a producer can mint a
// second book or a second key equally easily"). A link counts only when
// the linking record (1) is from another book than the Close's, (2) that
// book is the claim's named peer, and (3) it is signed under another
// verified key than the Close. Both keys reach here verified. Until the
// contract pins the peer's key, a second book named as the peer and signed
// under a second key still passes this check.
func counterpartyReason(closeBook, closeKey, peer, linkBook, linkKey string) string {
	switch {
	case closeBook == "":
		return "the Close names no book, so nothing can be its counterparty"
	case linkBook == closeBook:
		return "it is from this book, the Close's own (a producer cannot agree with itself; the named peer's links are in the peer's book)"
	case peer == "":
		return "the claim names no peer, so no book can be the counterparty"
	case linkBook != peer:
		return fmt.Sprintf("its book %q is not the claim's named peer %q", linkBook, peer)
	case linkKey == closeKey:
		return "it is signed under the Close's own key"
	}
	return ""
}

// closeLinker is one record linking to a Close, and why it made no state
// when it did not.
type closeLinker struct {
	recordID string
	link     evidencebook.LinkType
	ignored  string
}

// closeLinkRecord is a record carrying an acknowledges or rebuts link to a
// Close: its id, the link, its book and its (verified) signer.
type closeLinkRecord struct {
	recordID string
	link     evidencebook.LinkType
	book     string
	key      string
}

// readCloseState is the walk itself (#140 section 4.1): of the records
// linking to a Close, only a counterparty's count (counterpartyReason); a
// counted rebuts makes it CONTESTED, else a counted acknowledges AGREED,
// else UNILATERAL. It never reads a state field. linker is the record whose
// link decided the state; ignored lists every other linker with its reason.
// closeStateInBook feeds it the book's records, and the upstream walk
// vectors feed it their record sidecars, so both run the same rules.
func readCloseState(closeBook, closeKey, peer string, linkers []closeLinkRecord) (state, linker string, ignored []closeLinker) {
	state = string(evidencebook.Unilateral)
	for _, typ := range []evidencebook.LinkType{evidencebook.Rebuts, evidencebook.Acknowledges} {
		for _, record := range linkers {
			if record.link != typ {
				continue
			}
			if reason := counterpartyReason(closeBook, closeKey, peer, record.book, record.key); reason != "" {
				ignored = append(ignored, closeLinker{recordID: record.recordID, link: typ, ignored: reason})
				continue
			}
			if linker == "" {
				linker = record.recordID
				state = string(evidencebook.Contested)
				if typ == evidencebook.Acknowledges {
					state = string(evidencebook.Agreed)
				}
			}
		}
	}
	return state, linker, ignored
}

// closeStateInBook gathers the records in this book that link to a Close,
// each with its VERIFIED signer, and reads the Close's state from them. A
// linker whose signer does not verify refuses the build.
func closeStateInBook(ctx context.Context, book *evidencebook.Book, store recordStore, closeRecord evidencebook.Record, closeKey, peer string) (state, linker string, ignored []closeLinker, err error) {
	var linkers []closeLinkRecord
	for _, typ := range []evidencebook.LinkType{evidencebook.Rebuts, evidencebook.Acknowledges} {
		records, err := book.Query(ctx, evidencebook.Filter{LinkType: typ, LinkTarget: closeRecord.RecordID})
		if err != nil {
			return "", "", nil, err
		}
		for _, record := range records {
			key, err := verifiedKeyID(ctx, store, record.RecordID)
			if errors.Is(err, errUnverifiedSigner) {
				return "", "", nil, hint(ErrInput, fmt.Sprintf("record %s %s Close %s, but its Producer Envelope does not verify under its key_id; an unverified signer is refused, never read as stated", record.RecordID, typ, closeRecord.RecordID))
			}
			if err != nil {
				return "", "", nil, err
			}
			linkers = append(linkers, closeLinkRecord{recordID: record.RecordID, link: typ, book: record.Header.BookID, key: key})
		}
	}
	state, linker, ignored = readCloseState(closeRecord.Header.BookID, closeKey, peer, linkers)
	return state, linker, ignored, nil
}

func containsDigest(digests []string, digest string) bool {
	for _, d := range digests {
		if d == digest {
			return true
		}
	}
	return false
}

// citedClose returns the one Close among a claim's resolved evidence, or
// refuses: a close or reconcile claim reports exactly one Close.
func citedClose(claim resultClaim, resolved map[string]evidencebook.Record) (evidencebook.Record, error) {
	var closes []evidencebook.Record
	for _, digest := range claim.evidence {
		if record, ok := resolved[digest]; ok && record.Header.RecordType == evidencebook.RecordTypeClose {
			closes = append(closes, record)
		}
	}
	switch len(closes) {
	case 1:
		return closes[0], nil
	case 0:
		return evidencebook.Record{}, hint(ErrInput, fmt.Sprintf("%s is a %s claim but cites no Close record in this book; its evidence must name the Close it reports", claim.label(), claim.typ))
	default:
		return evidencebook.Record{}, hint(ErrInput, fmt.Sprintf("%s cites %d Close records; a %s claim reports exactly one Close", claim.label(), len(closes), claim.typ))
	}
}

// resolveResultCitations resolves every digest the Result cites and applies
// the checks that need the book. It returns the cites links in citation
// order, each record once.
func resolveResultCitations(ctx context.Context, book *evidencebook.Book, store recordStore, result resultDocument) ([]evidencebook.Link, error) {
	var links []evidencebook.Link
	linked := make(map[string]bool)
	resolved := make(map[string]evidencebook.Record)
	resolve := func(claim resultClaim, digest, role string) (evidencebook.Record, error) {
		if record, done := resolved[digest]; done {
			return record, nil
		}
		record, ok, err := resolveRecord(ctx, book, digest)
		if err != nil {
			return evidencebook.Record{}, err
		}
		if !ok {
			return evidencebook.Record{}, hint(ErrInput, fmt.Sprintf("%s cites %s as %s, which is not in this book (neither a published capsule nor a record id); a Result is sealed only over evidence the book holds", claim.label(), digest, role))
		}
		resolved[digest] = record
		if !linked[record.RecordID] {
			linked[record.RecordID] = true
			links = append(links, evidencebook.Link{Type: evidencebook.Cites, Target: record.RecordID})
		}
		return record, nil
	}
	for _, claim := range result.claims {
		if len(claim.evidence) == 0 {
			return nil, hint(ErrInput, fmt.Sprintf("%s cites no evidence; every claim must trace to at least one record in this book", claim.label()))
		}
		for _, digest := range claim.evidence {
			if _, err := resolve(claim, digest, "evidence"); err != nil {
				return nil, err
			}
		}
		for _, digest := range claim.carrierEvidence {
			if _, err := resolve(claim, digest, "presentation evidence"); err != nil {
				return nil, err
			}
		}
		if claim.closePeerRef != "" {
			if _, err := resolve(claim, claim.closePeerRef, "close.peer_close_ref"); err != nil {
				return nil, err
			}
		}
		switch claim.typ {
		case claimTypeClose:
			if err := checkCloseClaim(ctx, book, store, claim, resolved); err != nil {
				return nil, err
			}
		case claimTypeReconcile:
			if err := checkReconcileClaim(claim, resolved); err != nil {
				return nil, err
			}
		}
	}
	return links, nil
}

// checkCloseClaim applies #140's walk rules (section 4.1) to a close claim:
// close_ref -- and peer_close_ref, when present -- is among the claim's
// evidence[]; close_ref names a Close in this book; the claim's peer is the
// Close's counterparty; the Close's own signer verifies; close_state is what
// the counterparty's links to the Close read (counterpartyReason), never
// asserted; and when a counterparty record decided that state,
// peer_close_ref names it.
func checkCloseClaim(ctx context.Context, book *evidencebook.Book, store recordStore, claim resultClaim, resolved map[string]evidencebook.Record) error {
	closeRecord := resolved[claim.closeRef]
	if err := checkCloseRefs(claim, closeRecord.Header.RecordType); err != nil {
		return err
	}
	if claim.closePeer != "" && closeRecord.Header.CounterpartyRef != claim.closePeer {
		return hint(ErrInput, fmt.Sprintf("%s names peer %q, but Close %s was sealed against counterparty %q", claim.label(), claim.closePeer, closeRecord.RecordID, closeRecord.Header.CounterpartyRef))
	}
	closeKey, err := verifiedKeyID(ctx, store, closeRecord.RecordID)
	if errors.Is(err, errUnverifiedSigner) {
		return hint(ErrInput, fmt.Sprintf("%s: Close %s's Producer Envelope does not verify under its key_id, so no signer can be shown to differ from its own", claim.label(), closeRecord.RecordID))
	}
	if err != nil {
		return err
	}
	state, linker, ignored, err := closeStateInBook(ctx, book, store, closeRecord, closeKey, claim.closePeer)
	if err != nil {
		return err
	}
	return checkCloseState(claim, closeRecord.RecordID, state, linker, ignored)
}

// checkCloseRefs: close_ref -- and peer_close_ref, when present -- is among
// the claim's evidence[], and close_ref names a Close.
func checkCloseRefs(claim resultClaim, closeRecordType string) error {
	if !containsDigest(claim.evidence, claim.closeRef) {
		return hint(ErrInput, fmt.Sprintf("%s: close.close_ref %s is not among its evidence[]; a claim reports only on a Close it puts in evidence", claim.label(), claim.closeRef))
	}
	if claim.closePeerRef != "" && !containsDigest(claim.evidence, claim.closePeerRef) {
		return hint(ErrInput, fmt.Sprintf("%s: close.peer_close_ref %s is not among its evidence[]; a claim cites only a state-making record it puts in evidence", claim.label(), claim.closePeerRef))
	}
	if closeRecordType != evidencebook.RecordTypeClose {
		return hint(ErrInput, fmt.Sprintf("%s: close.close_ref %s is a %s record, not a Close; close_ref names the Close the claim reports", claim.label(), claim.closeRef, closeRecordType))
	}
	return nil
}

// checkCloseState: the claim's close_state is the state the walk read, and
// when a counterparty record decided it, peer_close_ref names that record.
func checkCloseState(claim resultClaim, closeID, state, linker string, ignored []closeLinker) error {
	if claim.closeState != state {
		reason := "no counterparty record acknowledges or rebuts it"
		switch state {
		case string(evidencebook.Agreed):
			reason = "record " + linker + " acknowledges it"
		case string(evidencebook.Contested):
			reason = "record " + linker + " rebuts it"
		}
		for _, l := range ignored {
			reason += fmt.Sprintf("; record %s %s it but is ignored: %s", l.recordID, l.link, l.ignored)
		}
		return hint(ErrInput, fmt.Sprintf("%s states close_state %s, but the counterparty's links to Close %s read %s (%s); close_state is read from the named peer's links, never asserted", claim.label(), claim.closeState, closeID, state, reason))
	}
	if linker != "" && claim.closePeerRef != linker {
		return hint(ErrInput, fmt.Sprintf("%s cites peer_close_ref %s, but the record whose link makes Close %s %s is %s", claim.label(), claim.closePeerRef, closeID, state, linker))
	}
	return nil
}

// reconcileTallyKeys maps a Result's lowercase tally keys onto the Close
// statement's state names.
var reconcileTallyKeys = []struct{ claim, close string }{
	{"matched", "MATCHED"}, {"a_only", "A_ONLY"}, {"b_only", "B_ONLY"},
	{"conflicting", "CONFLICTING"}, {"insufficient", "INSUFFICIENT"}, {"unresolved", "UNRESOLVED"},
}

// checkReconcileClaim: the claim's tallies are the ones the Close it cites
// sealed, state for state, and its peer is that Close's counterparty. A
// reconciliation is on record only as a Close (`reconcile` adds no record),
// so a reconcile claim that cites no Close has nothing to recompute from.
func checkReconcileClaim(claim resultClaim, resolved map[string]evidencebook.Record) error {
	closeRecord, err := citedClose(claim, resolved)
	if err != nil {
		return err
	}
	if closeRecord.Header.CounterpartyRef != claim.reconcilePeer {
		return hint(ErrInput, fmt.Sprintf("%s names peer %q, but Close %s was sealed against counterparty %q", claim.label(), claim.reconcilePeer, closeRecord.RecordID, closeRecord.Header.CounterpartyRef))
	}
	var statement evidencebook.CloseStatement
	if err := json.Unmarshal(closeRecord.Header.Statement, &statement); err != nil {
		return fmt.Errorf("close %s: statement: %w", closeRecord.RecordID, err)
	}
	var sealed map[string]int64
	raw, err := json.Marshal(statement.Reconciliation.Tallies)
	if err == nil {
		err = json.Unmarshal(raw, &sealed)
	}
	if err != nil {
		return err
	}
	for _, key := range reconcileTallyKeys {
		if claim.reconcileTallies[key.claim] != sealed[key.close] {
			return hint(ErrInput, fmt.Sprintf("%s tallies %s=%d, but Close %s recorded %s=%d; tallies are what the Close sealed, never restated", claim.label(), key.claim, claim.reconcileTallies[key.claim], closeRecord.RecordID, key.close, sealed[key.close]))
		}
	}
	return nil
}

// statementDigest is SHA-256 over the JCS form of the document: the bytes
// `--out` writes, and the bytes the header carries as its statement.
func statementDigest(doc map[string]interface{}) ([]byte, string, error) {
	encoded, err := canonical.JCS(doc)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(encoded)
	return encoded, hex.EncodeToString(sum[:]), nil
}

// priorResult finds an evidence_result record whose statement is this same
// document, so a repeat seals no second record.
func priorResult(ctx context.Context, book *evidencebook.Book, digest string) (evidencebook.Record, bool, error) {
	records, err := book.Query(ctx, evidencebook.Filter{RecordType: resultRecordType})
	if err != nil {
		return evidencebook.Record{}, false, err
	}
	for _, record := range records {
		statement, err := decodeResultDocument(record.Header.Statement)
		if err != nil {
			continue
		}
		encoded, err := canonical.JCS(statement)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(encoded)
		if hex.EncodeToString(sum[:]) == digest {
			return record, true, nil
		}
	}
	return evidencebook.Record{}, false, nil
}

func resultBuildCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "build", Short: "Seal a validated Evidence Result v0 into the book as the record a report is rooted on (signs; a repeat seals no second record)", Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		p, e := selected(c)
		if e != nil {
			return e
		}
		resultPath, _ := c.Flags().GetString("result")
		out, _ := c.Flags().GetString("out")
		contract, _ := c.Flags().GetString("contract")
		if resultPath == "" || out == "" {
			return inputError("--result and --out are required")
		}
		raw, e := readInput(resultPath)
		if e != nil {
			return e
		}
		doc, e := decodeResultDocument(raw)
		if e != nil {
			return e
		}
		// Everything the document alone can be checked for (schema, headline
		// cross-checks, --contract) is checked before the book is opened, so
		// such a document never takes the lock; what needs the book --
		// citations, close state, tallies -- is checked once it is open,
		// before anything is appended.
		checked, e := checkResult(doc, contract)
		if e != nil {
			return e
		}
		encoded, digest, e := statementDigest(doc)
		if e != nil {
			return e
		}
		opened, e := openBook(c.Context(), p, false)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, opened.release()) }()
		book := opened.book
		links, e := resolveResultCitations(c.Context(), book, opened.store, checked)
		if e != nil {
			return e
		}
		result := resultBuildResult{ContractRef: checked.contractRef, ContractRefs: checked.contracts, Claims: len(checked.claims), StatementDigest: digest, Cites: make([]string, 0, len(links))}
		for _, link := range links {
			result.Cites = append(result.Cites, link.Target)
		}
		sealed, already, e := priorResult(c.Context(), book, digest)
		if e != nil {
			return e
		}
		result.AlreadyBuilt = already
		if !already {
			// The statement is the document exactly as supplied (the book
			// canonicalizes the header it seals) and the subject is the one
			// contract it reports, when there is one; a checkpoint then
			// covers the record, so a bundle can be built on it at once.
			if sealed, e = book.Append(c.Context(), evidencebook.Entry{
				RecordType: resultRecordType, EpistemicType: evidencebook.DerivedMetric,
				SubjectRef: checked.contractRef, Links: links, Statement: json.RawMessage(raw),
			}); e != nil {
				return bookError(e)
			}
			checkpoint, e := book.Checkpoint(c.Context())
			if e != nil {
				result.RecordID, result.Seq = sealed.RecordID, sealed.Seq
				return errors.Join(e, output(c, result))
			}
			result.Checkpoint = checkpoint.Entries
		}
		result.RecordID, result.Seq = sealed.RecordID, sealed.Seq
		// The record is sealed from here on: a failure writing either file
		// still reports its record_id, so the caller never loses it. A
		// repeat with the same flags finds both files already holding these
		// bytes and rewrites nothing.
		if e = resultOutputs(c, opened, p, &result, sealed, encoded, out); e != nil {
			return errors.Join(e, output(c, result))
		}
		return output(c, result)
	}}
	cmd.Flags().String("result", "", "Evidence Result v0 document to seal (JSON)")
	cmd.Flags().String("contract", "", "The <contract_id>@<version> every claim must name (the record's subject); default: the one contract the claims name, if they name one")
	cmd.Flags().String("out", "", "Write the sealed document in its canonical (JCS) form, the bytes the record's statement digest covers")
	cmd.Flags().String("capsule-out", "", "Also write the evidence_result record as an artifact.Record for `verify --capsule`")
	return cmd
}

// writeOutput writes data at path as a new file, or leaves alone a regular
// file already holding exactly these bytes, so a repeat of the verb with the
// same output flags is the no-op it reports. Anything else at the path is
// refused by name and nothing is overwritten: the record is sealed either
// way (its record_id is in the output), so the caller passes another path,
// or moves the file, and repeats.
func writeOutput(flag, path string, data []byte) error {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return atomicFile(path, data, false)
	case err != nil:
		return err
	case !info.Mode().IsRegular():
		return hint(ErrInput, fmt.Sprintf("%s %s exists and is not a regular file; nothing was written there (the record is sealed: see record_id); pass another path", flag, path))
	}
	existing, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(existing, data) {
		return hint(ErrInput, fmt.Sprintf("%s %s already exists and does not hold these bytes; nothing was overwritten (the record is sealed: see record_id); pass another path, or move that file, and repeat", flag, path))
	}
	return nil
}

func resultOutputs(c *cobra.Command, opened openedBook, p Profile, result *resultBuildResult, sealed evidencebook.Record, encoded []byte, out string) error {
	if err := writeOutput("--out", out, encoded); err != nil {
		return err
	}
	result.Result = out
	if path, _ := c.Flags().GetString("capsule-out"); path != "" {
		if err := exportRecord(c.Context(), opened.store, p, sealed.RecordID, path); err != nil {
			return err
		}
		result.Capsule = path
	}
	return nil
}
