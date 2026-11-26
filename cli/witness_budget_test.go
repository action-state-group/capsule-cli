package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/action-state-group/checkpointed-local-log/go/witness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shortBudgetFloor lowers witnessBudgetFloor for one test.
func shortBudgetFloor(t *testing.T, floor time.Duration) {
	t.Helper()
	saved := witnessBudgetFloor
	witnessBudgetFloor = floor
	t.Cleanup(func() { witnessBudgetFloor = saved })
}

// hangingWitness is a plain-HTTP witness whose handler runs answer and, when
// answer returns false, blocks until the test ends.
func hangingWitness(t *testing.T, answer func(http.ResponseWriter, *http.Request) bool) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if answer != nil && answer(w, r) {
			return
		}
		<-release
	}))
	// Cleanups run last-in first-out: release the handlers, then close.
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	return server
}

// A witness that accepts the request and then hangs costs the caller its
// deadline, not the client's 30 s, and the attempt is recorded as a slow
// witness (capsule-cli#645). Both bindings go through DeliverWitnessBy's own
// client.
func TestDeliverWitnessByEndsAHangingWitness(t *testing.T) {
	shortBudgetFloor(t, 50*time.Millisecond)
	var polled atomic.Bool
	cases := map[string]struct {
		prefix string
		bound  time.Duration
		answer func(http.ResponseWriter, *http.Request) bool
	}{
		"plain witness": {"", 300 * time.Millisecond, nil},
		"SCRAPI POST":   {"scrapi+", 300 * time.Millisecond, nil},
		// CLL waits its 1 s poll interval before the GET, so the bound
		// leaves the GET part of the shared deadline: the POST and the
		// polls share it.
		"SCRAPI poll": {"scrapi+", 1500 * time.Millisecond, func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == http.MethodPost {
				w.Header().Set("Location", "/operations/1")
				w.WriteHeader(http.StatusAccepted)
				return true
			}
			polled.Store(true)
			return false
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p, store, _, _ := witnessReceiptFixture(t)
			defer func() { require.NoError(t, store.Close()) }()
			p.Checkpoint.Endpoint = c.prefix + hangingWitness(t, c.answer).URL
			start := time.Now()
			state, e := DeliverWitnessBy(t.Context(), p, store, "service-test", 1, start.Add(c.bound))
			elapsed := time.Since(start)
			require.NoError(t, e)
			assert.GreaterOrEqual(t, elapsed, c.bound)
			assert.Less(t, elapsed, c.bound+time.Second, "ends at the caller's deadline")
			assert.Equal(t, uint32(1), state.Attempts, "the attempt is recorded")
			assert.False(t, state.Permanent)
			reason, _ := WitnessPendingReason(state, p.Checkpoint.Endpoint)
			assert.Equal(t, "witness_timeout", reason)
		})
	}
	assert.True(t, polled.Load(), "the SCRAPI case reached its poll")
}

// stallSubmitter answers its first row after delay and blocks every later
// one until its context ends, classified as the CLI's submitters do.
type stallSubmitter struct {
	delay     time.Duration
	inner     *continuitySubmitter
	submitted int
}

func (s *stallSubmitter) Submit(ctx context.Context, b []byte) (witness.Receipt, error) {
	s.submitted++
	if s.submitted == 1 {
		time.Sleep(s.delay)
		return s.inner.Submit(ctx, b)
	}
	<-ctx.Done()
	return witness.Receipt{}, classifySubmitError(ctx.Err(), true)
}

// The deadline is one budget for the whole call: an older row that took
// most of it leaves the selection only the rest, not a fresh bound.
func TestDeliverWitnessByIsOneBudgetPerCall(t *testing.T) {
	shortBudgetFloor(t, 50*time.Millisecond)
	store, rows := checkpointRows(t, 2)
	submitter := &stallSubmitter{delay: 500 * time.Millisecond, inner: &continuitySubmitter{t: t}}
	start := time.Now()
	got, e := deliverSelected(t.Context(), store, "service-test", rows[1].CheckpointSize, submitter, acceptAll{}, time.Now().UTC().Add(time.Hour), start.Add(700*time.Millisecond))
	elapsed := time.Since(start)
	require.NoError(t, e)
	assert.Less(t, elapsed, time.Second, "a bound per row would take 1.2 s")
	assert.Equal(t, 2, submitter.submitted)
	older, e := store.GetWitness(t.Context(), "service-test", rows[0].CheckpointSize)
	require.NoError(t, e)
	assert.NotNil(t, older.Receipt)
	assert.Equal(t, uint32(1), got.Attempts)
	reason, _ := WitnessPendingReason(got, testWitness)
	assert.Equal(t, "witness_timeout", reason)
}

// A row whose request would start with less than the floor left is not
// sent and not recorded: neither a failed attempt with its backoff nor a
// consent wait comes from a budget the caller had already spent.
func TestDeliverWitnessByLeavesARowPastTheFloor(t *testing.T) {
	t.Run("budget spent before the call", func(t *testing.T) {
		shortBudgetFloor(t, time.Second)
		store, rows := checkpointRows(t, 1)
		submitter := &recordingSubmitter{}
		got, e := deliverSelected(t.Context(), store, "service-test", rows[0].CheckpointSize, submitter, acceptAll{}, time.Now().UTC().Add(time.Hour), time.Now().Add(500*time.Millisecond))
		require.NoError(t, e)
		assert.Empty(t, submitter.submitted)
		assert.Equal(t, rows[0], got, "the row is left as it was")
	})
	t.Run("budget spent by an older row", func(t *testing.T) {
		shortBudgetFloor(t, 300*time.Millisecond)
		store, rows := checkpointRows(t, 2)
		submitter := &stallSubmitter{delay: 300 * time.Millisecond, inner: &continuitySubmitter{t: t}}
		got, e := deliverSelected(t.Context(), store, "service-test", rows[1].CheckpointSize, submitter, acceptAll{}, time.Now().UTC().Add(time.Hour), time.Now().Add(500*time.Millisecond))
		require.NoError(t, e)
		assert.Equal(t, 1, submitter.submitted, "the selection is not sent")
		older, e := store.GetWitness(t.Context(), "service-test", rows[0].CheckpointSize)
		require.NoError(t, e)
		assert.NotNil(t, older.Receipt, "the older row's receipt is kept")
		assert.Equal(t, rows[1], got, "the selection is left as it was")
	})
}

// deadlineSubmitter records whether its requests carry a deadline.
type deadlineSubmitter struct {
	hasDeadline bool
	inner       *continuitySubmitter
}

func (d *deadlineSubmitter) Submit(ctx context.Context, b []byte) (witness.Receipt, error) {
	_, d.hasDeadline = ctx.Deadline()
	return d.inner.Submit(ctx, b)
}

// A caller that passes no deadline gets today's delivery: no deadline beyond
// the client's own per-request bound, and a receipt through the real binding.
func TestDeliverWitnessDefaultHasNoDeadline(t *testing.T) {
	store, rows := checkpointRows(t, 1)
	submitter := &deadlineSubmitter{inner: &continuitySubmitter{t: t}}
	got, e := deliverSelected(t.Context(), store, "service-test", rows[0].CheckpointSize, submitter, acceptAll{}, time.Now().UTC().Add(time.Hour), time.Time{})
	require.NoError(t, e)
	assert.NotNil(t, got.Receipt)
	assert.False(t, submitter.hasDeadline)

	p, logStore, _, body := witnessReceiptFixture(t)
	defer func() { require.NoError(t, logStore.Close()) }()
	p.Checkpoint.Endpoint = hangingWitness(t, func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "application/json")
		_, e := w.Write(body)
		assert.NoError(t, e)
		return true
	}).URL
	state, e := DeliverWitness(t.Context(), p, logStore, "service-test", 1)
	require.NoError(t, e)
	require.NotNil(t, state.Receipt)
	require.NoError(t, VerifyWitness(p, state))
}
