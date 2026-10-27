package cli

import (
	"database/sql"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type heldStore bool

func (h heldStore) HoldsPluginStore() bool { return bool(h) }

// sqliteStoreProfile is a plain sqlite profile (the core's, no plugin named)
// whose database the test then marks.
func sqliteStoreProfile(t *testing.T) Profile {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := profileFixture(t)
	p.Type, p.Log = "sqlite", ""
	p.Namespace = "capsule"
	p.Connection.Database = filepath.Join(t.TempDir(), "store.db")
	require.NoError(t, SaveProfile(p, false))
	_, err := invoke(t, "", "store", "init", "--profile", p.Name)
	require.NoError(t, err)
	return p
}

func execSQL(t *testing.T, p Profile, statement string) {
	t.Helper()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: p.Connection.Database}).String())
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(statement)
	require.NoError(t, err)
}

// A store carrying the owner marker is written by its owner only, whatever
// the profile says: a profile that declares the core's log over it is refused.
func TestACoreProfileCannotWriteAMarkedStore(t *testing.T) {
	p := sqliteStoreProfile(t)
	for _, tc := range []struct{ name, statement string }{
		{"the neutral marker", `CREATE TABLE plugin_owned (owner TEXT NOT NULL, version INTEGER NOT NULL); INSERT INTO plugin_owned VALUES ('ledger', 1)`},
		{"the older deal_shelf table, which records no owner", `CREATE TABLE deal_shelf (id INTEGER PRIMARY KEY)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			execSQL(t, p, tc.statement)
			defer execSQL(t, p, `DROP TABLE IF EXISTS plugin_owned; DROP TABLE IF EXISTS deal_shelf`)
			for _, use := range []TargetUse{UseCLL, UsePublication, UseInitialization} {
				_, err := OpenTarget(t.Context(), p, use)
				require.ErrorIs(t, err, ErrPluginStoreWrite, "use %d", use)
			}
			tg, err := OpenTarget(t.Context(), p, UseCLLRead)
			require.NoError(t, err, "a read opens")
			require.NoError(t, tg.Close())
		})
	}
}

// The owner writes through its handle while it holds the lock, and marks the
// store; a store marked for another owner is not taken over.
func TestTheOwnerMarksAndWritesItsStore(t *testing.T) {
	p := sqliteStoreProfile(t)
	for _, w := range []PluginStoreWriter{nil, heldStore(false)} {
		_, err := OpenOwnedTarget(t.Context(), p, UseInitialization, w)
		require.ErrorIs(t, err, ErrPluginStoreWrite)
	}
	tg, err := OpenOwnedTarget(t.Context(), p, UseInitialization, heldStore(true))
	require.NoError(t, err)
	require.ErrorIs(t, tg.MarkOwned(t.Context(), "ledger", heldStore(false)), ErrPluginStoreWrite, "no lock, no marker")
	require.ErrorIs(t, tg.MarkOwned(t.Context(), LogCLL, heldStore(true)), ErrInput, "cll owns no plugin store")
	require.NoError(t, tg.MarkOwned(t.Context(), "ledger", heldStore(true)))
	require.NoError(t, tg.MarkOwned(t.Context(), "ledger", heldStore(true)), "marking twice is fine")
	err = tg.MarkOwned(t.Context(), "other", heldStore(true))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "marked as owned by the ledger plugin")
	require.NoError(t, tg.Close())

	m, err := storeOwnerMarker(t.Context(), p)
	require.NoError(t, err)
	assert.Equal(t, ownerMarker{found: true, owner: "ledger"}, m)
	_, err = OpenTarget(t.Context(), p, UseCLL)
	require.ErrorIs(t, err, ErrPluginStoreWrite, "the disguised profile is refused after marking")
}

func TestOpenDealTargetStillWorksForTheOldWriter(t *testing.T) {
	p := sqliteStoreProfile(t)
	_, err := OpenDealTarget(t.Context(), p, UseInitialization, nil)
	require.ErrorIs(t, err, ErrDealProfileWrite)
	_, err = OpenDealTarget(t.Context(), p, UseInitialization, heldDealStore(false))
	require.ErrorIs(t, err, ErrPluginStoreWrite)
	tg, err := OpenDealTarget(t.Context(), p, UseInitialization, heldDealStore(true))
	require.NoError(t, err)
	require.NoError(t, tg.Close())
}

// profile update cannot disguise a marked store: a log owner that disagrees
// with the recorded one, or cll over an owner-less legacy marker, is refused
// before anything is saved.
func TestProfileUpdateCannotDisguiseAStore(t *testing.T) {
	p := sqliteStoreProfile(t)
	execSQL(t, p, `CREATE TABLE plugin_owned (owner TEXT NOT NULL, version INTEGER NOT NULL); INSERT INTO plugin_owned VALUES ('ledger', 1)`)
	before, err := LoadProfile(p.Name)
	require.NoError(t, err)

	_, err = invoke(t, "", "profile", "update", "--profile", p.Name, "--log", LogCLL)
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "records its owner as the ledger plugin")
	_, err = invoke(t, "", "profile", "update", "--profile", p.Name, "--log", "other")
	require.ErrorIs(t, err, ErrInput)
	after, err := LoadProfile(p.Name)
	require.NoError(t, err)
	assert.Equal(t, before, after, "the profile file is unchanged")

	// The recorded owner is accepted on a profile that already names it.
	owned := p
	owned.Log = "ledger"
	require.NoError(t, SaveProfile(owned, true))
	_, err = invoke(t, "", "profile", "update", "--profile", p.Name, "--log", "ledger")
	require.NoError(t, err, "the recorded owner is accepted")

	legacy := sqliteStoreProfile(t)
	execSQL(t, legacy, `CREATE TABLE deal_shelf (id INTEGER PRIMARY KEY)`)
	_, err = invoke(t, "", "profile", "update", "--profile", legacy.Name, "--log", LogCLL)
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "owned by a plugin (legacy marker)")
	assert.NotContains(t, SafeError(err), "deal", "the message names no plugin")
}
