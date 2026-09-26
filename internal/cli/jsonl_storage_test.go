package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/action-state-group/evidencebook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestJSONLSDKOnlyStorage exercises the file-based jsonl profile end to end:
// store init creates the artifact file and the evidence book (the profile's
// one log; no cll.jsonl), publish persists an artifact and commits it to the
// book (idempotently), the record reads back, and a checkpoint of the book
// verifies under the profile's own log_id.
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
	assert.FileExists(t, filepath.Join(dir, "book", "log.jsonl"))
	assert.NoFileExists(t, filepath.Join(dir, "cll.jsonl"), "a jsonl profile has one log")

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

	// A checkpoint of the book verifies under the profile's log_id.
	out, err := invoke(t, "", "cll", "checkpoint", "create", "--profile", p.Name)
	require.NoError(t, err)
	var checkpointed struct {
		LogID      string `json:"log_id"`
		Checkpoint uint64 `json:"checkpoint"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &checkpointed))
	assert.Equal(t, p.LogID, checkpointed.LogID)
	assert.Positive(t, checkpointed.Checkpoint)
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

// TestJSONLDiscloseIsOnTheBook exercises disclose and bundle over a jsonl
// profile's book: disclose carries the published capsule itself as a payload
// of its book record, and every bundle the book builds -- disclosing or not --
// is put on record as a disclosure record in the same log.
func TestJSONLDiscloseIsOnTheBook(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, key := profileFixture(t)
	p.Type = "jsonl"
	p.Connection.Database = filepath.Join(t.TempDir(), "store")
	require.NoError(t, saveProfile(p, false))
	_, err := invoke(t, "", "store", "init", "--profile", p.Name)
	require.NoError(t, err)

	request, err := parseRequest(requestFixture(t))
	require.NoError(t, err)
	target, err := openTarget(t.Context(), p, usePublication)
	require.NoError(t, err)
	published, err := target.publish(t.Context(), request, key)
	require.NoError(t, err)
	stored, err := target.artifacts.Get(t.Context(), published.CapsuleID)
	require.NoError(t, err)
	require.NoError(t, target.close())

	disclosures := func() []evidencebook.Record {
		opened, err := openBook(t.Context(), p, false)
		require.NoError(t, err)
		defer func() { require.NoError(t, opened.release()) }()
		records, err := opened.book.Query(t.Context(), evidencebook.Filter{RecordType: evidencebook.RecordTypeDisclosure})
		require.NoError(t, err)
		return records
	}

	out, err := invoke(t, "", "disclose", "--profile", p.Name, "--root", published.CapsuleID)
	require.NoError(t, err)
	verified, err := evidencebook.VerifyBundle([]byte(strings.TrimSpace(out)))
	require.NoError(t, err)
	var root evidencebook.PeerRecord
	for _, r := range verified.Records {
		if r.Header != nil && r.Header.SubjectRef == published.CapsuleID {
			root = r
		}
	}
	require.True(t, root.HeaderVerified, "disclose discloses the published record's header")
	assert.Equal(t, recordTypePublished, root.Header.RecordType)
	require.Len(t, root.Header.PayloadCommitments, 2)
	assert.Equal(t, stored.Capsule, verified.Payloads[root.Header.PayloadCommitments[0]], "the bundle carries the published capsule itself")
	assert.Equal(t, stored.ProducerEnvelope, verified.Payloads[root.Header.PayloadCommitments[1]])
	on := disclosures()
	require.Len(t, on, 1, "disclose is on record in the book")
	var statement evidencebook.DisclosureStatement
	require.NoError(t, json.Unmarshal(on[0].Header.Statement, &statement))
	assert.Equal(t, verified.Digest, statement.BundleDigest)

	out, err = invoke(t, "", "bundle", "--profile", p.Name, "--root", published.CapsuleID)
	require.NoError(t, err)
	verified, err = evidencebook.VerifyBundle([]byte(strings.TrimSpace(out)))
	require.NoError(t, err)
	for _, r := range verified.Records {
		assert.Nil(t, r.Header, "a plain bundle discloses no header")
	}
	assert.Empty(t, verified.Payloads, "a plain bundle discloses no payload")
	assert.Len(t, disclosures(), 2, "the book puts every bundle it builds on record")

	_, err = invoke(t, "", "disclose", "--profile", p.Name, "--root", published.CapsuleID, "--suppress", "agent_output")
	assert.ErrorIs(t, err, ErrInput, "a book record has no agent_output member")
}
