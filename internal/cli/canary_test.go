package cli

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeWitness answers GET /checkpoints/{log_id} with whatever the test last set.
type fakeWitness struct {
	mu     sync.Mutex
	status int
	cp     witnessCheckpoint
	server *httptest.Server
}

func newFakeWitness(t *testing.T) *fakeWitness {
	t.Helper()
	w := &fakeWitness{status: http.StatusOK}
	w.server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		if r.Method != http.MethodGet || r.URL.Path != "/checkpoints/deal-cadence/00aa" {
			http.NotFound(rw, r)
			return
		}
		if w.status != http.StatusOK {
			rw.WriteHeader(w.status)
			return
		}
		_ = json.NewEncoder(rw).Encode(w.cp)
	}))
	t.Cleanup(w.server.Close)
	return w
}

func (w *fakeWitness) set(size, prevSize uint64, root, prevRoot, key string, at time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status = http.StatusOK
	w.cp = witnessCheckpoint{LogID: "deal-cadence/00aa", MMRSize: size, Root: root, PrevSize: prevSize, PrevRoot: prevRoot,
		KeyID: key, Timestamp: at.UTC().Truncate(time.Minute).Format(time.RFC3339), Equivocations: []json.RawMessage{}}
}

func watch(t *testing.T, w *fakeWitness, state string) (string, error) {
	t.Helper()
	return invoke(t, "", "canary", "watch", "--log-id", "deal-cadence/00aa", "--witness", w.server.URL,
		"--expect-every", "26h", "--state", state)
}

func TestCanaryWatchPassesWhileTheLogAdvances(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	now := clockAt(t, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	w := newFakeWitness(t)
	state := filepath.Join(t.TempDir(), "state.json")

	w.set(3, 1, "r3", "r1", "k", *now)
	out, err := watch(t, w, state)
	require.NoError(t, err, out)
	assert.Contains(t, out, `"state":"advancing"`)

	// A day later the log is further on, chaining from what was seen.
	*now = now.Add(24 * time.Hour)
	w.set(5, 3, "r5", "r3", "k", *now)
	out, err = watch(t, w, state)
	require.NoError(t, err, out)

	// Ticks between polls are normal: the next checkpoint need not chain
	// directly from the last one this watcher saw.
	*now = now.Add(24 * time.Hour)
	w.set(9, 7, "r9", "r7", "k", *now)
	out, err = watch(t, w, state)
	require.NoError(t, err, out)
}

func TestCanaryWatchAlarmsWhenTheLogStalls(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	now := clockAt(t, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	w := newFakeWitness(t)
	state := filepath.Join(t.TempDir(), "state.json")

	w.set(3, 1, "r3", "r1", "k", *now)
	_, err := watch(t, w, state)
	require.NoError(t, err)

	*now = now.Add(27 * time.Hour)
	_, err = watch(t, w, state)
	require.ErrorIs(t, err, ErrAlarm)
	assert.Equal(t, 6, ExitCode(err))
	assert.Equal(t, "canary alarm: deal-cadence/00aa has not advanced since 2026-10-04T12:00:00Z (27h0m0s ago; expected every 26h0m0s)", SafeError(err))
}

func TestCanaryWatchAlarmsWhenTheSizeHoldsAlthoughTheTimeMoves(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	now := clockAt(t, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	w := newFakeWitness(t)
	state := filepath.Join(t.TempDir(), "state.json")
	w.set(3, 1, "r3", "r1", "k", *now)
	_, err := watch(t, w, state)
	require.NoError(t, err)

	// A checkpoint whose time moves but whose size does not.
	*now = now.Add(27 * time.Hour)
	w.set(3, 1, "r3", "r1", "k", *now)
	_, err = watch(t, w, state)
	require.ErrorIs(t, err, ErrAlarm)
	assert.Contains(t, SafeError(err), "has stayed at size 3 since 2026-10-04T12:00:00Z")
}

func TestCanaryWatchAlarmsWhenTheHistoryIsRewritten(t *testing.T) {
	for name, tc := range map[string]struct {
		size, prevSize uint64
		root, prevRoot string
		key            string
		equivocations  int
		want           string
	}{
		"went back":        {2, 1, "r2", "r1", "k", 0, "it went back from size 5 to 2"},
		"same size, other": {5, 3, "x5", "r3", "k", 0, "size 5 now has root x5, not r5"},
		"broken link":      {7, 5, "r7", "x5", "k", 0, "the checkpoint after size 5 says that size had root x5, not r5"},
		"other key":        {7, 5, "r7", "r5", "k2", 0, "its checkpoints are now signed by key k2, not k"},
		"equivocation":     {7, 5, "r7", "r5", "k", 1, "the witness has flagged 1 conflicting checkpoint(s) for this log"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			now := clockAt(t, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
			w := newFakeWitness(t)
			state := filepath.Join(t.TempDir(), "state.json")
			w.set(5, 3, "r5", "r3", "k", *now)
			_, err := watch(t, w, state)
			require.NoError(t, err)

			*now = now.Add(time.Hour)
			w.set(tc.size, tc.prevSize, tc.root, tc.prevRoot, tc.key, *now)
			w.mu.Lock()
			for i := 0; i < tc.equivocations; i++ {
				w.cp.Equivocations = append(w.cp.Equivocations, json.RawMessage(`{"mmr_size":5}`))
			}
			w.mu.Unlock()
			_, err = watch(t, w, state)
			require.ErrorIs(t, err, ErrAlarm)
			assert.Equal(t, "canary alarm: deal-cadence/00aa history was rewritten: "+tc.want, SafeError(err))

			// The last good observation is kept: it keeps alarming.
			_, err = watch(t, w, state)
			require.ErrorIs(t, err, ErrAlarm)
		})
	}
}

func TestCanaryWatchNeverWitnessedIsAnAlarmButAnUnreadableWitnessIsNot(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	clockAt(t, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	w := newFakeWitness(t)
	state := filepath.Join(t.TempDir(), "state.json")

	w.mu.Lock()
	w.status = http.StatusNotFound
	w.mu.Unlock()
	_, err := watch(t, w, state)
	require.ErrorIs(t, err, ErrAlarm)
	assert.Contains(t, SafeError(err), "has never been witnessed")

	w.mu.Lock()
	w.status = http.StatusBadGateway
	w.mu.Unlock()
	_, err = watch(t, w, state)
	require.ErrorIs(t, err, errWitnessUnread)
	assert.NotErrorIs(t, err, ErrAlarm)
	assert.Equal(t, 1, ExitCode(err))
	_, statErr := os.Stat(state)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "nothing observed, nothing saved")
}

// canaryFixture is a deal profile with a witness that never answers, so a
// tick's delivery stays pending.
func canaryFixture(t *testing.T) {
	t.Helper()
	endpoint, _ := countingWitness(t)
	pub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	cadenceFixture(t, endpoint, pub, "1h", "10m", 1)
}

func canaryRun(t *testing.T, args ...string) (map[string]any, error) {
	t.Helper()
	out, err := invoke(t, "", append([]string{"--profile", "deal", "canary", "run"}, args...)...)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m), out)
	return m, nil
}

func TestCanaryRunSealsTheFlowThenTicks(t *testing.T) {
	canaryFixture(t)
	skill := filepath.Join(t.TempDir(), "SKILL.md")
	require.NoError(t, os.WriteFile(skill, []byte("# deal skill\n"), 0o600))
	sum := sha256.Sum256([]byte("# deal skill\n"))

	got, err := canaryRun(t, "--skill", skill, "--expect-version", cliVersion, "--expect-skill-sha256", hex.EncodeToString(sum[:]))
	require.NoError(t, err)
	assert.Equal(t, "ok", got["canary"])
	assert.Equal(t, "ticked", got["tick"])
	assert.Equal(t, hex.EncodeToString(sum[:]), got["skill_sha256"])
	assert.Equal(t, cliVersion, got["binary_version"])
	assert.NotEmpty(t, got["approval_id"], "the passing check's approval is part of the flow")
	assert.True(t, strings.HasPrefix(got["cadence_log"].(string), "deal-cadence/"))

	// The tick is spent: the next one is not due for an interval.
	assert.Equal(t, "not_due", dealRun(t, "tick")["state"])

	// The run sealed the whole flow. Sealed records carry no raw values: the
	// note with the binary version and skill digest is sealed as a commitment.
	exported := filepath.Join(t.TempDir(), "records.json")
	dealRun(t, "export", "--deal", got["deal_id"].(string), "--output", exported)
	data, err := os.ReadFile(exported)
	require.NoError(t, err)
	var records []struct {
		Body   map[string]any `json:"body"`
		Header struct {
			RecordType string `json:"record_type"`
		} `json:"x-deal-v0"`
	}
	require.NoError(t, json.Unmarshal(data, &records))
	var kinds []string
	for _, r := range records {
		kinds = append(kinds, r.Header.RecordType)
	}
	assert.Equal(t, []string{"baseline", "evidence", "check", "verdict", "approval"}, kinds)
	assert.Equal(t, "capsulectl_canary_run", records[1].Body["source"])
	assert.NotEmpty(t, records[1].Body["detail_commitment"])
}

func assertNoTick(t *testing.T) {
	t.Helper()
	tick := dealRun(t, "tick")
	assert.Equal(t, "ticked", tick["state"])
	assert.Equal(t, float64(1), tick["tick"], "the canary never ticked")
}

func TestCanaryRunStopsBeforeSealingOnDrift(t *testing.T) {
	canaryFixture(t)
	_, err := canaryRun(t, "--expect-version", "v9.9.9")
	require.ErrorIs(t, err, ErrConflict)
	assert.Equal(t, ErrConflict.Error()+": canary: this binary is "+cliVersion+", expected v9.9.9; nothing was sealed", SafeError(err))

	skill := filepath.Join(t.TempDir(), "SKILL.md")
	require.NoError(t, os.WriteFile(skill, []byte("# drifted\n"), 0o600))
	_, err = canaryRun(t, "--skill", skill, "--expect-skill-sha256", strings.Repeat("ab", 32))
	require.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, SafeError(err), "expected "+strings.Repeat("ab", 32)+"; nothing was sealed")

	_, err = canaryRun(t, "--skill", filepath.Join(t.TempDir(), "gone", "SKILL.md"))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "(no such file); nothing was sealed")

	assertNoTick(t)
}

func TestCanaryRunRefusesAProfileNobodyCanSeeFromOutside(t *testing.T) {
	dealFixture(t) // no witness
	_, err := canaryRun(t)
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "this profile has no witness")
}

func TestCanaryRunStopsBeforeTheTickWhenAVerbFails(t *testing.T) {
	canaryFixture(t)
	// A profile whose pinned witness key has been damaged: `deal open` fails.
	p, err := loadProfile("deal")
	require.NoError(t, err)
	p.Checkpoint.PublicKey = "not-a-key"
	require.NoError(t, saveProfile(p, true))

	_, err = canaryRun(t)
	require.Error(t, err)
	assert.Contains(t, SafeError(err), "canary: `deal open` failed: ")
	assert.Contains(t, SafeError(err), "; stopped before the tick")

	p.Checkpoint.PublicKey = hex.EncodeToString(make([]byte, 32))
	require.NoError(t, saveProfile(p, true))
	assertNoTick(t)
}
