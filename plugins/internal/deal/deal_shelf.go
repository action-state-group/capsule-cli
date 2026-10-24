package deal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/action-state-group/capsule-cli/cli"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/action-state-group/evidencebook"
)

// The deal store on an evidence-book shelf (capsule-cli#242 PR2b, design
// note §1-§3 and §7): one SQLite file, one writer (the shelf, holding the
// store's own lock file), each deal's log in the cadence as a legacy log,
// and the cadence evidencebook's, taken over from deal_cadence once.

// dealShelfSchema is the migration marker and the store's mutation counter.
// shelf_mutations' triggers count every write any build makes to the tables
// a released capsulectl writes, so a write the new build did not make (a
// released build run on a migrated store) is seen on the next open. A
// trigger aborts the write it fires on when the counter row is missing, so
// the counter is never silently skipped (note §3.3, §7).
var dealShelfSchema = []string{
	`CREATE TABLE IF NOT EXISTS deal_shelf (id INTEGER PRIMARY KEY CHECK (id = 1), version INTEGER NOT NULL, seen INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS shelf_mutations (id INTEGER PRIMARY KEY CHECK (id = 1), n INTEGER NOT NULL)`,
	`INSERT OR IGNORE INTO shelf_mutations (id, n) VALUES (1, 0)`,
}

// dealShelfVersion is the deal_shelf marker's version.
const dealShelfVersion = 1

// dealCountedTables are the tables a released capsulectl writes on a deal
// store: every log append (cll_entries), every witness delivery attempt
// (cll_witnesses), and the deal index and cadence.
var dealCountedTables = []string{"cll_entries", "cll_witnesses", "deal_steps", "deal_reports", "deal_cadence"}

func dealCounterTriggers() []string {
	var out []string
	for _, table := range dealCountedTables {
		for _, event := range []string{"INSERT", "UPDATE"} {
			out = append(out, fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS shelf_mutations_%s_%s AFTER %s ON %s BEGIN
	SELECT RAISE(ABORT, 'the deal store has no mutation counter: it was changed outside capsulectl; restore it from a backup') WHERE NOT EXISTS (SELECT 1 FROM shelf_mutations WHERE id = 1);
	UPDATE shelf_mutations SET n = n + 1 WHERE id = 1;
END`, strings.ToLower(event), table, event, table))
		}
	}
	return out
}

// errNoCheckpointKey refuses a deal profile without a configured checkpoint
// key: the shelf signs every log's checkpoints with it (note §7.4).
func errNoCheckpointKey(p cli.Profile) error {
	return cli.InputError("deal profile " + p.Name + " has no checkpoint key, which its deal store needs to checkpoint each deal: `capsulectl profile update --profile " + p.Name + " --checkpoint-signing-key-file FILE --checkpoint-trusted-key <64-hex public key>`")
}

// openShelf opens the deal store's shelf, taking its writer lock (the same
// lock file every released build takes), in place of lockDealStore.
func (s *dealSession) openShelf(ctx context.Context, dbPath string, originals evidencebook.ArtifactStore) error {
	if s.p.Checkpoint.Signing == (cli.Secret{}) {
		return errNoCheckpointKey(s.p)
	}
	checkpointKey, err := cli.PrivateKey(s.p.Checkpoint.Signing)
	if err != nil {
		return err
	}
	signer, err := evidencebook.NewEd25519Signer(s.key)
	if err != nil {
		return err
	}
	shelf, err := evidencebook.OpenSQLiteShelf(ctx, evidencebook.ShelfConfig{
		Path: dbPath, LockPath: dbPath + ".deal.lock",
		Operator: s.p.Name, Developer: "capsulectl-deal",
		Signer: signer, CheckpointKey: checkpointKey, Originals: originals,
		// The deal clock (cli.DealClock), or, while a book record is appended,
		// the time it carries (appendBook, note §2.2).
		Now: func() time.Time {
			if !s.inFlight.IsZero() {
				return s.inFlight
			}
			return cli.DealClock().UTC()
		},
		// A deal's log holds its own steps and reports only: no
		// authenticated index, so no index_root leaves (note §2.2).
		AuthenticatedIndexes: func(string) []evidencebook.AuthenticatedIndex { return []evidencebook.AuthenticatedIndex{} },
	})
	if err != nil {
		return err
	}
	s.shelf = shelf
	s.unlock = shelf.Release
	return nil
}

// cadenceOf opens the profile's cadence on the shelf, once per session.
func (s *dealSession) cadenceOf(ctx context.Context) (*evidencebook.Cadence, cli.Profile, error) {
	p, err := s.cadenceProfile()
	if err != nil {
		return nil, cli.Profile{}, err
	}
	if s.cadence != nil {
		return s.cadence, p, nil
	}
	cfg, err := p.DealCadence()
	if err != nil {
		return nil, cli.Profile{}, err
	}
	service, err := cli.ServiceID(p)
	if err != nil {
		return nil, cli.Profile{}, err
	}
	cc := evidencebook.CadenceConfig{LogID: p.LogID, Interval: cfg.Interval, Jitter: cfg.Jitter, NoJitter: cfg.Jitter == 0, PadBucket: cfg.PadBucket, Ended: s.ended}
	if service != "" {
		cc.WitnessIDs = []string{service}
	}
	if s.cadence, err = s.shelf.Cadence(ctx, cc); err != nil {
		return nil, cli.Profile{}, err
	}
	return s.cadence, p, nil
}

// ended is the cadence's Ended predicate for the profile's logs: a deal has
// ended once its final close is indexed (finalClose: a close whose outcome
// is not open; a pending close leaves it open, note §2.4), and a sale once
// its sale_close is (capsule-cli#271). The cadence calls it under the shelf's lock, so it
// never reads the shelf: a legacy deal is answered from the SQLite index, a
// book deal from bookEnded, read before a receipt is reported (settleEnded).
func (s *dealSession) ended(ctx context.Context, logID string) (bool, error) {
	// A results book has ended exactly when its deal has (note §2.10 v7,
	// 7.1). Only book deals have one, and settleEnded settled its deal.
	if dealID, ok := dealOfResultsBook(logID); ok {
		return s.bookEnded[dealID], nil
	}
	dealID, ok := strings.CutPrefix(logID, "deal/")
	if !ok || !(cli.DealIDPattern.MatchString(dealID) || cli.SaleIDPattern.MatchString(dealID)) {
		return false, nil
	}
	legacy := sqliteDealIndex{s.db}
	if n, err := legacy.stepCount(ctx, dealID); err != nil || n == 0 {
		return s.bookEnded[dealID], err
	}
	steps, err := legacy.steps(ctx, stepFilter{DealID: dealID})
	if err != nil {
		return false, err
	}
	events := make([]sealedEvent, 0, len(steps))
	for _, st := range steps {
		var ev dealEvent
		if err := json.Unmarshal([]byte(st.Local), &ev); err != nil {
			return false, cli.ErrConflict
		}
		events = append(events, sealedEvent{Event: ev})
	}
	return dealFinallyClosed(events), nil
}

// migrate takes a store a released build wrote onto the shelf, once (note
// §3.2, §7.2), and on every later open repairs what a released build
// wrote since (note §3.3).
func (s *dealSession) migrate(ctx context.Context) error {
	var seen, mutations int64
	err := s.db.QueryRowContext(ctx, `SELECT seen FROM deal_shelf WHERE id = 1`).Scan(&seen)
	marked := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) && !strings.Contains(err.Error(), "no such table") {
		return err
	}
	if marked {
		if err := s.db.QueryRowContext(ctx, `SELECT n FROM shelf_mutations WHERE id = 1`).Scan(&mutations); err != nil {
			if errors.Is(err, sql.ErrNoRows) || strings.Contains(err.Error(), "no such table") {
				return cli.InputError("the deal store has its migration marker but no mutation counter: it was changed outside capsulectl; restore it from a backup")
			}
			return err
		}
		if mutations == seen {
			s.migrated = true
			return nil
		}
	}
	if err := dealMigrateStopsAt("start"); err != nil {
		return err
	}
	// Phase 1: recover every step a released build prepared but never
	// logged, as load does, before anything is seeded.
	ids, err := s.index.dealIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		indexed, err := s.index.stepCount(ctx, id)
		if err != nil {
			return err
		}
		if err := s.useDeal(ctx, id, false); err != nil {
			return err
		}
		entries, err := s.stepEntries(ctx, id)
		if err == nil && len(entries) < indexed {
			_, err = s.load(ctx, id)
		}
		err = errors.Join(err, s.t.Close())
		s.t, s.legacy = nil, nil
		if err != nil {
			return fmt.Errorf("deal %s: %w", id, err)
		}
	}
	// Phase 2: the cadence takes over the store's pre-shelf ticks and logs,
	// then the marker and the counter (two idempotent steps: H is a set).
	c, p, err := s.cadenceOf(ctx)
	if err != nil {
		return err
	}
	if last, ok, err := c.Last(ctx); err != nil {
		return err
	} else if ok && last.CheckpointSize == 0 {
		// The cadence's own tick, unfinished: finish it before importing
		// any later one.
		if _, err := c.Tick(ctx, cli.DealClock()); err != nil {
			return err
		}
	}
	legacy := evidencebook.LegacyCadence{}
	for _, id := range ids {
		legacy.Logs = append(legacy.Logs, cli.DealLogID(id))
	}
	if legacy.Ticks, err = s.legacyTicks(ctx, p); err != nil {
		return err
	}
	if err := c.ImportLegacy(ctx, legacy); err != nil {
		return err
	}
	if err := s.witnessNativeTicks(ctx, c, p); err != nil {
		return err
	}
	if err := dealMigrateStopsAt("imported"); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range append(dealShelfSchema, dealCounterTriggers()...) {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO deal_shelf (id, version, seen) VALUES (1, ?, (SELECT n FROM shelf_mutations WHERE id = 1))
ON CONFLICT(id) DO UPDATE SET seen = excluded.seen`, dealShelfVersion); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.migrated = true
	return nil
}

// dealMigrateStopsAt is a test hook: the migration or repair stops with
// its error at a stage, standing in for a crash or a cancellation there.
var dealMigrateStopsAt = func(stage string) error { return nil }

// legacyTicks reads the store's deal_cadence rows as cadence ticks, in
// cadence-log order, each witnessed when the cadence log holds a verified
// receipt for its checkpoint.
func (s *dealSession) legacyTicks(ctx context.Context, p cli.Profile) (_ []evidencebook.CadenceTick, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tick, at, due_next, entry_seq, checkpoint_size, checkpoint, leaves FROM deal_cadence WHERE checkpoint_size > 0 ORDER BY entry_seq`)
	if err != nil {
		return nil, err
	}
	var ticks []evidencebook.CadenceTick
	for rows.Next() {
		var tick evidencebook.CadenceTick
		var at, due, leaves string
		if err := rows.Scan(&tick.N, &at, &due, &tick.EntrySeq, &tick.CheckpointSize, &tick.Checkpoint, &leaves); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		if tick.At, err = time.Parse(time.RFC3339, at); err == nil {
			tick.DueNext, err = time.Parse(time.RFC3339, due)
		}
		if err == nil {
			err = json.Unmarshal([]byte(leaves), &tick.Leaves)
		}
		if err != nil {
			return nil, errors.Join(cli.ErrConflict, err, rows.Close())
		}
		// A released build keyed a tick's leaves by deal id; the cadence
		// keys them by log id. Each leaf names its own log (log_id, in the
		// leaf hash), so only the key changes, never a leaf or the tree.
		byLog := make(map[string]evidencebook.TickLeaf, len(tick.Leaves.Deals))
		for id, leaf := range tick.Leaves.Deals {
			if leaf.LogID == "" {
				leaf.LogID = cli.DealLogID(id)
			}
			byLog[leaf.LogID] = leaf
		}
		tick.Leaves.Deals = byLog
		ticks = append(ticks, tick)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	service, err := cli.ServiceID(p)
	if err != nil || service == "" || len(ticks) == 0 {
		return ticks, err
	}
	t, err := cli.OpenTarget(ctx, p, cli.UseCLLRead)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, t.Close()) }()
	for i := range ticks {
		if receipt, ok, err := verifiedReceipt(ctx, p, t.Log, service, ticks[i].CheckpointSize); err != nil {
			return nil, err
		} else if ok {
			ticks[i].Witnessed, ticks[i].Receipt = true, receipt
		}
	}
	return ticks, nil
}

// witnessNativeTicks reports to the cadence each verified receipt the
// cadence log holds for a tick the cadence has not recorded as witnessed:
// one a released build delivered on the shared log (note §7.2), or one this
// build stored but did not report (deliverCadence). Both are writes this
// build did not see, so it runs only in the repair they trigger. It walks
// back from the newest tick to the newest witnessed one: with a witness that
// never answers, that is every tick, which the repair's rarity bounds.
func (s *dealSession) witnessNativeTicks(ctx context.Context, c *evidencebook.Cadence, p cli.Profile) (err error) {
	service, err := cli.ServiceID(p)
	if err != nil || service == "" {
		return err
	}
	if err := s.settleEnded(ctx, c); err != nil {
		return err
	}
	last, ok, err := c.Last(ctx)
	if err != nil || !ok {
		return err
	}
	t, err := cli.OpenTarget(ctx, p, cli.UseCLLRead)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, t.Close()) }()
	for n := last.N; n >= 1; n-- {
		tick, err := c.Get(ctx, n)
		if err != nil {
			return err
		}
		if tick.Witnessed {
			break
		}
		if tick.CheckpointSize == 0 {
			continue
		}
		receipt, ok, err := verifiedReceipt(ctx, p, t.Log, service, tick.CheckpointSize)
		if err != nil {
			return err
		}
		if ok {
			if err := c.Witnessed(ctx, n, *receipt); err != nil {
				return err
			}
		}
	}
	return nil
}

// verifiedReceipt is the cadence checkpoint at size's witness receipt, when
// the cadence log holds one that verifies.
func verifiedReceipt(ctx context.Context, p cli.Profile, log cll.WitnessStateStore, service string, size uint64) (*evidencebook.WitnessReceipt, bool, error) {
	state, err := log.GetWitness(ctx, service, size)
	if errors.Is(err, cll.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if state.Receipt == nil || state.Receipt.LeafIndex == nil || state.Receipt.TreeSize == nil || cli.VerifyWitness(p, state) != nil {
		return nil, false, nil
	}
	return &evidencebook.WitnessReceipt{
		TSURL: trimEndpoint(p.Checkpoint.Endpoint), EntryHash: state.Receipt.EntryHash, Receipt: state.Receipt.Bytes,
		LeafIndex: uint64(*state.Receipt.LeafIndex), TreeSize: uint64(*state.Receipt.TreeSize),
	}, true, nil
}

// markSeen records the mutation counter as seen: every write so far is
// this build's own. Run before the shelf is released; a crash before it
// only makes the next open repair once more.
func (s *dealSession) markSeen(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE deal_shelf SET seen = (SELECT n FROM shelf_mutations WHERE id = 1) WHERE id = 1`)
	if err != nil && strings.Contains(err.Error(), "no such table") {
		return nil // a store whose migration never completed: the next open runs it
	}
	return err
}

// appendLegacy runs one append to the current deal's legacy log through the
// shelf (LegacyLog.Append), which puts the log in the cadence's live set
// before the write, so no append is ever left out of the tick.
func (s *dealSession) appendLegacy(ctx context.Context, publish func() (cli.Publication, error)) (cli.Publication, error) {
	if s.legacy == nil {
		return publish()
	}
	var pub cli.Publication
	err := s.legacy.Append(ctx, func() error {
		var err error
		pub, err = publish()
		return err
	})
	return pub, err
}
