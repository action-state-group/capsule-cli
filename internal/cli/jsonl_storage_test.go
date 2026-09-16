package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func viewFixture(t *testing.T, payload string) (Profile, string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, key := profileFixture(t)
	p.Type = "jsonl"
	p.Connection.Database = filepath.Join(t.TempDir(), "store")
	require.NoError(t, saveProfile(p, false))

	_, err := invoke(t, "", "store", "init", "--profile", p.Name)
	require.NoError(t, err)
	target, err := openTarget(t.Context(), p, usePublication)
	require.NoError(t, err)
	request, err := parseRequest(requestFixture(t))
	require.NoError(t, err)
	request.Payload = []byte(payload)
	record, err := target.publish(t.Context(), request, key)
	require.NoError(t, err)
	require.NoError(t, target.close())
	_, err = invoke(t, "", "cll", "checkpoint", "create", "--profile", p.Name)
	require.NoError(t, err)
	return p, record.CapsuleID
}

func TestViewWritesOfflineEvidenceGraphHTML(t *testing.T) {
	p, root := viewFixture(t, `{"spec_version":"evaluation-summary/v1","counts":{"reports":0,"unique_cases":0},"per_axis":{}}`)
	out := filepath.Join(t.TempDir(), "evidence-graph.html")
	confirmation, err := invoke(t, "", "view", "--profile", p.Name, "--root", root, "--out", out)
	require.NoError(t, err)
	assert.Contains(t, confirmation, out)
	html, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Contains(t, string(html), "renderEvidenceGraph(window.__BUNDLE__")
	assert.Contains(t, string(html), `id="app"`)
	assert.Contains(t, string(html), `"spec_version":"evaluation-summary/v1"`)
	assert.NotContains(t, string(html), "http://")
	assert.NotContains(t, string(html), "https://")
}

func TestViewRejectsUnrenderableRoot(t *testing.T) {
	for _, tc := range []struct {
		name, payload, suppress, want string
	}{
		{"wrong version", `{"spec_version":"evaluation-report/v1"}`, "", "spec_version=evaluation-report/v1"},
		{"missing version", `{"a":1}`, "", "spec_version=<nil>"},
		{"non-object", `[]`, "", "non-object agent_input"},
		{"suppressed input", `{"spec_version":"evaluation-summary/v1"}`, "agent_input", "cannot suppress agent_input"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, root := viewFixture(t, tc.payload)
			out := filepath.Join(t.TempDir(), "view.html")
			args := []string{"view", "--profile", p.Name, "--root", root, "--out", out}
			if tc.suppress != "" {
				args = append(args, "--suppress", tc.suppress)
			}
			_, err := invoke(t, "", args...)
			require.ErrorIs(t, err, ErrInput)
			require.ErrorContains(t, err, tc.want)
			_, err = os.Stat(out)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

// TestJSONLSDKOnlyStorage exercises the file-based jsonl profile end to end:
// store init creates the two files, publish persists an artifact and appends to
// the CLL (idempotently), the record reads back, and a checkpoint can be cut —
// proving the cll-go jsonl backend's witness/checkpoint methods are wired too.
func TestJSONLSDKOnlyStorage(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, key := profileFixture(t)
	p.Type = "jsonl"
	dir := filepath.Join(t.TempDir(), "store")
	p.Connection.Database = dir
	require.NoError(t, saveProfile(p, false))

	// store init provisions the directory and both files.
	_, err := invoke(t, "", "store", "init", "--profile", p.Name)
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(dir, "artifacts.jsonl"))
	assert.FileExists(t, filepath.Join(dir, "cll.jsonl"))

	// publish: seal -> persist artifact -> append to CLL, idempotent on retry.
	request, err := parseRequest(requestFixture(t))
	require.NoError(t, err)
	target, err := openTarget(t.Context(), p, usePublication)
	require.NoError(t, err)
	first, err := target.publish(t.Context(), request, key)
	require.NoError(t, err)
	require.NoError(t, target.close())
	assert.Equal(t, "appended", first.State)
	assert.GreaterOrEqual(t, first.Sequence, uint64(1))

	target, err = openTarget(t.Context(), p, usePublication)
	require.NoError(t, err)
	second, err := target.publish(t.Context(), request, key)
	require.NoError(t, err)
	require.NoError(t, target.close())
	assert.Equal(t, first, second)

	// The persisted record reads back through the jsonl artifact store.
	target, err = openTarget(t.Context(), p, useArtifacts)
	require.NoError(t, err)
	got, err := target.artifacts.Get(t.Context(), first.CapsuleID)
	require.NoError(t, err)
	require.NoError(t, target.close())
	assert.Equal(t, first.CapsuleID, got.CapsuleID)

	// A checkpoint can be cut over the jsonl log (LoadCLL/CommitCLL wired).
	_, err = invoke(t, "", "cll", "checkpoint", "create", "--profile", p.Name)
	require.NoError(t, err)
}

// TestJSONLMissingDirectoryFailsClosed verifies that a non-init command against
// an unprovisioned (e.g. mistyped) jsonl directory errors instead of silently
// creating it.
func TestJSONLMissingDirectoryFailsClosed(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := profileFixture(t)
	p.Type = "jsonl"
	missing := filepath.Join(t.TempDir(), "never-created")
	p.Connection.Database = missing

	_, err := openTarget(t.Context(), p, useArtifacts)
	require.Error(t, err)
	assert.NoDirExists(t, missing, "a read must not fabricate the storage directory")
}

// TestJSONLReadOnlyRejectsWrites confirms the shared read-only gate applies to
// jsonl profiles: publication requires writes.
func TestJSONLReadOnlyRejectsWrites(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := profileFixture(t)
	p.Type = "jsonl"
	p.Connection.Database = filepath.Join(t.TempDir(), "store")
	p.ReadOnly = true

	_, err := openTarget(t.Context(), p, usePublication)
	require.ErrorIs(t, err, ErrReadOnlyCLL)
}

// TestJSONLProfileCreate covers non-interactive creation of a jsonl profile and
// that connection.database carries the storage directory.
func TestJSONLProfileCreate(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "store")
	trusted := strings.Repeat("ab", 32)
	_, e := invoke(t, "", "profile", "create", "--name", "jl", "--type", "jsonl",
		"--jsonl-path", dir, "--namespace", "demo", "--log-id", "log-j", "--trusted-key", trusted)
	require.NoError(t, e)
	p, e := loadProfile("jl")
	require.NoError(t, e)
	assert.Equal(t, "jsonl", p.Type)
	assert.Equal(t, dir, p.Connection.Database)
	assert.Equal(t, "demo", p.Namespace)
	assert.Empty(t, p.Connection.Host)
}
