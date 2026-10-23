package plugin

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/action-state-group/checkpointed-local-log/go/checkpoint"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	cllsqlite "github.com/action-state-group/checkpointed-local-log/go/store/sqlite"
	"github.com/action-state-group/checkpointed-local-log/go/witness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rc16Deals are the fixture store's deals (testdata/deal/rc16-store/make.sh):
// one closed before the first tick, one open in both ticks, one opened
// between them.
type rc16Deals struct {
	Closed, Open, Later string
}

// rc16Store copies the deal store capsulectl v0.1.0-rc16 wrote into a fresh
// profile named deal, and returns the profile, its deals and the witness's
// key. Its two ticks' deliveries are pending (the witness was unreachable).
func rc16Store(t *testing.T) (cli.Profile, rc16Deals, ed25519.PrivateKey) {
	t.Helper()
	return rc16StoreFrom(t, filepath.Join("store", "deal.db"))
}

// rc16StoreFrom is rc16Store with the fixture's deal.db replaced by db, a
// path in the fixture directory written from the same store.
func rc16StoreFrom(t *testing.T, db string) (cli.Profile, rc16Deals, ed25519.PrivateKey) {
	t.Helper()
	home := t.TempDir()
	dealTestEnv(t, home)
	src := filepath.Join("testdata", "deal", "rc16-store")
	store := filepath.Join(t.TempDir(), "store")
	require.NoError(t, os.MkdirAll(store, 0o700))
	for name, from := range map[string]string{"deal.db": db, "signing.seed": filepath.Join("store", "signing.seed"), "checkpoint.seed": filepath.Join("store", "checkpoint.seed")} {
		b, err := os.ReadFile(filepath.Join(src, from))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(store, name), b, 0o600))
	}
	yaml, err := os.ReadFile(filepath.Join(src, "deal.yaml"))
	require.NoError(t, err)
	profiles := filepath.Join(home, "capsule", "profiles")
	require.NoError(t, os.MkdirAll(profiles, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(profiles, "deal.yaml"), []byte(strings.ReplaceAll(string(yaml), "@STORE@", store)), 0o600))
	p, err := cli.LoadProfile("deal")
	require.NoError(t, err)
	var deals rc16Deals
	raw, err := os.ReadFile(filepath.Join(src, "deals.json"))
	require.NoError(t, err)
	var ids map[string]string
	require.NoError(t, json.Unmarshal(raw, &ids))
	deals = rc16Deals{Closed: ids["closed"], Open: ids["open"], Later: ids["later"]}
	// A checkout keeps no file mode: the seed is copied owner-only, as a
	// secret file must be.
	seed, err := os.ReadFile(filepath.Join(src, "witness.seed"))
	require.NoError(t, err)
	seedPath := filepath.Join(t.TempDir(), "witness.seed")
	require.NoError(t, os.WriteFile(seedPath, seed, 0o600))
	key, err := cli.PrivateKey(cli.Secret{File: seedPath})
	require.NoError(t, err)
	return p, deals, key
}

func storeCount(t *testing.T, p cli.Profile, query string, args ...any) int {
	t.Helper()
	db, _, err := cli.SqliteConnection(p)
	require.NoError(t, err)
	defer db.Close()
	var n int
	require.NoError(t, db.QueryRow(query, args...).Scan(&n))
	return n
}

func liveLogs(t *testing.T, p cli.Profile) []string {
	t.Helper()
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	c, _, err := s.cadenceOf(t.Context())
	require.NoError(t, err)
	live, err := c.Live(t.Context())
	require.NoError(t, err)
	return live
}

// A store a released build wrote moves onto its shelf on the first open
// (note §3.2): its ticks continue (numbering, due time, a receipt delivered
// before the upgrade), every deal counts into H, the closed and witnessed
// deal leaves the tick, the next tick keeps H+1 occupancy, and the
// released build's bundle and a new one both verify.
func TestARC16StoreMovesOntoItsShelf(t *testing.T) {
	p, deals, witnessKey := rc16Store(t)
	// The released build's bundle verifies under this build.
	out, err := invoke(t, "", "verify", "--bundle", filepath.Join("testdata", "deal", "rc16-store", "bundle-a.json"))
	require.NoError(t, err, out)
	// The witness answered tick 1 before the upgrade (a released build's
	// delivery); the new build finds the receipt when it takes over.
	storeReceipt(t, p, 1, witnessKey)

	// The first deal command migrates.
	tick := dealRun(t, "tick")
	assert.Equal(t, "not_due", tick["state"], "the released build's due time is honored")
	assert.Equal(t, "2026-10-19T09:27:52Z", tick["due"])

	assert.Equal(t, 2, storeCount(t, p, `SELECT COUNT(*) FROM cadence_ticks`), "both released ticks, renumbered 1 and 2")
	assert.Equal(t, 1, storeCount(t, p, `SELECT COUNT(*) FROM cadence_ticks WHERE tick = 1 AND witnessed = 1`))
	assert.Equal(t, 3, storeCount(t, p, `SELECT COUNT(*) FROM cadence_logs`), "H counts every deal the released build held")
	assert.Equal(t, 1, storeCount(t, p, `SELECT COUNT(*) FROM deal_shelf`))
	assert.Equal(t, 10, storeCount(t, p, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name LIKE 'shelf_mutations_%'`))
	// The closed deal's own rc16 bundle sealed a report on its log after
	// tick 1 witnessed it: it grew past its witnessed checkpoint, so it is
	// live by the #36 rule, like the two open deals.
	assert.Len(t, liveLogs(t, p), 3)

	// The next tick, on time, with every log live: no derived filler.
	cli.DealClock = func() time.Time { return time.Date(2026, 10, 19, 9, 28, 0, 0, time.UTC) }
	third := dealRun(t, "tick")
	require.Equal(t, "ticked", third["state"])
	assert.EqualValues(t, 3, third["tick"])
	assert.Len(t, tickLeaves(t, p, 3).Deals, 3)
	// Its receipt arrives: the closed deal's final checkpoint is witnessed,
	// and it leaves the tick.
	deliver(t, p, uint64(third["checkpoint"].(float64)), witnessKey)
	assert.ElementsMatch(t, []string{cli.DealLogID(deals.Open), cli.DealLogID(deals.Later)}, liveLogs(t, p))
	cli.DealClock = func() time.Time { return time.Date(2026, 10, 19, 9, 29, 0, 0, time.UTC) }
	fourth := dealRun(t, "tick")
	require.Equal(t, "ticked", fourth["state"])
	leaves := tickLeaves(t, p, 4)
	assert.Len(t, leaves.Deals, 2)
	assert.EqualValues(t, 1, leaves.FillerCount, "H+1 = 4: two live leaves, the filler, one derived filler")

	// The closed deal's own bundle now: witnessed (its report adds to what
	// tick 3 holds, so in part), and verified.
	path := filepath.Join(t.TempDir(), "closed.json")
	dealRun(t, "report", "--deal", deals.Closed, "--bundle", path)
	out, err = invoke(t, "", "verify", "--bundle", path)
	require.NoError(t, err, out)
	var b map[string]any
	require.NoError(t, json.Unmarshal(mustRead(t, path), &b))
	chain := b["extensions"].(map[string]any)["x-cadence-witness/v0"].(map[string]any)
	assert.Equal(t, "witnessed", chain["state"])

	// Later evidence on the closed deal brings it back into the tick.
	dealRun(t, "note", "--deal", deals.Closed, "--kind", "evidence", "--email", filepath.Join(merchantFixture, "confirmation.eml"))
	assert.Contains(t, liveLogs(t, p), cli.DealLogID(deals.Closed))
}

// A released build run on a migrated store is seen on the next open (the
// mutation counter its writes move) and repaired: an ended deal it
// appended to is live again (note §3.3).
func TestAReleasedBuildsWriteIsRepaired(t *testing.T) {
	p, deals, witnessKey := rc16Store(t)
	// Migrated, then a witnessed tick holds the closed deal's final
	// checkpoint: it leaves the tick.
	cli.DealClock = func() time.Time { return time.Date(2026, 10, 19, 9, 28, 0, 0, time.UTC) }
	third := dealRun(t, "tick")
	require.Equal(t, "ticked", third["state"])
	deliver(t, p, uint64(third["checkpoint"].(float64)), witnessKey)
	require.NotContains(t, liveLogs(t, p), cli.DealLogID(deals.Closed))
	// A released build appends a report to the closed deal: straight to its
	// log, past the shelf.
	log, err := cllsqlite.Open(p.Connection.Database, cli.DealLogID(deals.Closed))
	require.NoError(t, err)
	_, err = log.Append(t.Context(), cll.AppendInput{Value: []byte(strings.Repeat("r", 32)), AppendedAt: time.Now().UTC()})
	require.NoError(t, err)
	require.NoError(t, log.Close())
	assert.Contains(t, liveLogs(t, p), cli.DealLogID(deals.Closed), "the repair on open put the grown deal back")
}

// A receipt the cadence log holds but the cadence was never told of (a
// released build's delivery, or this build's, stopped before it reported)
// is a write the next open did not see: its repair reports it, with no
// tick due and no delivery made.
func TestAReceiptStoredOutsideTheCadenceIsReportedOnTheNextOpen(t *testing.T) {
	p, _, witnessKey := rc16Store(t)
	cli.DealClock = func() time.Time { return time.Date(2026, 10, 19, 9, 28, 0, 0, time.UTC) }
	third := dealRun(t, "tick")
	require.Equal(t, "ticked", third["state"])
	storeReceipt(t, p, uint64(third["checkpoint"].(float64)), witnessKey)
	require.Equal(t, 0, storeCount(t, p, `SELECT COUNT(*) FROM cadence_ticks WHERE tick = 3 AND witnessed = 1`))
	again := dealRun(t, "tick")
	require.Equal(t, "not_due", again["state"])
	assert.Equal(t, 1, storeCount(t, p, `SELECT COUNT(*) FROM cadence_ticks WHERE tick = 3 AND witnessed = 1`))
	seen, counter := shelfSeen(t, p)
	assert.Equal(t, counter, seen)
}

// A delivery that stores its receipt and then fails (here the read after the
// commit is cancelled) leaves the session unmigrated: close does not record
// the store as seen, and the next open's repair reports the receipt.
func TestADeliveryThatFailsAfterStoringItsReceiptIsReportedOnTheNextOpen(t *testing.T) {
	p, _, witnessKey := rc16Store(t)
	cli.DealClock = func() time.Time { return time.Date(2026, 10, 19, 9, 27, 0, 0, time.UTC) }
	require.Equal(t, "not_due", dealRun(t, "tick")["state"], "migrated, no tick cut")
	cli.DealClock = func() time.Time { return time.Date(2026, 10, 19, 9, 28, 0, 0, time.UTC) }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var stored uint64
	old := dealDeliverWitness
	t.Cleanup(func() { dealDeliverWitness = old })
	dealDeliverWitness = func(_ context.Context, p cli.Profile, store cll.WitnessStateStore, service string, size uint64) (cll.WitnessState, error) {
		state, err := store.GetWitness(t.Context(), service, size)
		require.NoError(t, err)
		record, err := checkpoint.ParseRecord(state.Checkpoint)
		require.NoError(t, err)
		entry, err := record.EntryHash()
		require.NoError(t, err)
		next := state
		next.Receipt = &cll.WitnessReceiptState{Bytes: mintReceipt(t, entry, witnessKey), EntryHash: hex.EncodeToString(entry), EntryHashScheme: witness.EntryHashSchemeCheckpointDigest, LeafIndex: ptr(int64(0)), TreeSize: ptr(int64(1))}
		require.NoError(t, store.CommitWitness(t.Context(), state.Attempts, next))
		stored = size
		cancel()
		return cll.WitnessState{}, ctx.Err()
	}
	s, err := openDealSession(ctx, p)
	require.NoError(t, err)
	_, err = s.tick(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, s.close())
	seen, counter := shelfSeen(t, p)
	require.Less(t, seen, counter, "the stored receipt is a write close did not mark seen")
	require.NotZero(t, stored)
	require.Equal(t, 1, storeCount(t, p, `SELECT COUNT(*) FROM cadence_ticks WHERE checkpoint_size = ?`, stored))
	require.Equal(t, 0, storeCount(t, p, `SELECT COUNT(*) FROM cadence_ticks WHERE witnessed = 1`))

	dealDeliverWitness = old
	again := dealRun(t, "tick")
	require.Equal(t, "not_due", again["state"])
	assert.Equal(t, 1, storeCount(t, p, `SELECT COUNT(*) FROM cadence_ticks WHERE checkpoint_size = ? AND witnessed = 1`, stored), "the next open reported it")
}

// A read of a store no build with the shelf has opened is refused before
// the shelf is opened: the store's files are left exactly as they were.
func TestAReadOfAnUnmigratedStoreLeavesItAsItWas(t *testing.T) {
	p, _, _ := rc16Store(t)
	dir := filepath.Dir(p.Connection.Database)
	snapshot := func() map[string]string {
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		out := map[string]string{}
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			require.NoError(t, err)
			out[e.Name()] = hex.EncodeToString(b)
		}
		return out
	}
	before := snapshot()
	_, err := invoke(t, "", "--profile", "deal", "deal", "list")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not moved onto its shelf")
	assert.Equal(t, before, snapshot())
}

// A released build's `deal tick` on a migrated store writes a deal_cadence
// row and its cadence-log tick past the shelf (testdata make-downgrade.sh:
// this build migrated the rc16 store and cut tick 3, then rc16 cut its own
// tick at cadence checkpoint 7). The next open imports it after the
// cadence's own ticks, and the cadence continues from it (note §3.3).
func TestAReleasedBuildsTickIsImportedOnTheNextOpen(t *testing.T) {
	p, _, _ := rc16StoreFrom(t, "downgrade.db")
	seen, counter := shelfSeen(t, p)
	require.Less(t, seen, counter, "rc16's tick moved the counter")
	require.Equal(t, 3, storeCount(t, p, `SELECT COUNT(*) FROM cadence_ticks`))
	// rc16 drew its due time from the wall clock when make-downgrade.sh ran:
	// read it from the fixture rather than pin it.
	db, _, err := cli.SqliteConnection(p)
	require.NoError(t, err)
	var rawDue string
	require.NoError(t, db.QueryRow(`SELECT due_next FROM deal_cadence ORDER BY entry_seq DESC LIMIT 1`).Scan(&rawDue))
	require.NoError(t, db.Close())
	due, err := time.Parse(time.RFC3339, rawDue)
	require.NoError(t, err)
	cli.DealClock = func() time.Time { return due.Add(-time.Second) }
	tick := dealRun(t, "tick")
	assert.Equal(t, "not_due", tick["state"], "rc16's tick set the due time")
	assert.Equal(t, rawDue, tick["due"])
	assert.Equal(t, 1, storeCount(t, p, `SELECT COUNT(*) FROM cadence_ticks WHERE tick = 4 AND entry_seq = 4 AND checkpoint_size = 7`), "rc16's tick, numbered after the cadence's own")
	seen, counter = shelfSeen(t, p)
	assert.Equal(t, counter, seen)
	cli.DealClock = func() time.Time { return due.Add(time.Second) }
	next := dealRun(t, "tick")
	require.Equal(t, "ticked", next["state"])
	assert.EqualValues(t, 5, next["tick"])
}

// The mutation counter fails closed: with its row gone, a released build's
// write is refused by the trigger and the shelf refuses to open.
func TestAMissingMutationCounterFailsClosed(t *testing.T) {
	p, deals, _ := rc16Store(t)
	dealRun(t, "tick")
	db, _, err := cli.SqliteConnection(p)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM shelf_mutations`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	log, err := cllsqlite.Open(p.Connection.Database, cli.DealLogID(deals.Open))
	require.NoError(t, err)
	_, err = log.Append(t.Context(), cll.AppendInput{Value: []byte(strings.Repeat("s", 32)), AppendedAt: time.Now().UTC()})
	assert.ErrorContains(t, err, "no mutation counter")
	require.NoError(t, log.Close())
	_, err = invoke(t, "", "--profile", "deal", "deal", "tick")
	assert.ErrorContains(t, err, "restore it from a backup")
}

type storedTickLeaves struct {
	Deals       map[string]json.RawMessage `json:"deals"`
	FillerCount uint64                     `json:"filler_count"`
}

func tickLeaves(t *testing.T, p cli.Profile, n int) storedTickLeaves {
	t.Helper()
	db, _, err := cli.SqliteConnection(p)
	require.NoError(t, err)
	defer db.Close()
	var raw string
	require.NoError(t, db.QueryRow(`SELECT leaves FROM cadence_ticks WHERE tick = ?`, n).Scan(&raw))
	var out storedTickLeaves
	require.NoError(t, json.Unmarshal([]byte(raw), &out))
	return out
}

// A repair that fails part way (a crash, a cancellation) leaves the
// released build's write unseen-as-handled: the next open runs it again.
// It stops before any repair ("start") and after the import has already
// restored the live set ("imported"); either way seen stays behind the
// counter until a repair completes.
func TestAFailedRepairRunsAgain(t *testing.T) {
	for _, stage := range []string{"start", "imported"} {
		t.Run(stage, func(t *testing.T) {
			p, deals, witnessKey := rc16Store(t)
			cli.DealClock = func() time.Time { return time.Date(2026, 10, 19, 9, 28, 0, 0, time.UTC) }
			third := dealRun(t, "tick")
			deliver(t, p, uint64(third["checkpoint"].(float64)), witnessKey)
			require.NotContains(t, liveLogs(t, p), cli.DealLogID(deals.Closed))
			log, err := cllsqlite.Open(p.Connection.Database, cli.DealLogID(deals.Closed))
			require.NoError(t, err)
			_, err = log.Append(t.Context(), cll.AppendInput{Value: []byte(strings.Repeat("r", 32)), AppendedAt: time.Now().UTC()})
			require.NoError(t, err)
			require.NoError(t, log.Close())
			seen, counter := shelfSeen(t, p)
			require.Less(t, seen, counter, "the released build's append is counted and not yet seen")

			old := dealMigrateStopsAt
			t.Cleanup(func() { dealMigrateStopsAt = old })
			dealMigrateStopsAt = func(at string) error {
				if at == stage {
					return assert.AnError
				}
				return nil
			}
			_, err = invoke(t, "", "--profile", "deal", "deal", "tick")
			require.ErrorIs(t, err, assert.AnError)
			afterSeen, afterCounter := shelfSeen(t, p)
			assert.Equal(t, seen, afterSeen, "a failed repair records nothing as seen")
			assert.Less(t, afterSeen, afterCounter)

			ran := false
			dealMigrateStopsAt = func(at string) error {
				ran = ran || at == "imported"
				return nil
			}
			dealRun(t, "tick")
			assert.True(t, ran, "the next open repaired again")
			assert.Contains(t, liveLogs(t, p), cli.DealLogID(deals.Closed))
			finalSeen, finalCounter := shelfSeen(t, p)
			assert.Equal(t, finalCounter, finalSeen)
		})
	}
}

// shelfSeen is the deal_shelf marker's seen and the mutation counter.
func shelfSeen(t *testing.T, p cli.Profile) (seen, counter int64) {
	t.Helper()
	db, _, err := cli.SqliteConnection(p)
	require.NoError(t, err)
	defer db.Close()
	require.NoError(t, db.QueryRow(`SELECT (SELECT seen FROM deal_shelf WHERE id = 1), (SELECT n FROM shelf_mutations WHERE id = 1)`).Scan(&seen, &counter))
	return seen, counter
}

// Witness standing is read for the witness the profile names now: a
// changed witness key leaves a deal unwitnessed, as it always did, and a
// report sealed after a witnessed tick waits for the next tick (scheduled),
// never "pending" on a delivery that completed.
func TestWitnessStandingFollowsTheProfilesWitness(t *testing.T) {
	p, deals, witnessKey := rc16Store(t)
	cli.DealClock = func() time.Time { return time.Date(2026, 10, 19, 9, 28, 0, 0, time.UTC) }
	third := dealRun(t, "tick")
	deliver(t, p, uint64(third["checkpoint"].(float64)), witnessKey)
	witness := checkpointStatusOf(t, deals.Open)["witness"].(map[string]any)
	require.Equal(t, "witnessed", witness["state"])
	require.Equal(t, "all", witness["extent"])

	// Only a report since the witnessed tick: witnessed in part, the rest
	// scheduled.
	dealRun(t, "report", "--deal", deals.Open, "--bundle", filepath.Join(t.TempDir(), "open.json"))
	witness = checkpointStatusOf(t, deals.Open)["witness"].(map[string]any)
	assert.Equal(t, "witnessed", witness["state"])
	assert.Equal(t, "part", witness["extent"])
	assert.Equal(t, "scheduled", witness["current"].(map[string]any)["state"], "the report waits for the next tick")

	// Another witness key: the stored receipt no longer counts.
	other, otherKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	p.Checkpoint.PublicKey = hex.EncodeToString(other)
	require.NoError(t, cli.SaveProfile(p, true))
	witness = checkpointStatusOf(t, deals.Open)["witness"].(map[string]any)
	assert.NotEqual(t, "witnessed", witness["state"])
	require.NotEqual(t, "witnessed", checkpointStatusOf(t, deals.Later)["witness"].(map[string]any)["state"])

	// The new witness signs the next tick. The later deal has not grown, so
	// that tick carries the very checkpoint the old witness's tick did, and
	// its standing comes back from the new witness's tick.
	cli.DealClock = func() time.Time { return time.Date(2026, 10, 19, 9, 40, 0, 0, time.UTC) }
	fourth := dealRun(t, "tick")
	deliver(t, p, uint64(fourth["checkpoint"].(float64)), otherKey)
	witness = checkpointStatusOf(t, deals.Later)["witness"].(map[string]any)
	assert.Equal(t, "witnessed", witness["state"])
	assert.Equal(t, "all", witness["extent"])
	path := filepath.Join(t.TempDir(), "later.json")
	dealRun(t, "report", "--deal", deals.Later, "--bundle", path)
	result, err := verifyWithDirectory(t, path, writeDirectory(t, rawKeyRow(p.Checkpoint.Endpoint, other)))
	require.NoError(t, err)
	status, _ := bundleWitnesses(result)
	assert.Equal(t, "pass", status, "the bundle's chain runs through the new witness's tick")
}

// A report bundle a released build wrote still reads as this deal's own
// (#353). The deal plugin reads the checkpoint from cose, which every deal
// bundle has carried since the first release; the rc16 bundle also has the
// legacy statement copy, which new bundles drop.
func TestARC16ReportBundleStillReads(t *testing.T) {
	_, deals, _ := rc16Store(t)
	path := filepath.Join("testdata", "deal", "rc16-store", "bundle-a.json")
	cp := readBundleFile(t, path)["checkpoint"].(map[string]interface{})
	require.Contains(t, cp, "cose")
	require.Contains(t, cp, "statement")
	out := dealRun(t, "report", "--deal", deals.Closed, "--from-bundle", path)
	assert.NotEmpty(t, out["assurance"])
}
