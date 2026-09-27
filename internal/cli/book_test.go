package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/capsule-emit-go/artifact"
	"github.com/action-state-group/evidencebook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bookClock pins bookNow for one test and returns a setter.
func bookClock(t *testing.T, start time.Time) func(time.Time) {
	t.Helper()
	now := start
	previous := bookNow
	bookNow = func() time.Time { return now }
	t.Cleanup(func() { bookNow = previous })
	return func(next time.Time) { now = next }
}

type bookKeys struct {
	record, checkpoint ed25519.PublicKey
}

// bookProfile saves a jsonl profile whose record and checkpoint keys are
// distinct, so a test can tell which key signed what.
func bookProfile(t *testing.T, name string) (Profile, bookKeys) {
	t.Helper()
	recordPub, recordKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	checkpointPub, checkpointKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	p := Profile{Name: name, Type: "jsonl", LogID: name, Namespace: "capsule", Operator: name + "-operator"}
	p.Connection.Database = filepath.Join(t.TempDir(), name)
	p.Signing.Value = hex.EncodeToString(recordKey.Seed())
	p.TrustedKeys = []string{hex.EncodeToString(recordPub)}
	p.Checkpoint.Signing.Value = hex.EncodeToString(checkpointKey.Seed())
	p.Checkpoint.TrustedKeys = []string{hex.EncodeToString(checkpointPub)}
	require.NoError(t, saveProfile(p, false))
	return p, bookKeys{record: recordPub, checkpoint: checkpointPub}
}

// half is one side's record of one exchange.
type half struct {
	exchange, request string
	payload           string
}

func appendHalves(t *testing.T, p Profile, halves ...half) {
	t.Helper()
	opened, err := openBook(t.Context(), p, true)
	require.NoError(t, err)
	defer func() { require.NoError(t, opened.release()) }()
	for _, h := range halves {
		_, err := opened.book.Append(t.Context(), evidencebook.Entry{
			RecordType: "exchange", EpistemicType: evidencebook.ObservedEvent,
			Correlation: evidencebook.Correlation{ExchangeID: h.exchange, RequestDigest: h.request},
			Payloads:    [][]byte{[]byte(h.payload)},
		})
		require.NoError(t, err)
	}
}

func bookSize(t *testing.T, p Profile) uint64 {
	t.Helper()
	opened, err := openBook(t.Context(), p, true)
	require.NoError(t, err)
	defer func() { require.NoError(t, opened.release()) }()
	return opened.book.Size()
}

func runClose(t *testing.T, args ...string) (closeResult, error) {
	t.Helper()
	out, err := invoke(t, "", append([]string{"close"}, args...)...)
	var result closeResult
	if err == nil {
		require.NoError(t, json.Unmarshal([]byte(out), &result))
	}
	return result, err
}

var (
	day0 = time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	day1 = day0.AddDate(0, 0, 1)
)

func TestParsePeriod(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) // a Saturday
	p, err := parsePeriod("day", "", now)
	require.NoError(t, err)
	assert.Equal(t, "day:2026-09-25", p.key, "default is the most recent day that has ended")
	assert.Equal(t, time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC), p.start)
	assert.Equal(t, time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC), p.end)

	p, err = parsePeriod("week", "2026-09-20", now) // a Sunday: ISO week 38
	require.NoError(t, err)
	assert.Equal(t, "week:2026-W38", p.key)
	assert.Equal(t, time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC), p.start, "weeks start Monday")

	p, err = parsePeriod("week", "", now)
	require.NoError(t, err)
	assert.Equal(t, "week:2026-W38", p.key, "the current week (W39) has not ended")

	for _, bad := range [][2]string{{"day", "2026-09-26"}, {"week", "2026-09-21"}, {"month", ""}, {"day", "26/09/2026"}} {
		_, err = parsePeriod(bad[0], bad[1], now)
		assert.ErrorIs(t, err, ErrInput, "%v", bad)
	}
}

func TestSeqWindow(t *testing.T) {
	p := period{start: day1, end: day1.AddDate(0, 0, 1)}
	before, inside, after := day0, day1.Add(time.Hour), day1.AddDate(0, 0, 2)
	window := func(records []placed, last uint64) [3]any {
		from, to, ended, err := seqWindow(records, p, last)
		require.NoError(t, err)
		return [3]any{from, to, ended}
	}

	assert.Equal(t, [3]any{uint64(2), uint64(3), true}, window([]placed{{1, before}, {2, inside}, {3, inside}, {4, after}}, 4))
	assert.Equal(t, [3]any{uint64(2), uint64(2), false}, window([]placed{{1, before}, {2, inside}}, 2),
		"nothing placed after the period: the window runs to the last position and the end is unproven")
	w := window([]placed{{1, before}, {2, after}}, 2)
	assert.Greater(t, w[0], w[1], "no record in the period is an empty window, never an open one")
	// Positions 2 and 3 are unplaceable (not in the list); both sit between
	// the last record before and the first record after, so both are inside.
	assert.Equal(t, [3]any{uint64(2), uint64(3), true}, window([]placed{{1, before}, {4, after}}, 4))

	_, _, _, err := seqWindow([]placed{{1, after}}, p, 1)
	assert.ErrorIs(t, err, ErrInput, "to == 0 would read as an open end")
	_, _, _, err = seqWindow([]placed{{1, before}, {2, inside}, {3, before.Add(-time.Hour)}, {4, after}}, p, 4)
	assert.ErrorIs(t, err, ErrInput, "a commit time that goes backwards is refused, not mapped")
}

// TestCloseSigningPath is the line-review test for the only code that
// chooses keys: the Close record is signed by the profile's signing key and
// by nothing else, `verify` accepts it under that profile, the Close's
// bundle verifies with the neutral AAC bundle verifier, and its checkpoint is
// signed by the profile's checkpoint key.
func TestCloseSigningPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0)
	p, keys := bookProfile(t, "a")
	appendHalves(t, p, half{"x1", "r1", "p1"})
	set(day1)

	dir := t.TempDir()
	capsulePath, bundlePath := filepath.Join(dir, "close.json"), filepath.Join(dir, "close-bundle.json")
	result, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b", "--capsule-out", capsulePath, "--bundle-out", bundlePath)
	require.NoError(t, err)
	assert.Equal(t, "day:2026-09-24", result.Period)
	assert.False(t, result.AlreadyClosed)

	out, err := invoke(t, "", "verify", "--profile", "a", "--capsule", capsulePath)
	require.NoError(t, err, out)

	raw, err := os.ReadFile(capsulePath)
	require.NoError(t, err)
	var record artifact.Record
	require.NoError(t, json.Unmarshal(raw, &record))
	assert.Equal(t, result.RecordID, record.CapsuleID)
	_, err = artifact.Verify(record, []ed25519.PublicKey{keys.checkpoint})
	assert.ErrorIs(t, err, artifact.ErrUntrustedSigner, "the checkpoint key must not be the record signer")

	bundleRaw, err := os.ReadFile(bundlePath)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(bundleRaw, &decoded))
	verdict := aacbundle.VerifyBundle(decoded)
	for name, claim := range map[string]aacbundle.ClaimResult{"graph_closure": verdict.GraphClosure, "interval_coverage": verdict.IntervalCoverage, "per_record_membership": verdict.PerRecordMembership} {
		assert.Equal(t, "pass", claim.Status, "%s: %v", name, claim.Findings)
	}
	verified, err := evidencebook.VerifyBundle(bundleRaw)
	require.NoError(t, err)
	closeVerified := false
	for _, r := range verified.Records {
		closeVerified = closeVerified || (r.RecordID == result.RecordID && r.HeaderVerified && r.CapsuleOK && r.Header.RecordType == evidencebook.RecordTypeClose)
	}
	assert.True(t, closeVerified, "the bundle discloses the Close with a verified header")
	assert.True(t, verified.AnchorAuthenticated)
	assert.Equal(t, hex.EncodeToString(keys.checkpoint), verified.AnchorKeyID)
}

func TestCloseRefusesASigningKeyTheProfileDoesNotTrust(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bookClock(t, day1)
	p, keys := bookProfile(t, "a")
	p.TrustedKeys = []string{hex.EncodeToString(keys.checkpoint)}
	require.NoError(t, saveProfile(p, true))
	_, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b")
	require.ErrorIs(t, err, ErrInput)
	_, statErr := os.Stat(filepath.Join(p.Connection.Database, "book"))
	assert.ErrorIs(t, statErr, os.ErrNotExist, "refused before the book was opened")
}

// A second writer is refused by the log's lock before it touches the record
// store, so it cannot repair away a line another process is still writing.
func TestSecondBookWriterIsRefusedBeforeTouchingTheStore(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bookClock(t, day0)
	p, _ := bookProfile(t, "a")
	appendHalves(t, p, half{"x1", "r1", "p1"})
	holder, err := openBook(t.Context(), p, true)
	require.NoError(t, err)
	defer func() { require.NoError(t, holder.release()) }()

	journal := filepath.Join(p.Connection.Database, "book", "records", "records.jsonl")
	f, err := os.OpenFile(journal, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString(`{"seq":2,"record_id":"in-flight`)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	before, err := os.ReadFile(journal)
	require.NoError(t, err)

	_, err = openBook(t.Context(), p, true)
	require.Error(t, err)
	after, err := os.ReadFile(journal)
	require.NoError(t, err)
	assert.Equal(t, before, after, "the refused writer left the other writer's in-flight line alone")
}

func TestCloseNeedsAnOperator(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bookClock(t, day1)
	p, _ := bookProfile(t, "a")
	p.Operator = ""
	require.NoError(t, saveProfile(p, true))
	_, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b")
	require.ErrorIs(t, err, ErrInput)
	_, statErr := os.Stat(filepath.Join(p.Connection.Database, "book"))
	assert.ErrorIs(t, statErr, os.ErrNotExist, "refused before the book was opened")
}

func TestCloseIsIdempotentOnPeriodAndCounterparty(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0)
	p, _ := bookProfile(t, "a")
	appendHalves(t, p, half{"x1", "r1", "p1"})
	set(day1)
	first, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b")
	require.NoError(t, err)
	size := bookSize(t, p)

	again, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b")
	require.NoError(t, err)
	assert.True(t, again.AlreadyClosed)
	assert.Equal(t, first.RecordID, again.RecordID)
	assert.Equal(t, first.Reconciliation, again.Reconciliation)
	assert.Equal(t, size, bookSize(t, p), "a repeated close signs nothing")

	other, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "c")
	require.NoError(t, err)
	assert.False(t, other.AlreadyClosed, "a different counterparty is a different Close")
}

// With no peer bundle the book holds only its own half of every exchange:
// each one is INSUFFICIENT, never CONFLICTING and never A_ONLY.
func TestCloseWithoutPeerIsInsufficientNotDisagreement(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0)
	p, _ := bookProfile(t, "a")
	appendHalves(t, p, half{"x1", "r1", "p1"}, half{"x2", "r2", "p2"})
	set(day1)
	result, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b")
	require.NoError(t, err)
	assert.Equal(t, evidencebook.Tallies{Insufficient: 2}, result.Reconciliation.Tallies)
	assert.False(t, result.Reconciliation.PeerComplete)
	assert.Empty(t, result.PeerBundle)
}

// peerBundle has book b close the day with a bundle, the way a counterparty
// would hand one over.
func peerBundle(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+"-close-bundle.json")
	_, err := runClose(t, "--profile", name, "--period", "day", "--date", "2026-09-24", "--counterparty", "a", "--bundle-out", path)
	require.NoError(t, err)
	return path
}

func TestCloseAgainstPeerBundleClassifiesEveryExchange(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0.Add(-48*time.Hour))
	a, _ := bookProfile(t, "a")
	b, bKeys := bookProfile(t, "b")
	// Records committed before the period on both sides place its start.
	appendHalves(t, a, half{"x0", "r0", "p0"})
	appendHalves(t, b, half{"x0", "r0", "p0"})
	set(day0)
	appendHalves(t, a, half{"same", "r1", "p1"}, half{"differs", "r2", "mine"}, half{"a-only", "r3", "p3"})
	appendHalves(t, b, half{"same", "r1", "p1"}, half{"differs", "r2", "theirs"}, half{"b-only", "r4", "p4"})
	set(day1)
	bundle := peerBundle(t, "b")

	result, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b", "--peer", bundle, "--peer-checkpoint-key", hex.EncodeToString(bKeys.checkpoint))
	require.NoError(t, err)
	recon := result.Reconciliation
	assert.True(t, recon.PeerComplete, "the peer's window is placed at both ends and every header in it verified")
	assert.Equal(t, evidencebook.Tallies{Matched: 1, Conflicting: 1, AOnly: 1, BOnly: 1}, recon.Tallies)
	assert.NotEmpty(t, result.PeerBundle)
	for _, pair := range recon.Pairs {
		assert.NotEqual(t, "x0", pair.JoinKey, "records before the period are outside both windows")
	}

	out, err := invoke(t, "", "reconcile", "--profile", "a", "--period", "day", "--date", "2026-09-24", "--counterparty", "b", "--peer", bundle, "--peer-checkpoint-key", hex.EncodeToString(bKeys.checkpoint))
	require.NoError(t, err)
	var rec reconcileResult
	require.NoError(t, json.Unmarshal([]byte(out), &rec))
	assert.Equal(t, recon.Tallies, rec.Reconciliation.Tallies, "reconcile computes what close sealed")
}

func TestPeerBundleMustBeUnderThePinnedCheckpointKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0)
	a, aKeys := bookProfile(t, "a")
	b, _ := bookProfile(t, "b")
	appendHalves(t, a, half{"x1", "r1", "p1"})
	appendHalves(t, b, half{"x1", "r1", "p1"})
	set(day1)
	bundle := peerBundle(t, "b")
	size := bookSize(t, a)
	for _, key := range []string{"", hex.EncodeToString(aKeys.checkpoint)} {
		_, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b", "--peer", bundle, "--peer-checkpoint-key", key)
		require.ErrorIs(t, err, ErrInput, "key %q", key)
		_, err = invoke(t, "", "reconcile", "--profile", "a", "--period", "day", "--counterparty", "b", "--peer", bundle, "--peer-checkpoint-key", key)
		require.ErrorIs(t, err, ErrInput, "key %q", key)
	}
	assert.Equal(t, size, bookSize(t, a), "a refused peer signs nothing")
}

// A peer header the bundle withheld cannot be placed in time; inside the
// window it makes the peer's account incomplete, so an unpaired own half is
// INSUFFICIENT rather than A_ONLY.
func TestPeerWindowWithAnUnplaceableRecordIsIncomplete(t *testing.T) {
	p := period{start: day1, end: day1.AddDate(0, 0, 1)}
	header := func(seq uint64, at time.Time) *evidencebook.Header {
		return &evidencebook.Header{Seq: seq, CommittedAt: at.Format(time.RFC3339Nano)}
	}
	peer := evidencebook.VerifiedBundle{IntervalFirst: 1, IntervalLast: 3, Records: []evidencebook.PeerRecord{
		{Seq: 1, Header: header(1, day0), HeaderVerified: true, CapsuleOK: true},
		{Seq: 2},
		{Seq: 3, Header: header(3, day1.AddDate(0, 0, 2)), HeaderVerified: true, CapsuleOK: true},
	}}
	from, to, err := peerWindow(peer, p)
	require.NoError(t, err)
	assert.Equal(t, [2]uint64{2, 2}, [2]uint64{from, to})
	peer.Records[2].HeaderVerified = false
	from, to, err = peerWindow(peer, p)
	require.NoError(t, err)
	assert.Equal(t, [2]uint64{2, 4}, [2]uint64{from, to},
		"an unverified header is never used to place a record, so the end is unproven: one past the interval")
}

// A record committed on a day nobody closed is picked up by the next
// --since-last Close and missed by a Close of the period alone.
// A bundle cut mid-period says nothing about the rest of the period: an
// exchange the peer logged after the cut must read INSUFFICIENT, never
// A_ONLY, and the peer's account must not read complete.
func TestPeerBundleCutMidPeriodIsIncomplete(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0.Add(-48*time.Hour))
	a, _ := bookProfile(t, "a")
	b, bKeys := bookProfile(t, "b")
	appendHalves(t, a, half{"x0", "r0", "p0"})
	appendHalves(t, b, half{"x0", "r0", "p0"})
	set(day0)
	appendHalves(t, a, half{"early", "r1", "p1"})
	appendHalves(t, b, half{"early", "r1", "p1"})

	// b hands over a bundle cut here, before the day is over: every header
	// in its interval disclosed, so the only gap is the missing end.
	opened, err := openBook(t.Context(), b, true)
	require.NoError(t, err)
	anchor, err := opened.book.Checkpoint(t.Context())
	require.NoError(t, err)
	all, err := opened.book.Query(t.Context(), evidencebook.Filter{})
	require.NoError(t, err)
	var ids []string
	for _, r := range all {
		ids = append(ids, r.RecordID)
	}
	bundle, err := opened.book.Bundle(t.Context(), evidencebook.BundleRequest{Root: ids[len(ids)-1], Include: ids, WithholdPayloads: true, At: &anchor})
	require.NoError(t, err)
	require.NoError(t, opened.release())
	path := filepath.Join(t.TempDir(), "cut.json")
	require.NoError(t, os.WriteFile(path, bundle.JSON, 0o600))

	set(day0.Add(2 * time.Hour))
	appendHalves(t, a, half{"late", "r2", "p2"})
	appendHalves(t, b, half{"late", "r2", "p2"})
	set(day1)
	result, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b", "--peer", path, "--peer-checkpoint-key", hex.EncodeToString(bKeys.checkpoint))
	require.NoError(t, err)
	assert.False(t, result.Reconciliation.PeerComplete)
	assert.Equal(t, evidencebook.Tallies{Matched: 1, Insufficient: 1}, result.Reconciliation.Tallies)
}

// A wall clock that steps back 30 hours between two appends (NTP, a
// resumed VM, a store shared between machines) must not leave the day
// unclosable: the book's commit clock never goes below its last record.
func TestCloseSurvivesAClockThatStepsBack(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0.Add(12*time.Hour))
	p, _ := bookProfile(t, "a")
	appendHalves(t, p, half{"x1", "r1", "p1"})
	set(day0.Add(-18 * time.Hour))
	appendHalves(t, p, half{"x2", "r2", "p2"})
	set(day1)
	result, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b")
	require.NoError(t, err)
	assert.Equal(t, 2, result.Reconciliation.Tallies.Insufficient, "both exchanges are in the day's Close")
}

// appendEntries appends arbitrary entries, for records a helper above does
// not cover (a counterparty named, no correlation).
func appendEntries(t *testing.T, p Profile, entries ...evidencebook.Entry) {
	t.Helper()
	opened, err := openBook(t.Context(), p, true)
	require.NoError(t, err)
	defer func() { require.NoError(t, opened.release()) }()
	for _, e := range entries {
		_, err := opened.book.Append(t.Context(), e)
		require.NoError(t, err)
	}
}

// The book cannot limit a Close to one counterparty's exchanges, so a window
// holding an exchange named for another counterparty is refused.
func TestCloseRefusesAWindowWithAnotherCounterpartysExchange(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0)
	p, _ := bookProfile(t, "a")
	appendEntries(t, p,
		evidencebook.Entry{RecordType: "exchange", EpistemicType: evidencebook.ObservedEvent, CounterpartyRef: "b", Correlation: evidencebook.Correlation{ExchangeID: "with-b"}, Payloads: [][]byte{[]byte("p1")}},
		evidencebook.Entry{RecordType: "exchange", EpistemicType: evidencebook.ObservedEvent, CounterpartyRef: "c", Correlation: evidencebook.Correlation{ExchangeID: "with-c"}, Payloads: [][]byte{[]byte("p2")}},
	)
	set(day1)
	size := bookSize(t, p)
	_, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b")
	require.ErrorIs(t, err, ErrInput)
	assert.Equal(t, size, bookSize(t, p), "nothing sealed")
}

// A Close's bundle discloses only what the counterparty needs: nothing named
// for another counterparty, and nothing committed after the record that
// places the window's end.
func TestCloseBundleDisclosesOnlyTheWindow(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0)
	p, _ := bookProfile(t, "a")
	appendHalves(t, p, half{"x1", "r1", "p1"})
	set(day1.Add(time.Hour))
	appendEntries(t, p,
		evidencebook.Entry{RecordType: "note", EpistemicType: evidencebook.ProducerClaim, CounterpartyRef: "c", SubjectRef: "for-c-only"},
		evidencebook.Entry{RecordType: "note", EpistemicType: evidencebook.ProducerClaim, SubjectRef: "later-today"},
	)
	path := filepath.Join(t.TempDir(), "bundle.json")
	_, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b", "--date", "2026-09-24", "--bundle-out", path)
	require.NoError(t, err)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	verified, err := evidencebook.VerifyBundle(raw)
	require.NoError(t, err)
	for _, r := range verified.Records {
		if r.Header == nil {
			continue
		}
		assert.NotEqual(t, "c", r.Header.CounterpartyRef, "another counterparty's record is never disclosed")
		assert.NotEqual(t, "later-today", r.Header.SubjectRef, "nothing past the window's end is disclosed")
	}
}

func TestPeerBundleMustBeTheCounterpartysBook(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0)
	a, _ := bookProfile(t, "a")
	b, bKeys := bookProfile(t, "b")
	appendHalves(t, a, half{"x1", "r1", "p1"})
	appendHalves(t, b, half{"x1", "r1", "p1"})
	set(day1)
	bundle := peerBundle(t, "b")
	size := bookSize(t, a)
	_, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "c", "--peer", bundle, "--peer-checkpoint-key", hex.EncodeToString(bKeys.checkpoint))
	require.ErrorIs(t, err, ErrInput)
	assert.Equal(t, size, bookSize(t, a))
}

// A repeat that brings a peer bundle returns the first Close and does not
// report the bundle as if it had been applied.
func TestAlreadyClosedDoesNotReportAnUnappliedPeerBundle(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0)
	a, _ := bookProfile(t, "a")
	b, bKeys := bookProfile(t, "b")
	appendHalves(t, a, half{"x1", "r1", "p1"})
	appendHalves(t, b, half{"x1", "r1", "p1"})
	set(day1)
	first, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b")
	require.NoError(t, err)
	bundle := peerBundle(t, "b")
	again, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b", "--peer", bundle, "--peer-checkpoint-key", hex.EncodeToString(bKeys.checkpoint))
	require.NoError(t, err)
	assert.True(t, again.AlreadyClosed)
	assert.Equal(t, first.RecordID, again.RecordID)
	assert.Empty(t, again.PeerBundle)
	assert.Equal(t, first.Reconciliation, again.Reconciliation)
}

func TestCloseRefusesAnUntrustedCheckpointKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bookClock(t, day1)
	p, keys := bookProfile(t, "a")
	p.Checkpoint.TrustedKeys = []string{hex.EncodeToString(keys.record)}
	require.NoError(t, saveProfile(p, true))
	_, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b")
	require.ErrorIs(t, err, ErrInput)
	_, statErr := os.Stat(filepath.Join(p.Connection.Database, "book"))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

// The book's log is checkpointed under its own log id, never the profile
// CLL's, so one (log_id, key) never names two different trees.
func TestBookLogIsNamedApartFromTheProfileLog(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bookClock(t, day0)
	p, _ := bookProfile(t, "a")
	appendHalves(t, p, half{"x1", "r1", "p1"})
	opened, err := openBook(t.Context(), p, true)
	require.NoError(t, err)
	defer func() { require.NoError(t, opened.release()) }()
	cp, err := opened.book.Checkpoint(t.Context())
	require.NoError(t, err)
	assert.Equal(t, p.LogID+"/book", cp.LogID)
}

func TestReconcileNeedsAnExistingBookAndCreatesNothing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0)
	a, _ := bookProfile(t, "a")
	b, bKeys := bookProfile(t, "b")
	appendHalves(t, b, half{"x1", "r1", "p1"})
	set(day1)
	bundle := peerBundle(t, "b")
	_, err := invoke(t, "", "reconcile", "--profile", "a", "--period", "day", "--counterparty", "b", "--peer", bundle, "--peer-checkpoint-key", hex.EncodeToString(bKeys.checkpoint))
	require.ErrorIs(t, err, ErrInput)
	_, statErr := os.Stat(a.Connection.Database)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "reconcile made no directory")
}

// Once the Close is sealed, a failure writing an output file still reports
// the Close's record_id.
func TestCloseReportsTheSealedRecordWhenAnOutputFails(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0)
	p, _ := bookProfile(t, "a")
	appendHalves(t, p, half{"x1", "r1", "p1"})
	set(day1)
	taken := filepath.Join(t.TempDir(), "taken.json")
	require.NoError(t, os.WriteFile(taken, []byte("{}"), 0o600))
	out, err := invoke(t, "", "close", "--profile", "a", "--period", "day", "--counterparty", "b", "--bundle-out", taken)
	require.Error(t, err)
	var result closeResult
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	assert.Regexp(t, `^[0-9a-f]{64}$`, result.RecordID)
	again, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b")
	require.NoError(t, err)
	assert.Equal(t, result.RecordID, again.RecordID, "the reported record is the sealed Close")
}

func TestCloseSinceLastRefusesOutOfOrderAndPeer(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0)
	a, _ := bookProfile(t, "a")
	b, bKeys := bookProfile(t, "b")
	appendHalves(t, a, half{"x1", "r1", "p1"})
	appendHalves(t, b, half{"x1", "r1", "p1"})
	set(day1)
	appendHalves(t, a, half{"x2", "r2", "p2"})
	set(day1.AddDate(0, 0, 1))
	_, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b", "--date", "2026-09-25")
	require.NoError(t, err)
	size := bookSize(t, a)
	_, err = runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b", "--date", "2026-09-24", "--since-last")
	require.ErrorIs(t, err, ErrInput, "the later day's Close already reaches past this one")
	bundle := peerBundle(t, "b")
	_, err = runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b", "--date", "2026-09-24", "--since-last", "--peer", bundle, "--peer-checkpoint-key", hex.EncodeToString(bKeys.checkpoint))
	require.ErrorIs(t, err, ErrInput)
	assert.Equal(t, size, bookSize(t, a), "neither refusal sealed anything")
}

// In order, with a previous Close, --since-last would stretch only this
// book's window; with a peer bundle that is refused.
func TestCloseSinceLastWithPeerIsRefusedInOrder(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0)
	a, _ := bookProfile(t, "a")
	b, bKeys := bookProfile(t, "b")
	appendHalves(t, a, half{"x1", "r1", "p1"})
	appendHalves(t, b, half{"x1", "r1", "p1"})
	set(day1)
	_, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b")
	require.NoError(t, err)
	appendHalves(t, a, half{"x2", "r2", "p2"})
	appendHalves(t, b, half{"x2", "r2", "p2"})
	set(day1.AddDate(0, 0, 1))
	bundle := filepath.Join(t.TempDir(), "b.json")
	_, err = runClose(t, "--profile", "b", "--period", "day", "--counterparty", "a", "--bundle-out", bundle)
	require.NoError(t, err)
	size := bookSize(t, a)
	_, err = runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b", "--since-last", "--peer", bundle, "--peer-checkpoint-key", hex.EncodeToString(bKeys.checkpoint))
	require.ErrorIs(t, err, ErrInput)
	assert.Equal(t, size, bookSize(t, a))
	result, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b", "--peer", bundle, "--peer-checkpoint-key", hex.EncodeToString(bKeys.checkpoint))
	require.NoError(t, err, "the same close without --since-last is accepted")
	assert.Equal(t, 1, result.Reconciliation.Tallies.Matched)
}

func TestCloseSinceLastStartsAfterThePreviousClose(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0)
	p, _ := bookProfile(t, "a")
	appendHalves(t, p, half{"x1", "r1", "p1"})
	set(day1)
	first, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b")
	require.NoError(t, err)
	appendHalves(t, p, half{"unclosed-day", "r2", "p2"})
	set(day1.AddDate(0, 0, 1))
	appendHalves(t, p, half{"x3", "r3", "p3"})
	set(day1.AddDate(0, 0, 2))

	second, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b", "--since-last")
	require.NoError(t, err)
	assert.Equal(t, "day:2026-09-26", second.Period)
	assert.Equal(t, first.Reconciliation.ToSeq+1, second.Reconciliation.FromSeq)
	assert.Equal(t, 2, second.Reconciliation.Tallies.Insufficient, "the unclosed day's exchange and the period's own")

	periodOnly, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "c")
	require.NoError(t, err)
	assert.Equal(t, 1, periodOnly.Reconciliation.Tallies.Insufficient, "without --since-last only the period's exchange")
}

// TestRequestRespondRoundTrip runs the three answers a requester can hold:
// a granted artifact, a refusal, and a recorded absence.
func TestRequestRespondRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0)
	a, _ := bookProfile(t, "a")
	b, bKeys := bookProfile(t, "b")
	appendHalves(t, b, half{"x1", "r1", "p1"})
	set(day1)
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		return path
	}
	ask := func(name, body string) requestResult {
		out, err := invoke(t, "", "request", "--profile", "a", "--request", write(name+".json", body), "--responder", "b", "--output", filepath.Join(dir, name+".sent"))
		require.NoError(t, err)
		var r requestResult
		require.NoError(t, json.Unmarshal([]byte(out), &r))
		return r
	}
	respond := func(name string) respondResult {
		out, err := invoke(t, "", "respond", "--profile", "b", "--request", filepath.Join(dir, name+".sent"), "--requester", "a", "--output", filepath.Join(dir, name+".response"))
		require.NoError(t, err)
		var r respondResult
		require.NoError(t, json.Unmarshal([]byte(out), &r))
		return r
	}
	record := func(sent requestResult, name string) requestResult {
		out, err := invoke(t, "", "request", "--profile", "a", "--for", sent.RecordID, "--response", filepath.Join(dir, name+".response"), "--responder-key", hex.EncodeToString(bKeys.record), "--responder-checkpoint-key", hex.EncodeToString(bKeys.checkpoint))
		require.NoError(t, err)
		var r requestResult
		require.NoError(t, json.Unmarshal([]byte(out), &r))
		return r
	}

	sent := ask("history", `{"subject":{"kind":"full_history"},"coverage":{"min_freshness":{"size":1}}}`)
	answered := respond("history")
	assert.Equal(t, evidencebook.OutcomeArtifact, answered.Outcome)
	assert.Equal(t, sent.RequestDigest, answered.RequestDigest)
	assert.Equal(t, evidencebook.OutcomeArtifact, record(sent, "history").Outcome)

	sent = ask("missing", `{"subject":{"kind":"record","digest":"`+hex.EncodeToString(make([]byte, 32))+`"},"coverage":{"min_freshness":{"size":1}}}`)
	answered = respond("missing")
	assert.Equal(t, evidencebook.OutcomeRefusal, answered.Outcome)
	assert.Equal(t, evidencebook.ReasonNoSuchSubject, answered.Reason)
	got := record(sent, "missing")
	assert.Equal(t, evidencebook.OutcomeRefusal, got.Outcome)
	assert.Equal(t, evidencebook.ReasonNoSuchSubject, got.Reason)

	sent = ask("unanswered", `{"subject":{"kind":"checkpoints"},"coverage":{"min_freshness":{"size":1}}}`)
	set(day1.Add(time.Hour))
	out, err := invoke(t, "", "request", "--profile", "a", "--for", sent.RecordID, "--absent-until", day1.Add(30*time.Minute).Format(time.RFC3339))
	require.NoError(t, err)
	var absent requestResult
	require.NoError(t, json.Unmarshal([]byte(out), &absent))
	assert.Equal(t, evidencebook.RecordTypeAbsence, absent.Outcome)
	assert.Equal(t, sent.RequestDigest, absent.RequestDigest)

	// A response is recorded only under both pinned responder keys. Without
	// them it is refused before the book is touched; under another signing
	// key the book refuses it and records nothing, so the real response can
	// still be recorded afterwards: a refused message never forecloses.
	sent = ask("pinned", `{"subject":{"kind":"full_history"},"coverage":{"min_freshness":{"size":1}}}`)
	respond("pinned")
	pinned := filepath.Join(dir, "pinned.response")
	size := bookSize(t, a)
	for _, keys := range [][]string{
		{},
		{"--responder-key", hex.EncodeToString(bKeys.record)},
		{"--responder-checkpoint-key", hex.EncodeToString(bKeys.checkpoint)},
	} {
		_, err = invoke(t, "", append([]string{"request", "--profile", "a", "--for", sent.RecordID, "--response", pinned}, keys...)...)
		assert.ErrorIs(t, err, ErrInput, "%v", keys)
		assert.ErrorContains(t, err, "--responder-checkpoint-key", "refused by the verb, before the book is opened: %v", keys)
	}
	_, err = invoke(t, "", "request", "--profile", "a", "--for", sent.RecordID, "--response", pinned, "--responder-key", hex.EncodeToString(bKeys.checkpoint), "--responder-checkpoint-key", hex.EncodeToString(bKeys.checkpoint))
	assert.ErrorIs(t, err, evidencebook.ErrInvalid, "refused by the book's pinned signer check")
	assert.Equal(t, size, bookSize(t, a), "no refusal recorded anything")
	assert.Equal(t, evidencebook.OutcomeArtifact, record(sent, "pinned").Outcome, "the real response is still recorded")
}

// A peer bundle carrying a member twice under keys that differ only by case
// ("Disclosures" beside "disclosures") could have one copy verified and the
// other read. The bundle verifier refuses it, so close --peer and reconcile
// refuse it and seal nothing.
func TestCaseVariantPeerBundleIsRefused(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0)
	a, _ := bookProfile(t, "a")
	b, bKeys := bookProfile(t, "b")
	appendHalves(t, a, half{"x1", "r1", "p1"})
	appendHalves(t, b, half{"x1", "r1", "p1"})
	set(day1)
	honest := peerBundle(t, "b")
	raw, err := os.ReadFile(honest)
	require.NoError(t, err)
	_, err = invoke(t, "", "reconcile", "--profile", "a", "--period", "day", "--counterparty", "b", "--peer", honest, "--peer-checkpoint-key", hex.EncodeToString(bKeys.checkpoint))
	require.NoError(t, err, "the honest bundle is accepted")
	size := bookSize(t, a)
	for name, member := range map[string]string{
		"disclosures": `"Disclosures":{},`,
		"checkpoint":  `"CHECKPOINT":{"root":"00","mmr_size":1},`,
	} {
		forged := filepath.Join(t.TempDir(), name+".json")
		require.Equal(t, byte('{'), raw[0])
		require.NoError(t, os.WriteFile(forged, append([]byte("{"+member), raw[1:]...), 0o600))
		_, err = runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b", "--peer", forged, "--peer-checkpoint-key", hex.EncodeToString(bKeys.checkpoint))
		assert.ErrorIs(t, err, evidencebook.ErrInvalid, "%s: refused by the bundle verifier, not only by the key pin", name)
		_, err = invoke(t, "", "reconcile", "--profile", "a", "--period", "day", "--counterparty", "b", "--peer", forged, "--peer-checkpoint-key", hex.EncodeToString(bKeys.checkpoint))
		assert.ErrorIs(t, err, ErrInput, name)
	}
	assert.Equal(t, size, bookSize(t, a), "a refused bundle seals nothing")
}

func TestRequestModesAreExclusive(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bookProfile(t, "a")
	for _, args := range [][]string{
		{},
		{"--request", "x.json", "--for", "id"},
		{"--response", "x.json"},
		{"--response", "x.json", "--absent-until", "2026-09-25T00:00:00Z", "--for", "id"},
	} {
		_, err := invoke(t, "", append([]string{"request", "--profile", "a"}, args...)...)
		assert.ErrorIs(t, err, ErrInput, "%v", args)
	}
}

// Key ids are compared as lowercase hex: an uppercase pin of the right keys
// must record the genuine artifact, not a failed verification.
func TestUppercaseResponderKeysRecordTheArtifact(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0)
	a, _ := bookProfile(t, "a")
	b, bKeys := bookProfile(t, "b")
	appendHalves(t, a, half{"x0", "r0", "p0"})
	appendHalves(t, b, half{"x1", "r1", "p1"})
	set(day1)
	dir := t.TempDir()
	body := filepath.Join(dir, "ask.json")
	require.NoError(t, os.WriteFile(body, []byte(`{"subject":{"kind":"full_history"},"coverage":{"min_freshness":{"size":1}}}`), 0o600))
	out, err := invoke(t, "", "request", "--profile", "a", "--request", body, "--responder", "b", "--output", filepath.Join(dir, "ask.sent"))
	require.NoError(t, err)
	var sent requestResult
	require.NoError(t, json.Unmarshal([]byte(out), &sent))
	_, err = invoke(t, "", "respond", "--profile", "b", "--request", filepath.Join(dir, "ask.sent"), "--requester", "a", "--output", filepath.Join(dir, "ask.response"))
	require.NoError(t, err)
	out, err = invoke(t, "", "request", "--profile", "a", "--for", sent.RecordID, "--response", filepath.Join(dir, "ask.response"),
		"--responder-key", strings.ToUpper(hex.EncodeToString(bKeys.record)), "--responder-checkpoint-key", strings.ToUpper(hex.EncodeToString(bKeys.checkpoint)))
	require.NoError(t, err)
	var got requestResult
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, evidencebook.OutcomeArtifact, got.Outcome)
}

// Once request or respond has committed its record, a failed --output write
// still reports the record and the bytes that should have been written.
func TestRequestAndRespondReportTheirRecordWhenOutputFails(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0)
	a, _ := bookProfile(t, "a")
	b, _ := bookProfile(t, "b")
	appendHalves(t, a, half{"x0", "r0", "p0"})
	appendHalves(t, b, half{"x1", "r1", "p1"})
	set(day1)
	dir := t.TempDir()
	taken := filepath.Join(dir, "taken")
	require.NoError(t, os.WriteFile(taken, []byte("x"), 0o600))
	body := filepath.Join(dir, "ask.json")
	require.NoError(t, os.WriteFile(body, []byte(`{"subject":{"kind":"full_history"},"coverage":{"min_freshness":{"size":1}}}`), 0o600))

	size := bookSize(t, a)
	out, err := invoke(t, "", "request", "--profile", "a", "--request", body, "--responder", "b", "--output", taken)
	require.Error(t, err)
	var asked requestResult
	require.NoError(t, json.Unmarshal([]byte(out), &asked))
	assert.Regexp(t, `^[0-9a-f]{64}$`, asked.RecordID)
	assert.NotEmpty(t, asked.Request, "the request bytes to send are not lost")
	assert.Equal(t, size+1, bookSize(t, a), "the request is on record")

	sent := filepath.Join(dir, "ask.sent")
	require.NoError(t, os.WriteFile(sent, asked.Request, 0o600))
	out, err = invoke(t, "", "respond", "--profile", "b", "--request", sent, "--requester", "a", "--output", taken)
	require.Error(t, err)
	var answered respondResult
	require.NoError(t, json.Unmarshal([]byte(out), &answered))
	assert.Regexp(t, `^[0-9a-f]{64}$`, answered.RecordID)
	var response evidencebook.Response
	require.NoError(t, json.Unmarshal(answered.Response, &response))
	require.NotNil(t, response.Artifact, "the signed response is not lost")
	require.NoError(t, response.Artifact.Verify())
}

func TestPeerCheckpointKeyWithoutPeerIsRefused(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0)
	p, _ := bookProfile(t, "a")
	appendHalves(t, p, half{"x1", "r1", "p1"}) // a closable day: only the flag can refuse
	set(day1)
	_, err := runClose(t, "--profile", "a", "--period", "day", "--counterparty", "b", "--peer-checkpoint-key", strings.Repeat("ab", 32))
	assert.ErrorIs(t, err, ErrInput)
}
