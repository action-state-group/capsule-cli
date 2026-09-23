package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeResultFixture(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "result.json")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

const validResultFixture = `{
  "claims": [{"tier": "recomputed", "grade": "witnessed", "sufficiency": "SATISFIED", "verdict": "met"}],
  "aggregate": {
    "coverage": {"evaluated_population": 10, "excluded_not_applicable": 2, "unknown_count": 0}
  }
}`

func TestResultOpenTextMode(t *testing.T) {
	path := writeResultFixture(t, validResultFixture)
	out, e := invoke(t, "", "result", "open", path)
	require.NoError(t, e)
	assert.Contains(t, out, "Aggregate:")
	assert.Contains(t, out, "Coverage:")
	assert.Contains(t, out, "evaluated_population: 10")
	assert.Contains(t, out, "excluded_not_applicable: 2")
	assert.Contains(t, out, "unknown_count: 0")
	assert.Contains(t, out, "DRAFT")
}

func TestResultOpenJSONMode(t *testing.T) {
	path := writeResultFixture(t, validResultFixture)
	out, e := invoke(t, "", "result", "open", path, "--format", "json")
	require.NoError(t, e)
	var parsed struct {
		Aggregate map[string]any `json:"aggregate"`
		Note      string         `json:"note"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &parsed))
	coverage, ok := parsed.Aggregate["coverage"].(map[string]any)
	require.True(t, ok)
	assert.EqualValues(t, 10, coverage["evaluated_population"])
	assert.Contains(t, parsed.Note, "DRAFT")
}

func TestResultOpenRejectsMissingAggregate(t *testing.T) {
	path := writeResultFixture(t, `{"claims": []}`)
	_, e := invoke(t, "", "result", "open", path)
	require.Error(t, e)
	assert.Equal(t, 2, ExitCode(e))
}

func TestResultOpenRejectsAggregateWithoutCoverage(t *testing.T) {
	path := writeResultFixture(t, `{"aggregate": {"status": "ok"}}`)
	_, e := invoke(t, "", "result", "open", path)
	require.Error(t, e)
	assert.Equal(t, 2, ExitCode(e))
}

func TestResultOpenRejectsNonObject(t *testing.T) {
	path := writeResultFixture(t, `[1, 2, 3]`)
	_, e := invoke(t, "", "result", "open", path)
	require.Error(t, e)
}

func TestResultOpenRejectsTrailingData(t *testing.T) {
	path := writeResultFixture(t, validResultFixture+"\n{}")
	_, e := invoke(t, "", "result", "open", path)
	require.Error(t, e)
}

func TestResultOpenRejectsBadFormat(t *testing.T) {
	path := writeResultFixture(t, validResultFixture)
	_, e := invoke(t, "", "result", "open", path, "--format", "xml")
	require.Error(t, e)
}

func TestResultOpenMissingFile(t *testing.T) {
	_, e := invoke(t, "", "result", "open", filepath.Join(t.TempDir(), "missing.json"))
	require.Error(t, e)
	assert.Equal(t, 2, ExitCode(e))
}
