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
