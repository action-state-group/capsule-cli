package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// publishedTicks reads the time each cadence tick was published.
func publishedTicks(t *testing.T) []time.Time {
	t.Helper()
	p, err := loadProfile("deal")
	require.NoError(t, err)
	db, _, err := sqliteConnection(p)
	require.NoError(t, err)
	defer db.Close()
	rows, err := db.Query(`SELECT at FROM deal_cadence ORDER BY tick`)
	require.NoError(t, err)
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var at string
		require.NoError(t, rows.Scan(&at))
		ts, err := time.Parse(time.RFC3339, at)
		require.NoError(t, err)
		out = append(out, ts)
	}
	return out
}

// offsets are the publication times' positions within the scheduler period.
func offsets(times []time.Time, period time.Duration) map[time.Duration]int {
	out := map[time.Duration]int{}
	for _, ts := range times {
		out[time.Duration(ts.UnixNano())%period]++
	}
	return out
}

// runScheduler runs `deal tick` every period for the given span, with args.
func runScheduler(t *testing.T, now *time.Time, start time.Time, period, span time.Duration, args ...string) {
	t.Helper()
	for at := start; at.Before(start.Add(span)); at = at.Add(period) {
		*now = at
		dealRun(t, append([]string{"tick"}, args...)...)
	}
}

func cadenceAt(t *testing.T) (*time.Time, time.Time) {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	endpoint, _ := countingWitness(t)
	cadenceFixture(t, endpoint, public, "1h", "10m", 0)
	start := time.Date(2026, 10, 4, 4, 17, 0, 0, time.UTC)
	return clockAt(t, start), start
}

// The defect: an hourly host scheduler that runs `deal tick` on the minute
// publishes on the scheduler's own grid, so the jitter never shows.
func TestDealTickOnAnHourlySchedulerAlignsToIt(t *testing.T) {
	now, start := cadenceAt(t)
	runScheduler(t, now, start, time.Hour, 12*time.Hour)
	ticks := publishedTicks(t)
	require.NotEmpty(t, ticks)
	t.Logf("published at %v", ticks)
	require.Len(t, offsets(ticks, time.Hour), 1, "every tick lands on the scheduler's minute")
}

// fakeSleep makes dealSleep advance the test clock instead of waiting.
func fakeSleep(t *testing.T, now *time.Time) {
	t.Helper()
	old := dealSleep
	dealSleep = func(_ context.Context, d time.Duration) error { *now = now.Add(d); return nil }
	t.Cleanup(func() { dealSleep = old })
}

// assertJittered: the publication times are not on one scheduler grid, and
// no gap between ticks is longer than the interval plus the jitter (plus the
// one-minute granularity of the published time).
func assertJittered(t *testing.T, ticks []time.Time, period time.Duration) {
	t.Helper()
	require.GreaterOrEqual(t, len(ticks), 8)
	assert.Greater(t, len(offsets(ticks, period)), 2, "publication times are not aligned to the scheduler: %v", ticks)
	gaps := map[time.Duration]bool{}
	for i := 1; i < len(ticks); i++ {
		gap := ticks[i].Sub(ticks[i-1])
		assert.LessOrEqual(t, gap, time.Hour+10*time.Minute+time.Minute, "no tick slips to a later run: %v", ticks)
		assert.GreaterOrEqual(t, gap, time.Hour-10*time.Minute-time.Minute)
		gaps[gap] = true
	}
	assert.Greater(t, len(gaps), 2, "the gaps vary: %v", ticks)
}

// The documented schedule: run `deal tick` every minute.
func TestDealTickEveryMinuteFollowsTheJitter(t *testing.T) {
	now, start := cadenceAt(t)
	runScheduler(t, now, start, time.Minute, 10*time.Hour)
	assertJittered(t, publishedTicks(t), time.Hour)
}

// A scheduler that can only run hourly, with --wait-up-to its period.
func TestDealTickHourlyWithWaitFollowsTheJitter(t *testing.T) {
	now, start := cadenceAt(t)
	fakeSleep(t, now)
	runScheduler(t, now, start, time.Hour, 12*time.Hour, "--wait-up-to", "1h")
	assertJittered(t, publishedTicks(t), time.Hour)
}

// Two `deal tick --wait-up-to` runs that overlap (a scheduler firing again
// while an earlier run still waits) both wake for the same due tick: exactly
// one of them publishes it. The due time is re-checked under the store lock.
func TestDealTickOverlappingRunsPublishOnce(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	endpoint, _ := countingWitness(t)
	cadenceFixture(t, endpoint, public, "1h", "0s", 0)

	var mu sync.Mutex
	now := time.Date(2026, 10, 4, 4, 0, 0, 0, time.UTC)
	oldClock, oldSleep := dealClock, dealSleep
	t.Cleanup(func() { dealClock, dealSleep = oldClock, oldSleep })
	dealClock = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	dealRun(t, "tick") // the first tick; the next is due at 05:00
	require.Len(t, publishedTicks(t), 1)

	asleep := make(chan time.Duration, 2)
	wake := make(chan struct{})
	dealSleep = func(_ context.Context, d time.Duration) error {
		asleep <- d
		<-wake
		return nil
	}
	mu.Lock()
	now = now.Add(30 * time.Minute)
	mu.Unlock()

	var wg sync.WaitGroup
	outs := make([]string, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], errs[i] = invoke(t, "", "--profile", "deal", "deal", "tick", "--wait-up-to", "1h")
		}()
	}
	// Both runs found the tick not yet due and are waiting for it.
	for range 2 {
		select {
		case d := <-asleep:
			assert.Equal(t, 30*time.Minute, d)
		case <-time.After(30 * time.Second):
			t.Fatal("both runs should be waiting for the due tick")
		}
	}
	mu.Lock()
	now = time.Date(2026, 10, 4, 5, 0, 0, 0, time.UTC)
	mu.Unlock()
	close(wake)
	wg.Wait()

	published := 0
	for i := range 2 {
		require.NoError(t, errs[i], outs[i])
		var out map[string]any
		require.NoError(t, json.Unmarshal([]byte(outs[i]), &out))
		published += int(out["published_this_run"].(float64))
	}
	assert.Equal(t, 1, published, "one of the two runs publishes the tick, never both")
	ticks := publishedTicks(t)
	require.Len(t, ticks, 2, "the 05:00 tick is published exactly once")
	assert.Equal(t, time.Date(2026, 10, 4, 5, 0, 0, 0, time.UTC), ticks[1])
}
