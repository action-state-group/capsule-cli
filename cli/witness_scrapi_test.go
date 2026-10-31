package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	aacwitness "github.com/action-state-group/agent-action-capsule/go/witness"
	"github.com/action-state-group/checkpointed-local-log/go/checkpoint"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/action-state-group/checkpointed-local-log/go/store/memory"
	"github.com/action-state-group/checkpointed-local-log/go/witness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scrapiFixture is a memory log holding one checkpoint due for service-test,
// a profile whose witness is the test SCRAPI service at server, and that
// service's signing key. The service registers each statement it is sent and
// answers 201 with a one-leaf Receipt over SHA-256 of the statement.
func scrapiFixture(t *testing.T) (Profile, *memory.Store, ed25519.PrivateKey, *httptest.Server, *[][]byte) {
	t.Helper()
	p, key := profileFixture(t)
	public, witnessKey, e := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, e)
	var received [][]byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/entries" || r.Header.Get("Content-Type") != witness.SCRAPIContentType {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, e := io.ReadAll(r.Body)
		assert.NoError(t, e)
		received = append(received, body)
		entry := sha256.Sum256(body)
		iat := int64(1_790_000_000)
		w.Header().Set("Content-Type", witness.SCRAPIContentType)
		w.WriteHeader(http.StatusCreated)
		_, e = w.Write(mintReceiptAt(t, entry[:], witnessKey, &iat))
		assert.NoError(t, e)
	}))
	t.Cleanup(server.Close)
	p.Checkpoint.Endpoint = "scrapi+" + server.URL
	p.Checkpoint.PublicKey = hex.EncodeToString(public)

	store := memory.New()
	t.Cleanup(func() { require.NoError(t, store.Close()) })
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
	return p, store, witnessKey, server, &received
}

// scrapiDelivery is DeliverWitness's runner for p's SCRAPI witness, with
// the test server's TLS transport.
func scrapiDelivery(t *testing.T, p Profile, store cll.WitnessStateStore, server *httptest.Server) *witness.DeliveryRunner {
	t.Helper()
	client, e := witness.NewSCRAPIClient(witnessHTTPS(p.Checkpoint.Endpoint), &http.Client{Transport: bearerTransport{base: server.Client().Transport}}, 0)
	require.NoError(t, e)
	verifier, e := witnessVerifier(p)
	require.NoError(t, e)
	delivery, e := witness.NewDeliveryRunner(witness.DefaultDeliveryConfig(), selectedWitness{WitnessStateStore: store, id: "service-test", size: 1}, map[string]witness.Submitter{"service-test": scrapiSubmitter{client}}, map[string]witness.Verifier{"service-test": verifier})
	require.NoError(t, e)
	return delivery
}

func anchorPolicy(t *testing.T, key ed25519.PrivateKey) aacwitness.AnchorWitnessPolicy {
	t.Helper()
	spki, e := x509.MarshalPKIXPublicKey(key.Public())
	require.NoError(t, e)
	return aacwitness.AnchorWitnessPolicy{Witnesses: []aacwitness.AnchorWitness{{Name: "test witness", Keys: []string{hex.EncodeToString(spki)}}}}
}

// A scrapi+https:// witness gets a Receipt that agent-action-capsule's
// anchor_witnessed check passes for the checkpoint's anchor (interop profile
// v1 §6.2.2), stored and printed as a checkpoint.witnesses entry whose ts_url
// names the binding. A cll binding Receipt for the same checkpoint does not
// pass that check.
func TestSCRAPIWitnessReceiptPassesAnchorWitnessed(t *testing.T) {
	p, store, witnessKey, server, received := scrapiFixture(t)
	count, e := scrapiDelivery(t, p, store, server).RunOnce(t.Context(), time.Now().UTC(), 1)
	require.NoError(t, e)
	require.Equal(t, 1, count)
	saved, e := store.GetWitness(t.Context(), "service-test", 1)
	require.NoError(t, e)
	require.NotNil(t, saved.Receipt)
	assert.Equal(t, [][]byte{saved.Checkpoint}, *received, "the statement is sent exactly as signed")
	require.NoError(t, VerifyWitness(p, saved))

	anchor := sha256.Sum256(saved.Checkpoint)
	report := aacwitness.CheckAnchorWitnessed(hex.EncodeToString(anchor[:]), [][]byte{saved.Receipt.Bytes}, anchorPolicy(t, witnessKey))
	assert.Equal(t, aacwitness.AnchorPass, report.Status)
	require.NotNil(t, report.WitnessedTime)
	assert.Equal(t, int64(1_790_000_000), *report.WitnessedTime)

	result := witnessResult(p.Checkpoint.Endpoint, saved)
	assert.Equal(t, "verified", result["state"])
	entry := result["witness"].(map[string]any)
	assert.Equal(t, "scrapi+"+server.URL, entry["ts_url"])
	assert.Equal(t, hex.EncodeToString(anchor[:]), entry["entry_hash"])
	raw, e := json.Marshal([]any{entry})
	require.NoError(t, e)
	var entries []interface{}
	require.NoError(t, json.Unmarshal(raw, &entries))
	directory, e := aacwitness.ParseDirectory([]byte(`{"witnesses":[{"name":"test witness","endpoint":"` + server.URL + `","binding":"scrapi","key_ids":["` + hex.EncodeToString(witnessKey.Public().(ed25519.PublicKey)) + `"]}]}`))
	require.NoError(t, e)
	signed, e := aacwitness.ParseSignedCheckpoint(saved.Checkpoint)
	require.NoError(t, e)
	status, findings, _ := aacwitness.VerifyReceipts(entries, &signed, saved.Checkpoint, directory)
	assert.Equal(t, aacwitness.StatusPass, status, findings)

	// The cll binding's leaf is SHA-256 of the payload digest, not the anchor.
	record, e := checkpoint.ParseRecord(saved.Checkpoint)
	require.NoError(t, e)
	legacy, e := record.EntryHash()
	require.NoError(t, e)
	report = aacwitness.CheckAnchorWitnessed(hex.EncodeToString(anchor[:]), [][]byte{mintReceipt(t, legacy, witnessKey)}, anchorPolicy(t, witnessKey))
	assert.Equal(t, aacwitness.AnchorFail, report.Status)
}

func TestSCRAPIWitnessRefusesAReceiptUnderAnotherKey(t *testing.T) {
	p, store, _, server, _ := scrapiFixture(t)
	other, _, e := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, e)
	p.Checkpoint.PublicKey = hex.EncodeToString(other)
	_, e = scrapiDelivery(t, p, store, server).RunOnce(t.Context(), time.Now().UTC(), 1)
	require.NoError(t, e)
	saved, e := store.GetWitness(t.Context(), "service-test", 1)
	require.NoError(t, e)
	assert.Nil(t, saved.Receipt)
	assert.True(t, saved.Permanent)
	assert.Contains(t, saved.LastError, "does not witness this checkpoint's anchor")
}

func TestAnchorVerifierRefusesAMismatchedEntry(t *testing.T) {
	p, store, witnessKey, _, _ := scrapiFixture(t)
	state, e := store.GetWitness(t.Context(), "service-test", 1)
	require.NoError(t, e)
	verifier, e := witnessVerifier(p)
	require.NoError(t, e)
	anchor := sha256.Sum256(state.Checkpoint)
	receipt := witness.Receipt{Bytes: mintReceipt(t, anchor[:], witnessKey), EntryHash: hex.EncodeToString(anchor[:]), TreeSize: 1}
	require.NoError(t, verifier.Verify(state.Checkpoint, receipt))
	wrong := receipt
	wrong.EntryHash = hex.EncodeToString(make([]byte, 32))
	assert.Error(t, verifier.Verify(state.Checkpoint, wrong))
	wrong = receipt
	wrong.EntryHashScheme = witness.EntryHashSchemeCheckpointDigest
	assert.Error(t, verifier.Verify(state.Checkpoint, wrong))
	tampered := append([]byte(nil), state.Checkpoint...)
	tampered[len(tampered)-1] ^= 1
	assert.Error(t, verifier.Verify(tampered, receipt))
}

// The binding is the profile's own witness URL: scrapi+https:// selects the
// SCRAPI client, https:// keeps the cll client, and no other binding or a
// plain-HTTP SCRAPI URL is accepted.
func TestWitnessEndpointBindings(t *testing.T) {
	p, _ := profileFixture(t)
	p.Checkpoint.PublicKey = p.TrustedKeys[0]
	for endpoint, ok := range map[string]bool{
		"https://witness.example":          true,
		"scrapi+https://witness.example":   true,
		"SCRAPI+https://witness.example":   true,
		"scrapi+http://witness.example":    false,
		"rekor+https://witness.example":    false,
		"scrapi+https://u@witness.example": false,
	} {
		p.Checkpoint.Endpoint = endpoint
		_, e := ServiceID(p)
		if ok {
			assert.NoError(t, e, endpoint)
		} else {
			assert.ErrorIs(t, e, ErrInput, endpoint)
		}
	}

	p.Checkpoint.Endpoint = "scrapi+https://witness.example"
	scrapiID, e := ServiceID(p)
	require.NoError(t, e)
	submitter, verifier, e := witnessBinding(p, "")
	require.NoError(t, e)
	assert.IsType(t, scrapiSubmitter{}, submitter)
	assert.IsType(t, anchorVerifier{}, verifier)

	p.Checkpoint.Endpoint = "https://witness.example"
	cllID, e := ServiceID(p)
	require.NoError(t, e)
	assert.NotEqual(t, scrapiID, cllID, "the bindings keep separate delivery rows")
	submitter, verifier, e = witnessBinding(p, "")
	require.NoError(t, e)
	assert.IsType(t, safeSubmitter{}, submitter)
	assert.IsType(t, &witness.ReceiptVerifier{}, verifier)
}

// A registration still pending after the client's poll budget is retried
// like an outage, recording the 202 it answered.
func TestSCRAPIPendingIsRetried(t *testing.T) {
	err := classifySCRAPI(witness.ErrSCRAPIPending)
	assert.True(t, witness.IsRetryable(err))
	assert.Contains(t, err.Error(), "answered HTTP 202")
}
