package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
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
	p := Profile{Name: name, Type: "jsonl", LogID: name + "-book", Namespace: "capsule", Operator: name + "-operator"}
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
	opened, err := openBook(t.Context(), p)
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
	opened, err := openBook(t.Context(), p)
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

	from, to, err := seqWindow([]placed{{1, before}, {2, inside}, {3, inside}, {4, after}}, p, 4)
	require.NoError(t, err)
	assert.Equal(t, [2]uint64{2, 3}, [2]uint64{from, to})

	from, to, err = seqWindow([]placed{{1, before}, {2, inside}}, p, 2)
	require.NoError(t, err)
	assert.Equal(t, [2]uint64{2, 2}, [2]uint64{from, to}, "nothing after the period: the window runs to the last position")

	from, to, err = seqWindow([]placed{{1, before}, {2, after}}, p, 2)
	require.NoError(t, err)
	assert.Greater(t, from, to, "no record in the period is an empty window, never an open one")

	// Positions 2 and 3 are unplaceable (not in the list); both sit between
	// the last record before and the first record after, so both are inside.
	from, to, err = seqWindow([]placed{{1, before}, {4, after}}, p, 4)
	require.NoError(t, err)
	assert.Equal(t, [2]uint64{2, 3}, [2]uint64{from, to})

	_, _, err = seqWindow([]placed{{1, after}}, p, 1)
	assert.ErrorIs(t, err, ErrInput, "to == 0 would read as an open end")
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

	out, err := invoke(t, "", "reconcile", "--profile", "a", "--period", "day", "--date", "2026-09-24", "--peer", bundle, "--peer-checkpoint-key", hex.EncodeToString(bKeys.checkpoint))
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
		_, err = invoke(t, "", "reconcile", "--profile", "a", "--period", "day", "--peer", bundle, "--peer-checkpoint-key", key)
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
	assert.Equal(t, [2]uint64{2, 3}, [2]uint64{from, to}, "an unverified header is never used to place a record")
}

// A record committed on a day nobody closed is picked up by the next
// --since-last Close and missed by a Close of the period alone.
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
	bookProfile(t, "a")
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
		out, err := invoke(t, "", "request", "--profile", "a", "--for", sent.RecordID, "--response", filepath.Join(dir, name+".response"), "--responder-key", hex.EncodeToString(bKeys.record))
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

	// A response under a key other than the pinned responder key is refused.
	sent = ask("pinned", `{"subject":{"kind":"full_history"},"coverage":{"min_freshness":{"size":1}}}`)
	respond("pinned")
	_, err = invoke(t, "", "request", "--profile", "a", "--for", sent.RecordID, "--response", filepath.Join(dir, "pinned.response"), "--responder-key", hex.EncodeToString(bKeys.checkpoint))
	assert.Error(t, err)
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
