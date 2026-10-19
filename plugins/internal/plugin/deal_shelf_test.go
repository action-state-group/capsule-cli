package plugin

import (
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	cllsqlite "github.com/action-state-group/checkpointed-local-log/go/store/sqlite"
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
	home := t.TempDir()
	dealTestEnv(t, home)
	src := filepath.Join("testdata", "deal", "rc16-store")
	store := filepath.Join(t.TempDir(), "store")
	require.NoError(t, os.MkdirAll(store, 0o700))
	for _, name := range []string{"deal.db", "signing.seed", "checkpoint.seed"} {
		b, err := os.ReadFile(filepath.Join(src, "store", name))
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
	key, err := cli.PrivateKey(cli.Secret{File: filepath.Join(src, "witness.seed")})
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
// mutation counter its writes move) and reconciled: an ended deal it
// appended to is live again (note §3.3).
func TestAReleasedBuildsWriteIsReconciled(t *testing.T) {
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
	assert.Contains(t, liveLogs(t, p), cli.DealLogID(deals.Closed), "the reconcile on open put the grown deal back")
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
