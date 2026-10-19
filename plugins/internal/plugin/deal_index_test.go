package plugin

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestDealIndex(t *testing.T) sqliteDealIndex {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "deal.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	db.SetMaxOpenConns(1)
	for _, stmt := range dealIndexSchema {
		_, err = db.ExecContext(t.Context(), stmt)
		require.NoError(t, err)
	}
	return sqliteDealIndex{db}
}

func stepKeys(steps []indexedStep) []string {
	out := []string{}
	for _, st := range steps {
		out = append(out, st.DealID+"/"+st.Kind)
	}
	return out
}

// The index selects by deal, step, kind and capsule id, in deal then step
// order, or in the order steps were indexed; a prepared step has sequence 0
// until it is published, and a withdrawn one is gone.
func TestSQLiteDealIndexSteps(t *testing.T) {
	ctx := t.Context()
	x := newTestDealIndex(t)
	for _, st := range []indexedStep{
		{DealID: "deal-b", N: 1, Kind: "open", CapsuleID: "c-b1", Digest: "d-b1", Local: `{"n":1}`},
		{DealID: "deal-a", N: 1, Kind: "open", CapsuleID: "c-a1", Digest: "d-a1", Local: `{"n":1}`},
		{DealID: "deal-b", N: 2, Kind: "act", CapsuleID: "c-b2", Digest: "d-b2", Local: `{"n":2}`},
		{DealID: "deal-a", N: 2, Kind: "act", CapsuleID: "c-a2", Digest: "d-a2", Local: `{"n":2}`},
		{DealID: "deal-a", N: 3, Kind: "close", CapsuleID: "c-a3", Digest: "d-a3", Local: `{"n":3}`},
	} {
		require.NoError(t, x.prepareStep(ctx, st))
	}

	all, err := x.steps(ctx, stepFilter{})
	require.NoError(t, err)
	assert.Equal(t, []string{"deal-a/open", "deal-a/act", "deal-a/close", "deal-b/open", "deal-b/act"}, stepKeys(all))

	acts, err := x.steps(ctx, stepFilter{Kinds: []string{"act"}, Indexed: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"deal-b/act", "deal-a/act"}, stepKeys(acts), "indexed order, across deals")

	several, err := x.steps(ctx, stepFilter{Kinds: []string{"open", "close"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"deal-a/open", "deal-a/close", "deal-b/open"}, stepKeys(several))

	firsts, err := x.steps(ctx, stepFilter{N: 1, ExceptDeal: "deal-a"})
	require.NoError(t, err)
	assert.Equal(t, []string{"deal-b/open"}, stepKeys(firsts))

	byCapsule, err := x.steps(ctx, stepFilter{CapsuleID: "c-a2"})
	require.NoError(t, err)
	require.Len(t, byCapsule, 1)
	assert.Equal(t, indexedStep{DealID: "deal-a", N: 2, Kind: "act", CapsuleID: "c-a2", Digest: "d-a2", Local: `{"n":2}`}, byCapsule[0])

	none, err := x.steps(ctx, stepFilter{DealID: "deal-c"})
	require.NoError(t, err)
	assert.Empty(t, none)

	count, err := x.stepCount(ctx, "deal-a")
	require.NoError(t, err)
	assert.Equal(t, 3, count)
	ids, err := x.dealIDs(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"deal-a", "deal-b"}, ids)

	require.NoError(t, x.publishedStep(ctx, "deal-a", 3, 7))
	closed, err := x.steps(ctx, stepFilter{DealID: "deal-a", N: 3})
	require.NoError(t, err)
	assert.Equal(t, uint64(7), closed[0].Sequence)
	require.NoError(t, x.withdrawStep(ctx, "deal-a", 3))
	count, err = x.stepCount(ctx, "deal-a")
	require.NoError(t, err)
	assert.Equal(t, 2, count)
}

// Reports: prepared at sequence 0 (not yet in the log, so not counted),
// published at their position, found by audience and position, withdrawn.
func TestSQLiteDealIndexReports(t *testing.T) {
	ctx := t.Context()
	x := newTestDealIndex(t)
	require.NoError(t, x.prepareReport(ctx, "deal-a", "r1", "keep"))
	require.NoError(t, x.prepareReport(ctx, "deal-a", "r2", "counterparty"))
	require.NoError(t, x.prepareReport(ctx, "deal-a", "r3", "keep"))
	require.NoError(t, x.prepareReport(ctx, "deal-b", "r4", "keep"))

	n, err := x.reportsThrough(ctx, "deal-a", 100)
	require.NoError(t, err)
	assert.Zero(t, n, "a prepared report is not in the log")

	require.NoError(t, x.publishedReport(ctx, "deal-a", "r1", 3))
	require.NoError(t, x.publishedReport(ctx, "deal-a", "r2", 5))
	require.NoError(t, x.publishedReport(ctx, "deal-a", "r3", 8))

	ids, err := x.reportIDs(ctx, "deal-a")
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"r1": true, "r2": true, "r3": true}, ids)

	n, err = x.reportsThrough(ctx, "deal-a", 5)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	id, seq, ok, err := x.latestReport(ctx, "deal-a", "keep", 7)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "r1", id)
	assert.Equal(t, uint64(3), seq)
	_, _, ok, err = x.latestReport(ctx, "deal-a", "keep", 2)
	require.NoError(t, err)
	assert.False(t, ok)

	id, ok, err = x.reportAt(ctx, "deal-a", "counterparty", 5)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "r2", id)
	_, ok, err = x.reportAt(ctx, "deal-a", "keep", 5)
	require.NoError(t, err)
	assert.False(t, ok)

	require.NoError(t, x.withdrawReport(ctx, "deal-b", "r4"))
	ids, err = x.reportIDs(ctx, "deal-b")
	require.NoError(t, err)
	assert.Empty(t, ids)
}

// A sale whose task authority is missing reports it with the no-row cause,
// as it did when saleOf read the table itself; an unknown sale is an input
// error.
func TestSaleOfMissingAuthority(t *testing.T) {
	ctx := t.Context()
	x := newTestDealIndex(t)
	s := &dealSession{index: x}
	const sale = "sale-0123456789abcdef"
	_, _, err := s.saleOf(ctx, sale)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown sale")
	require.NoError(t, x.prepareStep(ctx, indexedStep{DealID: sale, N: 1, Kind: "sale", CapsuleID: "c1", Digest: "d1", Local: `{}`}))
	_, _, err = s.saleOf(ctx, sale)
	require.ErrorIs(t, err, sql.ErrNoRows)
	assert.Contains(t, err.Error(), "has no task authority")
}
