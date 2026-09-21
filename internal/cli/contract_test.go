package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSchema = "testdata/contract/evidence-contract-v0.json"

func contractFixtures(t *testing.T, dir string) []string {
	t.Helper()
	entries, e := os.ReadDir(filepath.Join("testdata", "contract", dir))
	require.NoError(t, e)
	var paths []string
	for _, entry := range entries {
		paths = append(paths, filepath.Join("testdata", "contract", dir, entry.Name()))
	}
	sort.Strings(paths)
	require.NotEmpty(t, paths, "fixture directory must not be empty")
	return paths
}

// TestContractValidatePositiveFixtures cross-checks every ledger-item
// positive fixture: the ground truth (`python -m
// capsule_engine.packs.contract_validate`, run by hand against the same
// schema+fixture set at authoring time) reports all three valid.
func TestContractValidatePositiveFixtures(t *testing.T) {
	for _, path := range contractFixtures(t, "positive") {
		t.Run(path, func(t *testing.T) {
			out, e := invoke(t, "", "contract", "validate", path, "--schema", testSchema, "--json")
			require.NoError(t, e)
			var report map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &report))
			assert.Equal(t, true, report["valid"])
			assert.Empty(t, report["issues"])
		})
	}
}

// TestContractValidateNegativeFixtures cross-checks every ledger-item
// negative fixture: the same ground-truth Python run reports all nine
// invalid. capsulectl must agree on the verdict (not necessarily the wording)
// for every one.
func TestContractValidateNegativeFixtures(t *testing.T) {
	for _, path := range contractFixtures(t, "negative") {
		t.Run(path, func(t *testing.T) {
			out, e := invoke(t, "", "contract", "validate", path, "--schema", testSchema, "--json")
			require.ErrorIs(t, e, ErrSchemaInvalid)
			require.Equal(t, 1, ExitCode(e))
			var report map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &report))
			assert.Equal(t, false, report["valid"])
			assert.NotEmpty(t, report["issues"])
		})
	}
}

// TestContractValidateProfileAwareMessages pins the readable, profile-aware
// wording for the three shapes of profile failure so a regression in the
// oneOf-discriminator walk (contract.go) is caught by message content, not
// just by the pass/fail verdict the fixture tests above already cover.
func TestContractValidateProfileAwareMessages(t *testing.T) {
	cases := []struct {
		fixture string
		want    string
	}{
		{"unknown-profile-value.json", `unknown profile "reputation" (expected one of: attribution, human_role, obligation, outcome, process, quality, settlement)`},
		{"missing-profile.json", `missing required property "profile" (expected one of: attribution, human_role, obligation, outcome, process, quality, settlement)`},
		{"obligation-missing-clause.json", `profile "obligation": at requirements/0: missing required property "clause_ref"`},
		{"requirement-self-declares-satisfied.json", `profile "human_role": at requirements/0/status: field is not permitted here`},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			path := filepath.Join("testdata", "contract", "negative", tc.fixture)
			out, e := invoke(t, "", "contract", "validate", path, "--schema", testSchema)
			require.ErrorIs(t, e, ErrSchemaInvalid)
			assert.Contains(t, out, tc.want)
		})
	}
}

func TestContractValidateHumanReadableOutput(t *testing.T) {
	valid := filepath.Join("testdata", "contract", "positive", "outcome-claims-adjudication.json")
	out, e := invoke(t, "", "contract", "validate", valid, "--schema", testSchema)
	require.NoError(t, e)
	assert.Equal(t, valid+": valid\n", out)

	invalid := filepath.Join("testdata", "contract", "negative", "empty-requirements.json")
	out, e = invoke(t, "", "contract", "validate", invalid, "--schema", testSchema)
	require.ErrorIs(t, e, ErrSchemaInvalid)
	assert.Contains(t, out, invalid+": INVALID\n")
	assert.Contains(t, out, "at requirements: expected at least 1 item(s), got 0")
}

func TestContractValidateSchemaFromURL(t *testing.T) {
	schema, e := os.ReadFile(testSchema)
	require.NoError(t, e)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(schema)
	}))
	defer server.Close()
	valid := filepath.Join("testdata", "contract", "positive", "dogfood-change-control.json")
	out, e := invoke(t, "", "contract", "validate", valid, "--schema", server.URL, "--json")
	require.NoError(t, e)
	var report map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, true, report["valid"])
}

func TestContractValidateUsageErrors(t *testing.T) {
	valid := filepath.Join("testdata", "contract", "positive", "outcome-claims-adjudication.json")
	t.Run("missing --schema", func(t *testing.T) {
		_, e := invoke(t, "", "contract", "validate", valid)
		require.ErrorIs(t, e, ErrInput)
		assert.Equal(t, 2, ExitCode(e))
	})
	t.Run("document file does not exist", func(t *testing.T) {
		_, e := invoke(t, "", "contract", "validate", "testdata/contract/does-not-exist.json", "--schema", testSchema)
		require.ErrorIs(t, e, ErrInput)
		assert.Equal(t, 2, ExitCode(e))
	})
	t.Run("schema does not exist", func(t *testing.T) {
		_, e := invoke(t, "", "contract", "validate", valid, "--schema", "testdata/contract/does-not-exist-schema.json")
		require.ErrorIs(t, e, ErrInput)
		assert.Equal(t, 2, ExitCode(e))
		assert.Contains(t, SafeError(e), "does-not-exist-schema.json")
	})
	t.Run("document is not valid JSON", func(t *testing.T) {
		dir := t.TempDir()
		bad := filepath.Join(dir, "bad.json")
		require.NoError(t, os.WriteFile(bad, []byte("{not json"), 0o600))
		_, e := invoke(t, "", "contract", "validate", bad, "--schema", testSchema)
		require.ErrorIs(t, e, ErrInput)
		assert.Equal(t, 2, ExitCode(e))
	})
	t.Run("no positional file argument", func(t *testing.T) {
		_, e := invoke(t, "", "contract", "validate", "--schema", testSchema)
		require.ErrorIs(t, e, ErrInput)
		assert.Equal(t, 2, ExitCode(e))
	})
}

// TestDiscriminatorValues pins the generic const-discriminator scan
// independent of the full Evidence Contract schema, since that is the piece
// that lets contract.go stay schema-agnostic (R4: mutating the "properties"
// or "const" key names below breaks this test before touching any fixture).
func TestDiscriminatorValues(t *testing.T) {
	schemaRaw := map[string]any{
		"$defs": map[string]any{
			"a": map[string]any{"properties": map[string]any{"kind": map[string]any{"const": "alpha"}}},
			"b": map[string]any{"allOf": []any{
				map[string]any{"properties": map[string]any{"kind": map[string]any{"const": "beta"}}},
			}},
			"c": map[string]any{"properties": map[string]any{"other": map[string]any{"const": "ignored"}}},
		},
	}
	assert.Equal(t, []string{"alpha", "beta"}, discriminatorValues(schemaRaw, "kind"))
	assert.Empty(t, discriminatorValues(schemaRaw, "missing-key"))
}

func TestPathString(t *testing.T) {
	assert.Equal(t, "<root>", pathString(nil))
	assert.Equal(t, "requirements/0/profile", pathString([]string{"requirements", "0", "profile"}))
	assert.Equal(t, "a~1b", pathString([]string{"a/b"}))
}
