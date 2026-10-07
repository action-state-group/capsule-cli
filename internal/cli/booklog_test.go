package cli

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/action-state-group/checkpointed-local-log/go/checkpoint"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	clljsonl "github.com/action-state-group/checkpointed-local-log/go/store/jsonl"
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
		if e.CapsuleID == published.CapsuleID {
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

	_, err := runMigrate(t)
	require.ErrorIs(t, err, ErrInput, "a migration always needs a new --log-id")
	result, err := runMigrate(t, "--log-id", "a-book-v2")
	require.NoError(t, err)
	assert.Equal(t, uint64(3), result.Backfilled)
	assert.Equal(t, "a-book-v2", result.LogID)
	retiredLogID := p.LogID
	p, err = loadProfile("a")
	require.NoError(t, err)

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
		assert.Equal(t, retiredLogID, st.RetiredLogID)
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
	listedBackfill := map[string]bookEntry{}
	for _, e := range listLog(t, "a").Entries {
		listedBackfill[e.CapsuleID] = e
	}
	digest := listedBackfill[hex.EncodeToString(entries[2].Value)]
	assert.False(t, digest.CapsuleCarried, "a retired disclosure digest is never listed as a carried capsule")
	assert.Equal(t, recordTypeBackfilled, digest.RecordType)

	size := bookSize(t, p)
	again, err := runMigrate(t)
	require.NoError(t, err, "a repeat migration is a no-op")
	assert.Equal(t, result.Migration, again.Migration)
	assert.Equal(t, size, bookSize(t, p))

	published := publishFile(t, "a", sealRequestFile(t, "after-migration"))
	assert.Greater(t, published.Sequence, records[2].Seq)
	legacy := listLog(t, "a").Entries
	assert.Equal(t, hex.EncodeToString(entries[0].Value), legacy[0].CapsuleID)

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
	assert.Positive(t, st.RetiredCheckpoint)
	assert.Regexp(t, `^[0-9a-f]{64}$`, st.CheckpointDigest)
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
	target := p
	target.LogID = "a-book-v2"
	opened, err := openBookUnguarded(t.Context(), target, true)
	require.NoError(t, err)
	require.NoError(t, appendBackfilled(t.Context(), opened.book, nil, p.LogID, entries[0], false, len(entries), day1))
	require.NoError(t, opened.release())

	result, err := runMigrate(t, "--log-id", "a-book-v2")
	require.NoError(t, err)
	assert.Equal(t, uint64(2), result.Backfilled)
	listed := listLog(t, "a").Entries
	var backfilled []string
	for _, e := range listed {
		if e.RecordType == recordTypeBackfilled {
			backfilled = append(backfilled, e.CapsuleID)
		}
	}
	assert.Equal(t, []string{hex.EncodeToString(entries[0].Value), hex.EncodeToString(entries[1].Value)}, backfilled)

	// A book whose backfilled history is out of the retired order is refused.
	o, _ := bookProfile(t, "o")
	out := legacyLog(t, o, 2, 0, false)
	oTarget := o
	oTarget.LogID = "o-book-v2"
	wrong, err := openBookUnguarded(t.Context(), oTarget, true)
	require.NoError(t, err)
	require.NoError(t, appendBackfilled(t.Context(), wrong.book, nil, o.LogID, out[1], false, len(out), day1))
	require.NoError(t, wrong.release())
	_, err = invoke(t, "", "store", "migrate", "--profile", "o", "--log-id", "o-book-v2")
	assert.ErrorIs(t, err, ErrConflict)

	q, _ := bookProfile(t, "q")
	legacyLog(t, q, 1, 0, false)
	foreign, err := openBookUnguarded(t.Context(), q, true)
	require.NoError(t, err)
	_, err = foreign.book.Append(t.Context(), evidencebook.Entry{RecordType: "exchange", EpistemicType: evidencebook.ObservedEvent})
	require.NoError(t, err)
	require.NoError(t, foreign.release())
	_, err = invoke(t, "", "store", "migrate", "--profile", "q", "--log-id", "q-book-v2")
	assert.ErrorIs(t, err, ErrInput, "migration must come first in a book")
	assert.ErrorContains(t, err, "move the book/ directory aside")
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

// After a migration, an older binary cutting a checkpoint on the retired
// file changes it from what was migrated; every command then refuses the
// profile rather than carrying on beside a moving retired log.
func TestRetiredCheckpointAfterMigrationIsRefused(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bookClock(t, day1)
	p, _ := bookProfile(t, "a")
	legacyLog(t, p, 1, 0, false)
	_, err := runMigrate(t, "--log-id", "a-book-v2")
	require.NoError(t, err)
	_, err = invoke(t, "", "cll", "list", "--profile", "a")
	require.NoError(t, err)

	log, err := clljsonl.Open(retiredLogPath(p))
	require.NoError(t, err)
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
	require.NoError(t, log.Close())

	for _, args := range [][]string{{"cll", "list", "--profile", "a"}, {"cll", "checkpoint", "create", "--profile", "a"}} {
		_, err = invoke(t, "", args...)
		assert.ErrorIs(t, err, ErrConflict, "%v", args)
	}
}

func TestRetiredFingerprintSeesARewriteOfTheSameLength(t *testing.T) {
	at := day0
	a := retiredLog{entries: []cll.Entry{{Seq: 1, Value: bytes.Repeat([]byte{1}, 32), AppendedAt: at}}}
	b := retiredLog{entries: []cll.Entry{{Seq: 1, Value: bytes.Repeat([]byte{2}, 32), AppendedAt: at}}}
	entries, checkpoint := a.fingerprint()
	st := migrationStatement{RetiredEntries: 1, EntriesDigest: entries, CheckpointDigest: checkpoint}
	assert.True(t, st.matches(a))
	assert.False(t, st.matches(b), "same length, different value")
	a.checkpoint = &cll.CheckpointState{Bytes: []byte("statement"), Size: 1}
	assert.False(t, st.matches(a), "a checkpoint that was not there at migration")
}

// A migration interrupted after its record but before the profile save is
// finished by re-running it; a repeat after success changes nothing.
func TestMigrateFinishesAnInterruptedProfileSave(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bookClock(t, day1)
	p, _ := bookProfile(t, "a")
	legacyLog(t, p, 1, 0, true)
	first, err := runMigrate(t, "--log-id", "a-book-v2")
	require.NoError(t, err)
	require.NoError(t, saveProfile(p, true)) // the save that did not happen
	_, err = invoke(t, "", "cll", "list", "--profile", "a")
	require.Error(t, err, "the profile still names the retired log")

	_, err = runMigrate(t)
	assert.ErrorContains(t, err, "re-run 'store migrate' with that --log-id", "a re-run without the --log-id says what to do")

	again, err := runMigrate(t, "--log-id", "a-book-v2")
	require.NoError(t, err)
	assert.Equal(t, first.Migration, again.Migration)
	saved, err := loadProfile("a")
	require.NoError(t, err)
	assert.Equal(t, "a-book-v2", saved.LogID)
	_, err = invoke(t, "", "cll", "list", "--profile", "a")
	require.NoError(t, err)

	size := bookSize(t, saved)
	repeat, err := runMigrate(t)
	require.NoError(t, err, "a plain repeat after success is a no-op")
	assert.Equal(t, first.Migration, repeat.Migration)
	assert.Equal(t, size, bookSize(t, saved))
}

// A book an earlier build checkpointed as "<log_id>/book" is refused when
// opened, with its log id named, instead of failing at its next checkpoint.
func TestBookCheckpointedUnderAnotherLogIDIsRefused(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bookClock(t, day1)
	p, _ := bookProfile(t, "a")
	// The layout the earlier build wrote: book id = log_id, log id suffixed.
	dir := filepath.Join(p.Connection.Database, "book")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	checkpointKey, err := privateKey(p.Checkpoint.Signing)
	require.NoError(t, err)
	recordKey, err := privateKey(p.Signing)
	require.NoError(t, err)
	substrate, err := evidencebook.OpenCLL(filepath.Join(dir, "log.jsonl"), p.LogID+"/book", checkpointKey)
	require.NoError(t, err)
	store, err := evidencebook.OpenFileStore(filepath.Join(dir, "records"))
	require.NoError(t, err)
	payloads, err := evidencebook.OpenPayloadDir(filepath.Join(dir, "payloads"))
	require.NoError(t, err)
	signer, err := evidencebook.NewEd25519Signer(recordKey)
	require.NoError(t, err)
	old, err := evidencebook.Open(t.Context(), evidencebook.Config{BookID: p.LogID, Operator: p.Operator, Store: store, Substrate: substrate, Payloads: payloads, Signer: signer, Now: bookNow})
	require.NoError(t, err)
	_, err = old.Append(t.Context(), evidencebook.Entry{RecordType: "exchange", EpistemicType: evidencebook.ObservedEvent})
	require.NoError(t, err)
	_, err = old.Checkpoint(t.Context())
	require.NoError(t, err)
	require.NoError(t, old.Release())
	_, err = openBook(t.Context(), p, false)
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, err.Error(), p.LogID+"/book")
}

// A retired capsule the artifact store holds but no longer verifies under
// the profile's trusted keys is recorded as omitted, not a migration stop.
func TestMigrateRecordsACapsuleUnderARotatedKeyAsOmitted(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bookClock(t, day1)
	p, _ := bookProfile(t, "a")
	legacyLog(t, p, 1, 0, false)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	p.Signing.Value = hex.EncodeToString(private.Seed())
	p.TrustedKeys = []string{hex.EncodeToString(public)}
	require.NoError(t, saveProfile(p, true))
	_, err = runMigrate(t, "--log-id", "a-book-v2")
	require.NoError(t, err)
	p, err = loadProfile("a")
	require.NoError(t, err)
	opened, err := openBook(t.Context(), p, false)
	require.NoError(t, err)
	defer func() { require.NoError(t, opened.release()) }()
	records, err := opened.book.Query(t.Context(), evidencebook.Filter{RecordType: recordTypeBackfilled})
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Empty(t, records[0].Header.PayloadCommitments)
	var st backfillStatement
	require.NoError(t, json.Unmarshal(records[0].Header.Statement, &st))
	assert.Equal(t, "untrusted_signer", st.CapsuleOmitted)
}

// A capsule backfilled without its bytes is not treated as already
// carried: appending it commits a published record that carries it.
func TestAppendAfterAPayloadlessBackfillCarriesTheCapsule(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bookClock(t, day1)
	p, _ := bookProfile(t, "a")
	entries := legacyLog(t, p, 1, 0, false)
	target := p
	target.LogID = "a-book-v2"
	opened, err := openBookUnguarded(t.Context(), target, true)
	require.NoError(t, err)
	require.NoError(t, appendBackfilled(t.Context(), opened.book, nil, p.LogID, entries[0], false, 1, day1))
	require.NoError(t, opened.release())
	_, err = runMigrate(t, "--log-id", "a-book-v2")
	require.NoError(t, err)

	id := hex.EncodeToString(entries[0].Value)
	capsulePath := filepath.Join(t.TempDir(), "retired.json")
	_, err = invoke(t, "", "get", "--profile", "a", "--capsule-id", id, "--raw", "--output", capsulePath)
	require.NoError(t, err)
	_, err = invoke(t, "", "cll", "append", "--profile", "a", "--capsule", capsulePath)
	require.NoError(t, err)
	var carried bookEntry
	for _, e := range listLog(t, "a").Entries {
		if e.CapsuleID == id && e.CapsuleCarried {
			carried = e
		}
	}
	assert.Equal(t, recordTypePublished, carried.RecordType)
}

func TestBookBundleRefusesARootOnlyClosure(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bookClock(t, day0)
	storedProfile(t, "a")
	published := publishFile(t, "a", sealRequestFile(t, "published-1"))
	_, err := invoke(t, "", "bundle", "--profile", "a", "--root", published.CapsuleID, "--closure-depth", "0")
	assert.ErrorIs(t, err, ErrInput)
}

// tamperRetiredEntry rewrites one entry's value in a retired cll.jsonl the
// way an attacker with write access to the store directory could.
func tamperRetiredEntry(t *testing.T, p Profile, seq uint64) {
	t.Helper()
	path := retiredLogPath(p)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n"))
	tampered := false
	for i, line := range lines {
		var event map[string]any
		require.NoError(t, json.Unmarshal(line, &event))
		entry, ok := event["entry"].(map[string]any)
		if event["type"] != "entry.append" || !ok || fmt.Sprint(entry["seq"]) != fmt.Sprint(seq) {
			continue
		}
		value, ok := entry["value"].(string)
		require.True(t, ok, "entry value is a string")
		// Swap the first character for another one of the same alphabet,
		// so the value stays well-formed and only its meaning changes.
		forged := []byte(value)
		if forged[0] == 'A' {
			forged[0] = 'B'
		} else {
			forged[0] = 'A'
		}
		entry["value"] = string(forged)
		lines[i], err = json.Marshal(event)
		require.NoError(t, err)
		tampered = true
	}
	require.True(t, tampered, "entry %d found", seq)
	require.NoError(t, os.WriteFile(path, append(bytes.Join(lines, []byte("\n")), '\n'), 0o600))
}

// A checkpointed retired log whose entries no longer rebuild the root
// its checkpoint signed is refused before anything is carried into the
// book; so is one whose checkpoint does not verify under a trusted key.
func TestMigrateRefusesATamperedCheckpointedRetiredLog(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bookClock(t, day1)
	p, _ := bookProfile(t, "a")
	legacyLog(t, p, 2, 1, true)
	tamperRetiredEntry(t, p, 2)
	_, err := runMigrate(t, "--log-id", "a-book-v2")
	require.ErrorIs(t, err, ErrConflict)
	assert.NoFileExists(t, filepath.Join(p.Connection.Database, "book", "log.jsonl"), "nothing was carried into a book")
	saved, err := loadProfile("a")
	require.NoError(t, err)
	assert.Equal(t, p.LogID, saved.LogID, "the profile is not repointed")

	// The retired checkpoint was signed with the checkpoint key this profile
	// has since rotated away from: it no longer verifies under a trusted key.
	q, _ := bookProfile(t, "q")
	legacyLog(t, q, 1, 0, true)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	q.Checkpoint.Signing.Value = hex.EncodeToString(private.Seed())
	q.Checkpoint.TrustedKeys = []string{hex.EncodeToString(public)}
	require.NoError(t, saveProfile(q, true))
	_, err = invoke(t, "", "store", "migrate", "--profile", "q", "--log-id", "q-book-v2")
	require.ErrorIs(t, err, ErrConflict)
	assert.NoFileExists(t, filepath.Join(q.Connection.Database, "book", "log.jsonl"))
	assert.Equal(t, ErrConflict.Error()+": the retired log's checkpoint does not verify: its signature, its log_id, or its signer (which must be one of the profile's checkpoint trusted_keys); nothing was migrated", SafeError(err), "the operator sees fixed text, not the verifier's error")
}

// A checkpointed retired log that verifies marks exactly the entries its
// checkpoint commits as anchored; an entry appended after it is not.
func TestMigrateMarksWhichEntriesTheRetiredCheckpointAnchors(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bookClock(t, day1)
	p, _ := bookProfile(t, "a")
	legacyLog(t, p, 2, 0, true)
	log, err := clljsonl.Open(retiredLogPath(p))
	require.NoError(t, err)
	_, err = log.Append(t.Context(), cll.AppendInput{Value: bytes.Repeat([]byte{0xab}, 32), AppendedAt: day0})
	require.NoError(t, err)
	require.NoError(t, log.Close())

	_, err = runMigrate(t, "--log-id", "a-book-v2")
	require.NoError(t, err)
	saved, err := loadProfile("a")
	require.NoError(t, err)
	opened, err := openBook(t.Context(), saved, false)
	require.NoError(t, err)
	defer func() { require.NoError(t, opened.release()) }()
	records, err := opened.book.Query(t.Context(), evidencebook.Filter{RecordType: recordTypeBackfilled})
	require.NoError(t, err)
	require.Len(t, records, 3)
	var anchored []bool
	for _, r := range records {
		var st backfillStatement
		require.NoError(t, json.Unmarshal(r.Header.Statement, &st))
		anchored = append(anchored, st.Anchored)
		assert.Equal(t, string(evidencebook.ProducerClaim), string(r.Header.EpistemicType))
	}
	assert.Equal(t, []bool{true, true, false}, anchored)
	st, _, _, err := migrationRecord(t.Context(), opened.book)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), st.AnchoredEntries)
}

// A read-only jsonl profile holding no signing or checkpoint secret can
// list its log and read witness status, even while a writer holds the book,
// and reading never writes: a torn line a writer is still appending stays.
func TestReadOnlyJSONLProfileReadsWithoutSecretsOrTheWriterLock(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bookClock(t, day0)
	p, _ := storedProfile(t, "a")
	witnessKey, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	p.Checkpoint.Endpoint = "https://witness.example.invalid"
	p.Checkpoint.PublicKey = hex.EncodeToString(witnessKey)
	require.NoError(t, saveProfile(p, true))
	published := publishFile(t, "a", sealRequestFile(t, "published-1"))
	out, err := invoke(t, "", "cll", "checkpoint", "create", "--profile", "a")
	require.NoError(t, err)
	var created struct {
		Checkpoint uint64 `json:"checkpoint"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &created))

	reader := p
	reader.Name, reader.ReadOnly = "reader", true
	reader.Signing, reader.Checkpoint.Signing = Secret{}, Secret{}
	require.NoError(t, saveProfile(reader, false))

	writer, err := openBook(t.Context(), p, false)
	require.NoError(t, err)
	defer func() { require.NoError(t, writer.release()) }()
	journal := filepath.Join(p.Connection.Database, "book", "records", "records.jsonl")
	f, err := os.OpenFile(journal, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString(`{"seq":99,"record_id":"in-flight`)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	before, err := os.ReadFile(journal)
	require.NoError(t, err)

	listed := listLog(t, "reader")
	require.NotEmpty(t, listed.Entries)
	assert.Equal(t, published.CapsuleID, listed.Entries[0].CapsuleID)
	_, err = invoke(t, "", "cll", "checkpoint", "status", "--profile", "reader", "--checkpoint", fmt.Sprint(created.Checkpoint))
	require.NoError(t, err)
	after, err := os.ReadFile(journal)
	require.NoError(t, err)
	assert.Equal(t, before, after, "reading repaired nothing")
}

// cll list keeps the contract every backend shares -- capsule_id is the
// published capsule, usable with get -- and lists only log entries that
// record a capsule unless --all asks for the book's internal records.
func TestCllListKeepsTheCapsuleContract(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0)
	storedProfile(t, "a")
	published := publishFile(t, "a", sealRequestFile(t, "published-1"))
	_, err := invoke(t, "", "cll", "checkpoint", "create", "--profile", "a")
	require.NoError(t, err)
	set(day1)
	_, err = runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b")
	require.NoError(t, err)

	listed := listLog(t, "a").Entries
	require.Len(t, listed, 1, "only the published capsule by default")
	assert.Equal(t, published.CapsuleID, listed[0].CapsuleID)
	assert.True(t, listed[0].CapsuleCarried)
	_, err = invoke(t, "", "get", "--profile", "a", "--capsule-id", listed[0].CapsuleID)
	require.NoError(t, err, "capsule_id feeds get --capsule-id, as on every backend")

	out, err := invoke(t, "", "cll", "list", "--profile", "a", "--all", "--limit", "1000")
	require.NoError(t, err)
	var all listedOut
	require.NoError(t, json.Unmarshal([]byte(out), &all))
	kinds := map[string]bool{}
	for _, e := range all.Entries {
		kinds[e.RecordType] = true
		if e.RecordType != recordTypePublished && e.RecordType != recordTypeBackfilled {
			assert.Empty(t, e.CapsuleID, "an internal record names no capsule")
		}
	}
	assert.True(t, kinds[evidencebook.RecordTypeIndexRoot] && kinds[evidencebook.RecordTypeClose], "--all lists the internal records")
}

// The file listing must agree with the book's own query, record for record,
// on a history holding every kind of record this CLI writes.
func TestBookFileListingAgreesWithTheBook(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0)
	p, _ := storedProfile(t, "a")
	publishFile(t, "a", sealRequestFile(t, "published-1"))
	appendHalves(t, p, half{"x1", "r1", "p1"})
	set(day1)
	_, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b", "--bundle-out", filepath.Join(t.TempDir(), "b.json"))
	require.NoError(t, err)

	fromFiles, err := readBookFiles(p)
	require.NoError(t, err)
	opened, err := openBook(t.Context(), p, false)
	require.NoError(t, err)
	defer func() { require.NoError(t, opened.release()) }()
	fromBook, err := opened.book.Query(t.Context(), evidencebook.Filter{})
	require.NoError(t, err)
	require.Equal(t, len(fromBook), len(fromFiles))
	for i := range fromBook {
		assert.Equal(t, fromBook[i].RecordID, fromFiles[i].RecordID)
		assert.Equal(t, fromBook[i].Seq, fromFiles[i].Seq)
		assert.Equal(t, fromBook[i].Header.RecordType, fromFiles[i].Header.RecordType)
		assert.Equal(t, fromBook[i].Header.SubjectRef, fromFiles[i].Header.SubjectRef)
		assert.Equal(t, fromBook[i].Header.CommittedAt, fromFiles[i].Header.CommittedAt)
	}
}

// The read-only listing takes order from the log and refuses a record
// journal whose header puts a record at a different position.
func TestBookFileListingRefusesAJournalThatDisagreesWithTheLog(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bookClock(t, day0)
	p, _ := storedProfile(t, "a")
	publishFile(t, "a", sealRequestFile(t, "published-1"))
	journal := filepath.Join(p.Connection.Database, "book", "records", "records.jsonl")
	raw, err := os.ReadFile(journal)
	require.NoError(t, err)
	lines := bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n"))
	var stored evidencebook.StoredRecord
	require.NoError(t, json.Unmarshal(lines[0], &stored))
	var header map[string]any
	require.NoError(t, json.Unmarshal(stored.Header, &header))
	header["seq"] = 7
	stored.Header, err = json.Marshal(header)
	require.NoError(t, err)
	lines[0], err = json.Marshal(stored)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(journal, append(bytes.Join(lines, []byte("\n")), '\n'), 0o600))
	_, err = invoke(t, "", "cll", "list", "--profile", "a")
	assert.ErrorIs(t, err, cll.ErrCorrupt)
}

// stripRetiredCommits removes every signed cll.commit line from a retired
// cll.jsonl, so it reads as a log that was never checkpointed.
func stripRetiredCommits(t *testing.T, p Profile) {
	t.Helper()
	raw, err := os.ReadFile(retiredLogPath(p))
	require.NoError(t, err)
	var kept [][]byte
	for _, line := range bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n")) {
		if !bytes.Contains(line, []byte(`"type":"cll.commit"`)) {
			kept = append(kept, line)
		}
	}
	require.NoError(t, os.WriteFile(retiredLogPath(p), append(bytes.Join(kept, []byte("\n")), '\n'), 0o600))
}

// On a migrated profile, checking the retired cll.jsonl takes
// no lock and needs no write access. A 0444 file, or an older binary holding
// the file's lock, stops neither a reader's list nor a writer's publish.
func TestRetiredLogCheckTakesNoLockAndNoWriteAccess(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bookClock(t, day1)
	p, _ := bookProfile(t, "a")
	legacyLog(t, p, 1, 0, false)
	_, err := runMigrate(t, "--log-id", "a-book-v2")
	require.NoError(t, err)

	require.NoError(t, os.Chmod(retiredLogPath(p), 0o444))
	listLog(t, "a")
	publishFile(t, "a", sealRequestFile(t, "while-read-only"))
	require.NoError(t, os.Chmod(retiredLogPath(p), 0o600))

	holder, err := clljsonl.Open(retiredLogPath(p))
	require.NoError(t, err, "an older binary holds the retired file's lock")
	defer func() { require.NoError(t, holder.Close()) }()
	listLog(t, "a")
	publishFile(t, "a", sealRequestFile(t, "while-locked"))
}

// The lock-free scan and the locked read agree, fingerprint for
// fingerprint, on a checkpointed retired log with entries past it.
func TestRetiredLogScanAgreesWithTheLockedRead(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bookClock(t, day1)
	p, _ := bookProfile(t, "a")
	legacyLog(t, p, 2, 1, true)
	log, err := clljsonl.Open(retiredLogPath(p))
	require.NoError(t, err)
	_, err = log.Append(t.Context(), cll.AppendInput{Value: bytes.Repeat([]byte{0xab}, 32), AppendedAt: day0})
	require.NoError(t, err)
	require.NoError(t, log.Close())

	scanned, err := scanRetiredLog(p)
	require.NoError(t, err)
	locked, release, err := openRetiredLog(t.Context(), p)
	require.NoError(t, err)
	require.NoError(t, release())
	require.Len(t, scanned.entries, 4)
	require.NotNil(t, scanned.checkpoint)
	assert.Equal(t, locked.checkpoint.Size, scanned.checkpoint.Size)
	se, sc := scanned.fingerprint()
	le, lc := locked.fingerprint()
	assert.Equal(t, le, se)
	assert.Equal(t, lc, sc)
}

// Stripping the signed commit line from a tampered checkpointed
// log makes it read as never checkpointed. Migration still needs a new log
// id, so the forged history can never share the retired log's (log_id, key).
func TestMigrateOfAStrippedTamperedLogStillNeedsANewLogID(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bookClock(t, day1)
	p, _ := bookProfile(t, "a")
	legacyLog(t, p, 2, 1, true)
	tamperRetiredEntry(t, p, 2)
	stripRetiredCommits(t, p)
	_, err := runMigrate(t)
	require.ErrorIs(t, err, ErrInput)
	assert.NoFileExists(t, filepath.Join(p.Connection.Database, "book", "log.jsonl"))
	_, err = runMigrate(t, "--log-id", p.LogID)
	require.ErrorIs(t, err, ErrInput, "the retired log's own id is not a new one")
}

// store init signs nothing, so a jsonl profile that only verifies other
// producers' capsules -- no signing key, its trusted keys all someone
// else's -- can initialize its store; its first write is what needs keys.
func TestStoreInitOnJSONLNeedsNoKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	other, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	p := Profile{Name: "verifier", Type: "jsonl", LogID: "verifier-log", Namespace: "capsule"}
	p.Connection.Database = filepath.Join(t.TempDir(), "store")
	p.TrustedKeys = []string{hex.EncodeToString(other)}
	require.NoError(t, saveProfile(p, false))
	_, err = invoke(t, "", "store", "init", "--profile", "verifier")
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(p.Connection.Database, "book", "log.jsonl"))
	listLog(t, "verifier")
	_, err = invoke(t, "", "publish", "--profile", "verifier", "--request", sealRequestFile(t, "needs-keys"))
	assert.ErrorIs(t, err, ErrInput, "writing is what needs the keys")
}

// The one-log refusals tell the operator what to do; the binary must print
// those instructions, not only the error class.
func TestOneLogRefusalsReachStderr(t *testing.T) {
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	bookClock(t, day1)
	bin := capsulectlBinary(t)
	run := func(args ...string) (string, int) {
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+config)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		var exit *exec.ExitError
		require.ErrorAs(t, err, &exit, "expected a refusal from %v", args)
		return stderr.String(), exit.ExitCode()
	}

	p, _ := bookProfile(t, "a")
	legacyLog(t, p, 1, 0, false)
	stderr, code := run("cll", "list", "--profile", "a")
	assert.Equal(t, 2, code)
	assert.Contains(t, stderr, "holds entries not yet in its book; run 'store migrate --log-id <new-log-id>'")
	stderr, code = run("store", "migrate", "--profile", "a")
	assert.Equal(t, 2, code)
	assert.Contains(t, stderr, "store migrate needs --log-id with a new log id")

	q, _ := bookProfile(t, "q")
	legacyLog(t, q, 1, 0, false)
	target := q
	target.LogID = "q-book-v2"
	foreign, err := openBookUnguarded(t.Context(), target, true)
	require.NoError(t, err)
	_, err = foreign.book.Append(t.Context(), evidencebook.Entry{RecordType: "exchange", EpistemicType: evidencebook.ObservedEvent})
	require.NoError(t, err)
	require.NoError(t, foreign.release())
	stderr, code = run("store", "migrate", "--profile", "q", "--log-id", "q-book-v2")
	assert.Equal(t, 2, code)
	assert.Contains(t, stderr, "move the book/ directory aside (keep it)")

	r, _ := bookProfile(t, "r")
	legacyLog(t, r, 1, 0, false)
	_, err = invoke(t, "", "store", "migrate", "--profile", "r", "--log-id", "r-book-v2")
	require.NoError(t, err)
	log, err := clljsonl.Open(retiredLogPath(r))
	require.NoError(t, err)
	_, err = log.Append(t.Context(), cll.AppendInput{Value: bytes.Repeat([]byte{0xee}, 32), AppendedAt: day1})
	require.NoError(t, err)
	require.NoError(t, log.Close())
	stderr, code = run("cll", "list", "--profile", "r")
	assert.Equal(t, 5, code)
	assert.Contains(t, stderr, "the retired cll.jsonl changed after it was migrated")
}
