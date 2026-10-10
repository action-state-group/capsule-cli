package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/action-state-group/checkpointed-local-log/go/checkpoint"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/action-state-group/checkpointed-local-log/go/witness"
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
	assert.Equal(t, "self_attested", pending["assurance"].(map[string]any)["rung"], "pending is never shown as witnessed")
	assert.Contains(t, pending["assurance"].(map[string]any)["text"], "Witness pending. This checkpoint was sent at a cadence tick")

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

// A delivery that never reached the witness (as a host's unanswered
// network-consent prompt looks from here) is shown as such, never silently.
func TestDealBlockedDeliveryIsShownAsNetworkConsent(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	cadenceFixture(t, "https://127.0.0.1:1", public, "1h", "0s", 0)
	dealID := retailDeal(t)
	tick := dealRun(t, "tick")
	pending := tick["pending"].([]any)
	require.Len(t, pending, 1)
	row := pending[0].(map[string]any)
	assert.Equal(t, "network_consent_needed", row["reason"])
	assert.True(t, strings.HasPrefix(row["text"].(string), "pending: network consent needed"))
	assert.Contains(t, row["text"], `"Always allow this site"`)
	withBundle := dealRun(t, "report", "--deal", dealID, "--bundle", filepath.Join(t.TempDir(), "b.json"))
	assurance := withBundle["assurance"].(map[string]any)
	assert.Equal(t, "pending", assurance["witness_state"])
	assert.Equal(t, "network_consent_needed", assurance["witness_reason"])
	assert.Contains(t, assurance["text"], "Witness pending: network consent needed")
}

func TestWitnessPendingReasons(t *testing.T) {
	reason, text := witnessPendingReason(cll.WitnessState{Attempts: 1, LastError: "witness returned HTTP 503: " + witnessNotReached}, dealDefaultWitness)
	assert.Equal(t, "network_consent_needed", reason)
	assert.Contains(t, text, `choose "Always allow this site" for agentactioncapsule.org (the witness is witness.agentactioncapsule.org)`)
	assert.Contains(t, text, "That grant covers agentactioncapsule.org and all its subdomains.")
	reason, _ = witnessPendingReason(cll.WitnessState{Attempts: 1, LastError: "witness returned HTTP 503: checkpoint submission failed; response details suppressed"}, dealDefaultWitness)
	assert.Equal(t, "witness_error", reason, "a witness that answered is not a consent problem")
	reason, _ = witnessPendingReason(cll.WitnessState{}, dealDefaultWitness)
	assert.Equal(t, "not_attempted", reason)
	// What the witness answered is kept (its status, never its body) and said.
	reason, text = witnessPendingReason(cll.WitnessState{Attempts: 1, LastError: "witness returned HTTP 400: " + witnessFailureBody(409)}, dealDefaultWitness)
	assert.Equal(t, "witness_error", reason)
	assert.Equal(t, "pending: the witness answered with an error (HTTP 409); it is retried at every tick.", text)
}

func TestDoctorExplainsWitnessConsent(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	cadenceFixture(t, "https://127.0.0.1:1", public, "1h", "0s", 0)
	out, err := invoke(t, "", "doctor", "--profile", "deal", "--check-witness")
	require.NoError(t, err, out)
	var report map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	w := report["witness"].(map[string]any)
	assert.Equal(t, false, w["reachable"])
	assert.Equal(t, witnessConsentText("https://127.0.0.1:1"), w["consent"])

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusMethodNotAllowed) }))
	t.Cleanup(server.Close)
	cadenceFixture(t, server.URL, public, "1h", "0s", 0)
	out, err = invoke(t, "", "doctor", "--profile", "deal", "--check-witness")
	require.NoError(t, err, out)
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Contains(t, report["witness"].(map[string]any)["consent"], `"Always allow this site"`)
}

// A deal is never called witnessed at the moment it happens: until a tick has
// carried it to the witness, the receipt says witness pending, and why.
func TestDealIsWitnessPendingUntilATick(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	endpoint, _ := countingWitness(t)
	p := cadenceFixture(t, endpoint, public, "5m", "2m", 0)
	dealID := retailDeal(t)
	page := filepath.Join(t.TempDir(), "r.html")
	report := dealRun(t, "report", "--deal", dealID, "--html", page)
	a := report["assurance"].(map[string]any)
	assert.Equal(t, "scheduled", a["witness_state"])
	assert.Equal(t, "self_attested", a["rung"])
	assert.Contains(t, a["text"], "Witness pending. A deal is not witnessed at the moment it happens: its checkpoint goes to the witness at the next tick of this profile's checkpoint cadence, within about 7m (every 5m, give or take 2m)")
	assert.NotContains(t, a["text"], "an hour by default", "the profile's own cadence, not the default")
	html := string(mustRead(t, page))
	assert.Contains(t, html, "Sealed by my agent, witness pending.")
	for _, text := range []string{a["text"].(string), html} {
		assert.NotContains(t, text, "witnessed at the time")
		assert.NotContains(t, text, "Witnessed when")
	}

	// The tick at 18:07 carries the deal's checkpoint; once its receipt is
	// back, the same deal is witnessed, and says when its checkpoint was cut.
	now := clockAt(t, time.Date(2026, 9, 27, 18, 7, 31, 0, time.UTC))
	require.Equal(t, "ticked", dealRun(t, "tick")["state"])
	*now = now.Add(time.Minute)
	deliver(t, p, cadenceSize(t, p), key)
	page = filepath.Join(t.TempDir(), "r2.html")
	report = dealRun(t, "report", "--deal", dealID, "--html", page)
	a = report["assurance"].(map[string]any)
	assert.Equal(t, "witnessed", a["rung"])
	assert.Equal(t, "2026-09-27T18:07:00Z", a["checkpoint_at"], "the tick's time, coarsened to the minute")
	assert.Contains(t, a["text"], "signed a receipt for this deal's checkpoint, cut at 2026-09-27T18:07:00Z at a cadence tick after the deal's steps")
	cadence := embeddedBundle(t, string(mustRead(t, page)))["extensions"].(map[string]any)[dealCadenceExtension].(map[string]any)
	assert.Equal(t, "2026-09-27T18:07:00Z", cadence["checkpoint_at"], "the page reads it from the bundle")
}

// The report names its rung from what it can show. A step sealed after the
// last tick no longer drops the report to self-attested: the witnessed
// earlier checkpoint is carried with a consistency proof to the current one,
// and the report says which steps the witness covers.
func TestDealReportRungWitnessedInPart(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	endpoint, _ := countingWitness(t)
	p := cadenceFixture(t, endpoint, public, "1h", "0s", 0)
	now := clockAt(t, time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC))
	dealID := retailDeal(t)
	dir := t.TempDir()
	report := func(name string) (map[string]any, string) {
		path := filepath.Join(dir, name+".json")
		return dealRun(t, "report", "--deal", dealID, "--bundle", path, "--html", filepath.Join(dir, name+".html"))["assurance"].(map[string]any), path
	}
	directory := writeDirectory(t, rawKeyRow(endpoint, public))

	none, _ := report("none")
	assert.Equal(t, "self_attested", none["rung"])
	assert.True(t, strings.HasPrefix(none["text"].(string), "Sealed by my agent"))

	require.Equal(t, "ticked", dealRun(t, "tick")["state"])
	deliver(t, p, cadenceSize(t, p), key)
	all, _ := report("all")
	assert.Equal(t, "witnessed", all["rung"])
	assert.Equal(t, "2026-10-04T09:00:00Z", all["checkpoint_at"])
	assert.Contains(t, all["text"], "cut at 2026-10-04T09:00:00Z")

	// One more step after the tick.
	*now = now.Add(10 * time.Minute)
	dealRun(t, "note", "--deal", dealID, "--kind", "message", "--input", writeJSON(t, `{"from":"counterparty","text":"Your order has shipped."}`))
	part, partPath := report("part")
	assert.Equal(t, "witnessed_in_part", part["rung"])
	k, n := part["steps_witnessed"].(float64), part["steps"].(float64)
	assert.Equal(t, k+1, n)
	assert.Contains(t, part["text"], fmt.Sprintf("covering steps 1 to %d of %d", int(k), int(n)))
	assert.Contains(t, part["text"], fmt.Sprintf("Steps %d to %d are sealed by my agent on this device only", int(k)+1, int(n)))
	assert.Contains(t, part["text"], "Witness pending for the rest: the current checkpoint goes to the witness at the next tick")
	// What the page shows a reader is checked rendered, in Chrome
	// (TestPresentationDealWitnessLine): the page's source always carries
	// the deal view's words, so it proves nothing here.
	result, err := verifyWithDirectory(t, partPath, directory)
	require.NoError(t, err)
	status, findings := bundleWitnesses(result)
	assert.Equal(t, "pass", status, "earlier checkpoint -> consistency -> current checkpoint, and the earlier one's chain: %v", findings)

	// The chain is written under x-cadence-witness/v0, and read under it and
	// under both names bundles carried it under before (x-deal-cadence-v0
	// named its log deal_log_id): each verifies, and the rung reads the same.
	{
		var written map[string]any
		require.NoError(t, json.Unmarshal(mustRead(t, partPath), &written))
		exts := written["extensions"].(map[string]any)
		require.Contains(t, exts, "x-cadence-witness/v0")
		for _, name := range []string{"cadence-witness/v0", "x-deal-cadence-v0"} {
			require.NotContains(t, exts, name)
		}
	}
	for _, name := range []string{"x-cadence-witness/v0", "cadence-witness/v0", "x-deal-cadence-v0"} {
		var renamed map[string]any
		require.NoError(t, json.Unmarshal(mustRead(t, partPath), &renamed))
		exts := renamed["extensions"].(map[string]any)
		chain := exts[dealCadenceExtension].(map[string]any)
		delete(exts, dealCadenceExtension)
		if name == "x-deal-cadence-v0" {
			chain["deal_log_id"] = chain["log_id"]
			delete(chain, "log_id")
		}
		exts[name] = chain
		path := filepath.Join(t.TempDir(), "renamed.json")
		raw, err := json.Marshal(renamed)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, raw, 0o600))
		result, _ := verifyWithDirectory(t, path, directory)
		status, findings := bundleWitnesses(result)
		assert.Equal(t, "pass", status, "%s: %v", name, findings)
		assert.Equal(t, "witnessed_in_part", dealAssuranceRung(renamed)["rung"], name)
	}

	// Tampering with the earlier checkpoint or its proof fails.
	for name, tamper := range map[string]func(map[string]any){
		"proof": func(c map[string]any) {
			proof := c["earlier"].(map[string]any)["consistency_proof"].(map[string]any)
			peaks := proof["new_peaks"].([]any)
			peaks[0] = strings.Repeat("ab", 32)
		},
		"checkpoint": func(c map[string]any) {
			c["earlier"].(map[string]any)["checkpoint"].(map[string]any)["cose"] = c["cadence"].(map[string]any)["checkpoint"].(map[string]any)["cose"]
		},
		"extent": func(c map[string]any) { c["extent"] = "all" },
	} {
		var copy map[string]any
		require.NoError(t, json.Unmarshal(mustRead(t, partPath), &copy))
		tamper(copy["extensions"].(map[string]any)[dealCadenceExtension].(map[string]any))
		path := filepath.Join(t.TempDir(), "tampered.json")
		raw, err := json.Marshal(copy)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, raw, 0o600))
		result, _ := verifyWithDirectory(t, path, directory)
		status, _ := bundleWitnesses(result)
		assert.Equal(t, "fail", status, name)
	}

	// The next tick holds the current checkpoint but its receipt has not come
	// back: still witnessed in part, the rest pending.
	*now = time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	require.Equal(t, "ticked", dealRun(t, "tick")["state"])
	pending, _ := report("pending")
	assert.Equal(t, "witnessed_in_part", pending["rung"])
	assert.Contains(t, pending["text"], "Witness pending for the rest.")

	// Once that receipt is in, every step is witnessed.
	deliver(t, p, cadenceSize(t, p), key)
	full, _ := report("full")
	assert.Equal(t, "witnessed", full["rung"])
	assert.Equal(t, "2026-10-04T10:00:00Z", full["checkpoint_at"])
	plain := dealRun(t, "report", "--deal", dealID)["assurance"].(map[string]any)
	assert.Equal(t, "witnessed", plain["rung"], "the plain report states the rung too")
}

// A later tick still waiting for its receipt never hides an earlier tick,
// holding the same checkpoint, whose receipt verified.
func TestDealReportUsesTheNewestVerifiedTick(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	endpoint, _ := countingWitness(t)
	p := cadenceFixture(t, endpoint, public, "1h", "0s", 0)
	now := clockAt(t, time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC))
	dealID := retailDeal(t)
	require.Equal(t, "ticked", dealRun(t, "tick")["state"])
	deliver(t, p, cadenceSize(t, p), key)
	*now = time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	require.Equal(t, "ticked", dealRun(t, "tick")["state"])
	assurance := dealRun(t, "report", "--deal", dealID, "--bundle", filepath.Join(t.TempDir(), "b.json"))["assurance"].(map[string]any)
	assert.Equal(t, "witnessed", assurance["rung"])
	assert.Equal(t, "2026-10-04T09:00:00Z", assurance["checkpoint_at"])
}

func TestMMRLeafCount(t *testing.T) {
	for leaves, size := range []uint64{0, 1, 3, 4, 7, 8, 10, 11, 15} {
		assert.Equal(t, uint64(leaves), mmrLeafCount(size), "size %d", size)
	}
}

// The poll: a `deal tick` that is not due publishes nothing. With every
// cadence delivery complete, it makes no connection to the witness at all,
// however often it runs. The one thing a poll may send is a retry of a
// cadence delivery still pending (the witness was down at its tick), once
// that delivery's backoff has passed: the checkpoint it carries was cut at
// its tick, so only the retry's time follows the poll, never a deal.
func TestAPollThatIsNotDueDoesNotContactTheWitness(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	endpoint, connections := countingWitness(t)
	p := cadenceFixture(t, endpoint, public, "1h", "0s", 0)
	retailDeal(t)

	tick := dealRun(t, "tick")
	require.Equal(t, "ticked", tick["state"])
	require.Len(t, tick["pending"], 1, "the counting witness accepts nothing: the delivery is pending")
	deliver(t, p, cadenceSize(t, p), key)
	before := connections.Load()
	for range 3 {
		poll := dealRun(t, "tick")
		assert.Equal(t, "not_due", poll["state"])
		assert.Empty(t, poll["pending"])
	}
	assert.Equal(t, before, connections.Load(), "a poll that is not due, with nothing pending, makes no connection to the witness")
}

// The checkpoint cadence by default: a tick every 5m, give or take 1m (288
// a day), and a jitter its own validation accepts. A profile that sets its
// own keeps it.
func TestTheCheckpointCadenceDefaultsToFiveMinutes(t *testing.T) {
	cfg, err := Profile{}.dealCadence()
	require.NoError(t, err, "the default passes the cadence's own validation (jitter under half the interval)")
	assert.Equal(t, 5*time.Minute, cfg.interval)
	assert.Equal(t, time.Minute, cfg.jitter)
	var p Profile
	p.Cadence.Interval, p.Cadence.Jitter = "1h", "10m"
	own, err := p.dealCadence()
	require.NoError(t, err)
	assert.Equal(t, time.Hour, own.interval, "a profile's own cadence is kept")
}

// The cadence's consumer name is "checkpoint cadence", in what a user or an
// agent reads: the init hint, both verbs' help and the receipt, which never
// say "deal tick" or "every minute". Its consumer verb is `cll checkpoint
// cadence` (beside the CLI's other checkpoint verbs); `deal tick` stays
// callable. The schedule is a 5-minute poll that waits for each tick due in
// its window; the time to a witness is computed from the profile's cadence.
// Wire names do not change: the cadence log is still deal-cadence/<id>.
func TestTheCheckpointCadenceIsNamedAndTimedFromTheProfile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	init := dealRun(t, "init", "--profile", "deal", "--dir", filepath.Join(t.TempDir(), "d"), "--materiality", neutralMateriality)
	assert.Regexp(t, `^deal-cadence/[0-9a-f]{16}$`, init["cadence_log"], "the wire log id is unchanged")
	next := init["next"].(string)
	assert.Contains(t, next, "checkpoint cadence")
	assert.Contains(t, next, "every 5m, give or take 1m", "the profile's cadence, from its configuration")
	assert.Contains(t, next, "capsulectl --profile deal cll checkpoint cadence --wait-up-to 5m")
	assert.Contains(t, next, "every 5 minutes")
	for _, words := range []string{init["witness_sees"].(string), next} {
		assert.NotContains(t, words, "deal tick")
		assert.NotContains(t, words, "every minute")
	}

	for _, verb := range [][]string{{"deal", "tick"}, {"cll", "checkpoint", "cadence"}} {
		help, err := invoke(t, "", append(verb, "--help")...)
		require.NoError(t, err)
		// The copy: the description and the flags' help, not the usage
		// line, which names the command as typed.
		usage := strings.Index(help, "\nUsage:")
		flags := strings.Index(help, "\nFlags:")
		require.True(t, usage > 0 && flags > usage, help)
		help = help[:usage] + help[flags:]
		assert.Contains(t, help, "checkpoint cadence", verb)
		assert.NotContains(t, help, "deal tick", verb)
		assert.NotContains(t, help, "every minute", verb)
	}

	for words, within := range map[string]string{
		"every 5m, give or take 1m":  "within about 6m",
		"every 1h, give or take 10m": "within about 1h10m",
		"every 30m":                  "within about 30m",
		"every 90m, give or take 1m": "within about 1h31m",
	} {
		assert.Equal(t, ", "+within+" ("+words+")", cadencePhrase(map[string]interface{}{"cadence": words}), words)
	}
	assert.Equal(t, " (unreadable)", cadencePhrase(map[string]interface{}{"cadence": "unreadable"}), "words it cannot read are shown as they are")
}

// `cll checkpoint cadence` is the checkpoint cadence's verb: on a deal
// profile it ticks exactly as `deal tick` does, and is not due again at once.
func TestTheCadenceVerbTicks(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	endpoint, _ := countingWitness(t)
	cadenceFixture(t, endpoint, public, "", "", 0)
	run := func() map[string]any {
		out, err := invoke(t, "", "--profile", "deal", "cll", "checkpoint", "cadence")
		require.NoError(t, err, out)
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &m), out)
		return m
	}
	first := run()
	assert.Equal(t, "ticked", first["state"])
	assert.Regexp(t, `^deal-cadence/[0-9a-f]{16}$`, first["cadence_log"])
	assert.Equal(t, "not_due", run()["state"])
	assert.Equal(t, "not_due", dealRun(t, "tick")["state"], "deal tick is the same cadence")
}

// A jitter that is not under half the interval is refused with both values
// and the fix. A profile that set a jitter for the old 1h default and no
// interval is told the interval is now the 5m default.
func TestACadenceJitterTooLargeForItsIntervalSaysWhy(t *testing.T) {
	var onlyJitter Profile
	onlyJitter.Cadence.Jitter = "10m"
	_, err := onlyJitter.dealCadence()
	require.ErrorIs(t, err, ErrInput)
	for _, part := range []string{"cadence.jitter is 10m", "cadence.interval is not set, so it is the default 5m", "under half the interval", "set cadence.interval", "a cadence.jitter under 2m30s"} {
		assert.Contains(t, err.Error(), part)
	}

	var both Profile
	both.Cadence.Interval, both.Cadence.Jitter = "15m", "8m"
	_, err = both.dealCadence()
	require.ErrorIs(t, err, ErrInput)
	for _, part := range []string{"cadence.jitter is 8m", "cadence.interval is 15m", "under 7m30s"} {
		assert.Contains(t, err.Error(), part)
	}

	var negative Profile
	negative.Cadence.Jitter = "-1m"
	_, err = negative.dealCadence()
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, err.Error(), "cadence.jitter must be at least 0")
}
