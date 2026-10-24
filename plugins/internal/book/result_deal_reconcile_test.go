package book

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/action-state-group/capsule-cli/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// result build refuses a claim under exactly ec:deal-reconcile@1 before it
// opens the book, and keeps its own rules for the same claim under another
// contract_ref. result open names the claim as not recomputed. The Result is
// one capsulectl-deal's `deal terms-result` wrote (testdata/deal-reconcile).
func TestResultCommandsAndTheDealReconcileContract(t *testing.T) {
	resultPath := filepath.Join("testdata", "deal-reconcile", "result.json")
	var doc map[string]any
	require.NoError(t, json.Unmarshal(mustRead(t, resultPath), &doc))

	text, err := invoke(t, "", "result", "open", resultPath)
	require.NoError(t, err)
	assert.Contains(t, text, "not recomputed: claim reconcile-amount")
	out, err := invoke(t, "", "result", "open", resultPath, "--format", "json")
	require.NoError(t, err)
	var opened map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &opened))
	assert.Equal(t, []any{"reconcile-amount", "reconcile-currency"}, opened["not_recomputed"])

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := bookProfile(t, "a") // no store: a refusal from the book would be "no book yet"
	_, _, err = runResultBuild(t, p, doc)
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Contains(t, cli.SafeError(err), "deal terms-result")

	for _, c := range doc["claims"].([]any) {
		c.(map[string]any)["contract_ref"] = "ec:deal-reconcile@2"
	}
	_, _, err = runResultBuild(t, p, doc)
	require.Error(t, err)
	assert.NotContains(t, cli.SafeError(err), "deal terms-result", "another version gets no exception and no refusal of this kind")
}
