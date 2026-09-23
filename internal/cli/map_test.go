package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const dogfoodChangeControlFixture = "testdata/contract/positive/dogfood-change-control.json"

func writeDiscoverEffects(t *testing.T, effects []effectBoundary) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "effects.json")
	b, e := json.Marshal(map[string]any{"effects": effects, "spec_version": "capsule-cli-result/v1"})
	require.NoError(t, e)
	require.NoError(t, os.WriteFile(path, b, 0o600))
	return path
}

// TestMapMatchesDeclaredRequiredSources uses the real, already-schema-valid
// dogfood-change-control fixture: both its requirements declare
// evidence_requirements.required_sources: ["vcs-events"].
func TestMapMatchesDeclaredRequiredSources(t *testing.T) {
	discoverPath := writeDiscoverEffects(t, []effectBoundary{
		{Source: "ci/workflows/merge.yaml", Surface: "vcs-events", SurfaceType: "bus_topic", Classification: classificationObservation, Signal: "operation=consume"},
	})
	out, e := invoke(t, "", "map", dogfoodChangeControlFixture, "--schema", testSchema, "--discover", discoverPath)
	require.NoError(t, e)
	assert.Contains(t, out, "req-process-1: vcs-events (ci/workflows/merge.yaml)")
	assert.Contains(t, out, "req-obligation-1: vcs-events (ci/workflows/merge.yaml)")
	assert.NotContains(t, out, "no source found")
}

func TestMapNoSourceFoundWhenNothingDiscovered(t *testing.T) {
	discoverPath := writeDiscoverEffects(t, []effectBoundary{
		{Source: "other.json", Surface: "unrelated-tool", SurfaceType: "mcp_tool", Classification: classificationEffect, Signal: "no signal present (fail-safe default)"},
	})
	out, e := invoke(t, "", "map", dogfoodChangeControlFixture, "--schema", testSchema, "--discover", discoverPath)
	require.NoError(t, e)
	assert.Contains(t, out, "req-process-1: no source found (declared: vcs-events)")
	assert.Contains(t, out, "req-obligation-1: no source found (declared: vcs-events)")
}

// TestMapNeverMatchesAnApproximateName proves the join is exact-string-only:
// a near-miss surface name (singular vs. plural) must never be treated as
// the same declared evidence class -- map never infers equivalence.
func TestMapNeverMatchesAnApproximateName(t *testing.T) {
	discoverPath := writeDiscoverEffects(t, []effectBoundary{
		{Source: "other.json", Surface: "vcs-event", SurfaceType: "bus_topic", Classification: classificationObservation, Signal: "operation=consume"},
	})
	out, e := invoke(t, "", "map", dogfoodChangeControlFixture, "--schema", testSchema, "--discover", discoverPath)
	require.NoError(t, e)
	assert.Contains(t, out, "no source found")
	assert.NotContains(t, out, "vcs-event (")
}

func TestMapRejectsAnInvalidContract(t *testing.T) {
	discoverPath := writeDiscoverEffects(t, []effectBoundary{})
	_, e := invoke(t, "", "map", "testdata/contract/negative/malformed-clause.json", "--schema", testSchema, "--discover", discoverPath)
	require.ErrorIs(t, e, ErrSchemaInvalid)
	assert.Equal(t, 1, ExitCode(e))
}

func TestMapRequiresSchemaAndDiscoverFlags(t *testing.T) {
	discoverPath := writeDiscoverEffects(t, []effectBoundary{})
	_, e := invoke(t, "", "map", dogfoodChangeControlFixture, "--discover", discoverPath)
	require.Error(t, e)
	assert.Equal(t, 2, ExitCode(e))
	_, e = invoke(t, "", "map", dogfoodChangeControlFixture, "--schema", testSchema)
	require.Error(t, e)
	assert.Equal(t, 2, ExitCode(e))
}

func TestMapRequiresDiscoverEffectsShape(t *testing.T) {
	notEffects := filepath.Join(t.TempDir(), "scan.json")
	require.NoError(t, os.WriteFile(notEffects, []byte(`{"files":[],"spec_version":"capsule-cli-result/v1"}`), 0o600))
	_, e := invoke(t, "", "map", dogfoodChangeControlFixture, "--schema", testSchema, "--discover", notEffects)
	require.Error(t, e)
	assert.Equal(t, 2, ExitCode(e))
}

func TestMapJSONFormat(t *testing.T) {
	discoverPath := writeDiscoverEffects(t, []effectBoundary{
		{Source: "ci/workflows/merge.yaml", Surface: "vcs-events", SurfaceType: "bus_topic", Classification: classificationObservation, Signal: "operation=consume"},
	})
	out, e := invoke(t, "", "map", dogfoodChangeControlFixture, "--schema", testSchema, "--discover", discoverPath, "--format", "json")
	require.NoError(t, e)
	var parsed struct {
		Requirements []struct {
			ID            string           `json:"id"`
			DeclaredNames []string         `json:"declared_names"`
			Found         bool             `json:"found"`
			Matches       []map[string]any `json:"matches"`
		} `json:"requirements"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &parsed))
	require.Len(t, parsed.Requirements, 2)
	for _, r := range parsed.Requirements {
		assert.Equal(t, []string{"vcs-events"}, r.DeclaredNames)
		assert.True(t, r.Found)
		require.Len(t, r.Matches, 1)
		assert.Equal(t, "vcs-events", r.Matches[0]["surface"])
	}
}

// TestMapDoesNotRequireAProfile proves map is a pure local join: read-only,
// no database, no --profile.
func TestMapDoesNotRequireAProfile(t *testing.T) {
	discoverPath := writeDiscoverEffects(t, []effectBoundary{})
	_, e := invoke(t, "", "map", dogfoodChangeControlFixture, "--schema", testSchema, "--discover", discoverPath)
	require.NoError(t, e)
}

func TestMapRejectsBadFormat(t *testing.T) {
	discoverPath := writeDiscoverEffects(t, []effectBoundary{})
	_, e := invoke(t, "", "map", dogfoodChangeControlFixture, "--schema", testSchema, "--discover", discoverPath, "--format", "xml")
	require.Error(t, e)
}
