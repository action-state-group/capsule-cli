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
	"testing"
	"time"

	"github.com/action-state-group/cll-go/checkpoint"
	"github.com/action-state-group/cll-go/cll"
	"github.com/action-state-group/cll-go/store/memory"
	"github.com/action-state-group/cll-go/witness"
	"github.com/fxamacker/cbor/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/veraison/go-cose"
)

func TestWitnessHTTPSReceiptAndTampering(t *testing.T) {
	p, key := profileFixture(t)
	public, witnessKey, e := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, e)
	p.Checkpoint.PublicKey = hex.EncodeToString(public)
	store := memory.New()
	defer func() { require.NoError(t, store.Close()) }()
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
	receipt, e := msg.MarshalCBOR()
	require.NoError(t, e)
	observed := make(chan string, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		e := json.NewEncoder(w).Encode(map[string]any{"receipt_b64": base64.StdEncoding.EncodeToString(receipt), "entry_hash": hex.EncodeToString(hash), "entry_hash_scheme": "legacy", "leaf_index": 0, "tree_size": 1})
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
	require.NoError(t, verifyWitness(p, saved))
	assert.Equal(t, "verified", witnessResult(saved)["state"])
	count, e = delivery.RunOnce(t.Context(), time.Now().UTC(), 1)
	require.NoError(t, e)
	assert.Zero(t, count)
	saved.Receipt.Bytes[0] ^= 1
	require.Error(t, verifyWitness(p, saved))
}
