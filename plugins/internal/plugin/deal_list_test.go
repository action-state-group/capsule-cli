package plugin

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dealListOf runs `deal list` and checks it changed no byte of the store.
func dealListOf(t *testing.T, args ...string) string {
	t.Helper()
	p, err := cli.LoadProfile("deal")
	require.NoError(t, err)
	before := storeFiles(t, p)
	out, err := invoke(t, "", append([]string{"--profile", "deal", "deal", "list"}, args...)...)
	require.NoError(t, err, out)
	assert.Equal(t, before, storeFiles(t, p), "the list changes no file under the store")
	return out
}

// stickerStore is the retail-checkout demo store (the cat sticker): one
// sticker deal checked, paid and closed, one opened and nothing more, and a
// sale with one buyer thread.
func stickerStore(t *testing.T) (closed, open, sale, thread string) {
	t.Helper()
	dealFixture(t)
	// A minute per step, so a deal's first and last step times differ.
	start, steps := cli.DealClock(), 0
	cli.DealClock = func() time.Time { steps++; return start.Add(time.Duration(steps) * time.Minute) }
	closed = retailDeal(t)
	dealRun(t, "close", "--deal", closed, "--input", writeJSON(t, `{"status":"received","delivered":{"item":"cat sticker"}}`))
	open = dealRun(t, "open", "--input", filepath.Join(retailDemo, "open.json"))["deal_id"].(string)
	sale, _ = newSale(t)
	thread = buyerThread(t, sale, "buyer.example")
	return closed, open, sale, thread
}

// What the store holds, read straight from its file with the plugin's own
// query (`SELECT DISTINCT deal_id FROM deal_steps`), and each deal's first
// and last step time and latest payee fingerprint from the same rows.
type storeDeal struct{ id, first, last, payee string }

func storeDeals(t *testing.T) []storeDeal {
	t.Helper()
	p, err := cli.LoadProfile("deal")
	require.NoError(t, err)
	db, err := sql.Open("sqlite", "file:"+p.Connection.Database+"?mode=ro")
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	ids, err := db.Query(`SELECT DISTINCT deal_id FROM deal_steps ORDER BY deal_id`)
	require.NoError(t, err)
	var out []storeDeal
	for ids.Next() {
		var d storeDeal
		require.NoError(t, ids.Scan(&d.id))
		out = append(out, d)
	}
	require.NoError(t, ids.Err())
	require.NoError(t, ids.Close())
	for i := range out {
		d := &out[i]
		require.NoError(t, db.QueryRow(`SELECT json_extract(local, '$.at') FROM deal_steps WHERE deal_id=? ORDER BY n LIMIT 1`, d.id).Scan(&d.first))
		require.NoError(t, db.QueryRow(`SELECT json_extract(local, '$.at') FROM deal_steps WHERE deal_id=? ORDER BY n DESC LIMIT 1`, d.id).Scan(&d.last))
		err := db.QueryRow(`SELECT json_extract(local, '$.counterparty_profile.payee') FROM deal_steps WHERE deal_id=? AND kind='counterparty_profile' ORDER BY n DESC LIMIT 1`, d.id).Scan(&d.payee)
		if !errors.Is(err, sql.ErrNoRows) {
			require.NoError(t, err)
		}
	}
	return out
}

// On the sticker store, `deal list --json` returns every id the plugin's
// SQLite path returned, in its order, with the same first and last step
// times, and those times are the sealed records' own (`deal export`).
func TestDealListMatchesTheStore(t *testing.T) {
	legacyDeals(t)
	closed, open, sale, thread := stickerStore(t)
	raw := dealListOf(t, "--json")
	var list []map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &list), raw)
	keys := objectKeys(t, raw)
	want := storeDeals(t)
	require.Len(t, want, 4)
	require.Len(t, list, len(want), "one entry per deal id in the store")
	for i, d := range want {
		l := list[i]
		wantKeys := []string{"deal_id", "kind", "state", "opened_at", "last_record_at", "counterparty_fingerprint", "log_id", "result", "summary"}
		if l["kind"] == "sale" {
			wantKeys = slices.Delete(wantKeys, 7, 8) // a sale's threads carry its results
		}
		assert.Equal(t, wantKeys, keys[i], "stable key order")
		assert.Equal(t, d.id, l["deal_id"])
		assert.Equal(t, d.first, l["opened_at"])
		assert.Equal(t, d.last, l["last_record_at"])
		assert.Equal(t, "deal/"+d.id, l["log_id"])
		if d.payee == "" {
			assert.Nil(t, l["counterparty_fingerprint"], d.id)
		} else {
			assert.Equal(t, d.payee, l["counterparty_fingerprint"], d.id)
		}
	}
	byID := map[string]map[string]any{}
	for _, l := range list {
		byID[l["deal_id"].(string)] = l
	}
	assert.NotEqual(t, byID[closed]["opened_at"], byID[closed]["last_record_at"], "the fixture's steps are a minute apart")
	assert.Equal(t, "closed", byID[closed]["state"])
	assert.Equal(t, "open", byID[open]["state"])
	assert.Equal(t, "deal", byID[closed]["kind"])
	assert.Equal(t, "sale", byID[sale]["kind"])
	assert.Equal(t, "open", byID[sale]["state"], "a sale never closes")
	assert.Equal(t, "deal", byID[thread]["kind"], "a sale's buyer thread is a deal")
	assert.Regexp(t, `^[0-9a-f]{64}$`, byID[closed]["counterparty_fingerprint"], "the closed deal's check sealed a counterparty_profile")
	assert.Nil(t, byID[open]["counterparty_fingerprint"], "the open deal has no check")
	assert.Equal(t, "bought", byID[closed]["result"])
	assert.Equal(t, "open", byID[open]["result"])
	assert.Equal(t, "open", byID[thread]["result"])

	for _, id := range []string{closed, open} {
		path := filepath.Join(t.TempDir(), "records.json")
		dealRun(t, "export", "--deal", id, "--output", path)
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		var records []map[string]any
		require.NoError(t, json.Unmarshal(raw, &records))
		require.NotEmpty(t, records)
		at := func(r map[string]any) any { return r["x-deal-v0"].(map[string]any)["at"] }
		assert.Equal(t, at(records[0]), byID[id]["opened_at"], "the first sealed record's time")
		assert.Equal(t, at(records[len(records)-1]), byID[id]["last_record_at"], "the last sealed record's time")
	}

	lines := strings.Split(strings.TrimRight(dealListOf(t), "\n"), "\n")
	require.Len(t, lines, len(list), "one line per deal")
	for i, line := range lines {
		l := list[i]
		fp, _ := l["counterparty_fingerprint"].(string)
		if fp == "" {
			fp = "-"
		}
		assert.Equal(t, strings.Join([]string{l["deal_id"].(string), l["kind"].(string), l["state"].(string), l["opened_at"].(string), l["last_record_at"].(string), fp, l["log_id"].(string)}, "\t"), line)
	}
}

// objectKeys is each object's keys, in the order the array's bytes carry them.
func objectKeys(t *testing.T, raw string) [][]string {
	t.Helper()
	var objects []json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(raw), &objects))
	var out [][]string
	for _, o := range objects {
		dec := json.NewDecoder(strings.NewReader(string(o)))
		_, err := dec.Token()
		require.NoError(t, err)
		var keys []string
		for dec.More() {
			k, err := dec.Token()
			require.NoError(t, err)
			keys = append(keys, k.(string))
			var v json.RawMessage
			require.NoError(t, dec.Decode(&v))
		}
		out = append(out, keys)
	}
	return out
}

// A store with no deal yet lists none: [] with --json, nothing without.
func TestDealListEmptyStore(t *testing.T) {
	dealFixture(t)
	assert.Equal(t, "[]\n", dealListOf(t, "--json"))
	assert.Equal(t, "", dealListOf(t))
}

// An index row that `load` would refuse is a conflict, never a silently
// different list, and the refusal writes nothing.
func TestDealListRefusesAnInconsistentIndex(t *testing.T) {
	legacyDeals(t)
	for name, tamper := range map[string]string{
		"a missing step":                     `DELETE FROM deal_steps WHERE deal_id=? AND n=1`,
		"a kind its record does not carry":   `UPDATE deal_steps SET kind='note' WHERE deal_id=? AND n=1`,
		"a broken digest chain":              `UPDATE deal_steps SET record_digest='00' WHERE deal_id=? AND n=1`,
		"a record with a field it never had": `UPDATE deal_steps SET local=json_set(local, '$.extra', 1) WHERE deal_id=? AND n=1`,
		"a record of another step number":    `UPDATE deal_steps SET local=json_set(local, '$.n', 7) WHERE deal_id=? AND n=1`,
	} {
		t.Run(name, func(t *testing.T) {
			dealFixture(t)
			id := retailDeal(t)
			p, err := cli.LoadProfile("deal")
			require.NoError(t, err)
			db, err := sql.Open("sqlite", p.Connection.Database)
			require.NoError(t, err)
			res, err := db.Exec(tamper, id)
			require.NoError(t, err)
			changed, err := res.RowsAffected()
			require.NoError(t, err)
			require.EqualValues(t, 1, changed)
			require.NoError(t, db.Close())
			before := storeFiles(t, p)
			_, err = invoke(t, "", "--profile", "deal", "deal", "list", "--json")
			require.ErrorIs(t, err, cli.ErrConflict)
			assert.Equal(t, before, storeFiles(t, p), "the refusal changes no file under the store")
		})
	}
}

// The list reads a read_only profile's store the same as a writable one's.
func TestDealListOnAReadOnlyProfile(t *testing.T) {
	stickerStore(t)
	writable := dealListOf(t, "--json")
	p, err := cli.LoadProfile("deal")
	require.NoError(t, err)
	p.ReadOnly = true
	require.NoError(t, cli.SaveProfile(p, true))
	assert.Equal(t, writable, dealListOf(t, "--json"))
}
