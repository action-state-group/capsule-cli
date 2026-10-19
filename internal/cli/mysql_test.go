package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	emit "github.com/action-state-group/capsule-emit/go"
	"github.com/action-state-group/capsule-emit/go/artifact"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Integration tests opt in by port and hardcode loopback plus capsule_cli_test.
// The operator must supply a disposable container port, never a production tunnel.
func mysqlProfile(t *testing.T) (Profile, ed25519.PrivateKey) {
	t.Helper()
	port := os.Getenv("CAPSULE_CLI_TEST_MYSQL_PORT")
	if port == "" {
		t.Skip("set CAPSULE_CLI_TEST_MYSQL_PORT for isolated MySQL integration")
	}
	p, key := profileFixture(t)
	n, e := strconv.Atoi(port)
	require.NoError(t, e)
	require.Greater(t, n, 0)
	require.LessOrEqual(t, n, 65535)
	require.Equal(t, "127.0.0.1", p.Connection.Host)
	require.Equal(t, "capsule_cli_test", p.Connection.Database)
	p.Connection.Port = n
	p.LogID = "cli-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	suffix := make([]byte, 8)
	_, e = rand.Read(suffix)
	require.NoError(t, e)
	p.Namespace = "test-" + hex.EncodeToString(suffix)
	p.LogID += "-" + hex.EncodeToString(suffix)
	return p, key
}

// lostAck performs the real durable library append, then loses its first ACK.
type lostAck struct {
	cll.Backend
	once bool
}

func (l *lostAck) Append(ctx context.Context, input cll.AppendInput) (cll.AppendResult, error) {
	r, e := l.Backend.Append(ctx, input)
	if e == nil && !l.once {
		l.once = true
		return r, errors.New("simulated lost acknowledgment")
	}
	return r, e
}
func TestMySQLPublishRecovery(t *testing.T) {
	p, key := mysqlProfile(t)
	target, e := OpenTarget(t.Context(), p, UseInitialization)
	require.NoError(t, e)
	defer func() { require.NoError(t, target.Close()) }()
	target.Profile = p
	raw := requestFixture(t)
	request, e := ParseRequest(raw)
	require.NoError(t, e)
	target.Log = &lostAck{Backend: target.Log}
	first, e := target.Publish(t.Context(), request, key)
	require.ErrorIs(t, e, ErrPending)
	assert.NotEmpty(t, first.CapsuleID)
	record, e := target.Artifacts.Get(t.Context(), first.CapsuleID)
	require.NoError(t, e)
	second, e := target.Publish(t.Context(), request, key)
	require.NoError(t, e)
	assert.Equal(t, first.CapsuleID, second.CapsuleID)
	assert.Equal(t, uint64(1), second.Sequence)
	again, e := target.Artifacts.Get(t.Context(), first.CapsuleID)
	require.NoError(t, e)
	assert.Equal(t, record, again)
	// A fresh client publishing identical input gets the same CLL entry.
	alias := p
	alias.Name = "alias"
	other, e := OpenTarget(t.Context(), alias, UsePublication)
	require.NoError(t, e)
	defer func() { require.NoError(t, other.Close()) }()
	third, e := other.Publish(t.Context(), request, key)
	require.NoError(t, e)
	assert.Equal(t, second, third)
	// Repeated publication must leave only one CLL entry.
	entries, e := other.Log.ScanEntries(t.Context(), 0, 100)
	require.NoError(t, e)
	require.Len(t, entries, 1)
	assert.Equal(t, first.CapsuleID, hex.EncodeToString(entries[0].Value))

}
func TestMySQLConcurrentPublishes(t *testing.T) {
	p, key := mysqlProfile(t)
	target, e := OpenTarget(t.Context(), p, UseInitialization)
	require.NoError(t, e)
	defer func() { require.NoError(t, target.Close()) }()
	raw := requestFixture(t)
	request, e := ParseRequest(raw)
	require.NoError(t, e)
	const n = 6
	results := make(chan Publication, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := target.Publish(t.Context(), request, key)
			results <- r
			errs <- e
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	var id string
	for r := range results {
		if id == "" {
			id = r.CapsuleID
		}
		assert.Equal(t, id, r.CapsuleID)
		assert.Equal(t, uint64(1), r.Sequence)
	}
}
func TestMySQLArtifactReadWithoutCLIIdentity(t *testing.T) {
	p, key := mysqlProfile(t)
	target, e := OpenTarget(t.Context(), p, UseInitialization)
	require.NoError(t, e)
	defer func() { require.NoError(t, target.Close()) }()
	request, e := ParseRequest(requestFixture(t))
	require.NoError(t, e)
	record, e := Seal(request, key)
	require.NoError(t, e)
	record, e = artifact.Prepare(record)
	require.NoError(t, e)
	require.NoError(t, target.Artifacts.Put(t.Context(), record))
	p.ReadOnly = true
	p.LogID = "unrelated-not-initialized-log"
	// Grant only SELECT on SDK tables: this catches accidental initialization,
	// CLI metadata reads or CLL access, not merely a read_only configuration bit.
	user := "reader_" + strings.TrimPrefix(p.Namespace, "test-")
	_, e = target.db.ExecContext(t.Context(), "CREATE USER '"+user+"'@'%' IDENTIFIED BY 'test-only-password'")
	require.NoError(t, e)
	defer func() {
		_, err := target.db.ExecContext(context.Background(), "DROP USER '"+user+"'@'%'")
		require.NoError(t, err)
	}()
	for _, table := range []string{"capsule_store_capsules", "capsule_store_artifacts"} {
		_, e = target.db.ExecContext(t.Context(), "GRANT SELECT ON capsule_cli_test."+table+" TO '"+user+"'@'%'")
		require.NoError(t, e)
	}
	p.Credentials.Username = user
	p.Credentials.Password = Secret{Value: "test-only-password"}
	reader, e := OpenTarget(t.Context(), p, UseArtifacts)
	require.NoError(t, e)
	defer func() { require.NoError(t, reader.Close()) }()
	got, e := reader.Artifacts.Get(t.Context(), record.CapsuleID)
	require.NoError(t, e)
	assert.Equal(t, record.CapsuleID, got.CapsuleID)
	assert.Equal(t, record.Capsule, got.Capsule)
	assert.Equal(t, record.ProducerEnvelope, got.ProducerEnvelope)
	assert.ElementsMatch(t, record.Artifacts, got.Artifacts)
}
func TestMySQLPendingSurvivesRestart(t *testing.T) {
	p, key := mysqlProfile(t)
	target, e := OpenTarget(t.Context(), p, UseInitialization)
	require.NoError(t, e)
	raw := requestFixture(t)
	request, e := ParseRequest(raw)
	require.NoError(t, e)
	prepared, e := target.preparePublication(t.Context(), request, key)
	require.NoError(t, e)
	require.NoError(t, target.Close())
	reopened, e := OpenTarget(t.Context(), p, UsePublication)
	require.NoError(t, e)
	defer func() { require.NoError(t, reopened.Close()) }()
	published, e := reopened.Publish(t.Context(), request, key)
	require.NoError(t, e)
	assert.Equal(t, prepared.CapsuleID, published.CapsuleID)
	assert.Equal(t, "appended", published.State)
}

func TestMySQLCheckpointCommandsAndRetry(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, key := mysqlProfile(t)
	p.Checkpoint.Endpoint = "https://127.0.0.1:1"
	p.Checkpoint.PublicKey = p.TrustedKeys[0]
	target, e := OpenTarget(t.Context(), p, UseInitialization)
	require.NoError(t, e)
	defer func() { require.NoError(t, target.Close()) }()
	target.Profile = p
	require.NoError(t, SaveProfile(p, false))
	raw := requestFixture(t)
	request, e := ParseRequest(raw)
	require.NoError(t, e)
	_, e = target.Publish(t.Context(), request, key)
	require.NoError(t, e)
	out, e := invoke(t, "", "cll", "checkpoint", "create", "--profile", p.Name)
	require.NoError(t, e)
	assert.Contains(t, out, `"checkpoint":1`)
	out, e = invoke(t, "", "cll", "checkpoint", "status", "--profile", p.Name, "--checkpoint", "1")
	require.NoError(t, e)
	assert.Contains(t, out, `"state":"pending"`)
	out, e = invoke(t, "", "cll", "checkpoint", "publish", "--profile", p.Name, "--checkpoint", "1")
	require.ErrorIs(t, e, ErrPending)
	assert.Contains(t, out, `"attempts":1`)
	service, e := ServiceID(p)
	require.NoError(t, e)
	state, e := target.Log.GetWitness(t.Context(), service, 1)
	require.NoError(t, e)
	assert.NotContains(t, state.LastError, "127.0.0.1")
	assert.Contains(t, state.LastError, "suppressed")
	// Freeze the retry deadline: jitter may be shorter than a CLI invocation.
	nextAttempt := time.Now().UTC().Add(time.Hour)
	state.NextAttemptAt = nextAttempt
	require.NoError(t, target.Log.CommitWitness(t.Context(), state.Attempts, state))
	// Backoff state survives a new CLI invocation and must not make another call.
	out, e = invoke(t, "", "cll", "checkpoint", "publish", "--profile", p.Name, "--checkpoint", "1")
	require.ErrorIs(t, e, ErrPending)
	assert.Contains(t, out, `"attempts":1`)
	p.Checkpoint.Endpoint = "https://other.invalid"
	require.NoError(t, SaveProfile(p, true))
	_, e = invoke(t, "", "cll", "checkpoint", "publish", "--profile", p.Name, "--checkpoint", "1")
	require.ErrorIs(t, e, cll.ErrNotFound)
	assert.Equal(t, 1, ExitCode(e))
}

func TestMySQLCheckpointNewServiceDoesNotRedirectOld(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, key := mysqlProfile(t)
	target, e := OpenTarget(t.Context(), p, UseInitialization)
	require.NoError(t, e)
	defer func() { require.NoError(t, target.Close()) }()
	require.NoError(t, SaveProfile(p, false))
	raw := requestFixture(t)
	request, e := ParseRequest(raw)
	require.NoError(t, e)
	_, e = target.Publish(t.Context(), request, key)
	require.NoError(t, e)
	_, e = invoke(t, "", "cll", "checkpoint", "create", "--profile", p.Name)
	require.NoError(t, e)
	p.Checkpoint.Endpoint = "https://127.0.0.1:1"
	p.Checkpoint.PublicKey = p.TrustedKeys[0]
	require.NoError(t, SaveProfile(p, true))
	request.Capsule.ActionID = "second-checkpoint-action"
	raw, e = json.Marshal(request)
	require.NoError(t, e)
	_, e = target.Publish(t.Context(), request, key)
	require.NoError(t, e)
	out, e := invoke(t, "", "cll", "checkpoint", "create", "--profile", p.Name)
	require.NoError(t, e)
	assert.Contains(t, out, `"checkpoint":3`)
	service, e := ServiceID(p)
	require.NoError(t, e)
	_, e = target.Log.GetWitness(t.Context(), service, 1)
	require.ErrorIs(t, e, cll.ErrNotFound)
	current, e := target.Log.GetWitness(t.Context(), service, 3)
	require.NoError(t, e)
	assert.Equal(t, service, current.WitnessID)
	_, e = invoke(t, "", "cll", "checkpoint", "publish", "--profile", p.Name, "--checkpoint", "1")
	require.ErrorIs(t, e, cll.ErrNotFound)
	assert.Equal(t, 1, ExitCode(e))
}

func TestMySQLPublishMissingEffectOriginalRejectedBeforePersist(t *testing.T) {
	p, key := mysqlProfile(t)
	target, e := OpenTarget(t.Context(), p, UseInitialization)
	require.NoError(t, e)
	defer func() { require.NoError(t, target.Close()) }()
	request, e := ParseRequest(requestFixture(t))
	require.NoError(t, e)
	request.Capsule.Effect = &emit.Effect{Type: "urn:test:publication:v1", Status: emit.EffectConfirmed, IrreversibilityClass: emit.IrreversibilityTwoWay, EffectAttestation: emit.AttestationRuntimeClaimed, RequestDigest: strings.Repeat("a", 64), ResponseDigest: strings.Repeat("b", 64)}
	_, e = target.Publish(t.Context(), request, key)
	require.ErrorIs(t, e, ErrPartial)
	record, e := Seal(request, key)
	require.NoError(t, e)
	_, e = target.Artifacts.Get(t.Context(), record.CapsuleID)
	require.ErrorIs(t, e, artifact.ErrNotFound)
	entries, e := target.Log.ScanEntries(t.Context(), 0, 10)
	require.NoError(t, e)
	assert.Empty(t, entries)
}

func TestMySQLCLLCommandsNeedNoProducerKeys(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, key := mysqlProfile(t)
	target, e := OpenTarget(t.Context(), p, UseInitialization)
	require.NoError(t, e)
	defer func() { require.NoError(t, target.Close()) }()
	raw := requestFixture(t)
	request, e := ParseRequest(raw)
	require.NoError(t, e)
	_, e = target.Publish(t.Context(), request, key)
	require.NoError(t, e)
	p.TrustedKeys = nil
	p.Signing = Secret{}
	require.NoError(t, SaveProfile(p, false))
	out, e := invoke(t, "", "cll", "list", "--profile", p.Name)
	require.NoError(t, e)
	assert.Contains(t, out, `"sequence":1`)
	out, e = invoke(t, "", "cll", "checkpoint", "create", "--profile", p.Name)
	require.NoError(t, e)
	assert.Contains(t, out, `"checkpoint":1`)
}

func TestMySQLPendingPurgeFailsWithoutAppendOrResurrection(t *testing.T) {
	p, key := mysqlProfile(t)
	target, e := OpenTarget(t.Context(), p, UseInitialization)
	require.NoError(t, e)
	defer func() { require.NoError(t, target.Close()) }()
	raw := requestFixture(t)
	request, e := ParseRequest(raw)
	require.NoError(t, e)
	prepared, e := target.preparePublication(t.Context(), request, key)
	require.NoError(t, e)
	// Purge is not part of the CLI-driven artifactStore interface; reach the
	// concrete backend to set up the purged-then-republish scenario.
	require.NoError(t, target.Artifacts.(interface {
		Purge(context.Context, string) error
	}).Purge(t.Context(), prepared.CapsuleID))
	_, e = target.Publish(t.Context(), request, key)
	require.ErrorIs(t, e, artifact.ErrPurged)
	entries, e := target.Log.ScanEntries(t.Context(), 0, 10)
	require.NoError(t, e)
	assert.Empty(t, entries)
	record, e := target.Artifacts.Get(t.Context(), prepared.CapsuleID)
	require.NoError(t, e)
	for _, a := range record.Artifacts {
		assert.Equal(t, artifact.Purged, a.State)
		assert.Nil(t, a.Content)
	}
}
