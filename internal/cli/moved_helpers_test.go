package cli

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/action-state-group/checkpointed-local-log/go/checkpoint"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/action-state-group/checkpointed-local-log/go/store/memory"
	"github.com/stretchr/testify/require"
)

// The test helpers below are also the plugins module's (its copies live with
// the deal and book tests); the core's own tests use these.

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return raw
}

func readBundle(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var b map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &b))
	return b
}

// checkpointedStore is one sealed capsule in an artifact store and a memory
// CLL that holds it, checkpointed under key.
func checkpointedStore(t *testing.T, key ed25519.PrivateKey, logID string) (mapArtifactStore, *memory.Store, string) {
	t.Helper()
	store := mapArtifactStore{}
	rec := bundleRecord(t, key, nil, nil)
	store[rec.CapsuleID] = rec
	log := memory.New()
	t.Cleanup(func() { require.NoError(t, log.Close()) })
	value, err := hex.DecodeString(rec.CapsuleID)
	require.NoError(t, err)
	_, err = log.Append(t.Context(), cll.AppendInput{Value: value, AppendedAt: time.Now().UTC()})
	require.NoError(t, err)
	signer, err := checkpoint.NewEd25519Signer(key)
	require.NoError(t, err)
	config := checkpoint.DefaultRunnerConfig(logID)
	config.Cadence.CadenceEntries = 1
	runner, err := checkpoint.NewRunner(config, log, signer)
	require.NoError(t, err)
	_, err = runner.RunOnce(t.Context(), time.Now().UTC())
	require.NoError(t, err)
	return store, log, rec.CapsuleID
}
