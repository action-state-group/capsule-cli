package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
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
