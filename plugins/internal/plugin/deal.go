package plugin

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/action-state-group/capsule-cli/internal/cli"
	emit "github.com/action-state-group/capsule-emit/go"
	"github.com/action-state-group/capsule-emit/go/artifact"
	"github.com/action-state-group/checkpointed-local-log/go/checkpoint"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/action-state-group/evidencebook"
	"github.com/spf13/cobra"
)

// The local store, in the profile's SQLite file. It never leaves the device:
// deal_steps.local holds each step's raw values and commitment nonces,
// deal_disclosures each shared copy's disclosure record, deal_reports the
// sealed report capsule of each copy written (dealSealReport),
// deal_keys each deal's key, and deal_store the store secret the keys derive
// from. What is sealed is only the x-deal-v0 record derived from them.
var dealIndexSchema = []string{
	dealStepsTable,
	`CREATE TABLE IF NOT EXISTS deal_disclosures (
	deal_id TEXT NOT NULL,
	n INTEGER NOT NULL,
	record_digest TEXT NOT NULL,
	cll_sequence INTEGER NOT NULL,
	record TEXT NOT NULL,
	PRIMARY KEY (deal_id, n)
)`,
	dealReportsTable,
	`CREATE TABLE IF NOT EXISTS deal_keys (deal_id TEXT PRIMARY KEY, deal_key BLOB NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS deal_store (id INTEGER PRIMARY KEY CHECK (id = 1), secret BLOB NOT NULL)`,
	dealCadenceSchema,
	dealDeferredEmailSchema,
	dealRequestsSchema,
}

// dealSession holds the write lock, the index handle and the signing key for
// one deal command, and the store opened for the one deal it acts on.
type dealSession struct {
	p      cli.Profile
	dp     cli.Profile // p with the deal's own log selected
	db     *sql.DB
	index  dealIndex // the store's step and report index (deal_index.go)
	t      *cli.Target
	key    ed25519.PrivateKey
	keys   []ed25519.PublicKey
	secret []byte // the store secret
	dkey   []byte // the current deal's key
	unlock func() error
	shared bool // a read under the shared lock (openDealStoreRead)
	closed bool
	// shelf is the deal store's one writer (deal_shelf.go), open from
	// openDealSession until close; legacy is the current deal's log on it,
	// and cadence the profile's cadence once opened.
	shelf   *evidencebook.Shelf
	legacy  *evidencebook.LegacyLog
	cadence *evidencebook.Cadence
	// onBook is set when the current deal is kept on a book (deal_books.go),
	// and book is that book, opened by a writer only (opening a book can
	// commit a pending record); legacy is nil then. inFlight is the time the
	// shelf's clock gives while a record is appended (appendBook).
	onBook   bool
	book     *evidencebook.Book
	inFlight time.Time
	// reportAt, when set, is the time a copy's report is computed at: the
	// bound checkpoint's, for a report off the deal's log (capsule-cli#294),
	// so its lifecycle never states when the copy was made.
	reportAt time.Time
	// originals is the store's artifact store, the shelf's; bookEnded is
	// which live book deals have ended, read before a receipt is reported
	// (settleEnded).
	originals cli.ArtifactStore
	bookEnded map[string]bool
	// migrated is set once migrate has completed this session: only then
	// does close record the mutation counter as seen, so a failed or
	// interrupted repair is run again on the next open.
	migrated bool
}

// HoldsDealStore is the session's proof, to cli.OpenDealTarget, that it
// holds the deal store's writer lock: from openDealSession until close. A
// read session holds only the shared lock, and is no writer.
func (s *dealSession) HoldsDealStore() bool { return s.shelf != nil && !s.shared && !s.closed }

// dealStoreLock is the writer lock held outside a session: deal init's,
// while it creates the store.
type dealStoreLock struct{ held bool }

func (l *dealStoreLock) HoldsDealStore() bool { return l.held }

func openDealSession(ctx context.Context, p cli.Profile) (_ *dealSession, err error) {
	if p.Type != "sqlite" {
		return nil, cli.InputError("deal commands need a sqlite profile; create one with `deal init`")
	}
	if p.ReadOnly {
		return nil, cli.ErrReadOnlyCLL
	}
	key, err := cli.PrivateKey(p.Signing)
	if err != nil {
		return nil, err
	}
	if err = cli.RequirePublisherKey(p, key); err != nil {
		return nil, err
	}
	keys, err := cli.ParseKeys(p.TrustedKeys)
	if err != nil {
		return nil, err
	}
	dbPath, err := filepath.Abs(p.Connection.Database)
	if err != nil {
		return nil, err
	}
	if _, err = os.Stat(dbPath); err != nil {
		return nil, cli.InputError("deal store does not exist; run `deal init`")
	}
	s := &dealSession{p: p, key: key, keys: keys}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.close())
		}
	}()
	if s.db, _, err = cli.SqliteConnection(p); err != nil {
		return nil, err
	}
	originals, err := cli.NewArtifactStore(p, s.db, keys)
	if err != nil {
		return nil, err
	}
	// The shelf takes the store's writer lock (the lock file every released
	// build takes), and holds it until close.
	if err = s.openShelf(ctx, dbPath, originals); err != nil {
		return nil, err
	}
	s.originals = originals
	s.index = sqliteDealIndex{s.db}
	for _, stmt := range dealIndexSchema {
		if _, err = s.db.ExecContext(ctx, stmt); err != nil {
			return nil, err
		}
	}
	// The store secret: 32 CSPRNG bytes, made once, never written to a record.
	err = s.db.QueryRowContext(ctx, `SELECT secret FROM deal_store WHERE id=1`).Scan(&s.secret)
	if errors.Is(err, sql.ErrNoRows) {
		s.secret = make([]byte, 32)
		if _, err = rand.Read(s.secret); err != nil {
			return nil, err
		}
		_, err = s.db.ExecContext(ctx, `INSERT INTO deal_store (id, secret) VALUES (1, ?)`, s.secret)
	}
	if err != nil {
		return nil, err
	}
	if len(s.secret) != 32 {
		return nil, cli.ErrConflict
	}
	if err = s.migrate(ctx); err != nil {
		return nil, err
	}
	// The migration reads legacy deals only; from here on the step source
	// holds book deals too (deal_books.go).
	s.index = s.books()
	return s, nil
}

// useDeal opens the store with the deal's own log, creating the log for a new
// deal. An existing deal must already have indexed steps.
func (s *dealSession) useDeal(ctx context.Context, dealID string, create bool) error {
	if !cli.DealIDPattern.MatchString(dealID) && !cli.SaleIDPattern.MatchString(dealID) {
		return cli.InputError("--deal must be a deal id as printed by `deal open`: deal- followed by 16 hex characters")
	}
	if create {
		// The deal key is stored at open, so rotating the store secret later
		// cannot break a deal in progress.
		s.dkey = dealKeyFor(s.secret, dealID)
		res, err := s.db.ExecContext(ctx, `INSERT INTO deal_keys (deal_id, deal_key) VALUES (?, ?) ON CONFLICT(deal_id) DO NOTHING`, dealID, s.dkey)
		if err != nil {
			return err
		}
		// A key row with nothing sealed under it is an open that stopped
		// before its baseline: it is taken up again, with the key it holds.
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			steps, err := s.index.stepCount(ctx, dealID)
			if err != nil {
				return err
			}
			if steps > 0 {
				return cli.InputError("this profile already holds deal " + dealID)
			}
			if err := s.db.QueryRowContext(ctx, `SELECT deal_key FROM deal_keys WHERE deal_id=?`, dealID).Scan(&s.dkey); err != nil {
				return err
			}
		}
	} else {
		err := s.db.QueryRowContext(ctx, `SELECT deal_key FROM deal_keys WHERE deal_id=?`, dealID).Scan(&s.dkey)
		if errors.Is(err, sql.ErrNoRows) {
			return cli.InputError("unknown deal: " + dealID)
		}
		if err != nil {
			return err
		}
	}
	s.dp = s.p
	s.dp.LogID = cli.DealLogID(dealID)
	s.onBook, s.book, s.legacy = false, nil, nil
	if x, ok := s.index.(shelfDealIndex); ok {
		legacy, err := x.isLegacy(ctx, dealID)
		if err != nil {
			return err
		}
		s.onBook = !legacy && (!create || dealNewDealsOnBooks)
	}
	if s.onBook {
		// A deal opened since books: its book appends every record, and the
		// target reads its originals, entries and checkpoints only.
		book, err := s.shelf.Open(ctx, s.dp.LogID)
		if err != nil {
			return err
		}
		s.book = book
		t, err := cli.OpenDealTarget(ctx, s.dp, cli.UseRead, s)
		if err != nil {
			return err
		}
		s.t = t
		// A writer finishes a step a crash left without its checkpoint before
		// anything else reads or extends the deal (deal_books.go coverSteps).
		if !create && !s.shared {
			return s.coverSteps(ctx, dealID, book)
		}
		return nil
	}
	use := cli.UsePublication
	if create {
		use = cli.UseInitialization
	}
	t, err := cli.OpenDealTarget(ctx, s.dp, use, s)
	if err != nil {
		return err
	}
	s.t = t
	// The deal's log is a legacy log of the shelf: every append to it goes
	// through the shelf, which keeps it in the cadence (note §3.1).
	if s.shelf != nil {
		if s.legacy, err = s.shelf.Legacy(ctx, s.dp.LogID); err != nil {
			return err
		}
	}
	return nil
}

func (s *dealSession) close() error {
	if s.closed {
		return nil
	}
	var e error
	if s.migrated && s.db != nil && !s.shared {
		e = s.markSeen(context.Background())
	}
	if s.t != nil {
		e = errors.Join(e, s.t.Close())
	}
	if s.db != nil {
		e = errors.Join(e, s.db.Close())
	}
	s.closed = true
	if s.unlock != nil {
		e = errors.Join(e, s.unlock())
	}
	return e
}

// stepEntries reads the deal's log, in order, without the entries of its
// sealed reports: the steps alone.
func (s *dealSession) stepEntries(ctx context.Context, dealID string) ([]cll.Entry, error) {
	if x, ok := s.bookIndex(); ok {
		return x.bookEntries(ctx, dealID, true)
	}
	entries, err := s.logEntries(ctx)
	if err != nil {
		return nil, err
	}
	reports, err := s.reportIDs(ctx, dealID)
	if err != nil {
		return nil, err
	}
	steps := entries[:0:0]
	for _, e := range entries {
		if !reports[hex.EncodeToString(e.Value)] {
			steps = append(steps, e)
		}
	}
	return steps, nil
}

// logEntries reads the deal's whole log, in order.
func (s *dealSession) logEntries(ctx context.Context) ([]cll.Entry, error) {
	if x, ok := s.bookIndex(); ok {
		dealID, _ := dealOfBook(s.dp.LogID)
		return x.bookEntries(ctx, dealID, false)
	}
	var out []cll.Entry
	for after := uint64(0); ; {
		batch, err := s.t.Log.ScanEntries(ctx, after, cll.MaxScanLimit)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			return out, nil
		}
		out = append(out, batch...)
		after = batch[len(batch)-1].Seq
	}
}

// load reads a deal's steps back and re-verifies each one against the deal's
// own log: the log, less its sealed reports' entries, must hold exactly one
// entry per indexed step, in order,
// so a lost or deleted step is a conflict, never a silently shorter deal.
// Then, per step: the Capsule signature and trust, the payload binding, the
// local step re-derived into exactly the sealed record bytes, and the prev
// chain of record digests. A last step that was indexed but never reached
// the log (a crash between the two writes) is recovered by publishing it
// from the stored step; its Capsule must come out identical.
func (s *dealSession) load(ctx context.Context, dealID string) (_ []sealedEvent, err error) {
	if s.onBook {
		return s.loadBook(ctx, dealID)
	}
	index, err := s.index.steps(ctx, stepFilter{DealID: dealID})
	if err != nil {
		return nil, err
	}
	entries, err := s.stepEntries(ctx, dealID)
	if err != nil {
		return nil, err
	}
	switch {
	case len(entries) > len(index), len(index) > len(entries)+1:
		return nil, cli.ErrConflict
	case len(index) == 0:
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
		if i == len(entries) {
			se, err := s.recoverStep(ctx, events, ev, r.CapsuleID)
			if err != nil {
				return nil, err
			}
			events = append(events, se)
			continue
		}
		if hex.EncodeToString(entries[i].Value) != r.CapsuleID {
			return nil, cli.ErrConflict
		}
		record, err := s.t.Artifacts.Get(ctx, r.CapsuleID)
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
		payload, digest, err := encodeDealRecord(ev, events, s.dkey)
		if err != nil || !bytes.Equal(payload, content) || digest != r.Digest {
			return nil, cli.ErrConflict
		}
		events = append(events, sealedEvent{CapsuleID: r.CapsuleID, Digest: digest, Sequence: entries[i].Seq, Event: ev})
	}
	return events, nil
}

// dealEffectTypes maps a deal action to its registered AAC effect.type
// (agent-action-capsule spec/REGISTRY.md §3, "effect.type"). Only an action
// listed here, or in dealActEffectTypes, is sealed as a decide Capsule. A
// disclosure (share_contact, share_credentials) has no effect type yet, so
// its Capsule stays fyi rather than carry one nobody proposed.
var dealEffectTypes = map[string]string{"pay": "send_payment"}

// dealActEffectTypes are the AAC effect.types of the other actions a deal
// takes. They are PROVISIONAL: proposed for agent-action-capsule's registry,
// held for ratification and not yet registered, so a verifier reads them as
// unregistered values (informational, never a rejection); the deal profile's
// PROFILE.md labels each one, and a test holds it to that. A step that
// carries ActEffects seals an approved offer, commit, sign or cancel as a
// decide Capsule with its disposition, as a pay is. A step sealed before has
// none, and re-derives fyi as it was sealed.
var dealActEffectTypes = map[string]string{
	offerAction: "make_offer",
	"commit":    "accept_agreement",
	"sign":      "sign_document",
	"cancel":    "cancel_commitment",
}

// dealActEffectsVersion marks a step whose act, other than a pay, carries
// its effect type and disposition (dealActEffectTypes).
const dealActEffectsVersion = "1"

// dealActCounterpartyVersion marks a step whose offer, commit, sign or cancel
// act seals the counterparty of the check it rests on (an act names no payee,
// so without it a rule keying earlier acts on who they were with has no
// target in the sealed record).
const dealActCounterpartyVersion = "1"

// dealIDMember is the top-level Capsule member holding the deal id on every
// step's Capsule (x-deal-v0 section 1), so a deal's copies held by two
// parties join on it.
const dealIDMember = "deal_id"

// dealCapsuleForm is which release's Capsule a step is sealed as. A step is
// sealed in the current form; the earlier forms are tried only to recover a
// step an earlier release prepared but never published.
type dealCapsuleForm int

const (
	// dealCapsuleCurrent carries deal_id.
	dealCapsuleCurrent dealCapsuleForm = iota
	// dealCapsuleNoDealID is the form before deal_id: classed, no deal_id.
	dealCapsuleNoDealID
	// dealCapsuleAllFYI is the all-fyi form of the releases before classing.
	dealCapsuleAllFYI
)

// dealCapsuleInput is a step's Capsule, claiming only what the deal record
// shows. A record that says an action was taken under a sealed approval (an
// authorized act whose action has an effect type: dealEffectTypes, or
// dealActEffectTypes on a step that carries ActEffects) is a decide
// Capsule:
//   - disposition: accepted, and executed, as the act reports. In a typed
//     action-record/v0 it is read from the record's own authority_basis, the
//     canonical authority field (basisDisposition): human only when the
//     user's own approval is a layer, policy when the task authority alone
//     covered it (a DO); a platform approval never makes it human. In an
//     x-deal-v0 record, the approver is the one the approval records: a human
//     only when the user approved it in their own words (committed as
//     said_commitment), policy when a passing check approved it under the
//     user's standing intent. An act approved by agent_card (a click on a
//     card the agent composed) stays fyi;
//   - effect: dispatched, runtime_claimed: the agent's report that it acted,
//     with no response in hand, so effect_mode is dispatched_unconfirmed;
//     one_way_recoverable when the agreed recourse says refundable, otherwise
//     one_way_consequential.
//
// Every other record stays fyi, including an unchecked act, which the deal
// record itself seals as an outcome and never as an action. Every step's
// Capsule carries the deal id as its top-level deal_id member, which a
// composition joins the parties' copies on. form gives instead a Capsule an
// earlier capsulectl release sealed, for recovering a step one of them
// prepared (see dealCapsuleForm).
func dealCapsuleInput(events []sealedEvent, ev dealEvent, record []byte, operator string, at time.Time, form dealCapsuleForm) emit.Input {
	legacy := form == dealCapsuleAllFYI
	in := emit.Input{ActionID: fmt.Sprintf("%s/%d", ev.DealID, ev.N), ActionType: emit.ActionTypeFYI, Operator: operator, Developer: "capsulectl-deal", Timestamp: at}
	if e := ev.Evidence; !legacy && e != nil && len(e.Settlement) > 0 {
		// A settlement leg rides on its step's Capsule as the top-level
		// settlement member, exactly as built and stored with the step.
		in.Extensions = []emit.Extension{{Name: "settlement", Value: e.Settlement}}
	}
	if form == dealCapsuleCurrent {
		id, _ := json.Marshal(ev.DealID) // a string always marshals
		in.Extensions = append(in.Extensions, emit.Extension{Name: dealIDMember, Value: id})
	}
	if legacy || ev.Kind != "act" || ev.Act == nil || ev.Act.Unchecked {
		return in
	}
	disposition, ok, typed := basisDisposition(record)
	if !typed {
		disposition, ok = dealDisposition(events, ev.Act.AuthorizedBy)
	}
	if !ok {
		return in
	}
	effectType, ok := dealEffectTypes[ev.Act.Action]
	if !ok && ev.ActEffects != "" {
		effectType, ok = dealActEffectTypes[ev.Act.Action]
	}
	if !ok {
		return in
	}
	state, err := foldDeal(events)
	if err != nil {
		return in
	}
	irreversibility := emit.IrreversibilityOneWayConsequential
	switch {
	case ev.Act.Action == offerAction:
		// An offer binds nothing until it is accepted, and a later offer
		// supersedes it.
		irreversibility = emit.IrreversibilityTwoWay
	case state.agreedRecourse.Refundable != nil && *state.agreedRecourse.Refundable:
		irreversibility = emit.IrreversibilityOneWayRecoverable
	}
	in.ActionType = emit.ActionTypeDecide
	in.Disposition = &disposition
	in.Effect = &emit.Effect{Type: effectType, Status: emit.EffectDispatched, IrreversibilityClass: irreversibility, EffectAttestation: emit.AttestationRuntimeClaimed}
	return in
}

// checkedCard is the check (verdict) step an approval answers, and the card it
// rendered.
func checkedCard(events []sealedEvent, check string) (sealedEvent, string, bool) {
	for _, se := range events {
		if se.CapsuleID == check && se.Event.Kind == "check" && se.Event.Check != nil {
			return se, se.Event.Check.Card, true
		}
	}
	return sealedEvent{}, "", false
}

// acceptableOffer is the snapshot (the proposed action) of the offer the
// check names, when the counterparty may accept it: the check is of an
// offer, the offer was made (an offer act rests on it), and no later offer
// superseded it.
func acceptableOffer(events []sealedEvent, check string) (string, error) {
	verdict, _, ok := checkedCard(events, check)
	if !ok {
		return "", cli.InputError("--check names no check of this deal: an acceptance names the check of the offer it accepts")
	}
	snapshot := verdict.Event.Check.Snapshot
	var offer *dealSnapshot
	for _, se := range events {
		if se.CapsuleID == snapshot {
			offer = se.Event.Snapshot
		}
	}
	if offer == nil || offer.Action != offerAction {
		return "", cli.InputError("--check names a check that is not of an offer")
	}
	made, later := false, false
	for _, se := range events {
		if a := se.Event.Act; se.Event.Kind == "act" && a != nil && a.Action == offerAction && !a.Unchecked && authorizesCheck(events, a.AuthorizedBy, check) {
			made = true
		}
		if p := se.Event.Snapshot; se.Event.Kind == "snapshot" && p != nil && p.Action == offerAction && se.Event.N > verdict.Event.N {
			later = true
		}
	}
	switch {
	case later:
		return "", cli.InputError("a later offer superseded that one: only the latest offer can be accepted")
	case !made:
		return "", cli.InputError("that offer was checked but never made: record the offer act before its acceptance")
	}
	return snapshot, nil
}

// authorizesCheck is whether authorizedBy (an act's authority: the check
// itself when it passed, or an approval of it) rests on that check.
func authorizesCheck(events []sealedEvent, authorizedBy, check string) bool {
	if authorizedBy == check {
		return true
	}
	for _, se := range events {
		if a := se.Event.Approval; se.CapsuleID == authorizedBy && a != nil && a.Check == check {
			return true
		}
	}
	return false
}

// dealNamesPayeeOnAmountChecks is whether a check other than a pay names the
// payee when it states an amount; a test turns it off to stand in for a
// release from before, when only a pay did.
var dealNamesPayeeOnAmountChecks = true

// checkNamesPayee is whether a check names the deal's payee: every pay, and,
// where the user buys, every other action that states an amount (a commit, a
// sign, a cancel with an amount), since each is money to or from that
// merchant. A seller's commit or offer is money from the buyer, never to a
// payee, and a share moves none.
func checkNamesPayee(snap dealSnapshot, role string) bool {
	switch {
	case snap.Action == "pay":
		return true
	case !dealNamesPayeeOnAmountChecks || role == dealRoleSeller || snap.AmountMinor == nil:
		return false
	}
	switch snap.Action {
	case "commit", "sign", "cancel":
		return true
	}
	return false
}

// shownCardMatches refuses an answer given on a card other than the one the
// check rendered: a card_commitment must equal the check's, never differ.
func shownCardMatches(events []sealedEvent, a dealApproval) error {
	if a.ShownCard == "" {
		return nil
	}
	_, card, ok := checkedCard(events, a.Check)
	switch {
	case !ok:
		return cli.InputError("--shown-card goes with an answer to a check")
	case card == "":
		return cli.InputError("that check rendered no card, so no card can have been shown for it")
	case a.ShownCard != card:
		return cli.InputError("the card shown is not the card checked: pass the check's card exactly as `deal check` returned it")
	}
	return nil
}

// basisDisposition is the Capsule disposition a typed action-record/v0
// supports, read from its own authority_basis (decide/fyi is presentation of
// that canonical field): accepted and executed; approver human, with
// human_disposed, when the user's own approval is a layer; policy when the
// task authority alone covered it (a DO). A platform approval is that
// platform's own check and changes neither. Any other basis (no task
// authority first, or a layer this producer defines no override for) maps to
// nothing: the act stays fyi. typed is false when the record is not an
// action-record/v0 (an x-deal-v0 record maps by dealDisposition).
func basisDisposition(record []byte) (d emit.Disposition, ok, typed bool) {
	var r struct {
		Type string `json:"type"`
		Body struct {
			AuthorityBasis []struct {
				Type string `json:"type"`
			} `json:"authority_basis"`
		} `json:"body"`
	}
	if json.Unmarshal(record, &r) != nil || r.Type != cli.TypeActionRecord {
		return emit.Disposition{}, false, false
	}
	basis := r.Body.AuthorityBasis
	if len(basis) == 0 || basis[0].Type != "task_authority" {
		return emit.Disposition{}, false, true
	}
	d = emit.Disposition{Decision: emit.DecisionAccept, VerdictClass: emit.VerdictExecuted, Approver: emit.ApproverPolicy}
	for _, layer := range basis[1:] {
		switch layer.Type {
		case "user_approval":
			d.Approver, d.HumanDisposed = emit.ApproverHuman, true
		case "platform_approval":
		default:
			return emit.Disposition{}, false, true
		}
	}
	return d, true, true
}

// dealDisposition is the disposition of an act in an x-deal-v0 record,
// authorized by the sealed approval id: accepted and executed, by the
// approver that approval records. It is false when id names no proceeding
// approval this maps.
func dealDisposition(events []sealedEvent, id string) (emit.Disposition, bool) {
	for _, se := range events {
		a := se.Event.Approval
		if id == "" || se.CapsuleID != id || se.Event.Kind != "approval" || a == nil || !a.Proceed {
			continue
		}
		d := emit.Disposition{Decision: emit.DecisionAccept, VerdictClass: emit.VerdictExecuted}
		switch a.Approver {
		case "user":
			// Every user approval this producer seals commits the user's words
			// (said_commitment, deal_profile.go) and refs the check it approves.
			d.Approver, d.HumanDisposed = emit.ApproverHuman, true
		case "standing_intent":
			d.Approver = emit.ApproverPolicy
		default:
			// agent_card: a click on a card the agent composed certifies no one's
			// consent, so the act stays fyi.
			return emit.Disposition{}, false
		}
		return d, true
	}
	return emit.Disposition{}, false
}

// stepRequest is the frozen seal request for a step: rebuilt from the stored
// step it gives the same Capsule, which is what makes recovery possible
// (form: the Capsule a release sealed; see dealCapsuleForm).
func (s *dealSession) stepRequest(events []sealedEvent, ev dealEvent, form dealCapsuleForm) (cli.Request, string, error) {
	payload, digest, err := encodeDealRecord(ev, events, s.dkey)
	if err != nil {
		return cli.Request{}, "", err
	}
	at, err := time.Parse("2006-01-02T15:04:05Z", ev.At)
	if err != nil {
		return cli.Request{}, "", cli.ErrConflict
	}
	request := cli.Request{
		Version: "capsule-seal-request/v1",
		Capsule: dealCapsuleInput(events, ev, payload, s.p.Name, at.UTC(), form),
		Payload: payload,
	}
	// Each step's Capsule follows the one before it (ordering only), so the
	// deal is a citation chain any Evidence Bundle verifier can close. A
	// record sealed after the close instead CONFIRMS the close (registered
	// chain.relation): `follows` is ordering only, and verifiers must not
	// read it as confirming anything.
	switch {
	case ev.Confirms != "":
		request.Capsule.Chain = &emit.Chain{ParentCapsuleID: ev.Confirms, Relation: "confirms"}
	case len(events) > 0:
		request.Capsule.Chain = &emit.Chain{ParentCapsuleID: events[len(events)-1].CapsuleID, Relation: "follows"}
	}
	return request, digest, nil
}

// prepareStep fixes a step's time, chain and nonces, derives its record
// (refusing one that fails the profile schema or would carry a raw
// identifier), and writes its index row with the local step BEFORE anything
// reaches the log, so a crash can never leave a logged step with no local
// record of it.
func (s *dealSession) prepareStep(ctx context.Context, dealID string, events []sealedEvent, ev dealEvent) (cli.Request, sealedEvent, error) {
	ev, err := s.fixStep(dealID, events, ev)
	if err != nil {
		return cli.Request{}, sealedEvent{}, err
	}
	request, digest, err := s.stepRequest(events, ev, dealCapsuleCurrent)
	if err != nil {
		return cli.Request{}, sealedEvent{}, err
	}
	record, err := cli.Seal(request, s.key)
	if err != nil {
		return cli.Request{}, sealedEvent{}, err
	}
	local, err := json.Marshal(ev)
	if err != nil {
		return cli.Request{}, sealedEvent{}, err
	}
	if err = s.index.prepareStep(ctx, indexedStep{DealID: dealID, N: ev.N, Kind: ev.Kind, CapsuleID: record.CapsuleID, Digest: digest, Local: string(local)}); err != nil {
		return cli.Request{}, sealedEvent{}, err
	}
	return request, sealedEvent{CapsuleID: record.CapsuleID, Digest: digest, Event: ev}, nil
}

// fixStep fixes a step's number, time, chain and nonces, and the versions
// this producer seals it under: what a step is before it is sealed, on a
// book or a legacy log alike.
func (s *dealSession) fixStep(dealID string, events []sealedEvent, ev dealEvent) (dealEvent, error) {
	now := cli.DealClock().UTC().Truncate(time.Second)
	ev.DealID = dealID
	ev.N = int64(len(events) + 1)
	ev.At = now.Format("2006-01-02T15:04:05Z")
	ev.Prev = ""
	if len(events) > 0 {
		ev.Prev = events[len(events)-1].Digest
	}
	ev.Producer = currentProducer()
	ev.TaxonomyVersion = dealTaxonomyVersion
	ev.CommitAlg = dealCommitAlg
	ev.RuleInputs = dealRuleInputsVersion
	ev.ClaimCommit = dealClaimCommitVersion
	ev.ReversesRef = dealReversesRefVersion
	if dealRegistersThreads {
		ev.ThreadRegistration = dealThreadRegistrationVersion
	}
	ev.ActEffects = dealActEffectsVersion
	ev.ActCounterparty = dealActCounterpartyVersion
	ev.ThreadCounterparty = dealThreadCounterpartyVersion
	ev.OfferClass = dealOfferClassVersion
	ev.Nonces = map[string]string{}
	for name := range dealTexts(ev) {
		nonce := make([]byte, 32)
		if _, err := rand.Read(nonce); err != nil {
			return dealEvent{}, err
		}
		ev.Nonces[name] = hex.EncodeToString(nonce)
	}
	shareCheckedNonces(events, &ev)
	return ev, nil
}

// publishStep persists and appends a prepared step and records its log
// position.
func (s *dealSession) publishStep(ctx context.Context, request cli.Request, se sealedEvent) (sealedEvent, error) {
	pub, err := s.appendLegacy(ctx, func() (cli.Publication, error) { return s.t.Publish(ctx, request, s.key) })
	if err != nil {
		return sealedEvent{}, err
	}
	if pub.CapsuleID != se.CapsuleID {
		return sealedEvent{}, cli.ErrConflict
	}
	if err = s.index.publishedStep(ctx, se.Event.DealID, se.Event.N, pub.Sequence); err != nil {
		return sealedEvent{}, err
	}
	se.Sequence = pub.Sequence
	return se, nil
}

// recoverStep publishes a step that was prepared but never reached the log.
// The step may have been prepared by an earlier capsulectl, which sealed it
// without deal_id, or every step fyi, so the request whose Capsule is the one
// prepared is the one used.
func (s *dealSession) recoverStep(ctx context.Context, events []sealedEvent, ev dealEvent, capsuleID string) (sealedEvent, error) {
	for _, form := range []dealCapsuleForm{dealCapsuleCurrent, dealCapsuleNoDealID, dealCapsuleAllFYI} {
		request, digest, err := s.stepRequest(events, ev, form)
		if err != nil {
			return sealedEvent{}, cli.ErrConflict
		}
		if record, err := cli.Seal(request, s.key); err == nil && record.CapsuleID == capsuleID {
			return s.publishStep(ctx, request, sealedEvent{CapsuleID: capsuleID, Digest: digest, Event: ev})
		}
	}
	return sealedEvent{}, cli.ErrConflict
}

// seal prepares and publishes one step. Any failure is returned: a step
// that is not sealed was not checked. If the step never reached
// the log, its index row is withdrawn; if it did, the next read recovers it.
func (s *dealSession) seal(ctx context.Context, dealID string, events []sealedEvent, ev dealEvent) (sealedEvent, error) {
	if s.onBook {
		if s.book == nil {
			return sealedEvent{}, cli.ErrReadOnlyCLL
		}
		return s.sealBookStep(ctx, dealID, events, ev)
	}
	request, se, err := s.prepareStep(ctx, dealID, events, ev)
	if err != nil {
		return sealedEvent{}, err
	}
	published, err := s.publishStep(ctx, request, se)
	if err != nil {
		if entries, scanErr := s.stepEntries(ctx, dealID); scanErr == nil && int64(len(entries)) < se.Event.N {
			err = errors.Join(err, s.index.withdrawStep(ctx, dealID, se.Event.N))
		}
		return sealedEvent{}, err
	}
	return published, nil
}

// milestone cuts a signed checkpoint of the deal's own log at a deal
// milestone (baseline, approval, close) when the profile has a checkpoint
// key. It never contacts the witness: a deal event publishing would tell the
// witness when deals happen. The deal's checkpoint reaches the witness inside
// the next cadence tick (`deal tick`), so its witness state is "scheduled".
func (s *dealSession) milestone(ctx context.Context) (map[string]any, error) {
	if s.p.Checkpoint.Signing == (cli.Secret{}) {
		return map[string]any{"state": "not_configured"}, nil
	}
	size, err := s.cutMilestone(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"state": "signed", "checkpoint": size, "witness": "not_configured"}
	service, err := cli.ServiceID(s.dp)
	if err != nil {
		return nil, err
	}
	if service != "" {
		out["witness"] = "scheduled"
	}
	return out, nil
}

func dealCommands() *cobra.Command {
	deal := &cobra.Command{Use: "deal", Short: "Seal a deal's baseline and check every point of no return against it"}
	deal.AddCommand(dealInitCommand(), dealOpenCommand(), dealNoteCommand(), dealCheckCommand(), dealCloseCommand(), dealReportCommand(), dealCountersignCommand(), dealExportCommand(), dealSaleCommands(), dealAuditHostCommand(), dealReconcileAlias(), dealTickCommand(), dealVerifyEmailCommand(), dealVerifyObjectCommand(), dealVerifierStatementCommand(), dealVerifyStatementCommand(), dealDeadlinesCommand(), dealCheckpointCommands(), dealListCommand(), dealSettlementCommand(), dealHistoryCommand(), dealTermsResultCommand())
	// A deal profile's copies: asked for, answered, composed.
	deal.AddCommand(dealEvidenceCommands()...)
	return deal
}

// runDeal selects the profile, opens a locked session, loads the named deal
// (when dealID is set) and refuses further steps on a finally closed deal.
func runDeal(c *cobra.Command, needDeal bool, fn func(ctx context.Context, s *dealSession, dealID string, events []sealedEvent) error) (err error) {
	p, err := cli.Selected(c)
	if err != nil {
		return err
	}
	dealID := ""
	if needDeal {
		dealID, _ = c.Flags().GetString("deal")
		if dealID == "" {
			return cli.InputError("--deal is required: a deal id as printed by `deal open`")
		}
		// A sale's log is not a deal: only `deal sale` commands read it.
		if !cli.DealIDPattern.MatchString(dealID) {
			return cli.InputError("--deal must be a deal id as printed by `deal open`: deal- followed by 16 hex characters")
		}
	}
	ctx := c.Context()
	s, err := openDealSession(ctx, p)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, s.close()) }()
	var events []sealedEvent
	if needDeal {
		if err = s.useDeal(ctx, dealID, false); err != nil {
			return err
		}
		if events, err = s.load(ctx, dealID); err != nil {
			return err
		}
	}
	return fn(ctx, s, dealID, events)
}

func stepOutput(dealID string, se sealedEvent) map[string]any {
	return map[string]any{"deal_id": dealID, "step": se.Event.N, "kind": se.Event.Kind, "capsule_id": se.CapsuleID, "sequence": se.Sequence}
}

func writeSeed(path string) (ed25519.PrivateKey, error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, cli.InputError("cannot create key file (it must not already exist): " + path)
	}
	_, werr := f.WriteString(hex.EncodeToString(private.Seed()))
	if err = errors.Join(werr, f.Sync(), f.Close()); err != nil {
		return nil, err
	}
	return private, nil
}

func dealInitCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "init", Short: "Create a local deal profile: SQLite store, 0600 signing and checkpoint seeds", Args: cli.NoArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		name, _ := c.Flags().GetString("profile")
		dir, _ := c.Flags().GetString("dir")
		if name == "" || dir == "" {
			return cli.InputError("--profile and --dir are required")
		}
		if dir, err = filepath.Abs(dir); err != nil {
			return err
		}
		if path, e := cli.ProfilePath(name); e != nil {
			return e
		} else if _, e = os.Lstat(path); e == nil {
			return cli.InputError("profile already exists: " + name)
		}
		if err = os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		signing, err := writeSeed(filepath.Join(dir, "signing.seed"))
		if err != nil {
			return err
		}
		cpKey, err := writeSeed(filepath.Join(dir, "checkpoint.seed"))
		if err != nil {
			return err
		}
		signer, err := checkpoint.NewEd25519Signer(cpKey)
		if err != nil {
			return err
		}
		public := hex.EncodeToString(signing.Public().(ed25519.PublicKey))
		// Each deal gets its own log (see dealLogID), never published. The
		// profile's log is the cadence log: the only one the witness sees.
		suffix, err := randomHex(8)
		if err != nil {
			return err
		}
		p := cli.Profile{Name: name, Type: "sqlite", Namespace: "deal", LogID: "deal-cadence/" + suffix}
		p.Connection.Database = filepath.Join(dir, "deal.db")
		p.Signing.File = filepath.Join(dir, "signing.seed")
		p.TrustedKeys = []string{public}
		p.Checkpoint.Signing.File = filepath.Join(dir, "checkpoint.seed")
		p.Checkpoint.TrustedKeys = []string{signer.KeyID()}
		if predicate, _ := c.Flags().GetString("materiality"); predicate != "" {
			if err = cli.PinMateriality(&p, predicate); err != nil {
				return err
			}
		}
		witness := "not_configured"
		if noWitness, _ := c.Flags().GetBool("no-witness"); !noWitness {
			p.Checkpoint.Endpoint, p.Checkpoint.PublicKey = cli.DealDefaultWitness, cli.DealDefaultWitnessKey
			witness = "scheduled"
		}
		if err = cli.SaveProfile(p, false); err != nil {
			return err
		}
		dbPath, err := filepath.Abs(p.Connection.Database)
		if err != nil {
			return err
		}
		unlock, err := lockDealStore(dbPath)
		if err != nil {
			return err
		}
		lock := &dealStoreLock{held: true}
		defer func() {
			if lock.held {
				lock.held = false
				err = errors.Join(err, unlock())
			}
		}()
		t, err := cli.OpenDealTarget(c.Context(), p, cli.UseInitialization, lock)
		if err != nil {
			return err
		}
		if err = t.Close(); err != nil {
			return err
		}
		if err = os.Chmod(p.Connection.Database, 0o600); err != nil {
			return err
		}
		lock.held = false
		if err = unlock(); err != nil {
			return err
		}
		// The first session puts the new store on its shelf: the shelf's
		// tables, its migration marker, and the mutation counter and its
		// triggers, so a fresh store is protected as a migrated one is.
		// The profile and store exist by now, so a failure here names the
		// way on rather than leaving an init that cannot be re-run.
		s, err := openDealSession(c.Context(), p)
		if err == nil {
			err = s.close()
		}
		if err != nil {
			return fmt.Errorf("deal profile %s and its store were created, but putting the store on its shelf failed; run `capsulectl --profile %s deal tick` to finish: %w", name, name, err)
		}
		out := map[string]any{
			"profile": name, "store": p.Connection.Database, "public_key": public, "witness": witness, "cadence_log": p.LogID,
			"guarantee": "tamper-evident against ourselves and the agent, not non-repudiation: the signing seed is on this machine",
		}
		if witness == "scheduled" {
			out["witness_endpoint"], out["witness_sees"] = cli.DealDefaultWitness, dealWitnessSees
			cadence := "its cadence"
			if cfg, err := p.DealCadence(); err == nil {
				cadence = "every " + cli.ShortDuration(cfg.Interval)
				if cfg.Jitter > 0 {
					cadence += ", give or take " + cli.ShortDuration(cfg.Jitter)
				}
			}
			out["next"] = "set up the checkpoint cadence: schedule `capsulectl --profile " + name + " cll checkpoint cadence --wait-up-to 5m` every 5 minutes. Each run is a poll: it waits for a tick due within its 5 minutes and publishes it on time, and a run with none due cuts no tick, only retrying any delivery still pending. The witness hears from this profile once per tick (" + cadence + ")"
		}
		return cli.Output(c, out)
	}}
	cmd.Flags().String("dir", "", "Directory for the store and seeds (created 0700)")
	cmd.Flags().Bool("no-witness", false, "Do not configure the default public witness; deals are sealed on this device only")
	cmd.Flags().String("materiality", "", "A materiality predicate (a materiality-predicate/v0 JSON file, or builtin:neutral) for this profile's checks to evaluate, pinned by its digest; without one, every attribute the agent picks pauses the check")
	return cmd
}

func dealOpenCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "open", Short: "Seal the baseline: the user's verbatim words, who, terms, claims and recourse", Args: cli.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		path, _ := c.Flags().GetString("input")
		raw, err := cli.ReadInput(path)
		if err != nil {
			return err
		}
		var o dealOpen
		if err = cli.DecodeJSONAs("--input", raw, &o); err != nil {
			return err
		}
		// The deal id the other party opened this deal under, when this side
		// joins it: the same opaque id on both copies, never a description.
		carried, _ := c.Flags().GetString("deal-id")
		if carried != "" && !cli.DealIDPattern.MatchString(carried) {
			return cli.InputError("--deal-id must be a deal id as `deal open` prints it: deal- followed by 16 lowercase hex characters")
		}
		// Set from the sale (deal open --sale), never from the input file.
		o.ItemRef, o.Sale, o.SaleAuthority = "", "", ""
		if saleID, _ := c.Flags().GetString("sale"); saleID != "" {
			if err = runDeal(c, false, func(ctx context.Context, s *dealSession, _ string, _ []sealedEvent) error {
				sale, authority, err := s.saleOf(ctx, saleID)
				if err != nil {
					return err
				}
				return underSale(&o, sale, saleID, authority)
			}); err != nil {
				return err
			}
		}
		if err = o.validate(); err != nil {
			return err
		}
		if err = normalizeOpen(&o); err != nil {
			return err
		}
		if o.Intent.OrderedRepresentations != nil {
			for _, cl := range o.Claims {
				if cl.SourceKind == "agent" {
					return cli.InputError("the agent's claims to the buyer are noted after the deal opens, each against a represent check (deal check, then deal note --kind claim --check): leave them out of the open input")
				}
			}
		}
		o.Skill = nil
		// The record set is fixed for the deal here, never taken from the
		// input file.
		o.Records = ""
		switch v, _ := c.Flags().GetString("records"); v {
		case "", "x-deal-v0":
			if o.Sale != "" {
				o.Records = recordsTyped // a sale's thread is typed
			}
		case "typed":
			o.Records = recordsTyped
		default:
			return cli.InputError("--records must be typed or x-deal-v0")
		}
		// A deal where the user sells has no untyped path: its offers, the
		// buyer's acceptance and the commit that rests on it are typed records.
		if o.Intent.PartyRole == dealRoleSeller && o.Records != recordsTyped {
			return cli.InputError("a deal where the user sells is recorded in the typed records: open it with deal open --records typed")
		}
		skillPath, _ := c.Flags().GetString("skill")
		if skillPath == "" {
			skillPath = os.Getenv(dealSkillEnv)
		}
		if skillPath != "" {
			if o.Skill, err = readDealSkill(skillPath); err != nil {
				return err
			}
		}
		switch {
		case o.ExpectCloseBy == "":
			o.ExpectCloseBy = defaultExpectClose(o.Type, cli.DealClock())
		case !validDate(o.ExpectCloseBy):
			return cli.InputError("expect_close_by must be a date, YYYY-MM-DD")
		}
		return runDeal(c, false, func(ctx context.Context, s *dealSession, _ string, _ []sealedEvent) error {
			// The pinned materiality predicate, sealed into the baseline from
			// the profile (never the input): a check under another one later
			// is flagged. One that cannot be read refuses the open.
			predicate, err := cli.PinnedMateriality(s.p)
			if err != nil {
				return err
			}
			o.Materiality = predicate.Ref()
			dealID := carried
			// A sale's thread is registered on the sale's log first: the
			// registration names the thread's id, and the thread's task
			// authority names the registration.
			registration := ""
			if o.Sale != "" && dealRegistersThreads {
				if dealID, registration, err = s.registerThread(ctx, o.Sale, carried); err != nil {
					return err
				}
				if err = s.t.Close(); err != nil {
					return err
				}
				s.t = nil
			}
			if dealID == "" {
				id := make([]byte, 8)
				if _, err := rand.Read(id); err != nil {
					return err
				}
				dealID = "deal-" + hex.EncodeToString(id)
			} else if carried != "" {
				var held int
				if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deal_keys WHERE deal_id=?`, dealID).Scan(&held); err != nil {
					return err
				}
				if held > 0 {
					return cli.InputError("this profile already holds deal " + dealID + ": --deal-id opens this side of a deal the other party opened, once")
				}
			}
			// The first deal this profile holds gets one line about the
			// receipt; later ones say nothing.
			var heldDeals int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deal_keys`).Scan(&heldDeals); err != nil {
				return err
			}
			if err := s.useDeal(ctx, dealID, true); err != nil {
				return err
			}
			if err := dealOpenStopsAt("keyed"); err != nil {
				return err
			}
			se, err := s.seal(ctx, dealID, nil, dealEvent{Kind: "open", Open: &o})
			if err != nil {
				return err
			}
			if err := dealOpenStopsAt("baseline"); err != nil {
				return err
			}
			// Typed records: the user's task authority is its own step,
			// right after the baseline it was asked in.
			var authority sealedEvent
			if o.Records == recordsTyped {
				intent := o.Intent
				if authority, err = s.seal(ctx, dealID, []sealedEvent{se}, dealEvent{Kind: "task_authority", TaskAuthority: &intent, SaleAuthority: o.SaleAuthority, SaleRegistration: registration}); err != nil {
					return err
				}
			}
			cp, err := s.milestone(ctx)
			if err != nil {
				return err
			}
			// The sale's log records that the thread opened, by its task
			// authority (as a commitment); a sale bundle seals it if a crash
			// came first.
			if registration != "" && dealSealsSaleEvidence {
				if err := dealOpenStopsAt("authority"); err != nil {
					return err
				}
				if err := s.sealThreadOpened(ctx, o.Sale, registration, authority.Digest); err != nil {
					return err
				}
			}
			out := stepOutput(dealID, se)
			out["demo"] = o.Demo
			out["points_of_no_return"] = dealPointsOfNoReturn[o.Type]
			out["checkpoint"] = cp
			out["remote_warm"] = dealRemoteWarm(ctx)
			if o.Skill != nil {
				out["skill"] = map[string]any{"skill_md_digest": o.Skill.Digest, "other_copies": o.Skill.OtherCopies, "others": o.Skill.Others}
			}
			return cli.Output(c, cli.Speak(out, s.p, func(w cli.Wording) cli.Narration {
				if heldDeals > 0 {
					return silent(w)
				}
				return line(w.Line("say.open.first", nil))
			}))
		})
	}}
	cmd.Flags().String("input", "", "Baseline JSON: type, intent, who, terms, claims, recourse")
	cmd.Flags().String("deal-id", "", "Open your side of a deal the other party opened, under the deal id it gave you (deal- and 16 hex): both copies then carry one deal id, so they can be composed. Default: a new random id")
	cmd.Flags().String("skill", "", "The SKILL.md the agent is following (or $"+dealSkillEnv+"): its digest is sealed in the baseline")
	cmd.Flags().String("records", "", "The record set: x-deal-v0 (the default) or typed (the typed action records, which carry the check contract)")
	cmd.Flags().String("sale", "", "A sale id from `deal sale new`: open this buyer's thread under the sale's request and task authority (typed records)")
	return cmd
}

// dealActInput is what an agent reports after acting. Authorization fields are
// computed by the wrapper and cannot be supplied.
type dealActInput struct {
	Action      string `json:"action"`
	Description string `json:"description,omitempty"`
	AmountMinor *int64 `json:"amount_minor,omitempty"`
	Currency    string `json:"currency,omitempty"`
	Payee       string `json:"payee,omitempty"`
	Rail        string `json:"rail,omitempty"`
	Reference   string `json:"reference,omitempty"`
	// FeeMinor is, on a cancel only, a fee the cancellation costs, recorded
	// as its own amount: never the amount a spend cap evaluates (spend_minor
	// is 0 on every cancel).
	FeeMinor *int64 `json:"fee_minor,omitempty"`
}

func dealNoteCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "note", Short: "Seal a message, claim, evidence, detail change, the user's answer to a check, an action taken, or something the agent told someone (disclosure)", Args: cli.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		kind, _ := c.Flags().GetString("kind")
		emailPath, _ := c.Flags().GetString("email")
		htmlPath, _ := c.Flags().GetString("html")
		if htmlPath != "" && kind != "act" && !(kind == "evidence" && emailPath != "") {
			return cli.InputError("--html writes the updated receipt page; it goes with --kind act or --kind evidence --email")
		}
		keyPath, _ := c.Flags().GetString("key-record")
		dmarcPath, _ := c.Flags().GetString("dmarc-record")
		ev := dealEvent{Kind: kind}
		var act dealActInput
		if (emailPath != "" || keyPath != "" || dmarcPath != "") && kind != "evidence" {
			return cli.InputError("--email, --key-record and --dmarc-record go with --kind evidence")
		}
		if keyPath != "" && emailPath == "" {
			return cli.InputError("--key-record goes with --email: the key record is read for that email's DKIM signature")
		}
		wrapped, err := wrappedNoteInput(c)
		if err != nil {
			return err
		}
		if wrapped != nil && kind != "evidence" {
			return cli.InputError("--object and --object-digest go with --kind evidence")
		}
		path, _ := c.Flags().GetString("input")
		settlementKind := ""
		switch {
		case kind == noteKindPaymentReceived || kind == noteKindDelivered:
			built, err := settlementNoteEvent(c, kind)
			if err != nil {
				return err
			}
			ev, settlementKind, kind = built, kind, "evidence"
		case kind == "evidence" && emailPath != "" && path == "":
			ev.Evidence = &dealEvidence{About: "merchant confirmation email", Source: "merchant_email"}
		case kind == "evidence" && wrapped != nil && path == "":
			ev.Evidence = &dealEvidence{About: wrappedAbout(wrapped), Source: wrappedSource}
		case slices.Contains([]string{"message", "claim", "evidence", "change", "intent", "act", "disclosure"}, kind):
			raw, err := cli.ReadInput(path)
			if err != nil {
				return err
			}
			var target any
			switch kind {
			case "message":
				ev.Message = &dealMessage{}
				target = ev.Message
			case "claim":
				ev.Claim = &dealClaim{}
				target = ev.Claim
			case "evidence":
				ev.Evidence = &dealEvidence{}
				target = ev.Evidence
			case "change":
				ev.Change = &dealChange{}
				target = ev.Change
			case "intent":
				ev.Intent = &dealIntent{}
				target = ev.Intent
			case "act":
				target = &act
			case "disclosure":
				ev.Disclosure = &dealDisclosure{}
				target = ev.Disclosure
			}
			if err = cli.DecodeJSONAs("--input", raw, target); err != nil {
				return err
			}
			if ev.Claim != nil {
				// What authorizes a claim is found when it is noted, never
				// taken from the input file; the check it is noted against is
				// the --check flag.
				check, _ := c.Flags().GetString("check")
				*ev.Claim = dealClaim{Text: ev.Claim.Text, Source: ev.Claim.Source, SourceKind: ev.Claim.SourceKind, Class: ev.Claim.Class, Check: check}
			}
			if err = refuseSettlementInput(ev.Evidence); err != nil {
				return err
			}
			if ev.Evidence != nil && ev.Evidence.Wrapped != nil {
				return cli.InputError("evidence --input cannot set a held object: use deal note --object or --object-digest")
			}
			if wrapped != nil {
				// A held object's source is the fixed token, so its record
				// shares as one (dealShareKeys).
				if ev.Evidence.Source != "" && ev.Evidence.Source != wrappedSource {
					return cli.InputError("a held object's source is " + wrappedSource + ": leave source out of --input")
				}
				ev.Evidence.Source = wrappedSource
				if ev.Evidence.About == "" {
					ev.Evidence.About = wrappedAbout(wrapped)
				}
			}
		case kind == "approval":
			check, _ := c.Flags().GetString("check")
			choice, _ := c.Flags().GetString("choice")
			said, _ := c.Flags().GetString("said")
			if check == "" || choice == "" {
				return cli.InputError("approval needs --check and --choice")
			}
			// approver "user" certifies the user's consent, so it is sealed only with their own
			// words, committed as said_commitment beside the approval's ref to the check (which
			// pins the proposed action). A click on a card the agent composed carries no words of
			// theirs: it is sealed agent_card, never user.
			approver := "user"
			if strings.TrimSpace(said) == "" {
				if choice == "confirm_limits" {
					return cli.InputError("confirm_limits needs --said with the user's own words: raising their limits takes their consent, not a card click")
				}
				approver, said = "agent_card", ""
			}
			ev.Approval = &dealApproval{Check: check, Choice: choice, Approver: approver, Said: said}
			if path, _ := c.Flags().GetString("shown-card"); path != "" {
				if choice == "confirm_limits" {
					return cli.InputError("--shown-card goes with an answer to a check's card, not with confirm_limits")
				}
				shown, err := os.ReadFile(path)
				if err != nil {
					return cli.InputError("cannot read --shown-card " + path)
				}
				ev.Approval.ShownCard = strings.TrimSuffix(string(shown), "\n")
			}
		case kind == "platform_approval":
			p := &dealPlatformApproval{}
			p.Check, _ = c.Flags().GetString("check")
			p.Platform, _ = c.Flags().GetString("platform")
			p.Mechanism, _ = c.Flags().GetString("mechanism")
			p.DisplayedText, _ = c.Flags().GetString("displayed-text")
			p.UserText, _ = c.Flags().GetString("user-text")
			p.ObservedAt, _ = c.Flags().GetString("observed-at")
			if p.Check == "" || p.Platform == "" || p.Mechanism == "" || strings.TrimSpace(p.DisplayedText) == "" {
				return cli.InputError("platform_approval needs --check, --platform, --mechanism and --displayed-text (the text exactly as the platform displayed it)")
			}
			if !callerToken.MatchString(p.Platform) || !callerToken.MatchString(p.Mechanism) {
				return cli.InputError("--platform and --mechanism are names you give the platform and its approval: lowercase letters, digits and . _ - (at most 64)")
			}
			if p.ObservedAt == "" {
				p.ObservedAt = cli.DealClock().UTC().Format(time.RFC3339)
			} else if at, err := time.Parse(time.RFC3339, p.ObservedAt); err != nil || at.UTC().Format(time.RFC3339) != p.ObservedAt || at.After(cli.DealClock()) {
				return cli.InputError("--observed-at is when you observed it, in UTC (2026-10-06T09:14:00Z), not in the future")
			}
			if c.Flags().Changed("amount-minor") {
				amount, _ := c.Flags().GetInt64("amount-minor")
				if amount < 0 {
					return cli.InputError("--amount-minor is the amount the platform's approval stated, in minor units")
				}
				p.AmountMinor = &amount
				p.Currency, _ = c.Flags().GetString("currency")
			}
			ev.Platform = p
		case kind == "acceptance":
			a := &dealAcceptance{}
			a.Offer, _ = c.Flags().GetString("check")
			a.Words, _ = c.Flags().GetString("words")
			channel, _ := c.Flags().GetString("channel")
			a.Channel = channelToken(channel)
			a.ObservedAt, _ = c.Flags().GetString("observed-at")
			if a.Offer == "" || strings.TrimSpace(a.Words) == "" {
				return cli.InputError("acceptance needs --check (the offer's check) and --words (the counterparty's words, exactly as they were given)")
			}
			if a.ObservedAt == "" {
				a.ObservedAt = cli.DealClock().UTC().Format(time.RFC3339)
			} else if at, err := time.Parse(time.RFC3339, a.ObservedAt); err != nil || at.UTC().Format(time.RFC3339) != a.ObservedAt || at.After(cli.DealClock()) {
				return cli.InputError("--observed-at is when you observed it, in UTC (2026-10-06T09:14:00Z), not in the future")
			}
			ev.Acceptance = a
		default:
			return cli.InputError("--kind must be message, claim, evidence, change, intent, approval, act, disclosure, platform_approval, acceptance, payment_received or delivered")
		}
		if wrapped != nil {
			ev.Evidence.Wrapped = wrapped
		}
		var deferred *keysUnreachableError
		if emailPath != "" {
			// Captured before the deal is locked: the key records are read
			// from DNS now, as close to receipt as the agent can make it.
			// When DNS cannot be reached the capture is deferred, not refused.
			if err := attachEmail(ev.Evidence, emailPath, keyPath, dmarcPath); err != nil && !errors.As(err, &deferred) {
				return err
			}
		}
		if err := normalizeNote(&ev); err != nil {
			return err
		}
		switch {
		case ev.Message != nil && (!slices.Contains([]string{"counterparty", "user", "agent"}, ev.Message.From) || strings.TrimSpace(ev.Message.Text) == ""):
			return cli.InputError("message needs from (counterparty, user or agent) and text")
		case ev.Claim != nil:
			if err := ev.Claim.validate(); err != nil {
				return err
			}
		case ev.Evidence != nil && (ev.Evidence.About == "" || ev.Evidence.Source == ""):
			return cli.InputError("evidence needs what it is about and its source")
		case ev.Change != nil && (ev.Change.Source == "" || (ev.Change.Who == nil && ev.Change.Terms == nil && ev.Change.Recourse == nil)):
			return cli.InputError("change needs its source and at least one of who, terms or recourse")
		}
		return runDeal(c, true, func(ctx context.Context, s *dealSession, dealID string, events []sealedEvent) error {
			if ev.Intent != nil && ev.Intent.PartyRole != "" && ev.Intent.PartyRole != dealRole(events) {
				return cli.InputError("this deal's party_role is " + dealRole(events) + ", set when it opened; a note cannot change it")
			}
			if finalClose(events) >= 0 && kind != "evidence" {
				// A closed deal takes later evidence only, linked to the close
				// (it confirms the close); every other step needs a new deal.
				return cli.InputError("this deal is closed; it takes later evidence only (`deal note --kind evidence`, linked to the close), and any other step needs a new deal (`deal open`)")
			}
			if ev.Evidence != nil {
				if err := evidenceAgainst(events, &ev); err != nil {
					return err
				}
				if ev.Evidence.Wrapped != nil {
					if err := checkWrappedNote(s, events); err != nil {
						return err
					}
				}
			}
			if settlementKind != "" {
				if err := checkSettlementNote(events, &ev, settlementKind); err != nil {
					return err
				}
			}
			open := events[0].Event.Open
			switch kind {
			case "message":
				if ev.Message.Channel == "" {
					ev.Message.Channel = open.Channel
				}
				// What the agent writes to the other party never states the
				// user's floor or limit, in any form; the user's own words
				// are recorded as they were said.
				if ev.Message.From == "agent" {
					if field := statesBound(ev.Message.Text, events, true); field != "" {
						return cli.InputError("refusing to seal: the agent's message would tell the other party " + field + "; leave the user's bounds out of what the agent writes")
					}
				}
			case "claim":
				// Where the user sells, the agent's own claims are opened to the
				// buyer: they never state the floor or a limit.
				if ev.Claim.SourceKind == "agent" && dealRole(events) == dealRoleSeller {
					if field := statesBound(ev.Claim.Text, events, true); field != "" {
						return cli.InputError("refusing to seal: the agent's claim, shown to the buyer, would state " + field + "; leave the user's bounds out of what the agent writes")
					}
				}
				if err := checkSellerClaim(events, ev.Claim); err != nil {
					return err
				}
				// A representation is sealed whether or not anything
				// authorized it: the words may already be said, and the record
				// of them is the evidence (deal_representations.go).
				if ev.Claim.SourceKind == "agent" && representationRules(events) {
					cl := ev.Claim
					cl.AuthorizedBy, cl.Reason, cl.Rule = authorizeClaim(events, *cl, cli.DealClock().UTC())
					cl.Unchecked = cl.AuthorizedBy == ""
				}
			case "intent":
				if open.Sale != "" {
					return cli.InputError("a sale's thread is under the sale's one task authority: its request and limits do not change per buyer")
				}
				for _, a := range ev.Intent.Allowed {
					if !slices.Contains(dealPointsOfNoReturn[open.Type], a) {
						return cli.InputError("intent.allowed names an action that is not a point of no return for this deal type: " + a)
					}
				}
				if ev.Intent.OrderedRepresentations != nil && !representationRules(events) {
					return cli.InputError("ordered_representations go with a deal where the user sells, opened with them")
				}
				if err := normalizeOrdered(ev.Intent.OrderedRepresentations); err != nil {
					return err
				}
			case "approval":
				if err := judgeApproval(events, ev.Approval); err != nil {
					return err
				}
				if err := shownCardMatches(events, *ev.Approval); err != nil {
					return err
				}
				// Typed records: the user's answer to an ask is bound to what
				// they were shown, so it can stand as the check's approval.
				if verdict, _, ok := checkedCard(events, ev.Approval.Check); ok && dealRecordSet(dealEvent{}, events) == recordsTyped &&
					ev.Approval.Approver == "user" && verdict.Event.Check.Verdict != "pass" && ev.Approval.ShownCard == "" {
					return cli.InputError("in a typed deal the user's answer to a paused check is bound to the card it was given on: pass that card with --shown-card")
				}
			case "acceptance":
				if dealRecordSet(dealEvent{}, events) != recordsTyped {
					return cli.InputError("an acceptance is recorded in typed deals (deal open --records typed)")
				}
				offer, err := acceptableOffer(events, ev.Acceptance.Offer)
				if err != nil {
					return err
				}
				ev.Acceptance.Offer = offer
			case "platform_approval":
				if dealRecordSet(dealEvent{}, events) != recordsTyped {
					return cli.InputError("platform approvals are recorded in typed deals (deal open --records typed)")
				}
				if _, _, ok := checkedCard(events, ev.Platform.Check); !ok {
					return cli.InputError("--check names no check of this deal: a platform approval is recorded beside the check of the same action")
				}
			case "act":
				if !slices.Contains(dealPointsOfNoReturn[events[0].Event.Open.Type], act.Action) {
					return cli.InputError("act.action is not a point of no return for this deal type")
				}
				if act.Action == offerAction && dealRole(events) != dealRoleSeller {
					return cli.InputError("an offer is made on a deal where the user sells (intent.party_role seller)")
				}
				if err := checkFee(act.Action, act.FeeMinor); err != nil {
					return err
				}
				ev.Act = &dealAct{Action: act.Action, Description: act.Description, AmountMinor: act.AmountMinor, Currency: act.Currency, Payee: act.Payee, Rail: act.Rail, Reference: act.Reference, FeeMinor: act.FeeMinor}
				ev.Act.AuthorizedBy, ev.Act.Reason, ev.Act.Rule = authorizeAct(events, *ev.Act, cli.DealClock().UTC())
				// An ordered representation still unmade holds the act, even
				// one checked before the user ordered it.
				if hold := representationHold(events, ev.Act.Action); ev.Act.AuthorizedBy != "" && hold != "" {
					ev.Act.AuthorizedBy, ev.Act.Reason, ev.Act.Rule = "", hold, ruleRepresentationMissing
				}
				ev.Act.Unchecked = ev.Act.AuthorizedBy == ""
				if !ev.Act.Unchecked && ev.Act.Action == "commit" && dealRole(events) == dealRoleSeller {
					ev.Act.Accepted, _, _ = offerAccepted(events)
				}
				ev.Act.Direction, ev.Act.Reverses = actDirection(events, *ev.Act, events[0].Event.Open.Terms.Currency)
			case "disclosure":
				// Covered by the same rules as an action: a check of
				// share_contact (or share_credentials) answered with proceed,
				// and no approval covers two steps.
				d := ev.Disclosure
				if !slices.Contains(dealPointsOfNoReturn[open.Type], d.action()) {
					return cli.InputError("this deal type has no " + d.action() + " point of no return")
				}
				d.AuthorizedBy, d.Reason, d.Rule = authorizeAct(events, dealAct{Action: d.action()}, cli.DealClock().UTC())
				// The representation gate, before first tellings: a telling it
				// holds is authorized by nothing.
				if hold := representationHold(events, d.action()); d.AuthorizedBy != "" && hold != "" {
					d.AuthorizedBy, d.Reason, d.Rule = "", hold, ruleRepresentationMissing
				}
				// A seller gives out where the item is only for an offer the
				// other party accepted, as a commit is.
				if d.AuthorizedBy != "" && d.addressed() && dealRole(events) == dealRoleSeller && dealRecordSet(dealEvent{}, events) == recordsTyped {
					if d.Accepted, d.Reason, d.Rule = offerAccepted(events); d.Rule != "" {
						d.AuthorizedBy = ""
					}
				}
				// An approval covers a telling only to the party its check was
				// about: approving the address for the seller is not approving
				// it for a courier.
				if d.AuthorizedBy != "" && !sameRecipient(events, d.AuthorizedBy, *d, *open) {
					d.AuthorizedBy, d.Reason, d.Rule = "", "that approval was for a telling to someone else", "approval_for_another_party"
				}
				// Noting runs when the form is filled, before it is sent: a
				// class going to this recipient for the first time that no
				// approved check named is held, and nothing is sealed. Leaving
				// a class out of the check is no way around its first telling.
				memory, err := s.counterpartyMemory(ctx, dealID)
				if err != nil {
					return err
				}
				keys := disclosureRecipientKeys(*d, *open)
				if first := firstTellings(events, d.AuthorizedBy, d.Fields, memory, keys); len(first) > 0 {
					name := recipientName(open.Who)
					if d.Who != nil && d.To == "other" {
						name = recipientName(*d.Who)
					}
					words := make([]string, len(first))
					for i, c := range first {
						words[i] = classWord(c)
					}
					if err := cli.Output(c, map[string]any{
						"deal_id": dealID, "proceed": false, "verdict": "pause", "held": first,
						"reason": "First time telling " + name + " your " + strings.Join(words, ", ") + ", and no check the user approved named it",
						"next":   `do not send it; run deal check with "disclosing" naming ` + strings.Join(first, ", ") + `, show its card, and note this again only once the user approves`,
					}); err != nil {
						return err
					}
					return cli.ErrPaused
				}
				if class := uncheckedClass(events, d.AuthorizedBy, d.Fields); d.AuthorizedBy != "" && class != "" {
					d.AuthorizedBy, d.Reason, d.Rule = "", "the check did not name your "+classWord(class), "class_not_checked"
				}
			}
			if deferred != nil {
				return s.deferEmail(ctx, c, dealID, *ev.Evidence, deferred.raw)
			}
			se, err := s.seal(ctx, dealID, events, ev)
			if err != nil {
				return err
			}
			out := stepOutput(dealID, se)
			switch kind {
			case "intent":
				// A note that asks for more than the limits in force is a
				// proposal: sealed as written, applied only on the user's
				// confirmation.
				state, err := foldDeal(events)
				if err != nil {
					return err
				}
				if proposed, more := proposedLimits(state.intent.limits(), withBoundsCommit(*ev.Intent, se.Event.Nonces)); more {
					cli.Speak(out, s.p, func(w cli.Wording) cli.Narration {
						return intentAsk(w, state.intent.limits(), proposed, state.terms.Currency)
					})
					out["proposed"] = proposed
					out["in_force"] = state.intent.limits()
					out["next"] = "this note asks for a higher limit or a new action: it is recorded as a proposal and not applied. " +
						"Ask the user; only on their explicit yes, seal deal note --kind approval --check " + se.CapsuleID +
						" --choice confirm_limits --said \"<their words>\""
				}
			case "acceptance":
				out["accepts"] = ev.Acceptance.Offer
				out["note"] = "recorded as an observation that the other party accepted that exact offer; it authorizes nothing by itself. A commit rests on it while no later offer and no change of details leave it behind"
			case "platform_approval":
				out["answers_check"] = false
				out["note"] = "recorded as an observation of " + ev.Platform.Platform + "'s own approval interaction; it authorizes nothing and does not answer the deal check"
			case "approval":
				out["proceed"] = ev.Approval.Proceed && ev.Approval.Reason == ""
				out["reason"] = ev.Approval.Reason
				if state, err := foldDeal(append(events, se)); err == nil {
					cli.Speak(out, s.p, func(w cli.Wording) cli.Narration {
						return approvalNarration(w, *ev.Approval, append(events, se), state)
					})
				}
				if ev.Approval.Limits != nil {
					out["limits"] = ev.Approval.Limits
				}
				cp, err := s.milestone(ctx)
				if err != nil {
					return err
				}
				out["checkpoint"] = cp
			case "act":
				out["unchecked"] = ev.Act.Unchecked
				out["authorized_by"] = ev.Act.AuthorizedBy
				out["reason"] = ev.Act.Reason
				out["rule"] = ev.Act.Rule
				all := append(events, se)
				s.stepReport(ctx, out, all, htmlPath, func(w cli.Wording) []string { return receiptLines(w, all) })
			case "claim":
				if cl := ev.Claim; cl.SourceKind == "agent" && representationRules(events) {
					out["authorized_by"] = cl.AuthorizedBy
					out["unchecked"] = cl.Unchecked
					if cl.Unchecked {
						out["reason"] = cl.Reason
						out["rule"] = cl.Rule
						out["next"] = "this was sealed as said without your authority; before the agent tells the buyer anything, run deal check with {\"action\": \"represent\", \"representing\": {\"class\": ..., \"text\": ...}} and note the claim with --check"
					}
				}
			case "disclosure":
				d := ev.Disclosure
				out["approved"] = d.AuthorizedBy != ""
				out["authorized_by"] = d.AuthorizedBy
				out["reason"] = d.Reason
				classes := make([]string, len(d.Fields))
				for i, f := range d.Fields {
					classes[i] = f.Class
				}
				out["classes"] = classes
			case "evidence":
				// Keys rotate and are revoked: a sealed key record, like an
				// obligation, is checkpointed at once (the next cadence tick
				// witnesses it).
				if ev.Evidence.Email != nil || ev.Evidence.Obligation != nil {
					cp, err := s.milestone(ctx)
					if err != nil {
						return err
					}
					out["checkpoint"] = cp
				}
				evidenceOutput(out, *ev.Evidence, events)
				all := append(events, se)
				switch {
				case settlementKind == noteKindPaymentReceived:
					cli.Speak(out, s.p, func(w cli.Wording) cli.Narration { return line(w.Line("say.payment_received", nil)) })
				case settlementKind == noteKindDelivered:
					cli.Speak(out, s.p, func(w cli.Wording) cli.Narration { return line(w.Line("say.delivered", nil)) })
				case ev.Evidence.Email != nil:
					if state, err := foldDeal(all); err == nil {
						s.stepReport(ctx, out, all, htmlPath, func(w cli.Wording) []string { return emailLines(w, ev.Evidence.Email, state) })
						out["voice"] = cli.VoiceLine
					}
				}
			}
			if _, spoken := out["voice"]; !spoken {
				cli.Narrate(out, cli.Narration{Voice: cli.VoiceSilent})
			}
			return cli.Output(c, out)
		})
	}}
	cmd.Flags().String("deal", "", "Deal ID from `deal open`")
	cmd.Flags().String("kind", "", "message, claim, evidence, change, intent, approval, act, disclosure, platform_approval, acceptance, payment_received or delivered")
	cmd.Flags().String("html", "", "With --kind act or --kind evidence --email: also write the updated receipt page (your own copy) to this new file, as `deal report --html` does")
	addSettlementNoteFlags(cmd)
	cmd.Flags().String("words", "", "acceptance: the counterparty's words accepting the offer, exactly as given (committed, never stored)")
	cmd.Flags().String("channel", "", "acceptance: the channel the counterparty accepted on (x-deal-v0's channel set)")
	cmd.Flags().String("input", "", "JSON body for message, claim, evidence, change, intent or act (optional for evidence with --email)")
	cmd.Flags().String("email", "", "evidence: a merchant's email as a raw RFC 822 file (.eml), headers intact; sealed with its DKIM key records")
	cmd.Flags().String("key-record", "", "evidence: the DKIM key record to check --email against instead of DNS (marked supplied)")
	cmd.Flags().String("dmarc-record", "", "evidence: the sender's DMARC record, with --key-record (marked supplied)")
	cmd.Flags().String("object", "", "evidence: a held object, exactly as received (at most 4 MiB); its SHA-256 is sealed, the octets stay on this device")
	cmd.Flags().String("object-digest", "", "evidence: only the SHA-256 of an object you do not hold (64 lowercase hex)")
	cmd.Flags().String("object-type", "", "evidence, with --object or --object-digest: the wrapped type, e.g. delivery.proof (a settlement-records registered type) or your own label")
	cmd.Flags().String("octets", wrappedAsTransmitted, "evidence, with --object or --object-digest: what the digest is over: as-transmitted (the octets as delivered) or jcs (their RFC 8785 form)")
	cmd.Flags().String("headers", "", "evidence, with --object or --object-digest: the carrier headers it arrived with (start line and fields, as received); sealed as a salted commitment")
	cmd.Flags().String("check", "", "approval: the check_id being answered, or the capsule_id of an intent note whose proposed limits the user confirms; claim, where the user sells: the represent check the agent's claim was checked with")
	cmd.Flags().String("choice", "", "approval: the option id the user chose, or confirm_limits for an intent note")
	cmd.Flags().String("said", "", "approval: the user's own words")
	cmd.Flags().String("shown-card", "", "approval: a file holding the exact card text the answer was given on, as `deal check` returned it")
	cmd.Flags().String("platform", "", "platform_approval: the platform whose own approval interaction you observed (a name you choose)")
	cmd.Flags().String("mechanism", "", "platform_approval: that platform's approval mechanism (a name you choose)")
	cmd.Flags().String("displayed-text", "", "platform_approval: the text exactly as the platform displayed it (committed, never stored)")
	cmd.Flags().String("user-text", "", "platform_approval: the user's text exactly as the platform returned it, if any (committed, never stored)")
	cmd.Flags().String("observed-at", "", "platform_approval: when you observed it, UTC RFC 3339 (default: now)")
	cmd.Flags().Int64("amount-minor", 0, "platform_approval: the amount the displayed text stated, in minor units")
	cmd.Flags().String("currency", "", "platform_approval: the currency of --amount-minor (default: the deal's)")
	return cmd
}

// judgeApproval binds an answer to one sealed check. It proceeds only when the
// user chose proceed, the check has no earlier answer (the first answer is
// the one an action is held to), and nothing was sealed since that check that
// could change what they were shown.
func judgeApproval(events []sealedEvent, a *dealApproval) error {
	for i, se := range events {
		if se.CapsuleID != a.Check {
			continue
		}
		if se.Event.Kind == "intent" {
			return judgeLimitsConfirmation(events, i, a)
		}
		if se.Event.Kind != "check" {
			return cli.InputError("--check does not name a check of this deal")
		}
		if a.Choice == "confirm_limits" {
			return cli.InputError("confirm_limits answers an intent note that proposed new limits: --check names that note")
		}
		check := se.Event.Check
		if check.Verdict == "pass" {
			return cli.InputError("this check passed; no answer is needed")
		}
		if !slices.ContainsFunc(check.Options, func(o dealOption) bool { return o.ID == a.Choice }) {
			return cli.InputError("--choice is not one of the options the check offered")
		}
		var answered, changed bool
		for _, later := range events[i+1:] {
			switch k := later.Event.Kind; {
			case k == "approval" && later.Event.Approval.Check == a.Check:
				answered = true
			case changesDetails(later.Event), k == "snapshot", k == "check", k == "intent":
				changed = true
			}
		}
		switch {
		case answered:
			a.Reason = approvalAnswered
		case changed:
			a.Reason = approvalChanged
		}
		// The record says what the user chose. A stale answer cannot authorize
		// anything: the action rule requires no change after the check.
		a.Proceed = a.Choice == "proceed"
		return nil
	}
	return cli.InputError("--check does not name a step of this deal")
}

// The reasons an answer is sealed but not applied (the user is told which:
// approvalReasonKeys).
const (
	approvalAnswered  = "this check was already answered; check again"
	approvalChanged   = "details changed after this check; check again"
	proposalReplaced  = "a later intent note replaced this proposal; confirm that one"
	proposalConfirmed = "this proposal was already confirmed"
)

// judgeLimitsConfirmation judges the user's answer to an intent note
// (events[i]) that proposed a higher limit or more actions. Only
// confirm_limits answers one; the answer seals the limits in force and the
// new version, which then apply. An intent note sealed after the proposal
// replaces it, and a proposal is confirmed once.
func judgeLimitsConfirmation(events []sealedEvent, i int, a *dealApproval) error {
	if a.Choice != "confirm_limits" {
		return cli.InputError("an intent note is answered only with --choice confirm_limits")
	}
	state, err := foldDeal(events)
	if err != nil {
		return err
	}
	cur := state.intent.limits()
	proposed, more := proposedLimits(cur, withBoundsCommit(*events[i].Event.Intent, events[i].Event.Nonces))
	for _, later := range events[i+1:] {
		switch {
		case later.Event.Kind == "intent":
			a.Reason = proposalReplaced
		case later.Event.Approval != nil && later.Event.Approval.Check == a.Check && later.Event.Approval.Limits != nil:
			a.Reason = proposalConfirmed
		}
	}
	if a.Reason == "" && !more {
		return cli.InputError("this intent note asks for no higher limit and no new action: there is nothing to confirm")
	}
	a.Limits = &dealLimits{Previous: cur, New: proposed}
	a.Proceed = a.Reason == ""
	return nil
}

func dealCheckCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "check", Short: "Seal what is about to happen, compare it with the baseline, and return the difference card", Args: cli.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		path, _ := c.Flags().GetString("input")
		raw, err := cli.ReadInput(path)
		if err != nil {
			return err
		}
		var snap dealSnapshot
		if err = cli.DecodeJSONAs("--input", raw, &snap); err != nil {
			return err
		}
		staleAfter, _ := c.Flags().GetDuration("stale-after")
		if staleAfter < time.Minute {
			return cli.InputError("--stale-after must be at least one minute")
		}
		return runDeal(c, true, func(ctx context.Context, s *dealSession, dealID string, events []sealedEvent) error {
			if dealFinallyClosed(events) {
				return cli.InputError("this deal is closed and takes no more steps; start a new one with `deal open`")
			}
			// The profile's pinned materiality predicate, read before anything
			// is sealed: one that cannot be read, or no longer has the pinned
			// digest, refuses the check and never falls back. The check run
			// cannot choose another: which picks pause is the user's policy.
			predicate, err := cli.PinnedMateriality(s.p)
			if err != nil {
				return err
			}
			open := events[0].Event.Open
			// represent is a check, never an act: admitted beside the points
			// of no return, on a deal where the user sells.
			if err := checkRepresenting(events, &snap); err != nil {
				return err
			}
			if snap.Action != representAction && !slices.Contains(dealPointsOfNoReturn[open.Type], snap.Action) {
				return cli.InputError("action is not a point of no return for a " + open.Type + " deal; use one of " + strings.Join(dealPointsOfNoReturn[open.Type], ", "))
			}
			if snap.Action == offerAction && dealRole(events) != dealRoleSeller {
				return cli.InputError("an offer is made on a deal where the user sells (intent.party_role seller)")
			}
			// On a sale's thread, the check is about the sale's item, under
			// the sale's task authority as it was when the thread opened.
			snap.ItemRef = open.ItemRef
			if err := s.saleUnchanged(ctx, events); err != nil {
				return err
			}
			if err := checkFee(snap.Action, snap.FeeMinor); err != nil {
				return err
			}
			if err := checkAuthorizedMax(snap, open.Recourse.Rail); err != nil {
				return err
			}
			if err := normalizeTerms(snap.Terms); err != nil {
				return err
			}
			if snap.Who != nil && snap.Who.DomainAgeDays != nil {
				return cli.InputError("record the website's age as evidence, not in the check")
			}
			if snap.Action == "share_contact" && len(snap.Disclosing) == 0 {
				return cli.InputError(`a share_contact check needs "disclosing": the class of every field about to be given, for example ["name","email","address"]`)
			}
			if len(snap.Disclosing) > 0 {
				for i := range snap.Disclosing {
					snap.Disclosing[i] = strings.ToLower(strings.TrimSpace(snap.Disclosing[i]))
				}
				action, err := disclosureAction(snap.Disclosing)
				if err != nil {
					return err
				}
				snap.Disclosing = sortedClasses(snap.Disclosing)
				if action != snap.Action {
					return cli.InputError("disclosing " + strings.Join(snap.Disclosing, ", ") + " goes with action " + action)
				}
			}
			// Every share check says who receives it; nothing defaults to the
			// counterparty, so an approval always names the party it is for.
			if strings.HasPrefix(snap.Action, "share_") {
				switch snap.DisclosingTo {
				case "":
					return cli.InputError(`a share check needs "disclosing_to": "counterparty" (the deal's counterparty) or "other" (with a recipient)`)
				case "counterparty":
					if snap.Recipient != nil {
						return cli.InputError(`recipient goes with "disclosing_to": "other"; a telling to the counterparty is about the deal's own counterparty`)
					}
				case "other":
					if snap.Recipient == nil || len(counterpartyKeys(*snap.Recipient, "")) == 0 {
						return cli.InputError(`"disclosing_to": "other" needs a recipient with a phone, email, profile id, reply address or website, so the approval names who it is for`)
					}
				default:
					return cli.InputError(`disclosing_to must be counterparty or other`)
				}
			} else if snap.DisclosingTo != "" || snap.Recipient != nil {
				return cli.InputError(`disclosing_to and recipient go with a share check`)
			}
			if checkNamesPayee(snap, dealRole(events)) {
				// The check names the payee it is about, so an action can be
				// held to the same payee, and a rule keyed on who was paid
				// (the profile companion below) sees the merchant on every
				// check that moves money to it, not only on a pay.
				state, err := foldDeal(events)
				if err != nil {
					return err
				}
				payee := state.who.Payee
				if payee == "" {
					payee = state.who.Name
				}
				if snap.Who == nil {
					snap.Who = &dealWho{}
				}
				if snap.Who.Payee == "" {
					snap.Who.Payee = payee
				}
			}
			// A floor in force goes to the rules checker opened, or the check
			// does not run: refused here, before anything is sealed.
			if len(s.p.RulesChecker.Command) > 0 {
				if _, err := dealBoundsOpening(events); err != nil {
					return err
				}
			}
			// snapshot -> seal
			snapped, err := s.seal(ctx, dealID, events, dealEvent{Kind: "snapshot", Snapshot: &snap})
			if err != nil {
				return err
			}
			events = append(events, snapped)
			// The payee keyed per profile, sealed right after the check it
			// is about, before any checker runs.
			companion, err := s.counterpartyProfileStep(snap)
			if err != nil {
				return err
			}
			if companion != nil {
				companion.Check = snapped.CapsuleID
				sealed, err := s.seal(ctx, dealID, events, dealEvent{Kind: "counterparty_profile", CounterpartyProfile: companion})
				if err != nil {
					return err
				}
				events = append(events, sealed)
			}
			// -> diff
			state, err := foldDeal(events)
			if err != nil {
				return err
			}
			if state.memory, err = s.counterpartyMemory(ctx, dealID); err != nil {
				return err
			}
			state.materiality = predicate
			result := evaluateDeal(state, snap)
			result.Materiality = predicate.Ref()
			if result.Remote, err = dealRemoteCheck(ctx, state, snap, result); err != nil {
				return err
			}
			result.Differences = append(result.Differences, result.Remote.Differences...)
			// The profile's rules, by its pinned checker, on the record of
			// what is about to happen (the capsule just sealed, with its
			// disclosed record) and the profile's recent acts. One verdict and
			// one prompt with the check's own differences.
			rules := dealRules{Status: "not_configured"}
			if len(s.p.RulesChecker.Command) > 0 {
				input, history, err := s.rulesInput(ctx, snapped.CapsuleID, events)
				if err != nil {
					return err
				}
				rules = runRulesChecker(ctx, s.p, input, history)
				rules = sealableRules(rules, dealLocalValues(events, dealEvent{}))
			}
			result.Rules = &rules
			result.Differences = append(result.Differences, rulesDifferences(&rules)...)
			settleCheck(&result, state)
			result.Snapshot = snapped.CapsuleID
			// One block: the approval text, and on a pause the card the
			// answer is sealed against. A passing check has no card.
			approvalText := dealApprovalText(state, snap, result, events)
			if result.Verdict != "pass" {
				result.Card = approvalText
			}
			// -> seal the result before it is shown
			if dealRecordSet(dealEvent{}, events) == recordsTyped {
				// The check contract's fields, fixed when the evaluation is sealed.
				result.ValidUntil = cli.DealClock().UTC().Truncate(time.Second).Add(staleAfter).Format(time.RFC3339)
				// The rule table and the predicate this check evaluated
				// (result.Materiality, the one source the output, the record
				// and the check response all state).
				if result.RulesetDigest, err = rulesetDigest(result.Materiality.PredicateDigest()); err != nil {
					return err
				}
			}
			checked, err := s.seal(ctx, dealID, events, dealEvent{Kind: "check", Check: &result})
			if err != nil {
				return err
			}
			events = append(events, checked)
			out := stepOutput(dealID, checked)
			// A passing check of an action the user already allowed is
			// approved by that standing intent, sealed as its own step: every
			// action cites a sealed approval.
			typed := dealRecordSet(dealEvent{}, events) == recordsTyped
			if typed {
				out["check_response"] = checkResponseFor(checked.Event, events, checked.Digest)
			}
			if result.Verdict == "pass" && !typed {
				approval := &dealApproval{Check: checked.CapsuleID, Choice: "proceed", Approver: "standing_intent", Proceed: true}
				approved, err := s.seal(ctx, dealID, events, dealEvent{Kind: "approval", Approval: approval})
				if err != nil {
					return err
				}
				out["approval_id"] = approved.CapsuleID
			}
			out["check_id"] = checked.CapsuleID
			out["snapshot_id"] = snapped.CapsuleID
			out["verdict"] = result.Verdict
			out["proceed"] = result.Verdict == "pass"
			out["card"] = result.Card
			out["options"] = result.Options
			out["differences"] = result.Differences
			out["unverified"] = result.Unverified
			out["asked_attributes"] = result.Asked
			out["picked_by_agent"] = result.Picked
			out["materiality"] = result.Materiality
			out["rules"] = result.Rules
			if rc := result.Recipient; rc != nil {
				out["recipient"] = rc
				if len(rc.Repeat) > 0 {
					words := make([]string, len(rc.Repeat))
					for i, c := range rc.Repeat {
						words[i] = classWord(c)
					}
					out["note"] = "Told " + rc.Name + " your " + strings.Join(words, ", ") + " before: noted, no pause for that."
				}
			}
			out["remote"] = result.Remote.Status
			out["demo"] = open.Demo
			// A date passing is a point of no return too: every check lists
			// the deal's open cancel-by dates.
			out["open_deadlines"] = openDeadlines(events, cli.DealClock())
			out["checked_at"] = checked.Event.At
			out["stale_after_minutes"] = int(staleAfter / time.Minute)
			out["approval_text"] = approvalText
			// A pause or a deny asks the user with the card; a pass goes
			// ahead and says nothing.
			cli.Speak(out, s.p, func(w cli.Wording) cli.Narration {
				if result.Verdict == "pass" {
					return silent(w)
				}
				return cli.Narration{Voice: cli.VoiceAsk, Say: result.Card}
			})
			return cli.Output(c, out)
		})
	}}
	cmd.Flags().String("deal", "", "Deal ID from `deal open`")
	cmd.Flags().String("input", "", "Snapshot JSON: the action and exactly what is about to happen")
	cmd.Flags().Duration("stale-after", 15*time.Minute, "How long the check stays current; the approval text says when it goes stale")
	return cmd
}

func dealCloseCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "close", Short: "Compare what was delivered with what was agreed: completed, mismatch or open", Args: cli.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		path, _ := c.Flags().GetString("input")
		htmlPath, _ := c.Flags().GetString("html")
		raw, err := cli.ReadInput(path)
		if err != nil {
			return err
		}
		var in dealCloseInput
		if err = cli.DecodeJSONAs("--input", raw, &in); err != nil {
			return err
		}
		if !slices.Contains([]string{"received", "pending", "not_received", "not_selected"}, in.Status) {
			return cli.InputError("status must be received, pending, not_received or not_selected")
		}
		if err = normalizeTerms(in.Delivered); err != nil {
			return err
		}
		return runDeal(c, true, func(ctx context.Context, s *dealSession, dealID string, events []sealedEvent) error {
			if dealFinallyClosed(events) {
				return cli.InputError("this deal is closed and takes no more steps; start a new one with `deal open`")
			}
			state, err := foldDeal(events)
			if err != nil {
				return err
			}
			if in.Status == "not_selected" {
				for _, se := range events {
					if se.Event.Kind == "act" {
						return cli.InputError("not_selected closes a deal where nothing was done, and this deal has an action on record: close it received or not_received")
					}
				}
			}
			carry, _ := c.Flags().GetBool("carry-open-obligations")
			open := openDeadlines(events, cli.DealClock())
			if len(open) > 0 && in.Status != "pending" && !carry {
				return cli.InputError("this deal has an open date (" + open[0].date() + ": " + open[0].Text + "); closing would end its record. Close with status pending, close after the cancel is sealed or the date has passed, or close with --carry-open-obligations to keep the date open after the close")
			}
			result := closeDeal(state, in)
			if err = settleCloseActorReason(&result.dealCloseInput, result.Outcome); err != nil {
				return err
			}
			if in.Status != "pending" {
				for _, d := range open {
					result.Carried = append(result.Carried, dealCarried{Step: d.Step, CapsuleID: d.CapsuleID, CancelBy: d.CancelBy, DueBy: d.DueBy})
				}
			}
			for _, se := range events {
				if se.Event.Kind == "act" && se.Event.Act.Unchecked {
					result.UncheckedActions++
				}
			}
			observed, err := s.seal(ctx, dealID, events, dealEvent{Kind: "outcome", Outcome: &result})
			if err != nil {
				return err
			}
			events = append(events, observed)
			closing := result
			closing.Note = ""
			se, err := s.seal(ctx, dealID, events, dealEvent{Kind: "close", Close: &closing})
			if err != nil {
				return err
			}
			cp, err := s.milestone(ctx)
			if err != nil {
				return err
			}
			out := stepOutput(dealID, se)
			out["outcome"] = result.Outcome
			out["actor"], out["reason"] = result.Actor, result.Reason
			out["differences"] = result.Differences
			out["unchecked_actions"] = result.UncheckedActions
			out["checkpoint"] = cp
			if len(result.Carried) > 0 {
				out["carried_open_obligations"] = result.Carried
				out["note"] = "The deal is closed; the carried cancel-by dates stay open, and `deal deadlines` lists them as CARRIED AT CLOSE until a later record resolves them or the date passes."
			}
			all := append(events, se)
			if key, final := closeKeys[result.Outcome]; final {
				s.stepReport(ctx, out, all, htmlPath, func(w cli.Wording) []string { return []string{w.Line(key, nil)} })
			} else {
				cli.Speak(out, s.p, func(w cli.Wording) cli.Narration { return line(w.Line("say.close.open", nil)) })
			}
			return cli.Output(c, out)
		})
	}}
	cmd.Flags().String("deal", "", "Deal ID from `deal open`")
	cmd.Flags().String("input", "", "Close JSON: status and what was delivered")
	cmd.Flags().Bool("carry-open-obligations", false, "Close although a cancel-by date is open: the close seals it, and it stays open after the close")
	cmd.Flags().String("html", "", "Also write the final receipt page (your own copy) to this new file, as `deal report --html` does")
	return cmd
}

func dealReportCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "report", Short: "Report the deal in three parts (what you asked, what the agent did, anomalies); --html writes one local page", Args: cli.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		htmlPath, _ := c.Flags().GetString("html")
		emailPath, _ := c.Flags().GetString("email")
		bundlePath, _ := c.Flags().GetString("bundle")
		audience, _ := c.Flags().GetString("share")
		recipient, _ := c.Flags().GetString("to")
		recipient = strings.TrimSpace(recipient)
		fromBundle, _ := c.Flags().GetString("from-bundle")
		directory, _ := c.Flags().GetString("directory")
		// A countersignature covers one exact bundle: your own copy. A shared
		// copy withholds records, so it is a different bundle.
		if fromBundle != "" && audience != cli.DealAudienceKeep {
			return cli.InputError("--from-bundle renders your own copy, the bundle a countersignature covers; a shared copy (--share) withholds records, so it is a different bundle and is not countersigned")
		}
		switch audience {
		case cli.DealAudienceKeep:
			if recipient != "" {
				return cli.InputError("--to names who a shared copy is for; use it with --share counterparty or adjudicator")
			}
		case cli.DealAudienceCounterparty, cli.DealAudienceAdjudicator:
			if htmlPath == "" || recipient == "" {
				return cli.InputError("--share writes a copy for someone else: it needs --html FILE and --to (who it is for)")
			}
			// The email and the bundle file are the user's own copy, with
			// nothing withheld; a shared copy is the page alone.
			if emailPath != "" || bundlePath != "" {
				return cli.InputError("--share writes only the shared page; --email and --bundle are the user's own copy")
			}
		default:
			return cli.InputError("--share must be keep, counterparty or adjudicator")
		}
		return runDeal(c, true, func(ctx context.Context, s *dealSession, dealID string, events []sealedEvent) error {
			report, reportErr := s.dealReportFor(ctx, events)
			if reportErr != nil {
				return reportErr
			}
			lines := make([]string, 0, len(events))
			outcome := "open"
			restated := restatedIntents(events)
			for i, se := range events {
				if se.Event.Kind == "close" {
					outcome = se.Event.Close.Outcome
				}
				lines = append(lines, fmt.Sprintf("%d. %s %s", se.Event.N, se.Event.At, trailLineIn(events, restated, i)))
			}
			out := map[string]any{
				"deal_id": dealID, "scope": dealScopeLine, "did_line": dealDidLine(dealDidSources(events)), "demo": events[0].Event.Open.Demo, "outcome": outcome,
				"asked": report.Asked, "did": report.Did, "told": report.Told, "anomalies": report.Anomalies, "merchant": report.Merchant,
				"instructions": report.Instructions,
				"produced_by":  dealProducers(events),
				"deadlines":    dealDeadlines(events, cli.DealClock(), 2), "cancellations": dealCancellations(events), "lifecycle": buildDealLifecycle(events, cli.DealClock()), "trail": strings.Join(lines, "\n"),
				"countersign": dealNotCountersigned(),
			}
			if report.Money != nil {
				out["money"] = report.Money
			}
			if report.Authority != nil {
				out["authority"], out["authority_order"] = report.Authority, dealAuthorityOrder
			}
			if htmlPath == "" && emailPath == "" && bundlePath == "" && fromBundle == "" {
				// Every report states its rung, read from the same bundle the
				// page and the email would carry.
				if s.dp.Checkpoint.Signing == (cli.Secret{}) {
					out["assurance"] = dealAssurance(map[string]interface{}{})
					return cli.Output(c, reportSay(out, s.p, events, ""))
				}
				b, err := s.dealReportBundle(ctx, events, report, cli.DealAudienceKeep, "", false)
				if err != nil {
					return err
				}
				out["assurance"] = dealAssurance(b)
				return cli.Output(c, reportSay(out, s.p, events, ""))
			}
			// --from-bundle renders the receipt from a bundle file this deal
			// wrote earlier (and someone may have countersigned since), so a
			// countersignature covers exactly what the receipt shows.
			var b map[string]interface{}
			var err error
			if fromBundle != "" {
				b, err = s.dealBundleOf(fromBundle, dealID, events)
			} else {
				b, err = s.dealReportBundle(ctx, events, report, audience, dealVerifyCommand(htmlPath), true)
			}
			if err != nil {
				return err
			}
			countersign := dealNotCountersigned()
			if fromBundle != "" {
				if countersign, err = dealCountersignVerify(ctx, b, directory, s.p); err != nil {
					return err
				}
			}
			out["countersign"] = countersign
			page, err := dealReportHTML(b, countersign, dealVerifyCommand(htmlPath))
			if err != nil {
				return err
			}
			assurance := dealAssurance(b)
			if audience != cli.DealAudienceKeep {
				// The final bytes are checked before anything is on record.
				if err = dealShareGate([]byte(page), events, audience); err != nil {
					return err
				}
				// Sharing is a disclose act: it is on record before the file
				// exists, and the output carries none of the local report's
				// raw text.
				share, err := s.recordShare(ctx, dealID, b, audience, recipient, dealRole(events), []byte(page), "")
				if err != nil {
					return err
				}
				if err = cli.AtomicFile(htmlPath, []byte(page), false); err != nil {
					return err
				}
				// A copy for someone else is sent by the user, not narrated.
				return cli.Output(c, cli.Narrate(map[string]any{"deal_id": dealID, "scope": dealScopeLine, "html": htmlPath, "assurance": assurance, "countersign": countersign, "share": share}, cli.Narration{Voice: cli.VoiceSilent}))
			}
			bundle, err := json.Marshal(b)
			if err != nil {
				return err
			}
			out["assurance"] = assurance
			if htmlPath != "" {
				if err = cli.AtomicFile(htmlPath, []byte(page), false); err != nil {
					return err
				}
				out["html"] = htmlPath
			}
			if bundlePath != "" {
				if err = cli.AtomicFile(bundlePath, bundle, false); err != nil {
					return err
				}
				out["bundle"] = bundlePath
			}
			if emailPath != "" {
				// A report off the deal's log is as of its checkpoint; the email
				// shows that, and says apart, dated, where the deal stood when
				// it was made (capsule-cli#294). Its cancel-by dates are the
				// email's own, dated as of when it was made.
				now := cli.DealClock()
				lifecycle := buildDealLifecycle(events, now)
				composed := ""
				if signed, _, form := cli.DealSealedReport(b); form == "standalone" {
					// The lifecycle is the one the report signed, as of its
					// checkpoint, even when the bundle is an earlier one
					// (--from-bundle); where the deal stands now is the
					// composed note's, dated.
					at, err := bundleCheckpointTime(b)
					if err != nil {
						return err
					}
					raw, err := json.Marshal(signed["lifecycle"])
					if err != nil {
						return err
					}
					lifecycle = dealLifecycle{}
					if err = json.Unmarshal(raw, &lifecycle); err != nil {
						return cli.ErrConflict
					}
					composed = fmt.Sprintf("Made by capsulectl at %s, when the deal stood so: %s This report shows the deal as of %s; open the attached page for where it stands now.", now.UTC().Format("2006-01-02T15:04:05Z"), buildDealLifecycle(events, now).Text, at.Format("2006-01-02T15:04:05Z"))
				}
				view := dealEmailView{Demo: events[0].Event.Open.Demo, Asked: report.Asked, Outcome: outcome, Assurance: assurance["text"].(string), Countersign: countersign.Text, Did: report.Did, Authority: report.Authority, AuthorityOrder: dealAuthorityOrder, Anomalies: report.Anomalies, Merchant: report.Merchant, Deadlines: dealDeadlines(events, now, 2), Cancellations: dealCancellations(events), Lifecycle: lifecycle, ComposedNote: composed, Steps: len(events)}
				eml, subject, text, htmlBody, err := dealEmail(view, []byte(page), bundle, cli.DealClock())
				if err != nil {
					return err
				}
				if err = cli.AtomicFile(emailPath, eml, false); err != nil {
					return err
				}
				out["email"] = map[string]any{"eml": emailPath, "subject": subject, "text": text, "html": htmlBody, "attachments": []string{"receipt.html", "bundle.json"}}
			}
			return cli.Output(c, reportSay(out, s.p, events, htmlPath))
		})
	}}
	cmd.Flags().String("deal", "", "Deal ID from `deal open`")
	cmd.Flags().String("html", "", "Write the report as one local, self-contained page to this new file")
	cmd.Flags().String("email", "", "Write the receipt as a ready-to-send email (.eml, no sender or recipient) to this new file, for the agent host's own email tool to send")
	cmd.Flags().String("bundle", "", "Write the deal's Evidence Bundle (for `capsulectl verify --bundle`) to this new file")
	cmd.Flags().String("share", cli.DealAudienceKeep, "Who the page is for: keep (your own copy, nothing withheld), counterparty or adjudicator (a shared copy, put on record first)")
	cmd.Flags().String("to", "", "With --share: who the shared copy is for, as sealed in the disclosure record")
	cmd.Flags().String("from-bundle", "", "Render from this deal's earlier bundle file instead of a new one, with any countersignatures it carries (see `deal countersign`)")
	cmd.Flags().String("directory", "", "With --from-bundle: the countersigner directory (HTTPS URL or file) that resolves who countersigned")
	return cmd
}

func dealExportCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "export", Short: "Write the deal's sealed x-deal-v0 records as one JSON array, in order (no raw values)", Args: cli.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		path, _ := c.Flags().GetString("output")
		if path == "" {
			return cli.InputError("--output is required: the file to write the result to")
		}
		return runDeal(c, true, func(ctx context.Context, s *dealSession, dealID string, events []sealedEvent) error {
			records := make([]json.RawMessage, 0, len(events))
			var before []sealedEvent
			for _, se := range events {
				payload, _, err := encodeDealRecord(se.Event, before, s.dkey)
				if err != nil {
					return err
				}
				records = append(records, payload)
				before = append(before, se)
			}
			b, err := json.Marshal(records)
			if err != nil {
				return err
			}
			if err = cli.AtomicFile(path, b, false); err != nil {
				return err
			}
			return cli.Output(c, cli.Narrate(map[string]any{"deal_id": dealID, "records": len(records), "output": path}, cli.Narration{Voice: cli.VoiceSilent}))
		})
	}}
	cmd.Flags().String("deal", "", "Deal ID from `deal open`")
	cmd.Flags().String("output", "", "New file for the JSON array of records")
	return cmd
}

// callerToken is a name the caller gives a platform or its approval
// mechanism: capsulectl names none itself.
var callerToken = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
