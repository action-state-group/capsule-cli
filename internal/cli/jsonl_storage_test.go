package cli

import (
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/capsule-emit-go/artifact"
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

// jsonlPublished is a jsonl profile holding one published capsule whose
// agent_input original the artifact store retains.
func jsonlPublished(t *testing.T) (Profile, artifact.Record) {
	t.Helper()
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
	return p, stored
}

// The book never stores an original: a published record commits the capsule
// and its envelope only. disclose carries the agent_input original only on
// --attach-input-originals, in the bundle extension the disclosure record
// commits to, checked against the capsule's agent_input_digest.
func TestJSONLDiscloseAttachesInputOriginalsOnlyOnOptIn(t *testing.T) {
	p, stored := jsonlPublished(t)
	original, err := verifiedInputOriginal(stored)
	require.NoError(t, err)
	require.NotNil(t, original, "the fixture's request retains its agent_input original")
	encodedOriginal := base64.RawURLEncoding.EncodeToString(original)

	plain, err := invoke(t, "", "disclose", "--profile", p.Name, "--root", stored.CapsuleID)
	require.NoError(t, err)
	verified, err := evidencebook.VerifyBundle([]byte(strings.TrimSpace(plain)))
	require.NoError(t, err)
	for _, r := range verified.Records {
		if r.Header != nil && r.Header.SubjectRef == stored.CapsuleID {
			assert.Len(t, r.Header.PayloadCommitments, 2, "capsule and envelope only: no text in the book")
		}
	}
	assert.NotContains(t, plain, encodedOriginal, "no original without the opt-in")
	assert.NotContains(t, plain, inputOriginalsExtension)

	attached, err := invoke(t, "", "disclose", "--profile", p.Name, "--root", stored.CapsuleID, "--attach-input-originals")
	require.NoError(t, err)
	verified, err = evidencebook.VerifyBundle([]byte(strings.TrimSpace(attached)))
	require.NoError(t, err)
	var bundle struct {
		Extensions map[string]map[string]string `json:"extensions"`
	}
	require.NoError(t, json.Unmarshal([]byte(attached), &bundle))
	carried, err := base64.RawURLEncoding.DecodeString(bundle.Extensions[inputOriginalsExtension][stored.CapsuleID])
	require.NoError(t, err)
	assert.Equal(t, original, carried)
	if strings.Contains(plain, producerKeyExtensionKind) {
		// The producer-key extension and the opt-in originals travel together:
		// declaring the key must never drop the originals, or the reverse.
		assert.Contains(t, attached, producerKeyExtensionKind)
	}
	var decoded any
	require.NoError(t, decodeJSONPreserveNumbers("the carried original", carried, &decoded))
	digest, err := canonical.JSONDigest(decoded)
	require.NoError(t, err)
	var capsule struct {
		ModelAttestation struct {
			ComputeAttestation struct {
				AgentInputDigest string `json:"agent_input_digest"`
			} `json:"compute_attestation"`
		} `json:"model_attestation"`
	}
	require.NoError(t, json.Unmarshal(stored.Capsule, &capsule))
	assert.Equal(t, capsule.ModelAttestation.ComputeAttestation.AgentInputDigest, digest, "the carried original is the preimage the capsule commits to")
	opened, err := openBook(t.Context(), p, false)
	require.NoError(t, err)
	records, err := opened.book.Query(t.Context(), evidencebook.Filter{RecordType: evidencebook.RecordTypeDisclosure})
	require.NoError(t, opened.release())
	require.NoError(t, err)
	var statement evidencebook.DisclosureStatement
	require.NoError(t, json.Unmarshal(records[len(records)-1].Header.Statement, &statement))
	assert.Equal(t, verified.Digest, statement.BundleDigest, "the disclosure record commits to the bundle carrying the original")

	_, err = invoke(t, "", "disclose", "--profile", p.Name, "--root", stored.CapsuleID, "--attach-input-originals", "--suppress", "agent_input")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "contradict")
}

// An original that does not hash to the capsule's agent_input_digest is
// never attached.
func TestVerifiedInputOriginalChecksTheDigest(t *testing.T) {
	_, stored := jsonlPublished(t)
	tampered := stored
	tampered.Artifacts = append([]artifact.Artifact(nil), stored.Artifacts...)
	for i, a := range tampered.Artifacts {
		if a.Binding == artifact.PayloadDigest {
			tampered.Artifacts[i].Content = []byte(`{"tampered":true}`)
		}
	}
	_, err := verifiedInputOriginal(tampered)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not hash to the capsule's agent_input_digest")
	none := stored
	none.Artifacts = nil
	original, err := verifiedInputOriginal(none)
	require.NoError(t, err)
	assert.Nil(t, original, "nothing retained: nothing attached")
}

// A published record an earlier build wrote with the original as a third
// payload: --suppress agent_input withholds those bytes as well as the
// header (under --payloads selected), and refuses under --payloads all
// rather than ship them.
func TestJSONLSuppressWithholdsACommittedInputOriginal(t *testing.T) {
	p, stored := jsonlPublished(t)
	original, err := verifiedInputOriginal(stored)
	require.NoError(t, err)
	opened, err := openBook(t.Context(), p, false)
	require.NoError(t, err)
	_, err = opened.book.Append(t.Context(), evidencebook.Entry{
		RecordType: recordTypePublished, EpistemicType: evidencebook.ProducerClaim,
		SubjectRef: strings.Repeat("e", 64),
		Payloads:   [][]byte{stored.Capsule, stored.ProducerEnvelope, original},
	})
	require.NoError(t, err)
	interim, err := opened.book.Query(t.Context(), evidencebook.Filter{RecordType: recordTypePublished, SubjectRef: strings.Repeat("e", 64)})
	require.NoError(t, err)
	require.Len(t, interim, 1)
	_, err = opened.book.Checkpoint(t.Context())
	require.NoError(t, err)
	require.NoError(t, opened.release())
	root := interim[0].RecordID
	encodedOriginal := base64.RawURLEncoding.EncodeToString(original)

	shipped, err := invoke(t, "", "disclose", "--profile", p.Name, "--root", root)
	require.NoError(t, err)
	assert.Contains(t, shipped, encodedOriginal, "unsuppressed, the committed original is a payload like any other")

	_, err = invoke(t, "", "disclose", "--profile", p.Name, "--root", root, "--suppress", "agent_input")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "needs --payloads selected")

	withheld, err := invoke(t, "", "disclose", "--profile", p.Name, "--root", root, "--suppress", "agent_input", "--payloads", "selected")
	require.NoError(t, err)
	assert.NotContains(t, withheld, encodedOriginal, "suppressed: the original's bytes do not ship")
	_, err = evidencebook.VerifyBundle([]byte(strings.TrimSpace(withheld)))
	require.NoError(t, err)
}
