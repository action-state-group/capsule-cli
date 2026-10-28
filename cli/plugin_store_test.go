package cli

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type heldStore bool

func (h heldStore) HoldsPluginStore() bool { return bool(h) }
func (heldStore) PluginStoreOwner() string { return "ledger" }

// otherOwner holds the lock for a plugin that is not the store's owner.
type otherOwner struct{}

func (otherOwner) HoldsPluginStore() bool   { return true }
func (otherOwner) PluginStoreOwner() string { return "other" }

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

// A plugin pointed at a store another plugin owns is refused before it
// initializes anything in it.
func TestAnotherOwnerIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	p := sqliteStoreProfile(t)
	tg, err := OpenOwnedTarget(t.Context(), p, UseInitialization, heldStore(true))
	require.NoError(t, err)
	require.NoError(t, tg.MarkOwned(t.Context(), "ledger", heldStore(true)))
	require.NoError(t, tg.Close())
	before, err := os.ReadFile(p.Connection.Database)
	require.NoError(t, err)

	q := p
	q.Namespace, q.LogID = "elsewhere", "another-log"
	_, err = OpenOwnedTarget(t.Context(), q, UseInitialization, otherOwner{})
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "marked as owned by the ledger plugin, not other")
	after, err := os.ReadFile(p.Connection.Database)
	require.NoError(t, err)
	assert.Equal(t, before, after, "the database is untouched")
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

	// Pointing a core-owned profile at the marked store is refused.
	marked := p.Connection.Database
	other := p
	other.Name = "elsewhere"
	other.Connection.Database = filepath.Join(t.TempDir(), "other.db")
	require.NoError(t, SaveProfile(other, false))
	_, err = invoke(t, "", "profile", "update", "--profile", other.Name, "--sqlite-path", marked)
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "records its owner as the ledger plugin")
	_, err = invoke(t, "", "profile", "update", "--profile", p.Name, "--log", "other")
	require.ErrorIs(t, err, ErrInput)
	after, err := LoadProfile(p.Name)
	require.NoError(t, err)
	assert.Equal(t, before, after, "the profile file is unchanged")

	// The profile can be brought in line with the owner the store records,
	// though a profile's log owner is otherwise fixed once set.
	_, err = invoke(t, "", "profile", "update", "--profile", p.Name, "--log", "ledger")
	require.NoError(t, err, "the recorded owner is accepted")

	// A profile that would sit over an owner-less legacy marker as the core's
	// is refused when it is created or its owner or file changes.
	legacy := sqliteStoreProfile(t)
	execSQL(t, legacy, `CREATE TABLE deal_shelf (id INTEGER PRIMARY KEY)`)
	err = checkOwnerAgainstStore(t.Context(), legacy)
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "owned by a plugin (legacy marker)")
	assert.NotContains(t, SafeError(err), "deal", "the message names no plugin")
	legacy.Log = "ledger"
	require.NoError(t, checkOwnerAgainstStore(t.Context(), legacy), "a plugin-owned profile is fine over it")
	// An update that changes neither reads no database.
	plain := sqliteStoreProfile(t)
	execSQL(t, plain, `CREATE TABLE plugin_owned (owner TEXT NOT NULL, version INTEGER NOT NULL); INSERT INTO plugin_owned VALUES ('ledger', 1)`)
	_, err = invoke(t, "", "profile", "update", "--profile", plain.Name, "--checkpoint-endpoint", "https://witness.example/v1", "--checkpoint-public-key", strings.Repeat("ab", 32))
	require.NoError(t, err, "no owner or file change: the store is not read")

	// Switching a profile's type onto a marked file is a change too.
	mysqlish := plain
	mysqlish.Name, mysqlish.Type = "was-mysql", "mysql"
	mysqlish.Connection.Host, mysqlish.Connection.TLS = "localhost", "false"
	require.NoError(t, SaveProfile(mysqlish, false))
	_, err = invoke(t, "", "profile", "update", "--profile", mysqlish.Name, "--type", "sqlite")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "records its owner as the ledger plugin")
}
