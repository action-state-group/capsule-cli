package plugin

import (
	"path/filepath"
	"testing"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A deal profile is written by capsulectl deal only (capsule-cli#242): a
// core write on it is refused, so it never lands on the witnessed cadence
// log or a deal's log behind the deal store's back, while the deal's own
// steps still seal and core reads still work.
func TestCoreRefusesWritesOnADealProfile(t *testing.T) {
	dealFixture(t)
	dealID := retailDeal(t)
	step := dealRun(t, "note", "--deal", dealID, "--kind", "message", "--input", writeJSON(t, `{"from":"user","text":"sealed by deal"}`))
	capsule := filepath.Join(t.TempDir(), "step.json")
	_, err := invoke(t, "", "get", "--profile", "deal", "--capsule-id", step["capsule_id"].(string), "--raw", "--output", capsule)
	require.NoError(t, err, "a core read of a deal's record still works")
	before, err := invoke(t, "", "cll", "list", "--profile", "deal", "--log-id", cli.DealLogID(dealID))
	require.NoError(t, err, "a core read of a deal's log still works")

	for _, args := range [][]string{
		{"publish", "--profile", "deal", "--request", sealRequestFile(t, "stray")},
		{"cll", "append", "--profile", "deal", "--capsule", capsule},
		{"store", "init", "--profile", "deal"},
	} {
		_, err = invoke(t, "", args...)
		assert.ErrorIs(t, err, cli.ErrDealProfileWrite, "%v", args)
	}
	after, err := invoke(t, "", "cll", "list", "--profile", "deal", "--log-id", cli.DealLogID(dealID))
	require.NoError(t, err)
	assert.Equal(t, before, after, "the deal's log is unchanged")

	dealRun(t, "note", "--deal", dealID, "--kind", "message", "--input", writeJSON(t, `{"from":"user","text":"still sealing"}`))
}

// The deal session is a writer while it holds the exclusive lock, and no
// longer once closed; a read session under the shared lock never is.
func TestDealSessionHoldsTheStoreWhileOpen(t *testing.T) {
	dealFixture(t)
	retailDeal(t)
	p, err := cli.LoadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	assert.True(t, s.HoldsDealStore())
	require.NoError(t, s.close())
	assert.False(t, s.HoldsDealStore(), "a closed session")
	_, err = cli.OpenDealTarget(t.Context(), p, cli.UseCLL, s)
	assert.ErrorIs(t, err, cli.ErrDealProfileWrite)

	r, err := openDealStoreRead(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, r.close()) }()
	assert.False(t, r.HoldsDealStore(), "a read under the shared lock")
	_, err = cli.OpenDealTarget(t.Context(), p, cli.UseCLL, r)
	assert.ErrorIs(t, err, cli.ErrDealProfileWrite)
}

// A deal profile edited to look plain (its namespace cleared) still cannot
// be written by core: the store carries the shelf's marker.
func TestCoreRefusesWritesOnAShelfStoreWhateverTheProfileSays(t *testing.T) {
	dealFixture(t)
	retailDeal(t)
	p, err := cli.LoadProfile("deal")
	require.NoError(t, err)
	p.Namespace = "plain"
	require.NoError(t, cli.SaveProfile(p, true))
	require.False(t, p.IsShelf())
	_, err = invoke(t, "", "publish", "--profile", "deal", "--request", sealRequestFile(t, "stray"))
	assert.ErrorIs(t, err, cli.ErrDealProfileWrite)
}
