package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/action-state-group/cll-go/checkpoint"
	"github.com/action-state-group/cll-go/cll"
	"github.com/action-state-group/cll-go/witness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingWitness is an endpoint that only counts connections: it never
// answers, so every delivery to it stays pending.
func countingWitness(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var n atomic.Int64
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = l.Close() })
	return "https://" + l.Addr().String(), &n
}

// clockAt lets a test move dealClock.
func clockAt(t *testing.T, start time.Time) *time.Time {
	t.Helper()
	now := start
	old := dealClock
	dealClock = func() time.Time { return now }
	t.Cleanup(func() { dealClock = old })
	return &now
}

// cadenceProfile configures a witness and a cadence on the fixture profile.
func cadenceFixture(t *testing.T, endpoint string, pinned ed25519.PublicKey, interval, jitter string, pad uint64) Profile {
	t.Helper()
	dealFixture(t)
	p, err := loadProfile("deal")
	require.NoError(t, err)
	p.Checkpoint.Endpoint, p.Checkpoint.PublicKey = endpoint, hex.EncodeToString(pinned)
	p.Cadence.Interval, p.Cadence.Jitter, p.Cadence.PadBucket = interval, jitter, pad
	require.NoError(t, saveProfile(p, true))
	return p
}

func retailDeal(t *testing.T) string {
	t.Helper()
	dealID := dealRun(t, "open", "--input", filepath.Join(retailDemo, "open.json"))["deal_id"].(string)
	dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(retailDemo, "check-pay.json"))
	dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", filepath.Join(retailDemo, "act-pay.json"))
	return dealID
}

// deliver stores, for the cadence checkpoint at size, a receipt signed by
// signer, as a witness delivery would.
func deliver(t *testing.T, p Profile, size uint64, signer ed25519.PrivateKey) {
	t.Helper()
	target, err := openTarget(t.Context(), p, useCLL)
	require.NoError(t, err)
	defer func() { require.NoError(t, target.close()) }()
	service, err := serviceID(p)
	require.NoError(t, err)
	state, err := target.log.GetWitness(t.Context(), service, size)
	require.NoError(t, err)
	record, err := checkpoint.ParseRecord(state.Checkpoint)
	require.NoError(t, err)
	entry, err := record.EntryHash()
	require.NoError(t, err)
	next := state
	next.Receipt = &cll.WitnessReceiptState{Bytes: mintReceipt(t, entry, signer), EntryHash: hex.EncodeToString(entry), EntryHashScheme: witness.EntryHashSchemeCheckpointDigest, LeafIndex: ptr(int64(0)), TreeSize: ptr(int64(1))}
	require.NoError(t, target.log.CommitWitness(t.Context(), state.Attempts, next))
}

func cadenceSize(t *testing.T, p Profile) uint64 {
	t.Helper()
	target, err := openTarget(t.Context(), p, useCLLRead)
	require.NoError(t, err)
	defer func() { require.NoError(t, target.close()) }()
	state, err := target.log.LoadCLL(t.Context())
	require.NoError(t, err)
	require.NotNil(t, state.Checkpoint)
	return state.Checkpoint.Size
}

func cadenceLeaves(t *testing.T, p Profile) uint64 {
	t.Helper()
	target, err := openTarget(t.Context(), p, useCLLRead)
	require.NoError(t, err)
	defer func() { require.NoError(t, target.close()) }()
	state, err := target.log.LoadCLL(t.Context())
	require.NoError(t, err)
	return state.Checkpoint.IndexedSeq
}

// Deal events never contact the witness; only a due tick does.
func TestDealEventsNeverPublish(t *testing.T) {
	endpoint, connections := countingWitness(t)
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	cadenceFixture(t, endpoint, public, "1h", "0s", 0)
	dealID := dealRun(t, "open", "--input", filepath.Join(retailDemo, "open.json"))["deal_id"].(string)
	dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(retailDemo, "check-pay.json"))
	closed := dealRun(t, "close", "--deal", dealID, "--input", writeJSON(t, `{"status":"received","delivered":{"item":"cat sticker"}}`))
	assert.Equal(t, "scheduled", closed["checkpoint"].(map[string]any)["witness"])
	assert.Zero(t, connections.Load(), "opening, checking and closing a deal sends nothing to the witness")
	tick := dealRun(t, "tick")
	assert.Equal(t, "ticked", tick["state"])
	assert.Positive(t, connections.Load(), "a due tick is what reaches the witness")
}

// A2: a witness that is down never stops a deal; the receipt says pending
// until a later delivery, and then witnessed, with a chain verify checks.
func TestDealWitnessStatesAreShownAsTheyAre(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	endpoint, _ := countingWitness(t)
	p := cadenceFixture(t, endpoint, public, "1h", "0s", 0)
	dealID := retailDeal(t)
	dir := t.TempDir()
	report := func(name string) map[string]any {
		return dealRun(t, "report", "--deal", dealID, "--bundle", filepath.Join(dir, name+".json"), "--html", filepath.Join(dir, name+".html"))
	}
	assert.Equal(t, "scheduled", report("before")["assurance"].(map[string]any)["witness_state"], "not in a tick yet")

	tick := dealRun(t, "tick")
	require.Equal(t, "ticked", tick["state"])
	assert.Len(t, tick["pending"], 1, "the witness is down: the delivery is pending")
	pending := report("down")
	assert.Equal(t, "pending", pending["assurance"].(map[string]any)["witness_state"])
	assert.Equal(t, "sealed", pending["assurance"].(map[string]any)["rung"], "pending is never shown as witnessed")
	assert.Contains(t, pending["assurance"].(map[string]any)["text"], "Witness: pending")

	deliver(t, p, cadenceSize(t, p), key)
	witnessed := report("up")
	assurance := witnessed["assurance"].(map[string]any)
	assert.Equal(t, "witnessed", assurance["rung"])
	assert.NotContains(t, strings.ToLower(assurance["text"].(string)), "verified")
	page := string(mustRead(t, filepath.Join(dir, "up.html")))
	assert.Contains(t, page, "x-deal-cadence-v0")

	bundlePath := filepath.Join(dir, "up.json")
	result, err := verifyWithDirectory(t, bundlePath, writeDirectory(t, rawKeyRow(endpoint, public)))
	require.NoError(t, err)
	status, _ := bundleWitnesses(result)
	assert.Equal(t, "pass", status, "deal checkpoint -> tree path -> cadence entry -> witnessed cadence checkpoint")
	other, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	result, _ = verifyWithDirectory(t, bundlePath, writeDirectory(t, rawKeyRow(endpoint, other)))
	status, _ = bundleWitnesses(result)
	assert.Equal(t, "fail", status)
	result, _ = verifyWithDirectory(t, bundlePath, "")
	status, _ = bundleWitnesses(result)
	assert.Equal(t, "withheld", status, "without a directory the receipt is not checked")

	// Any change to the chain fails.
	var bundle map[string]any
	require.NoError(t, json.Unmarshal(mustRead(t, bundlePath), &bundle))
	chain := bundle["extensions"].(map[string]any)[dealCadenceExtension].(map[string]any)
	path := chain["path"].([]any)
	require.Len(t, path, dealCadenceDepth, "the path is always the same length")
	for _, tamper := range []func(map[string]any){
		func(c map[string]any) { c["salt"] = strings.Repeat("0", 64) },
		func(c map[string]any) { c["path"].([]any)[3] = strings.Repeat("ab", 32) },
		func(c map[string]any) { c["index"] = json.Number("7") },
	} {
		var copy map[string]any
		require.NoError(t, json.Unmarshal(mustRead(t, bundlePath), &copy))
		tamper(copy["extensions"].(map[string]any)[dealCadenceExtension].(map[string]any))
		path := filepath.Join(t.TempDir(), "tampered.json")
		raw, err := json.Marshal(copy)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, raw, 0o600))
		result, _ := verifyWithDirectory(t, path, writeDirectory(t, rawKeyRow(endpoint, public)))
		status, findings := bundleWitnesses(result)
		assert.Equal(t, "fail", status)
		assert.Contains(t, findings, "cadence_entry_not_included")
	}
}

func TestDealReceiptUnderAnotherKeyStaysPending(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, stranger, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	endpoint, _ := countingWitness(t)
	p := cadenceFixture(t, endpoint, public, "1h", "0s", 0)
	dealID := retailDeal(t)
	dealRun(t, "tick")
	deliver(t, p, cadenceSize(t, p), stranger)
	report := dealRun(t, "report", "--deal", dealID, "--bundle", filepath.Join(t.TempDir(), "b.json"))
	assert.Equal(t, "pending", report["assurance"].(map[string]any)["witness_state"])
}

// The tick times follow the clock and the jitter alone, and every tick adds
// the same number of entries to the published log, whatever the deals did.
func TestDealTickTimingAndSizeDoNotDependOnActivity(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	endpoint, _ := countingWitness(t)
	p := cadenceFixture(t, endpoint, public, "1h", "0s", 0)
	now := clockAt(t, time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC))
	var leaves []uint64
	for hour, deals := range []int{0, 3, 0, 1} {
		for range deals {
			retailDeal(t)
		}
		*now = time.Date(2026, 10, 4, 9+hour, 0, 0, 0, time.UTC)
		tick := dealRun(t, "tick")
		require.Equal(t, "ticked", tick["state"], "hour %d", hour)
		assert.Equal(t, time.Date(2026, 10, 4, 10+hour, 0, 0, 0, time.UTC).Format(time.RFC3339), tick["due"])
		*now = now.Add(30 * time.Minute)
		retailDeal(t)
		assert.Equal(t, "not_due", dealRun(t, "tick")["state"], "activity never brings a tick forward")
		leaves = append(leaves, cadenceLeaves(t, p))
	}
	assert.Equal(t, []uint64{1, 2, 3, 4}, leaves, "one entry per tick, with 0, 3, 0 or 1 new deals")
}

func TestDealTickJitterStaysInItsWindow(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	endpoint, _ := countingWitness(t)
	cadenceFixture(t, endpoint, public, "1h", "10m", 0)
	start := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	now := clockAt(t, start)
	tick := dealRun(t, "tick")
	due, err := time.Parse(time.RFC3339, tick["due"].(string))
	require.NoError(t, err)
	assert.False(t, due.Before(now.Add(50*time.Minute)))
	assert.False(t, due.After(now.Add(70*time.Minute)))
}

// With a bucket K > 1, every published leaf count is a multiple of K; the
// padding records are skipped everywhere a deal is read.
func TestDealCadencePadsToTheBucket(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	endpoint, _ := countingWitness(t)
	p := cadenceFixture(t, endpoint, public, "1h", "0s", 4)
	now := clockAt(t, time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC))
	dealID := retailDeal(t)
	for hour := range 3 {
		*now = time.Date(2026, 10, 4, 9+hour, 0, 0, 0, time.UTC)
		require.Equal(t, "ticked", dealRun(t, "tick")["state"])
		assert.Zero(t, cadenceLeaves(t, p)%4, "leaf count %d", cadenceLeaves(t, p))
	}
	assert.Equal(t, uint64(12), cadenceLeaves(t, p))
	deliver(t, p, cadenceSize(t, p), key)
	bundlePath := filepath.Join(t.TempDir(), "b.json")
	report := dealRun(t, "report", "--deal", dealID, "--bundle", bundlePath)
	assert.Equal(t, "witnessed", report["assurance"].(map[string]any)["rung"])
	assert.NotContains(t, strings.ToLower(report["trail"].(string)), "padding")
	for _, item := range report["did"].([]any) {
		assert.NotContains(t, strings.ToLower(item.(map[string]any)["text"].(string)), "padding")
	}
	result, err := verifyWithDirectory(t, bundlePath, writeDirectory(t, rawKeyRow(endpoint, public)))
	require.NoError(t, err)
	status, _ := bundleWitnesses(result)
	assert.Equal(t, "pass", status, "padding entries are ordinary leaves to the proof")
}

func TestDealInitTurnsTheWitnessOnByDefault(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	out, err := invoke(t, "", "deal", "init", "--profile", "w", "--dir", filepath.Join(t.TempDir(), "w"))
	require.NoError(t, err, out)
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	assert.Equal(t, "scheduled", result["witness"])
	assert.Equal(t, dealWitnessSees, result["witness_sees"])
	p, err := loadProfile("w")
	require.NoError(t, err)
	assert.Equal(t, dealDefaultWitness, p.Checkpoint.Endpoint)
	assert.Equal(t, dealDefaultWitnessKey, p.Checkpoint.PublicKey)
	assert.True(t, strings.HasPrefix(p.LogID, "deal-cadence/"))

	out, err = invoke(t, "", "deal", "init", "--profile", "n", "--dir", filepath.Join(t.TempDir(), "n"), "--no-witness")
	require.NoError(t, err, out)
	p, err = loadProfile("n")
	require.NoError(t, err)
	assert.Empty(t, p.Checkpoint.Endpoint)
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return raw
}
