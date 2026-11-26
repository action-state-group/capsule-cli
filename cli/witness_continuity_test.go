package cli

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/action-state-group/checkpointed-local-log/go/checkpoint"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/action-state-group/checkpointed-local-log/go/store/memory"
	"github.com/action-state-group/checkpointed-local-log/go/witness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tipRoot is the root a tipWitness reports as its last accepted one.
var tipRoot = strings.Repeat("ab", 32)

// continuityRefusal is what safeSubmitter records for a witness's
// continuity 409 that names last as its last-accepted size (capsule-anchor
// router.py, the ContinuityMismatchError detail).
func continuityRefusal(last uint64) error {
	body := fmt.Sprintf(`{"detail":{"error":"prev_size/prev_root does not match","last_accepted_mmr_size":%d,"last_accepted_root":%q}}`, last, tipRoot)
	return classifySubmitError(&witness.HTTPError{StatusCode: 409, Body: body}, true)
}

// tipWitness is a fake continuity witness: it accepts a checkpoint whose
// prev size is its tip and refuses any other with a continuity 409. It also
// refuses each size in refuse, valid or not, as often as that entry says,
// as the hosted witness did for a checkpoint whose predecessor's receipt it
// had issued before its tip moved (capsule-cli#647). fail answers each
// listed size with that error first.
type tipWitness struct {
	t      *testing.T
	last   uint64
	refuse map[uint64]int
	fail   map[uint64][]error
	sent   []uint64
}

func (w *tipWitness) Submit(_ context.Context, b []byte) (witness.Receipt, error) {
	record, e := checkpoint.ParseRecord(b)
	require.NoError(w.t, e)
	w.sent = append(w.sent, record.MMRSize)
	if errs := w.fail[record.MMRSize]; len(errs) > 0 {
		w.fail[record.MMRSize] = errs[1:]
		return witness.Receipt{}, errs[0]
	}
	if w.refuse[record.MMRSize] > 0 {
		w.refuse[record.MMRSize]--
		return witness.Receipt{}, continuityRefusal(w.last)
	}
	if record.PrevSize != w.last {
		return witness.Receipt{}, continuityRefusal(w.last)
	}
	w.last = record.MMRSize
	return witness.Receipt{Bytes: []byte{1}, EntryHash: strings.Repeat("00", 32), EntryHashScheme: "legacy", LeafIndex: 0, TreeSize: 1}, nil
}

func newTipWitness(t *testing.T) *tipWitness {
	return &tipWitness{t: t, refuse: map[uint64]int{}, fail: map[uint64][]error{}}
}

func row(t *testing.T, store cll.WitnessStateStore, size uint64) cll.WitnessState {
	t.Helper()
	w, e := store.GetWitness(t.Context(), "service-test", size)
	require.NoError(t, e)
	return w
}

// The 409 body's last-accepted size and root are kept, strictly shaped;
// nothing else from the body is.
func TestContinuityRefusalKeepsOnlyTheWitnessTip(t *testing.T) {
	e := continuityRefusal(35)
	require.Error(t, e)
	assert.Contains(t, e.Error(), "answered HTTP 409")
	assert.Contains(t, e.Error(), "; the witness last accepted mmr_size 35 root "+tipRoot)
	assert.NotContains(t, e.Error(), "does not match", "the witness's text is not kept")
	assert.False(t, witness.IsRetryable(e), "the runner stores it as failed; the next call re-arms it")

	upper := strings.ToUpper(tipRoot)
	for name, c := range map[string]struct {
		body string
		ok   bool
	}{
		"tip":             {`{"detail":{"last_accepted_mmr_size":7,"last_accepted_root":"` + tipRoot + `"}}`, true},
		"upper-case root": {`{"detail":{"last_accepted_mmr_size":7,"last_accepted_root":"` + upper + `"}}`, true},
		"negative size":   {`{"detail":{"last_accepted_mmr_size":-1,"last_accepted_root":"` + tipRoot + `"}}`, false},
		"string size":     {`{"detail":{"last_accepted_mmr_size":"7","last_accepted_root":"` + tipRoot + `"}}`, false},
		"fraction size":   {`{"detail":{"last_accepted_mmr_size":7.5,"last_accepted_root":"` + tipRoot + `"}}`, false},
		"no size":         {`{"detail":{"last_accepted_root":"` + tipRoot + `"}}`, false},
		"short root":      {`{"detail":{"last_accepted_mmr_size":7,"last_accepted_root":"abcd"}}`, false},
		"root not hex":    {`{"detail":{"last_accepted_mmr_size":7,"last_accepted_root":"` + strings.Repeat("zz", 32) + `"}}`, false},
		"no detail":       {`{"last_accepted_mmr_size":7,"last_accepted_root":"` + tipRoot + `"}`, false},
		"not json":        {`conflict`, false},
	} {
		size, root, ok := lastAccepted(c.body)
		assert.Equal(t, c.ok, ok, name)
		if c.ok {
			assert.Equal(t, uint64(7), size, name)
			assert.Equal(t, tipRoot, root, name)
		}
	}
	plain := classifySubmitError(&witness.HTTPError{StatusCode: 409, Body: "conflict"}, true)
	assert.Equal(t, "witness returned HTTP 400: "+WitnessFailureBody(409), plain.Error(), "an unreadable body keeps the status only")
}

// A 409 is not final on its first answer: the next call re-arms it, and a
// witness that has caught up delivers it. Until then it reads as pending.
func TestAContinuityRefusalIsSentAgainAtTheNextCall(t *testing.T) {
	store, rows := checkpointRows(t, 1)
	w := newTipWitness(t)
	w.refuse[rows[0].CheckpointSize] = 1
	now := time.Now().UTC().Add(time.Hour)

	got, e := deliverSelected(t.Context(), store, "service-test", rows[0].CheckpointSize, w, acceptAll{}, now, time.Time{})
	require.NoError(t, e)
	assert.True(t, got.Permanent)
	assert.Nil(t, got.Receipt)
	reason, text := WitnessPendingReason(got, testWitness)
	assert.Equal(t, "witness_error", reason)
	assert.Equal(t, continuityPending, text)
	assert.NotContains(t, text, "not retried")
	assert.Equal(t, "pending", witnessResult(testWitness, got)["state"])
	assert.ErrorIs(t, deliveryOutcome(got), ErrPending, "publish ends pending, not failed")

	got, e = deliverSelected(t.Context(), store, "service-test", rows[0].CheckpointSize, w, acceptAll{}, now.Add(2*time.Minute), time.Time{})
	require.NoError(t, e)
	require.NotNil(t, got.Receipt)
	assert.NoError(t, deliveryOutcome(got))
	assert.Equal(t, []uint64{rows[0].CheckpointSize, rows[0].CheckpointSize}, w.sent)
}

// A 409 refused again after its re-arm stays failed, says so, and is not
// re-armed a second time.
func TestAContinuityRefusalIsReArmedOnce(t *testing.T) {
	store, rows := checkpointRows(t, 1)
	w := newTipWitness(t)
	w.refuse[rows[0].CheckpointSize] = 3
	now := time.Now().UTC().Add(time.Hour)
	for tick := 0; tick < 3; tick++ {
		_, e := deliverSelected(t.Context(), store, "service-test", rows[0].CheckpointSize, w, acceptAll{}, now.Add(time.Duration(tick)*time.Hour), time.Time{})
		require.NoError(t, e)
	}
	assert.Len(t, w.sent, 2, "sent, then sent once more, then left")
	got := row(t, store, rows[0].CheckpointSize)
	assert.True(t, got.Permanent)
	assert.False(t, rearmable(got))
	assert.Contains(t, got.LastError, "last accepted mmr_size 0")
	reason, text := WitnessPendingReason(got, testWitness)
	assert.Equal(t, "witness_error", reason)
	assert.Equal(t, "not delivered: the witness refused it again after a retry, as not continuing the checkpoint it last accepted (HTTP 409); this delivery is not retried.", text)
	assert.Equal(t, "failed", witnessResult(testWitness, got)["state"])
	assert.EqualError(t, deliveryOutcome(got), "checkpoint delivery permanently failed")
}

// continuityPending is the status of a continuity refusal due for its one
// retry.
const continuityPending = "pending: the witness refused it as not continuing the checkpoint it last accepted (HTTP 409); it is sent once more at a later capsulectl run, a minute or more after the refusal."

// A delivery call made right after the refusal, in the same tick, re-arms
// the row but does not spend its retry yet, and the later row it was called
// for waits behind it: evidencebook's cadence delivers each pending row in
// turn, and each call re-arms first.
func TestTheRetryComesAMinuteAfterTheRefusal(t *testing.T) {
	store, rows := checkpointRows(t, 2)
	first, second := rows[0].CheckpointSize, rows[1].CheckpointSize
	w := newTipWitness(t)
	w.refuse[first] = 1
	now := time.Now().UTC().Add(time.Hour)

	got, e := deliverSelected(t.Context(), store, "service-test", first, w, acceptAll{}, now, time.Time{})
	require.NoError(t, e)
	require.True(t, rearmable(got))
	got, e = deliverSelected(t.Context(), store, "service-test", second, w, acceptAll{}, now, time.Time{})
	require.NoError(t, e)
	assert.Zero(t, got.Attempts, "the later row waits")
	assert.False(t, got.Permanent)
	assert.Equal(t, []uint64{first}, w.sent, "the refused row is not sent again in the same tick")
	held := row(t, store, first)
	assert.False(t, held.Permanent, "re-armed")
	assert.True(t, held.NextAttemptAt.After(now), "but not due yet")
	_, text := WitnessPendingReason(held, testWitness)
	assert.Equal(t, continuityPending, text)
	assert.ErrorIs(t, deliveryOutcome(held), ErrPending)

	got, e = deliverSelected(t.Context(), store, "service-test", second, w, acceptAll{}, now.Add(2*time.Minute), time.Time{})
	require.NoError(t, e)
	require.NotNil(t, got.Receipt)
	assert.Equal(t, []uint64{first, first, second}, w.sent)
}

// A row an older build stored as failed for good after a 409, long ago, is
// due at once when re-armed.
func TestAnOldContinuityRefusalIsDueAtOnce(t *testing.T) {
	store, rows := checkpointRows(t, 1)
	now := time.Now().UTC().Add(time.Hour)
	old := rows[0]
	old.Attempts, old.Permanent, old.NextAttemptAt = 1, true, now.Add(-24*time.Hour)
	old.LastError = "witness returned HTTP 400: checkpoint submission failed (the witness answered HTTP 409); response details suppressed"
	require.NoError(t, store.CommitWitness(t.Context(), rows[0].Attempts, old))
	got, e := deliverSelected(t.Context(), store, "service-test", rows[0].CheckpointSize, newTipWitness(t), acceptAll{}, now, time.Time{})
	require.NoError(t, e)
	assert.NotNil(t, got.Receipt)
}

// The re-arm mark outlives a retry that ends otherwise: 409, re-arm, a
// timeout, then 409 again is the second refusal, not a first one.
func TestTheReArmMarkOutlivesATimeout(t *testing.T) {
	store, rows := checkpointRows(t, 1)
	size := rows[0].CheckpointSize
	w := newTipWitness(t)
	w.refuse[size] = 3
	timeout := classifySubmitError(context.DeadlineExceeded, true)
	now := time.Now().UTC().Add(time.Hour)
	tick := func(n int) cll.WitnessState {
		got, e := deliverSelected(t.Context(), store, "service-test", size, w, acceptAll{}, now.Add(time.Duration(n)*24*time.Hour), time.Time{})
		require.NoError(t, e)
		return got
	}
	assert.True(t, rearmable(tick(0)), "first refusal")
	w.fail[size] = []error{timeout}
	got := tick(1)
	assert.False(t, got.Permanent, "the re-armed retry timed out")
	assert.Contains(t, got.LastError, WitnessTimedOut)
	assert.Contains(t, got.LastError, witnessRearmed)
	reason, _ := WitnessPendingReason(got, testWitness)
	assert.Equal(t, "witness_timeout", reason)
	got = tick(2)
	assert.True(t, got.Permanent)
	assert.False(t, rearmable(got), "refused again: not re-armed a second time")
	tick(3)
	assert.Len(t, w.sent, 3)
}

// A timeout before any refusal leaves no mark: the first 409 after it is
// still re-armed.
func TestATimeoutBeforeTheFirstRefusalLeavesNoMark(t *testing.T) {
	store, rows := checkpointRows(t, 1)
	size := rows[0].CheckpointSize
	w := newTipWitness(t)
	w.fail[size] = []error{classifySubmitError(context.DeadlineExceeded, true)}
	w.refuse[size] = 1
	now := time.Now().UTC().Add(time.Hour)
	for n := 0; n < 3; n++ {
		_, e := deliverSelected(t.Context(), store, "service-test", size, w, acceptAll{}, now.Add(time.Duration(n)*24*time.Hour), time.Time{})
		require.NoError(t, e)
	}
	got := row(t, store, size)
	require.NotNil(t, got.Receipt, "timeout, refusal, then delivered after the re-arm")
	assert.Len(t, w.sent, 3)
}

// While a refused row waits for its re-arm, and while the re-armed row is
// pending, a later checkpoint is held, not sent into a certain refusal.
func TestALaterCheckpointIsHeldBehindAReArmedRow(t *testing.T) {
	store, rows := checkpointRows(t, 2)
	first, second := rows[0].CheckpointSize, rows[1].CheckpointSize
	w := newTipWitness(t)
	w.refuse[first] = 1
	now := time.Now().UTC().Add(time.Hour)

	got, e := deliverSelected(t.Context(), store, "service-test", second, w, acceptAll{}, now, time.Time{})
	require.NoError(t, e)
	assert.Zero(t, got.Attempts, "held behind the refused row")
	assert.Equal(t, []uint64{first}, w.sent)

	// The re-armed retry does not reach the witness: still held.
	w.fail[first] = []error{classifySubmitError(context.DeadlineExceeded, false)}
	got, e = deliverSelected(t.Context(), store, "service-test", second, w, acceptAll{}, now.Add(time.Hour), time.Time{})
	require.NoError(t, e)
	assert.Zero(t, got.Attempts)
	assert.Equal(t, []uint64{first, first}, w.sent)

	got, e = deliverSelected(t.Context(), store, "service-test", second, w, acceptAll{}, now.Add(48*time.Hour), time.Time{})
	require.NoError(t, e)
	require.NotNil(t, got.Receipt)
	assert.NotNil(t, row(t, store, first).Receipt)
	assert.Equal(t, []uint64{first, first, first, second}, w.sent)
}

// rowsOnly is a store that cannot list its rows, as a plugin's.
type rowsOnly struct{ cll.WitnessStateStore }

// A store that cannot list its rows still re-arms the refused predecessor
// it chains on, so the selection held behind it is delivered.
func TestAStoreWithoutListingReArmsTheRefusedPredecessor(t *testing.T) {
	logStore, rows := checkpointRows(t, 3)
	store := rowsOnly{logStore}
	_, listed := any(store).(cll.CheckpointStateStore)
	require.False(t, listed)
	w := newTipWitness(t)
	w.refuse[rows[1].CheckpointSize] = 1
	now := time.Now().UTC().Add(time.Hour)
	for n := 0; n < 2; n++ {
		_, e := deliverSelected(t.Context(), store, "service-test", rows[2].CheckpointSize, w, acceptAll{}, now.Add(time.Duration(n)*time.Hour), time.Time{})
		require.NoError(t, e)
	}
	for _, r := range rows {
		assert.NotNil(t, row(t, store, r.CheckpointSize).Receipt, "size %d", r.CheckpointSize)
	}
	chain, e := witnessChain(t.Context(), store, "service-test", rows[2].CheckpointSize)
	require.NoError(t, e)
	assert.Len(t, chain, 1, "the walk stops at the first receipted row")
}

// The regression run of capsule-cli#647: entries are appended, cut and
// delivered tick by tick, and the witness refuses one valid checkpoint once
// mid-run. Every checkpoint after it still gets a receipt.
func TestOneTransientContinuityRefusalMidRun(t *testing.T) {
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

	w := newTipWitness(t)
	start := time.Now().UTC()
	var sizes []uint64
	const ticks = 8
	for n := 0; n < ticks+2; n++ {
		at := start.Add(time.Duration(n) * time.Hour)
		if n < ticks {
			_, e = store.Append(t.Context(), cll.AppendInput{Value: bytes.Repeat([]byte{byte(n + 1)}, 32), AppendedAt: at})
			require.NoError(t, e)
			_, e = runner.RunOnce(t.Context(), at)
			require.NoError(t, e)
			state, e := store.LoadCLL(t.Context())
			require.NoError(t, e)
			sizes = append(sizes, state.Checkpoint.Size)
			if n == 3 {
				w.refuse[state.Checkpoint.Size] = 1
			}
		}
		_, e = deliverSelected(t.Context(), store, "service-test", sizes[len(sizes)-1], w, acceptAll{}, at, time.Time{})
		require.NoError(t, e)
	}
	for _, size := range sizes {
		got := row(t, store, size)
		assert.NotNil(t, got.Receipt, "checkpoint %d has its receipt", size)
		assert.False(t, got.Permanent, "checkpoint %d", size)
	}
	assert.Equal(t, sizes[len(sizes)-1], w.last, "the witness's tip is the last checkpoint")
	refused := 0
	for _, s := range w.sent {
		if s == sizes[3] {
			refused++
		}
	}
	assert.Equal(t, 2, refused, "the refused checkpoint was sent, refused, and sent again")
}
