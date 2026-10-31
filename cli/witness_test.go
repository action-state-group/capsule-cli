package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	aacwitness "github.com/action-state-group/agent-action-capsule/go/witness"
	"github.com/action-state-group/checkpointed-local-log/go/checkpoint"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/action-state-group/checkpointed-local-log/go/store/memory"
	"github.com/action-state-group/checkpointed-local-log/go/witness"
	"github.com/fxamacker/cbor/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/veraison/go-cose"
)

// witnessReceiptFixture is a memory log holding one checkpoint due for
// service-test, and the witness response that carries a real one-leaf
// RFC9162 receipt for it, signed by the profile's pinned witness key.
func witnessReceiptFixture(t *testing.T) (Profile, *memory.Store, ed25519.PublicKey, []byte) {
	t.Helper()
	p, key := profileFixture(t)
	public, witnessKey, e := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, e)
	p.Checkpoint.PublicKey = hex.EncodeToString(public)
	store := memory.New()
	_, e = store.Append(t.Context(), cll.AppendInput{Value: bytes.Repeat([]byte{7}, 32), AppendedAt: time.Now().UTC()})
	require.NoError(t, e)
	signer, e := checkpoint.NewEd25519Signer(key)
	require.NoError(t, e)
	cfg := checkpoint.DefaultRunnerConfig(p.LogID)
	cfg.Cadence.CadenceEntries = 1
	cfg.WitnessIDs = []string{"service-test"}
	runner, e := checkpoint.NewRunner(cfg, store, signer)
	require.NoError(t, e)
	_, e = runner.RunOnce(t.Context(), time.Now().UTC())
	require.NoError(t, e)
	state, e := store.LoadCLL(t.Context())
	require.NoError(t, e)
	require.NotNil(t, state.Checkpoint)
	record, e := checkpoint.ParseRecord(state.Checkpoint.Bytes)
	require.NoError(t, e)
	hash, e := record.EntryHash()
	require.NoError(t, e)
	// A real, one-leaf RFC9162 receipt, signed by an independently pinned witness.
	leaf := sha256.Sum256(append([]byte{0}, hash...))
	proof, e := cbor.Marshal([]any{int64(1), int64(0), [][]byte{}})
	require.NoError(t, e)
	msg := cose.NewSign1Message()
	msg.Headers.Protected.SetAlgorithm(cose.AlgorithmEdDSA)
	msg.Headers.Protected[int64(395)] = int64(1)
	msg.Headers.Unprotected[int64(396)] = map[any]any{int64(-1): []any{proof}}
	msg.Payload = leaf[:]
	ws, e := cose.NewSigner(cose.AlgorithmEdDSA, witnessKey)
	require.NoError(t, e)
	require.NoError(t, msg.Sign(rand.Reader, nil, ws))
	msg.Payload = nil // a Receipt's payload, the tree root, is detached
	receipt, e := msg.MarshalCBOR()
	require.NoError(t, e)
	body, e := json.Marshal(map[string]any{"receipt_b64": base64.StdEncoding.EncodeToString(receipt), "entry_hash": hex.EncodeToString(hash), "entry_hash_scheme": "legacy", "leaf_index": 0, "tree_size": 1})
	require.NoError(t, e)
	return p, store, public, body
}

// witnessDelivery is one delivery runner for service-test's checkpoint 1
// through safeSubmitter to server.
func witnessDelivery(t *testing.T, store cll.WitnessStateStore, public ed25519.PublicKey, server *httptest.Server) *witness.DeliveryRunner {
	t.Helper()
	client, e := witness.NewClient(server.URL, &http.Client{Transport: bearerTransport{base: server.Client().Transport}}, 0)
	require.NoError(t, e)
	verifier, e := witness.NewReceiptVerifier(public)
	require.NoError(t, e)
	delivery, e := witness.NewDeliveryRunner(witness.DefaultDeliveryConfig(), selectedWitness{WitnessStateStore: store, id: "service-test", size: 1}, map[string]witness.Submitter{"service-test": safeSubmitter{client}}, map[string]witness.Verifier{"service-test": verifier})
	require.NoError(t, e)
	return delivery
}

func TestWitnessHTTPSReceiptAndTampering(t *testing.T) {
	p, store, public, body := witnessReceiptFixture(t)
	defer func() { require.NoError(t, store.Close()) }()
	observed := make(chan string, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, e := w.Write(body)
		assert.NoError(t, e)
	}))
	defer server.Close()
	client, e := witness.NewClient(server.URL, &http.Client{Transport: bearerTransport{token: "test-token", base: server.Client().Transport}}, 0)
	require.NoError(t, e)
	verifier, e := witness.NewReceiptVerifier(public)
	require.NoError(t, e)
	delivery, e := witness.NewDeliveryRunner(witness.DefaultDeliveryConfig(), selectedWitness{WitnessStateStore: store, id: "service-test", size: 1}, map[string]witness.Submitter{"service-test": safeSubmitter{client}}, map[string]witness.Verifier{"service-test": verifier})
	require.NoError(t, e)
	count, e := delivery.RunOnce(t.Context(), time.Now().UTC(), 1)
	require.NoError(t, e)
	assert.Equal(t, 1, count)
	assert.Equal(t, "Bearer test-token", <-observed)
	saved, e := store.GetWitness(t.Context(), "service-test", 1)
	require.NoError(t, e)
	require.NotNil(t, saved.Receipt)
	require.NoError(t, VerifyWitness(p, saved))
	p.Checkpoint.Endpoint = server.URL
	result := witnessResult(p.Checkpoint.Endpoint, saved)
	assert.Equal(t, "verified", result["state"])
	// The printed entry is a checkpoint.witnesses entry the cll binding accepts.
	raw, e := json.Marshal([]any{result["witness"]})
	require.NoError(t, e)
	var entries []interface{}
	require.NoError(t, json.Unmarshal(raw, &entries))
	directory, e := aacwitness.ParseDirectory([]byte(`{"witnesses":[{"name":"service-test","endpoint":"` + strings.TrimRight(p.Checkpoint.Endpoint, "/") + `","binding":"cll","key_ids":["` + hex.EncodeToString(public) + `"]}]}`))
	require.NoError(t, e)
	signed, e := aacwitness.ParseSignedCheckpoint(saved.Checkpoint)
	require.NoError(t, e)
	status, findings, results := aacwitness.VerifyReceipts(entries, &signed, saved.Checkpoint, directory)
	assert.Equal(t, aacwitness.StatusPass, status, "%v %+v", findings, results)
	count, e = delivery.RunOnce(t.Context(), time.Now().UTC(), 1)
	require.NoError(t, e)
	assert.Zero(t, count)
	saved.Receipt.Bytes[0] ^= 1
	require.Error(t, VerifyWitness(p, saved))
}

// A 403 may come from a network gate on this host, not the witness
// (capsule-cli#335): it is kept retryable and said without claiming the
// witness answered, so the delivery goes through once the host grants the
// site.
func TestWitnessRefusedDeliveryIsRetried(t *testing.T) {
	p, store, public, body := witnessReceiptFixture(t)
	defer func() { require.NoError(t, store.Close()) }()
	status := http.StatusForbidden
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			http.Error(w, "blocked by policy", status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, e := w.Write(body)
		assert.NoError(t, e)
	}))
	defer server.Close()
	delivery := witnessDelivery(t, store, public, server)
	now := time.Now().UTC()
	_, e := delivery.RunOnce(t.Context(), now, 1)
	require.NoError(t, e)
	saved, e := store.GetWitness(t.Context(), "service-test", 1)
	require.NoError(t, e)
	assert.False(t, saved.Permanent, "a 403 is retried")
	assert.NotContains(t, saved.LastError, "blocked by policy", "the response body is never kept")
	assert.NotContains(t, saved.LastError, "the witness answered")
	reason, text := WitnessPendingReason(saved, DealDefaultWitness)
	assert.Equal(t, "refused", reason)
	assert.Equal(t, "pending: the request was refused (HTTP 403), by the witness or by a network gate on this host; "+WitnessRetryText()+" "+WitnessGrantText(DealDefaultWitness), text)

	// Once the host grants the site, the next attempt after the backoff
	// delivers it.
	status = http.StatusOK
	later := now.Add(48 * time.Hour)
	count, e := delivery.RunOnce(t.Context(), later, 1)
	require.NoError(t, e)
	assert.Equal(t, 1, count)
	saved, e = store.GetWitness(t.Context(), "service-test", 1)
	require.NoError(t, e)
	require.NotNil(t, saved.Receipt)
	require.NoError(t, VerifyWitness(p, saved))
}

// A build before capsule-cli#335 stored a 403 as failed for good; that row
// is re-armed once and then delivers. Other failures stay as stored.
func TestRearmRefusedDelivery(t *testing.T) {
	p, store, public, body := witnessReceiptFixture(t)
	defer func() { require.NoError(t, store.Close()) }()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, e := w.Write(body)
		assert.NoError(t, e)
	}))
	defer server.Close()
	now := time.Now().UTC()
	due, e := store.GetWitness(t.Context(), "service-test", 1)
	require.NoError(t, e)
	stored := due
	stored.Attempts, stored.Permanent, stored.NextAttemptAt = 1, true, now.Add(time.Hour)
	stored.LastError = "witness returned HTTP 400: checkpoint submission failed (the witness answered HTTP 403); response details suppressed"
	require.NoError(t, store.CommitWitness(t.Context(), due.Attempts, stored))
	reason, _ := WitnessPendingReason(stored, DealDefaultWitness)
	assert.Equal(t, "refused", reason, "a stored 403 is not said as the witness's answer")

	require.NoError(t, RearmRefusedLog(t.Context(), store, "service-test", now))
	saved, e := store.GetWitness(t.Context(), "service-test", 1)
	require.NoError(t, e)
	assert.False(t, saved.Permanent)
	assert.Equal(t, uint32(1), saved.Attempts)
	count, e := witnessDelivery(t, store, public, server).RunOnce(t.Context(), now, 1)
	require.NoError(t, e)
	assert.Equal(t, 1, count)
	saved, e = store.GetWitness(t.Context(), "service-test", 1)
	require.NoError(t, e)
	require.NotNil(t, saved.Receipt)
	require.NoError(t, VerifyWitness(p, saved))
	// A delivered row is left alone.
	again, e := RearmRefused(t.Context(), store, saved, now)
	require.NoError(t, e)
	assert.Equal(t, saved, again)

	// A conflict the witness answered stays failed for good, and says so.
	conflict := saved
	conflict.Receipt, conflict.Permanent, conflict.LastError = nil, true, "witness returned HTTP 400: "+WitnessFailureBody(409)
	kept, e := RearmRefused(t.Context(), store, conflict, now)
	require.NoError(t, e)
	assert.True(t, kept.Permanent)
	reason, text := WitnessPendingReason(conflict, DealDefaultWitness)
	assert.Equal(t, "witness_error", reason)
	assert.Equal(t, "not delivered: the witness answered with an error (HTTP 409); this delivery is not retried.", text)
}

// cll checkpoint publish (DeliverWitness) re-arms its own refused row
// before it delivers: the row is attempted again instead of being skipped
// as failed for good.
func TestDeliverWitnessRearmsARefusedRow(t *testing.T) {
	p, store, _, _ := witnessReceiptFixture(t)
	defer func() { require.NoError(t, store.Close()) }()
	p.Checkpoint.Endpoint = "https://127.0.0.1:1"
	due, e := store.GetWitness(t.Context(), "service-test", 1)
	require.NoError(t, e)
	stored := due
	stored.Attempts, stored.Permanent = 1, true
	stored.LastError = "witness returned HTTP 400: checkpoint submission failed (the witness answered HTTP 403); response details suppressed"
	require.NoError(t, store.CommitWitness(t.Context(), due.Attempts, stored))
	state, e := DeliverWitness(t.Context(), p, store, "service-test", 1)
	require.NoError(t, e)
	assert.Equal(t, uint32(2), state.Attempts, "attempted again")
	assert.False(t, state.Permanent)
	reason, _ := WitnessPendingReason(state, p.Checkpoint.Endpoint)
	assert.Equal(t, "network_consent_needed", reason, "this attempt did not reach the witness")
}

// A proxy asking for authorization did not let the request reach the
// witness: it stays network_consent_needed, as a transport error does.
func TestWitnessProxyAuthIsNotReached(t *testing.T) {
	_, store, public, _ := witnessReceiptFixture(t)
	defer func() { require.NoError(t, store.Close()) }()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer server.Close()
	_, e := witnessDelivery(t, store, public, server).RunOnce(t.Context(), time.Now().UTC(), 1)
	require.NoError(t, e)
	saved, e := store.GetWitness(t.Context(), "service-test", 1)
	require.NoError(t, e)
	assert.False(t, saved.Permanent)
	reason, _ := WitnessPendingReason(saved, DealDefaultWitness)
	assert.Equal(t, "network_consent_needed", reason)
	server.Close()
	_, e = witnessDelivery(t, store, public, server).RunOnce(t.Context(), time.Now().UTC().Add(48*time.Hour), 1)
	require.NoError(t, e)
	saved, e = store.GetWitness(t.Context(), "service-test", 1)
	require.NoError(t, e)
	reason, _ = WitnessPendingReason(saved, DealDefaultWitness)
	assert.Equal(t, "network_consent_needed", reason, "a transport error did not reach the witness")
}

// The pending texts quote the retry cap of the delivery configuration the CLI
// runs with, so a change to the library's backoff changes the text with it.
func TestWitnessRetryTextFollowsBackoffCap(t *testing.T) {
	config := witness.DefaultDeliveryConfig()
	require.Equal(t, time.Hour, config.MaxBackoff, "the text says an hour; update WitnessRetryText with the cap")
	assert.Contains(t, WitnessRetryText(), "a wait that grows up to an hour")
	assert.NotContains(t, WitnessRetryText(), "every tick")
	for _, state := range []cll.WitnessState{{Attempts: 1, LastError: "boom"}, {Attempts: 1, LastError: WitnessFailureBody(500)}} {
		_, text := WitnessPendingReason(state, DealDefaultWitness)
		assert.Contains(t, text, WitnessRetryText())
	}
	_, text := WitnessPendingReason(cll.WitnessState{Attempts: 1, LastError: WitnessFailureBody(500)}, DealDefaultWitness)
	assert.Contains(t, text, "HTTP 500")
}
