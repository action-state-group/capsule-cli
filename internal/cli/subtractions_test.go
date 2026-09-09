package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/action-state-group/capsule-emit-go/artifact"
	"github.com/action-state-group/cll-go/checkpoint"
	"github.com/action-state-group/cll-go/cll"
	"github.com/action-state-group/cll-go/store/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSealRejectsInvalidArtifactBeforeWriting(t *testing.T) {
	for _, bad := range []struct {
		name  string
		value artifact.Artifact
		want  error
	}{
		{"binding", artifact.Artifact{Name: "extra", Binding: artifact.PayloadDigest, State: artifact.Present, Content: []byte(`{"a":2}`)}, artifact.ErrDigestMismatch},
		{"duplicate_name", artifact.Artifact{Name: "payload", State: artifact.Present, Content: []byte(`{}`)}, artifact.ErrInvalid},
		{"purged", artifact.Artifact{Name: "extra", State: artifact.Purged}, artifact.ErrPurged},
	} {
		t.Run(bad.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			p, _ := profileFixture(t)
			require.NoError(t, saveProfile(p, false))
			request, err := parseRequest(requestFixture(t))
			require.NoError(t, err)
			request.Artifacts = []artifact.Artifact{bad.value}
			raw, err := json.Marshal(request)
			require.NoError(t, err)
			input, output := filepath.Join(t.TempDir(), "request.json"), filepath.Join(t.TempDir(), "record.json")
			require.NoError(t, os.WriteFile(input, raw, 0600))
			_, err = invoke(t, "", "seal", "--profile", p.Name, "--request", input, "--output", output)
			require.ErrorIs(t, err, bad.want)
			_, err = os.Stat(output)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestMySQLPublishRejectsInvalidArtifactBeforePersistence(t *testing.T) {
	p, key := mysqlProfile(t)
	target, err := openTarget(t.Context(), p, useInitialization)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.close()) })
	request, err := parseRequest(requestFixture(t))
	require.NoError(t, err)
	request.Artifacts = []artifact.Artifact{{Name: "extra", Binding: artifact.PayloadDigest, State: artifact.Present, Content: []byte(`{"a":2}`)}}
	record, err := seal(request, key)
	require.NoError(t, err)
	_, err = target.publish(t.Context(), request, key)
	require.ErrorIs(t, err, artifact.ErrDigestMismatch)
	_, err = target.artifacts.Get(t.Context(), record.CapsuleID)
	require.ErrorIs(t, err, artifact.ErrNotFound)
	entries, err := target.log.ScanEntries(t.Context(), 0, 10)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestMySQLCheckpointReadsAndValidatesCLLState(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, key := mysqlProfile(t)
	p.Checkpoint.Endpoint = "https://127.0.0.1:1"
	p.Checkpoint.PublicKey = p.TrustedKeys[0]
	target, err := openTarget(t.Context(), p, useInitialization)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.close()) })
	require.NoError(t, saveProfile(p, false))
	request, err := parseRequest(requestFixture(t))
	require.NoError(t, err)
	_, err = target.publish(t.Context(), request, key)
	require.NoError(t, err)
	service, err := serviceID(p)
	require.NoError(t, err)
	signer, err := checkpoint.NewEd25519Signer(key)
	require.NoError(t, err)
	cfg := checkpoint.DefaultRunnerConfig(p.LogID)
	cfg.Cadence.CadenceEntries = 1
	cfg.WitnessIDs = []string{service}
	runner, err := checkpoint.NewRunner(cfg, target.log, signer)
	require.NoError(t, err)
	// Commit only through CLL, as a process can exit immediately after its CAS.
	_, err = runner.RunOnce(t.Context(), time.Now().UTC())
	require.NoError(t, err)
	out, err := invoke(t, "", "cll", "checkpoint", "status", "--profile", p.Name, "--checkpoint", "1")
	require.NoError(t, err)
	assert.Contains(t, out, `"attempts":0`)
	_, err = invoke(t, "", "cll", "checkpoint", "create", "--profile", p.Name)
	require.NoError(t, err)
	var tables int
	require.NoError(t, target.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='capsule_cli_checkpoints'").Scan(&tables))
	assert.Zero(t, tables)
	state, err := target.log.GetWitness(t.Context(), service, 1)
	require.NoError(t, err)
	var original []byte
	require.NoError(t, target.db.QueryRowContext(t.Context(), "SELECT witness FROM cll_witnesses WHERE log_id=? AND witness_id=? AND checkpoint_size=?", p.LogID, service, "1").Scan(&original))
	var wire map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(original, &wire))
	signed := func(logID string, private ed25519.PrivateKey, leaves int) []byte {
		store := memory.New()
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		for i := 0; i < leaves; i++ {
			_, e := store.Append(t.Context(), cll.AppendInput{Value: bytes.Repeat([]byte{byte(i + 1)}, 32), AppendedAt: time.Now().UTC()})
			require.NoError(t, e)
		}
		signer, e := checkpoint.NewEd25519Signer(private)
		require.NoError(t, e)
		cfg := checkpoint.DefaultRunnerConfig(logID)
		cfg.Cadence.CadenceEntries = 1
		runner, e := checkpoint.NewRunner(cfg, store, signer)
		require.NoError(t, e)
		_, e = runner.RunOnce(t.Context(), time.Now().UTC())
		require.NoError(t, e)
		current, e := store.LoadCLL(t.Context())
		require.NoError(t, e)
		require.NotNil(t, current.Checkpoint)
		return current.Checkpoint.Bytes
	}
	_, otherKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	tampered := append([]byte(nil), state.Checkpoint...)
	tampered[len(tampered)-1] ^= 1
	for _, bad := range []struct {
		name      string
		statement []byte
		conflict  bool
	}{
		{"signature", tampered, false},
		{"untrusted_signer", signed(p.LogID, otherKey, 1), false},
		{"wrong_log", signed("other-log", key, 1), true},
		{"wrong_size", signed(p.LogID, key, 2), true},
	} {
		t.Run(bad.name, func(t *testing.T) {
			encoded, e := json.Marshal(bad.statement)
			require.NoError(t, e)
			wire["checkpoint"] = encoded
			updated, e := json.Marshal(wire)
			require.NoError(t, e)
			// Corrupt only this disposable test database; production writes reject this mutation.
			_, e = target.db.ExecContext(t.Context(), "UPDATE cll_witnesses SET witness=? WHERE log_id=? AND witness_id=? AND checkpoint_size=?", updated, p.LogID, service, "1")
			require.NoError(t, e)
			for _, verb := range []string{"status", "publish"} {
				out, e := invoke(t, "", "cll", "checkpoint", verb, "--profile", p.Name, "--checkpoint", "1")
				require.Error(t, e)
				assert.NotErrorIs(t, e, ErrPending)
				assert.Empty(t, out)
				if bad.conflict {
					assert.ErrorIs(t, e, ErrConflict)
				}
			}
		})
	}
}
