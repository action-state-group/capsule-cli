package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/action-state-group/checkpointed-local-log/go/checkpoint"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/action-state-group/checkpointed-local-log/go/mmr"
	"github.com/action-state-group/checkpointed-local-log/go/store/memory"
	"github.com/action-state-group/checkpointed-local-log/go/witness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chainWitness is a fake witness with capsule-anchor's continuity rules
// (service.py, _check_checkpoint_continuity): once it holds a checkpoint for
// the log, it takes one only whose prev size and root are its tip and whose
// consistency proof bridges its tip to the new root; any other is refused
// with a continuity 409 naming its tip. A statement it already took is
// answered with its earlier receipt before any continuity check, as
// capsule-anchor answers a resubmission. Its receipts are COSE receipts over
// the statement's entry hash, signed by key, which the production verifier
// checks (recordingVerifier).
type chainWitness struct {
	t        *testing.T
	key      ed25519.PrivateKey
	size     uint64
	root     string
	accepted map[[32]byte]witness.Receipt
	sent     [][]byte
	// before answers a statement with an error before anything else, when
	// it returns one.
	before func(checkpoint.Record) error
	// lose takes a statement at its size and then answers as timed out, as
	// often as the entry says.
	lose map[uint64]int
}

func newChainWitness(t *testing.T) *chainWitness {
	_, key, e := ed25519.GenerateKey(nil)
	require.NoError(t, e)
	return &chainWitness{t: t, key: key, accepted: map[[32]byte]witness.Receipt{}, lose: map[uint64]int{}}
}

// verifier is the production receipt verifier pinned to this witness's key,
// recording each statement it checks in checked.
func (w *chainWitness) verifier(checked *[][]byte) recordingVerifier {
	v, e := witness.NewReceiptVerifier(w.key.Public().(ed25519.PublicKey))
	require.NoError(w.t, e)
	return recordingVerifier{inner: v, checked: checked}
}

func (w *chainWitness) Submit(_ context.Context, b []byte) (witness.Receipt, error) {
	record, e := checkpoint.ParseRecord(b)
	require.NoError(w.t, e)
	w.sent = append(w.sent, append([]byte(nil), b...))
	if w.before != nil {
		if e := w.before(record); e != nil {
			return witness.Receipt{}, e
		}
	}
	key := sha256.Sum256(b)
	if r, ok := w.accepted[key]; ok {
		return r, nil
	}
	if w.size > 0 {
		if record.PrevSize != w.size || record.PrevRoot != w.root {
			return witness.Receipt{}, w.refusal()
		}
		old, e := hex.DecodeString(w.root)
		require.NoError(w.t, e)
		next, e := hex.DecodeString(record.Root)
		require.NoError(w.t, e)
		if record.ConsistencyProof == nil || !mmr.VerifyConsistency(old, next, *record.ConsistencyProof) {
			return witness.Receipt{}, w.refusal()
		}
	}
	w.size, w.root = record.MMRSize, record.Root
	entry, e := record.EntryHash()
	require.NoError(w.t, e)
	r := witness.Receipt{Bytes: mintReceipt(w.t, entry, w.key), EntryHash: hex.EncodeToString(entry), EntryHashScheme: witness.EntryHashSchemeCheckpointDigest, LeafIndex: 0, TreeSize: 1}
	w.accepted[key] = r
	if w.lose[record.MMRSize] > 0 {
		w.lose[record.MMRSize]--
		return witness.Receipt{}, timedOutAnswer()
	}
	return r, nil
}

// refusal is the continuity 409 as safeSubmitter records it.
func (w *chainWitness) refusal() error {
	body := fmt.Sprintf(`{"detail":{"error":"prev_size/prev_root does not match","last_accepted_mmr_size":%d,"last_accepted_root":%q}}`, w.size, w.root)
	return classifySubmitError(&witness.HTTPError{StatusCode: 409, Body: body}, true)
}

// timedOutAnswer is a request that reached the witness and got no answer in
// time, as safeSubmitter records it.
func timedOutAnswer() error {
	return &witness.HTTPError{StatusCode: 503, Body: WitnessTimedOut}
}

// sentCount is how many times b was sent.
func (w *chainWitness) sentCount(b []byte) int {
	n := 0
	for _, s := range w.sent {
		if bytes.Equal(s, b) {
			n++
		}
	}
	return n
}

// recordingVerifier is a production receipt verifier that records what it
// checks.
type recordingVerifier struct {
	inner   witness.Verifier
	checked *[][]byte
}

func (v recordingVerifier) Verify(statement []byte, r witness.Receipt) error {
	if v.checked != nil {
		*v.checked = append(*v.checked, append([]byte(nil), statement...))
	}
	return v.inner.Verify(statement, r)
}

// receiptOf is a stored receipt as the verifier reads it.
func receiptOf(t *testing.T, w cll.WitnessState) witness.Receipt {
	t.Helper()
	require.NotNil(t, w.Receipt)
	require.NotNil(t, w.Receipt.LeafIndex)
	require.NotNil(t, w.Receipt.TreeSize)
	return witness.Receipt{Bytes: w.Receipt.Bytes, EntryHash: w.Receipt.EntryHash, EntryHashScheme: w.Receipt.EntryHashScheme, LeafIndex: *w.Receipt.LeafIndex, TreeSize: *w.Receipt.TreeSize}
}

// reanchorLog is a log whose checkpoint key is the profile's, cut one entry
// per tick.
type reanchorLog struct {
	t      *testing.T
	p      Profile
	store  *memory.Store
	runner *checkpoint.Runner
	start  time.Time
	sizes  []uint64
}

func newReanchorLog(t *testing.T) *reanchorLog {
	p, key := profileFixture(t)
	store := memory.New()
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	signer, e := checkpoint.NewEd25519Signer(key)
	require.NoError(t, e)
	cfg := checkpoint.DefaultRunnerConfig(p.LogID)
	cfg.Cadence.CadenceEntries = 1
	cfg.WitnessIDs = []string{"service-test"}
	runner, e := checkpoint.NewRunner(cfg, store, signer)
	require.NoError(t, e)
	return &reanchorLog{t: t, p: p, store: store, runner: runner, start: time.Now().UTC().Truncate(time.Second)}
}

// at is tick n's time: ticks are an hour apart, past any retry gap.
func (l *reanchorLog) at(n int) time.Time { return l.start.Add(time.Duration(n) * time.Hour) }

// cut appends one entry and cuts a checkpoint at tick n.
func (l *reanchorLog) cut(n int) uint64 {
	_, e := l.store.Append(l.t.Context(), cll.AppendInput{Value: bytes.Repeat([]byte{byte(n + 1)}, 32), AppendedAt: l.at(n)})
	require.NoError(l.t, e)
	_, e = l.runner.RunOnce(l.t.Context(), l.at(n))
	require.NoError(l.t, e)
	state, e := l.store.LoadCLL(l.t.Context())
	require.NoError(l.t, e)
	l.sizes = append(l.sizes, state.Checkpoint.Size)
	return state.Checkpoint.Size
}

// deliver delivers the checkpoint at size at tick n, re-anchoring as
// DeliverWitnessBy does.
func (l *reanchorLog) deliver(n int, size uint64, w witness.Submitter, v witness.Verifier) cll.WitnessState {
	got, e := deliverSelected(l.t.Context(), l.store, "service-test", size, w, v, newReprover(l.p, l.store), l.at(n), time.Time{})
	require.NoError(l.t, e)
	return got
}

func (l *reanchorLog) record(size uint64) checkpoint.Record {
	r, e := checkpoint.ParseRecord(row(l.t, l.store, size).Checkpoint)
	require.NoError(l.t, e)
	return r
}

// The witness forgets the checkpoint it took last (its tip goes back one),
// so the next checkpoint is refused for good, retry included. capsulectl
// re-anchors it from the witness's tip, and every later checkpoint gets its
// own receipt.
func TestAPersistentContinuityRefusalIsReanchored(t *testing.T) {
	l := newReanchorLog(t)
	w := newChainWitness(t)
	var checked [][]byte
	v := w.verifier(&checked)
	for n := 0; n < 2; n++ {
		require.NotNil(t, l.deliver(n, l.cut(n), w, v).Receipt)
	}
	first := l.record(l.sizes[0])
	w.size, w.root = first.MMRSize, first.Root

	refused := l.cut(2)
	got := l.deliver(2, refused, w, v)
	assert.True(t, rearmable(got), "the first refusal is retried")
	for n := 3; n < 6; n++ {
		l.deliver(n, l.cut(n), w, v)
	}

	got = row(t, l.store, refused)
	assert.Nil(t, got.Receipt)
	assert.True(t, got.Permanent)
	assert.True(t, reanchored(got))
	reason, text := WitnessPendingReason(got, testWitness)
	assert.Equal(t, "witness_error", reason)
	assert.Equal(t, fmt.Sprintf("not delivered: the witness refused it again after a retry, as not continuing the checkpoint it last accepted (HTTP 409); capsulectl re-anchored it, proving it again from that checkpoint (mmr_size %d), so later checkpoints are delivered; the receipt for the re-anchor is not kept.", first.MMRSize), text)
	assert.Equal(t, "re-anchored", witnessResult(testWitness, got)["state"])
	assert.NoError(t, deliveryOutcome(got))
	for _, size := range l.sizes[3:] {
		later := row(t, l.store, size)
		require.NotNil(t, later.Receipt, "checkpoint %d has its receipt", size)
		assert.NoError(t, v.Verify(later.Checkpoint, receiptOf(t, later)), "for its own statement")
	}
	assert.Equal(t, l.sizes[len(l.sizes)-1], w.size, "the witness's tip is the last checkpoint")
	assert.Equal(t, 2, w.sentCount(got.Checkpoint), "the stored checkpoint was sent, refused, sent again and refused")

	// The re-anchor is the refused checkpoint with the witness's tip as its
	// predecessor, signed with the profile's key, and its receipt was checked
	// against it, not the stored statement.
	var reanchor []byte
	for _, s := range w.sent {
		r, e := checkpoint.ParseRecord(s)
		require.NoError(t, e)
		if r.MMRSize == refused && !bytes.Equal(s, got.Checkpoint) {
			reanchor = s
		}
	}
	require.NotNil(t, reanchor)
	r, e := VerifyCheckpoint(l.p, reanchor)
	require.NoError(t, e)
	stored := l.record(refused)
	assert.Equal(t, stored.LogID, r.LogID)
	assert.Equal(t, stored.KeyID, r.KeyID)
	assert.Equal(t, stored.MMRSize, r.MMRSize)
	assert.Equal(t, stored.Root, r.Root)
	assert.True(t, stored.Timestamp.Equal(r.Timestamp))
	assert.Equal(t, stored.NewPeaks, r.NewPeaks)
	assert.Equal(t, first.MMRSize, r.PrevSize)
	assert.Equal(t, first.Root, r.PrevRoot)
	assert.Equal(t, first.NewPeaks, r.PrevPeaks)
	assert.Contains(t, checked, reanchor)
	key := sha256.Sum256(reanchor)
	receipt := w.accepted[key]
	assert.NoError(t, v.Verify(reanchor, receipt), "the re-anchor's receipt is for the re-anchor")
	assert.Error(t, v.Verify(got.Checkpoint, receipt), "and not for the stored statement")
}

// A witness whose last accepted checkpoint is not this log's (a fork) is
// never re-proved to: the refused row says so and holds no later row.
func TestAForkIsNeverReanchored(t *testing.T) {
	l := newReanchorLog(t)
	w := newChainWitness(t)
	v := w.verifier(nil)
	require.NotNil(t, l.deliver(0, l.cut(0), w, v).Receipt)
	w.root = hex.EncodeToString(bytes.Repeat([]byte{7}, 32))
	refused := l.cut(1)
	l.deliver(1, refused, w, v)
	l.deliver(2, refused, w, v)

	got := row(t, l.store, refused)
	assert.False(t, reanchored(got))
	assert.False(t, reanchorDue(got))
	_, text := WitnessPendingReason(got, testWitness)
	assert.Equal(t, "not delivered: the witness refused it again after a retry, as not continuing the checkpoint it last accepted (HTTP 409); it was not re-anchored: the witness's last accepted checkpoint is not this log's; this delivery is not retried.", text)
	assert.Equal(t, "failed", witnessResult(testWitness, got)["state"])
	assert.Equal(t, 2, len(w.sent)-1, "only the stored checkpoint was sent, twice")
	_, e := l.store.GetWitness(t.Context(), "service-test", refused)
	require.NoError(t, e)
	assert.Equal(t, cll.ErrNotFound, func() error {
		_, e := (selectedWitness{WitnessStateStore: l.store, id: "service-test", size: refused + 100, reanchors: true}).GetWitness(t.Context(), "service-test", refused)
		return e
	}(), "it holds no later row")
}

// A re-anchor that gets no answer is tried again at a later call and holds
// the later rows meanwhile, so each of them still gets a receipt for its own
// statement. While it waits it reads as pending.
func TestAReanchorThatTimesOutHoldsLaterRows(t *testing.T) {
	l := newReanchorLog(t)
	w := newChainWitness(t)
	v := w.verifier(nil)
	for n := 0; n < 2; n++ {
		require.NotNil(t, l.deliver(n, l.cut(n), w, v).Receipt)
	}
	first := l.record(l.sizes[0])
	w.size, w.root = first.MMRSize, first.Root
	refused := l.cut(2)
	l.deliver(2, refused, w, v)
	timeouts := 2
	w.before = func(r checkpoint.Record) error {
		if r.MMRSize == refused && r.PrevSize == first.MMRSize && timeouts > 0 {
			timeouts--
			return timedOutAnswer()
		}
		return nil
	}
	for n := 3; n < 5; n++ {
		l.deliver(n, l.cut(n), w, v)
		got := row(t, l.store, refused)
		assert.True(t, reanchorDue(got), "tick %d", n)
		reason, text := WitnessPendingReason(got, testWitness)
		assert.Equal(t, "witness_error", reason)
		assert.Equal(t, "pending: the witness refused it again after a retry, as not continuing the checkpoint it last accepted (HTTP 409); capsulectl proves it again from that checkpoint at a later capsulectl run, and later checkpoints wait for it.", text)
		assert.Equal(t, "pending", witnessResult(testWitness, got)["state"])
		assert.ErrorIs(t, deliveryOutcome(got), ErrPending)
		for _, size := range l.sizes[3:] {
			assert.Zero(t, row(t, l.store, size).Attempts, "checkpoint %d waits unsent", size)
		}
	}
	l.deliver(5, l.cut(5), w, v)
	assert.True(t, reanchored(row(t, l.store, refused)))
	for _, size := range l.sizes[3:] {
		later := row(t, l.store, size)
		require.NotNil(t, later.Receipt, "checkpoint %d", size)
		assert.NoError(t, v.Verify(later.Checkpoint, receiptOf(t, later)), "for its own statement")
	}
}

// A re-anchor the witness took but whose answer was lost is the same
// statement when sent again, which the witness answers with its receipt.
func TestALostReanchorAnswerIsAnsweredOnItsRetry(t *testing.T) {
	l := newReanchorLog(t)
	w := newChainWitness(t)
	v := w.verifier(nil)
	for n := 0; n < 2; n++ {
		require.NotNil(t, l.deliver(n, l.cut(n), w, v).Receipt)
	}
	first := l.record(l.sizes[0])
	w.size, w.root = first.MMRSize, first.Root
	refused := l.cut(2)
	l.deliver(2, refused, w, v)
	w.lose[refused] = 1
	l.deliver(3, refused, w, v)
	got := row(t, l.store, refused)
	assert.True(t, reanchorDue(got), "the answer was lost")
	assert.Equal(t, refused, w.size, "the witness took it")

	l.deliver(4, refused, w, v)
	got = row(t, l.store, refused)
	assert.True(t, reanchored(got))
	last := w.sent[len(w.sent)-1]
	assert.Equal(t, 2, w.sentCount(last), "the same re-anchor, sent twice")
	later := l.cut(5)
	require.NotNil(t, l.deliver(5, later, w, v).Receipt)
}

// The witness's answer to a re-anchor decides as for any delivery
// (witness.IsRetryable): a retryable one leaves it to a later call, any
// other marks it and it is not sent again.
func TestAReanchorRefusalEndsIt(t *testing.T) {
	l := newReanchorLog(t)
	w := newChainWitness(t)
	v := w.verifier(nil)
	for n := 0; n < 2; n++ {
		require.NotNil(t, l.deliver(n, l.cut(n), w, v).Receipt)
	}
	first := l.record(l.sizes[0])
	w.size, w.root = first.MMRSize, first.Root
	refused := l.cut(2)
	l.deliver(2, refused, w, v)
	answers := []int{429, 408, 403, 422}
	w.before = func(r checkpoint.Record) error {
		if r.MMRSize == refused && r.PrevSize == first.MMRSize && len(answers) > 0 {
			status := answers[0]
			answers = answers[1:]
			return classifySubmitError(&witness.HTTPError{StatusCode: status, Body: "no"}, true)
		}
		return nil
	}
	for n := 3; n < 6; n++ {
		l.deliver(n, refused, w, v)
		assert.True(t, reanchorDue(row(t, l.store, refused)), "tick %d: a retryable answer", n)
	}
	l.deliver(6, refused, w, v)
	got := row(t, l.store, refused)
	assert.False(t, reanchorDue(got))
	assert.Contains(t, got.LastError, "; not re-anchored: the witness refused the re-anchor (HTTP 422).")
	_, text := WitnessPendingReason(got, testWitness)
	assert.Equal(t, "not delivered: the witness refused it again after a retry, as not continuing the checkpoint it last accepted (HTTP 409); it was not re-anchored: the witness refused the re-anchor (HTTP 422); this delivery is not retried.", text)
	sent := len(w.sent)
	l.deliver(7, refused, w, v)
	assert.Len(t, w.sent, sent, "not sent again")
}

// A re-anchor is signed only for the profile's own, authentic checkpoint of
// the row's size, from an earlier size of the same log.
func TestAReanchorNeedsTheProfilesOwnCheckpoint(t *testing.T) {
	l := newReanchorLog(t)
	for n := 0; n < 3; n++ {
		l.cut(n)
	}
	first := l.record(l.sizes[0])
	good := row(t, l.store, l.sizes[2])
	_, e := reproveCheckpoint(t.Context(), l.p, l.store, good, first.MMRSize, first.Root)
	require.NoError(t, e)

	notReanchoredBy := func(name string, p Profile, w cll.WitnessState, size uint64, root, want string) {
		t.Helper()
		_, e := reproveCheckpoint(t.Context(), p, l.store, w, size, root)
		var no notReanchored
		require.ErrorAs(t, e, &no, name)
		assert.Equal(t, want, string(no), name)
	}
	tampered := good
	tampered.Checkpoint = append([]byte(nil), good.Checkpoint...)
	tampered.Checkpoint[len(tampered.Checkpoint)-1] ^= 1
	notReanchoredBy("bad signature", l.p, tampered, first.MMRSize, first.Root, "its stored checkpoint does not verify as this profile's")
	other := l.p
	other.LogID = "another-log"
	notReanchoredBy("another log", other, good, first.MMRSize, first.Root, "its stored checkpoint does not verify as this profile's")
	untrusted := l.p
	untrusted.Checkpoint.TrustedKeys = []string{hex.EncodeToString(make([]byte, ed25519.PublicKeySize))}
	notReanchoredBy("untrusted key", untrusted, good, first.MMRSize, first.Root, "its stored checkpoint does not verify as this profile's")
	moved := good
	moved.CheckpointSize = l.sizes[1]
	notReanchoredBy("row of another size", l.p, moved, first.MMRSize, first.Root, "its stored checkpoint names another size")
	notReanchoredBy("tip at the checkpoint", l.p, good, good.CheckpointSize, l.record(l.sizes[2]).Root, "the witness's last accepted checkpoint is not an earlier one")
	notReanchoredBy("no tip", l.p, good, 0, first.Root, "the witness's last accepted checkpoint is not an earlier one")
	notReanchoredBy("not an MMR size", l.p, good, 2, first.Root, "the witness's last accepted size is not a size of this log")
	notReanchoredBy("a fork", l.p, good, first.MMRSize, l.record(l.sizes[1]).Root, "the witness's last accepted checkpoint is not this log's")
	otherKey, _ := profileFixture(t)
	otherKey.LogID, otherKey.Checkpoint.TrustedKeys = l.p.LogID, append(otherKey.Checkpoint.TrustedKeys, l.p.Checkpoint.TrustedKeys...)
	notReanchoredBy("another signing key", otherKey, good, first.MMRSize, first.Root, "it was signed with another key than the profile's checkpoint key")

	assert.Nil(t, newReprover(l.p, rowsOnly{l.store}), "a store that keeps no tree here never re-anchors")
}

// A refused row with a later row already delivered is not re-anchored: the
// witness has moved past it.
func TestARowBehindADeliveredOneIsNotReanchored(t *testing.T) {
	l := newReanchorLog(t)
	for n := 0; n < 2; n++ {
		l.cut(n)
	}
	older := row(t, l.store, l.sizes[0])
	older.Attempts, older.Permanent = 2, true
	older.LastError = withRearmed(continuityRefusal(1).Error())
	require.NoError(t, l.store.CommitWitness(t.Context(), 0, older))
	newer := row(t, l.store, l.sizes[1])
	leaf, size := int64(0), int64(1)
	newer.Attempts, newer.Receipt = 1, &cll.WitnessReceiptState{Bytes: []byte{1}, EntryHash: hex.EncodeToString(make([]byte, 32)), EntryHashScheme: "legacy", LeafIndex: &leaf, TreeSize: &size}
	require.NoError(t, l.store.CommitWitness(t.Context(), 0, newer))
	require.True(t, reanchorDue(row(t, l.store, l.sizes[0])))

	w := newChainWitness(t)
	moved, e := reanchorRefused(t.Context(), l.store, "service-test", l.sizes[1], newReprover(l.p, l.store), w, w.verifier(nil), l.at(2))
	require.NoError(t, e)
	assert.False(t, moved)
	assert.Empty(t, w.sent)
	assert.Contains(t, row(t, l.store, l.sizes[0]).LastError, "; not re-anchored: a later checkpoint was delivered.")
}

// A log from before re-anchoring can hold several checkpoints each refused
// for good with the same old tip. Once the oldest is re-anchored, the next
// one's re-anchor is refused naming the new tip, which is the checkpoint it
// chains on, so its stored statement is sent as it is and gets its own
// receipt; the one after it the same. The witness ends at the last one.
func TestABacklogOfRefusedCheckpointsIsCleared(t *testing.T) {
	l := newReanchorLog(t)
	w := newChainWitness(t)
	v := w.verifier(nil)
	require.NotNil(t, l.deliver(0, l.cut(0), w, v).Receipt)
	tip := l.record(l.sizes[0])
	for n := 1; n < 4; n++ {
		l.cut(n)
	}
	refusal := withRearmed(w.refusal().Error())
	for _, size := range l.sizes[1:] {
		r := row(t, l.store, size)
		next := r
		next.Attempts, next.Permanent, next.LastError = 2, true, refusal
		require.NoError(t, l.store.CommitWitness(t.Context(), r.Attempts, next))
	}
	w.size, w.root = tip.MMRSize, tip.Root
	// The witness lost nothing: its tip is the first checkpoint, and the
	// second chains on it, so the second is sent as it is first.
	l.deliver(4, l.sizes[3], w, v)

	for _, size := range l.sizes[1:] {
		got := row(t, l.store, size)
		require.NotNil(t, got.Receipt, "checkpoint %d", size)
		assert.NoError(t, v.Verify(got.Checkpoint, receiptOf(t, got)), "checkpoint %d, for its own statement", size)
	}
	assert.Equal(t, l.sizes[3], w.size)
}

// The same backlog behind a witness that lost one checkpoint: the oldest is
// re-anchored, and the later ones follow on the tip it gives.
func TestABacklogBehindALostCheckpointIsCleared(t *testing.T) {
	l := newReanchorLog(t)
	w := newChainWitness(t)
	v := w.verifier(nil)
	for n := 0; n < 2; n++ {
		require.NotNil(t, l.deliver(n, l.cut(n), w, v).Receipt)
	}
	tip := l.record(l.sizes[0])
	w.size, w.root = tip.MMRSize, tip.Root
	for n := 2; n < 5; n++ {
		l.cut(n)
	}
	refusal := withRearmed(w.refusal().Error())
	for _, size := range l.sizes[2:] {
		r := row(t, l.store, size)
		next := r
		next.Attempts, next.Permanent, next.LastError = 2, true, refusal
		require.NoError(t, l.store.CommitWitness(t.Context(), r.Attempts, next))
	}
	l.deliver(5, l.sizes[4], w, v)

	assert.True(t, reanchored(row(t, l.store, l.sizes[2])), "the oldest is re-anchored")
	for _, size := range l.sizes[3:] {
		got := row(t, l.store, size)
		require.NotNil(t, got.Receipt, "checkpoint %d", size)
		assert.NoError(t, v.Verify(got.Checkpoint, receiptOf(t, got)), "checkpoint %d, for its own statement", size)
	}
	assert.Equal(t, l.sizes[4], w.size)
}

// A signing key that cannot be read now fails the call and leaves the row
// due, so a later run with the key re-anchors it.
func TestAnUnreadableKeyLeavesTheReanchorDue(t *testing.T) {
	l := newReanchorLog(t)
	w := newChainWitness(t)
	v := w.verifier(nil)
	for n := 0; n < 2; n++ {
		require.NotNil(t, l.deliver(n, l.cut(n), w, v).Receipt)
	}
	first := l.record(l.sizes[0])
	w.size, w.root = first.MMRSize, first.Root
	refused := l.cut(2)
	l.deliver(2, refused, w, v)

	seed := l.p.Checkpoint.Signing.Value
	unset := l.p
	unset.Checkpoint.Signing = Secret{Env: "CAPSULE_TEST_REANCHOR_KEY"}
	t.Setenv("CAPSULE_TEST_REANCHOR_KEY", "")
	_, e := deliverSelected(t.Context(), l.store, "service-test", refused, w, v, newReprover(unset, l.store), l.at(3), time.Time{})
	require.Error(t, e)
	assert.True(t, reanchorDue(row(t, l.store, refused)), "still due")

	t.Setenv("CAPSULE_TEST_REANCHOR_KEY", seed)
	_, e = deliverSelected(t.Context(), l.store, "service-test", refused, w, v, newReprover(unset, l.store), l.at(4), time.Time{})
	require.NoError(t, e)
	assert.True(t, reanchored(row(t, l.store, refused)))
}

// In a backlog, a row sent again as it is whose answer does not come holds
// the rows after it: none is re-anchored past it, and once it is delivered
// each gets a receipt for its own statement.
func TestABacklogWaitsForAResentRow(t *testing.T) {
	l := newReanchorLog(t)
	w := newChainWitness(t)
	v := w.verifier(nil)
	require.NotNil(t, l.deliver(0, l.cut(0), w, v).Receipt)
	tip := l.record(l.sizes[0])
	for n := 1; n < 4; n++ {
		l.cut(n)
	}
	refusal := withRearmed(w.refusal().Error())
	for _, size := range l.sizes[1:] {
		r := row(t, l.store, size)
		next := r
		next.Attempts, next.Permanent, next.LastError = 2, true, refusal
		require.NoError(t, l.store.CommitWitness(t.Context(), r.Attempts, next))
	}
	w.size, w.root = tip.MMRSize, tip.Root
	resent := row(t, l.store, l.sizes[1]).Checkpoint
	timeouts := 1
	w.before = func(r checkpoint.Record) error {
		if r.MMRSize == l.sizes[1] && timeouts > 0 {
			timeouts--
			return timedOutAnswer()
		}
		return nil
	}
	l.deliver(4, l.sizes[3], w, v)
	assert.Equal(t, 1, w.sentCount(resent), "the second checkpoint was sent as it is, and timed out")
	for _, s := range w.sent {
		r, e := checkpoint.ParseRecord(s)
		require.NoError(t, e)
		assert.LessOrEqual(t, r.MMRSize, l.sizes[1], "nothing after it was sent")
	}
	assert.Nil(t, row(t, l.store, l.sizes[1]).Receipt)

	l.deliver(6, l.sizes[3], w, v)
	for _, size := range l.sizes[1:] {
		got := row(t, l.store, size)
		require.NotNil(t, got.Receipt, "checkpoint %d", size)
		assert.NoError(t, v.Verify(got.Checkpoint, receiptOf(t, got)), "checkpoint %d, for its own statement", size)
	}
	assert.Equal(t, l.sizes[3], w.size)
}
