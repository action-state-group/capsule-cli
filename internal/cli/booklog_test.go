package cli

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/action-state-group/cll-go/checkpoint"
	"github.com/action-state-group/cll-go/cll"
	clljsonl "github.com/action-state-group/cll-go/store/jsonl"
	"github.com/action-state-group/evidencebook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storedProfile is a bookProfile whose store has been initialized, the way
// an operator starts one.
func storedProfile(t *testing.T, name string) (Profile, bookKeys) {
	t.Helper()
	p, keys := bookProfile(t, name)
	_, err := invoke(t, "", "store", "init", "--profile", name)
	require.NoError(t, err)
	return p, keys
}

func sealRequestFile(t *testing.T, action string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), action+".json")
	body := fmt.Sprintf(`{"spec_version":"capsule-seal-request/v1","capsule":{"ActionID":%q,"ActionType":"fyi","Operator":"test-operator","Developer":"test-developer","Timestamp":"2026-09-08T00:00:00Z"},"payload":{"a":1},"agent_output":{"ok":true}}`, action)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

type publishedOut struct {
	CapsuleID string `json:"capsule_id"`
	Sequence  uint64 `json:"sequence"`
	State     string `json:"state"`
}

func publishFile(t *testing.T, profile, request string) publishedOut {
	t.Helper()
	out, err := invoke(t, "", "publish", "--profile", profile, "--request", request)
	require.NoError(t, err)
	var p publishedOut
	require.NoError(t, json.Unmarshal([]byte(out), &p))
	return p
}

type listedOut struct {
	Entries []bookEntry `json:"entries"`
}

func listLog(t *testing.T, profile string) listedOut {
	t.Helper()
	out, err := invoke(t, "", "cll", "list", "--profile", profile, "--limit", "1000")
	require.NoError(t, err)
	var l listedOut
	require.NoError(t, json.Unmarshal([]byte(out), &l))
	return l
}

// TestPublishedCapsuleIsClosedAndBundled is the one-log round trip: a
// capsule `publish` commits is in the log `cll list` reads, inside the window
// of the Close for its day, disclosed by that Close's bundle, and both the
// Close and the published capsule pass `verify --capsule`.
func TestPublishedCapsuleIsClosedAndBundled(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0)
	p, _ := storedProfile(t, "a")
	published := publishFile(t, "a", sealRequestFile(t, "published-1"))
	assert.Equal(t, "appended", published.State)

	listed := listLog(t, "a")
	var found bookEntry
	for _, e := range listed.Entries {
		if e.PublishedCapsuleID == published.CapsuleID {
			found = e
		}
	}
	require.Equal(t, published.Sequence, found.Sequence, "cll list reads the book publish wrote")
	assert.Equal(t, recordTypePublished, found.RecordType)

	set(day1)
	dir := t.TempDir()
	closed, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b",
		"--capsule-out", filepath.Join(dir, "close.json"), "--bundle-out", filepath.Join(dir, "close-bundle.json"))
	require.NoError(t, err)
	window := closed.Reconciliation
	assert.True(t, window.FromSeq <= published.Sequence && published.Sequence <= window.ToSeq, "the published capsule is inside the day's Close")

	raw, err := os.ReadFile(filepath.Join(dir, "close-bundle.json"))
	require.NoError(t, err)
	verified, err := evidencebook.VerifyBundle(raw)
	require.NoError(t, err)
	disclosed := false
	for _, r := range verified.Records {
		disclosed = disclosed || (r.HeaderVerified && r.Header.SubjectRef == published.CapsuleID)
	}
	assert.True(t, disclosed, "the Close's bundle discloses the published capsule's record")

	_, err = invoke(t, "", "verify", "--profile", "a", "--capsule", filepath.Join(dir, "close.json"))
	require.NoError(t, err)
	capsulePath := filepath.Join(dir, "published.json")
	_, err = invoke(t, "", "get", "--profile", "a", "--capsule-id", published.CapsuleID, "--raw", "--output", capsulePath)
	require.NoError(t, err)
	_, err = invoke(t, "", "verify", "--profile", "a", "--capsule", capsulePath)
	require.NoError(t, err)
	assert.NoFileExists(t, retiredLogPath(p), "nothing wrote a second log")
}

func TestPublishAndAppendAreIdempotentOnTheBook(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bookClock(t, day0)
	p, _ := storedProfile(t, "a")
	request := sealRequestFile(t, "published-1")
	first := publishFile(t, "a", request)
	size := bookSize(t, p)
	again := publishFile(t, "a", request)
	assert.Equal(t, first, again)

	capsulePath := filepath.Join(t.TempDir(), "published.json")
	_, err := invoke(t, "", "get", "--profile", "a", "--capsule-id", first.CapsuleID, "--raw", "--output", capsulePath)
	require.NoError(t, err)
	out, err := invoke(t, "", "cll", "append", "--profile", "a", "--capsule", capsulePath)
	require.NoError(t, err)
	var appended publishedOut
	require.NoError(t, json.Unmarshal([]byte(out), &appended))
	assert.Equal(t, first.Sequence, appended.Sequence)
	assert.Equal(t, size, bookSize(t, p), "neither repeat added a record")
}

// legacyLog writes what an earlier capsulectl left in a jsonl store: a
// cll.jsonl whose entries are capsule ids (or, for a disclose act, a bare
// digest), and the capsules themselves in artifacts.jsonl.
func legacyLog(t *testing.T, p Profile, capsules int, digests int, checkpointed bool) []cll.Entry {
	t.Helper()
	require.NoError(t, os.MkdirAll(p.Connection.Database, 0o700))
	keys, err := parseKeys(p.TrustedKeys)
	require.NoError(t, err)
	store, err := newArtifactStore(p, nil, keys)
	require.NoError(t, err)
	require.NoError(t, store.Init(t.Context()))
	signing, err := privateKey(p.Signing)
	require.NoError(t, err)
	path := retiredLogPath(p)
	require.NoError(t, clljsonl.Init(path))
	log, err := clljsonl.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, log.Close()) }()
	at := day0.Add(-72 * time.Hour)
	for i := range capsules + digests {
		value := bytes.Repeat([]byte{byte(0xd0 + i)}, 32)
		if i < capsules {
			raw, err := os.ReadFile(sealRequestFile(t, fmt.Sprintf("legacy-%d", i)))
			require.NoError(t, err)
			request, err := parseRequest(raw)
			require.NoError(t, err)
			record, err := seal(request, signing)
			require.NoError(t, err)
			require.NoError(t, store.Put(t.Context(), record))
			value, err = hex.DecodeString(record.CapsuleID)
			require.NoError(t, err)
		}
		_, err = log.Append(t.Context(), cll.AppendInput{Value: value, AppendedAt: at.Add(time.Duration(i) * time.Hour)})
		require.NoError(t, err)
	}
	if checkpointed {
		key, err := privateKey(p.Checkpoint.Signing)
		require.NoError(t, err)
		signer, err := checkpoint.NewEd25519Signer(key)
		require.NoError(t, err)
		cfg := checkpoint.DefaultRunnerConfig(p.LogID)
		cfg.Cadence.CadenceEntries = 1
		runner, err := checkpoint.NewRunner(cfg, log, signer)
		require.NoError(t, err)
		_, err = runner.RunOnce(t.Context(), time.Now().UTC())
		require.NoError(t, err)
	}
	entries, err := log.ScanEntries(t.Context(), 0, cll.MaxScanLimit)
	require.NoError(t, err)
	return entries
}

func TestUnmigratedRetiredLogIsRefused(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bookClock(t, day1)
	p, _ := bookProfile(t, "a")
	legacyLog(t, p, 1, 0, false)
	for _, args := range [][]string{
		{"store", "init", "--profile", "a"},
		{"publish", "--profile", "a", "--request", sealRequestFile(t, "new")},
		{"cll", "list", "--profile", "a"},
		{"close", "--profile", "a", "--period", "day", "--counterparty", "b"},
	} {
		_, err := invoke(t, "", args...)
		assert.ErrorIs(t, err, ErrInput, "%v", args)
	}
}

func runMigrate(t *testing.T, args ...string) (migrateResult, error) {
	t.Helper()
	out, err := invoke(t, "", append([]string{"store", "migrate", "--profile", "a"}, args...)...)
	var r migrateResult
	if err == nil {
		require.NoError(t, json.Unmarshal([]byte(out), &r))
	}
	return r, err
}

func TestMigrateBackfillsTheRetiredLogInOrder(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	now := day1.Add(3 * time.Hour)
	bookClock(t, now)
	p, _ := bookProfile(t, "a")
	entries := legacyLog(t, p, 2, 1, false)

	result, err := runMigrate(t)
	require.NoError(t, err)
	assert.Equal(t, uint64(3), result.Backfilled)
	assert.Equal(t, p.LogID, result.LogID, "an uncheckpointed retired log keeps its log_id")

	opened, err := openBook(t.Context(), p, false)
	require.NoError(t, err)
	records, err := opened.book.Query(t.Context(), evidencebook.Filter{RecordType: recordTypeBackfilled})
	require.NoError(t, err)
	require.NoError(t, opened.release())
	require.Len(t, records, 3)
	for i, r := range records {
		assert.Equal(t, hex.EncodeToString(entries[i].Value), r.Header.SubjectRef, "retired order kept")
		var st backfillStatement
		require.NoError(t, json.Unmarshal(r.Header.Statement, &st))
		assert.Equal(t, entries[i].Seq, st.RetiredSeq)
		assert.Equal(t, "backfilled", st.ProvenanceMode.Mode)
		assert.Equal(t, entries[i].AppendedAt.UTC().Format(time.RFC3339Nano), st.ProvenanceMode.SourceAssertedAt)
		assert.Equal(t, now.UTC().Format(time.RFC3339Nano), st.ProvenanceMode.ImportedAt)
		assert.NotEqual(t, st.ProvenanceMode.SourceAssertedAt, st.ProvenanceMode.ImportedAt)
		if i < 2 {
			assert.Len(t, r.Header.PayloadCommitments, 2, "a retired capsule is carried into the book")
		} else {
			assert.Empty(t, r.Header.PayloadCommitments, "a bare digest has no capsule behind it")
		}
	}

	size := bookSize(t, p)
	again, err := runMigrate(t)
	require.NoError(t, err, "a repeat migration is a no-op")
	assert.Equal(t, result.Migration, again.Migration)
	assert.Equal(t, size, bookSize(t, p))

	published := publishFile(t, "a", sealRequestFile(t, "after-migration"))
	assert.Greater(t, published.Sequence, records[2].Seq)
	legacy := listLog(t, "a").Entries
	assert.Equal(t, hex.EncodeToString(entries[0].Value), legacy[0].PublishedCapsuleID)

	// An older binary appending to the retired log afterwards is caught.
	log, err := clljsonl.Open(retiredLogPath(p))
	require.NoError(t, err)
	_, err = log.Append(t.Context(), cll.AppendInput{Value: bytes.Repeat([]byte{0xee}, 32), AppendedAt: now})
	require.NoError(t, err)
	require.NoError(t, log.Close())
	_, err = invoke(t, "", "cll", "list", "--profile", "a")
	assert.ErrorIs(t, err, ErrConflict)
}

func TestMigrateACheckpointedRetiredLogNeedsANewLogID(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bookClock(t, day1)
	p, _ := bookProfile(t, "a")
	legacyLog(t, p, 1, 0, true)
	_, err := runMigrate(t)
	require.ErrorIs(t, err, ErrInput)
	_, err = runMigrate(t, "--log-id", p.LogID)
	require.ErrorIs(t, err, ErrInput)

	result, err := runMigrate(t, "--log-id", "a-book-v2")
	require.NoError(t, err)
	assert.Equal(t, "a-book-v2", result.LogID)
	assert.Equal(t, p.LogID, result.RetiredLogID)
	saved, err := loadProfile("a")
	require.NoError(t, err)
	assert.Equal(t, "a-book-v2", saved.LogID, "the profile now names the book's log")

	opened, err := openBook(t.Context(), saved, false)
	require.NoError(t, err)
	defer func() { require.NoError(t, opened.release()) }()
	st, _, ok, err := migrationRecord(t.Context(), opened.book)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Positive(t, st.RetiredCheckpointSize)
	assert.Regexp(t, `^[0-9a-f]{64}$`, st.RetiredCheckpointDigest)
	cp, err := opened.book.Checkpoint(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "a-book-v2", cp.LogID)
}

// An interrupted migration resumes where it stopped and never duplicates;
// a book that already holds other records is not migrated into.
func TestMigrateResumesAndRefusesAForeignBook(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bookClock(t, day1)
	p, _ := bookProfile(t, "a")
	entries := legacyLog(t, p, 2, 0, false)
	opened, err := openBookUnguarded(t.Context(), p, true)
	require.NoError(t, err)
	require.NoError(t, appendBackfilled(t.Context(), opened.book, nil, p.LogID, entries[0], len(entries), day1))
	require.NoError(t, opened.release())

	result, err := runMigrate(t)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), result.Backfilled)
	listed := listLog(t, "a").Entries
	var backfilled []string
	for _, e := range listed {
		if e.RecordType == recordTypeBackfilled {
			backfilled = append(backfilled, e.PublishedCapsuleID)
		}
	}
	assert.Equal(t, []string{hex.EncodeToString(entries[0].Value), hex.EncodeToString(entries[1].Value)}, backfilled)

	// A book whose backfilled history is out of the retired order is refused.
	o, _ := bookProfile(t, "o")
	out := legacyLog(t, o, 2, 0, false)
	wrong, err := openBookUnguarded(t.Context(), o, true)
	require.NoError(t, err)
	require.NoError(t, appendBackfilled(t.Context(), wrong.book, nil, o.LogID, out[1], len(out), day1))
	require.NoError(t, wrong.release())
	_, err = invoke(t, "", "store", "migrate", "--profile", "o")
	assert.ErrorIs(t, err, ErrConflict)

	q, _ := bookProfile(t, "q")
	legacyLog(t, q, 1, 0, false)
	foreign, err := openBookUnguarded(t.Context(), q, true)
	require.NoError(t, err)
	_, err = foreign.book.Append(t.Context(), evidencebook.Entry{RecordType: "exchange", EpistemicType: evidencebook.ObservedEvent})
	require.NoError(t, err)
	require.NoError(t, foreign.release())
	_, err = invoke(t, "", "store", "migrate", "--profile", "q")
	assert.ErrorIs(t, err, ErrInput, "migration must come first in a book")
}

func TestBookWitnessStoreCompareAndSet(t *testing.T) {
	store := bookWitnessStore{path: filepath.Join(t.TempDir(), "witness.json")}
	now := day1
	require.NoError(t, store.seed("w", 3, []byte("statement"), now))
	require.NoError(t, store.seed("w", 3, []byte("other"), now), "seeding twice keeps the first")
	got, err := store.GetWitness(t.Context(), "w", 3)
	require.NoError(t, err)
	assert.Equal(t, []byte("statement"), got.Checkpoint)
	_, err = store.GetWitness(t.Context(), "w", 4)
	assert.ErrorIs(t, err, cll.ErrNotFound)

	pending, err := store.PendingWitnesses(t.Context(), now, 10)
	require.NoError(t, err)
	assert.Len(t, pending, 1)

	next := got
	next.Attempts = 1
	assert.ErrorIs(t, store.CommitWitness(t.Context(), 1, next), cll.ErrContention, "stale attempts")
	changed := next
	changed.Checkpoint = []byte("other")
	assert.ErrorIs(t, store.CommitWitness(t.Context(), 0, changed), cll.ErrContention, "checkpoint bytes never change")
	next.Receipt = &cll.WitnessReceiptState{Bytes: []byte("receipt")}
	require.NoError(t, store.CommitWitness(t.Context(), 0, next))
	pending, err = store.PendingWitnesses(t.Context(), now, 10)
	require.NoError(t, err)
	assert.Empty(t, pending, "a received checkpoint is no longer pending")
}

// `cll checkpoint create` on a profile with a witness service records the
// book's checkpoint as due, and `cll checkpoint status` reads it back.
func TestBookCheckpointIsQueuedForTheWitness(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bookClock(t, day0)
	p, _ := storedProfile(t, "a")
	witnessKey, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	p.Checkpoint.Endpoint = "https://witness.example.invalid"
	p.Checkpoint.PublicKey = hex.EncodeToString(witnessKey)
	require.NoError(t, saveProfile(p, true))
	publishFile(t, "a", sealRequestFile(t, "published-1"))

	out, err := invoke(t, "", "cll", "checkpoint", "create", "--profile", "a")
	require.NoError(t, err)
	var created struct {
		Checkpoint uint64 `json:"checkpoint"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &created))
	out, err = invoke(t, "", "cll", "checkpoint", "status", "--profile", "a", "--checkpoint", fmt.Sprint(created.Checkpoint))
	require.NoError(t, err)
	var status struct {
		State string `json:"state"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &status))
	assert.Equal(t, "pending", status.State)
	_, err = invoke(t, "", "cll", "checkpoint", "status", "--profile", "a", "--checkpoint", fmt.Sprint(created.Checkpoint+1))
	assert.Error(t, err, "no witness state for a checkpoint that was not cut")
}

func TestCountersignRootRefusesABookProfile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bookClock(t, day0)
	storedProfile(t, "a")
	published := publishFile(t, "a", sealRequestFile(t, "published-1"))
	_, err := invoke(t, "", "countersign", "request", "--profile", "a", "--service", "https://countersign.example.invalid", "--window", "2026-09", "--root", published.CapsuleID, "--out", filepath.Join(t.TempDir(), "b.json"))
	assert.ErrorIs(t, err, errBookProfile)
}
