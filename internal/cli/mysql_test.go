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

	emit "github.com/action-state-group/capsule-emit-go"
	"github.com/action-state-group/capsule-emit-go/artifact"
	"github.com/action-state-group/cll-go/cll"
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
	target, e := openTarget(t.Context(), p, useInitialization)
	require.NoError(t, e)
	defer func() { require.NoError(t, target.close()) }()
	p.StoreID = target.storeID
	target.profile = p
	raw := requestFixture(t)
	request, e := parseRequest(raw)
	require.NoError(t, e)
	target.log = &lostAck{Backend: target.log}
	first, e := target.publish(t.Context(), raw, request, "operation-one", key)
	require.ErrorIs(t, e, ErrPending)
	assert.NotEmpty(t, first.CapsuleID)
	record, e := target.artifacts.Get(t.Context(), first.CapsuleID)
	require.NoError(t, e)
	second, e := target.publish(t.Context(), raw, request, "operation-one", key)
	require.NoError(t, e)
	assert.Equal(t, first.CapsuleID, second.CapsuleID)
	assert.Equal(t, uint64(1), second.Sequence)
	again, e := target.artifacts.Get(t.Context(), first.CapsuleID)
	require.NoError(t, e)
	assert.Equal(t, record, again)
	changed := append(append([]byte(nil), raw...), '\n')
	_, e = target.publish(t.Context(), changed, request, "operation-one", key)
	require.ErrorIs(t, e, ErrConflict)
	// A different alias and fresh client must consult the same shared authority.
	alias := p
	alias.Name = "alias"
	other, e := openTarget(t.Context(), alias, usePublication)
	require.NoError(t, e)
	defer func() { require.NoError(t, other.close()) }()
	third, e := other.publish(t.Context(), raw, request, "operation-one", key)
	require.NoError(t, e)
	assert.Equal(t, second, third)
	// A completed journal row is checked against the real backend, not trusted alone.
	entries, e := other.log.ScanEntries(t.Context(), 0, 100)
	require.NoError(t, e)
	require.Len(t, entries, 1)
	assert.Equal(t, first.CapsuleID, hex.EncodeToString(entries[0].Value))
	wrong := p
	wrong.StoreID = strings.Repeat("f", 64)
	_, e = openTarget(t.Context(), wrong, usePublication)
	require.ErrorIs(t, e, ErrConflict)
}
func TestMySQLConcurrentClaims(t *testing.T) {
	p, key := mysqlProfile(t)
	target, e := openTarget(t.Context(), p, useInitialization)
	require.NoError(t, e)
	defer func() { require.NoError(t, target.close()) }()
	target.profile.StoreID = target.storeID
	raw := requestFixture(t)
	request, e := parseRequest(raw)
	require.NoError(t, e)
	const n = 6
	results := make(chan Publication, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := target.publish(t.Context(), raw, request, "shared-key", key)
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
	target, e := openTarget(t.Context(), p, useInitialization)
	require.NoError(t, e)
	defer func() { require.NoError(t, target.close()) }()
	request, e := parseRequest(requestFixture(t))
	require.NoError(t, e)
	record, e := seal(request, key)
	require.NoError(t, e)
	require.NoError(t, target.artifacts.Put(t.Context(), record))
	p.ReadOnly = true
	p.StoreID = ""
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
	reader, e := openTarget(t.Context(), p, useArtifacts)
	require.NoError(t, e)
	defer func() { require.NoError(t, reader.close()) }()
	got, e := reader.artifacts.Get(t.Context(), record.CapsuleID)
	require.NoError(t, e)
	assert.Equal(t, record.CapsuleID, got.CapsuleID)
	assert.Equal(t, record.Capsule, got.Capsule)
	assert.Equal(t, record.ProducerEnvelope, got.ProducerEnvelope)
	assert.ElementsMatch(t, record.Artifacts, got.Artifacts)
}
func TestMySQLPendingSurvivesRestart(t *testing.T) {
	p, key := mysqlProfile(t)
	target, e := openTarget(t.Context(), p, useInitialization)
	require.NoError(t, e)
	p.StoreID = target.storeID
	raw := requestFixture(t)
	request, e := parseRequest(raw)
	require.NoError(t, e)
	prepared, e := target.preparePublication(t.Context(), raw, request, "prepared", key)
	require.NoError(t, e)
	require.NoError(t, target.close())
	reopened, e := openTarget(t.Context(), p, usePublication)
	require.NoError(t, e)
	defer func() { require.NoError(t, reopened.close()) }()
	published, e := reopened.publish(t.Context(), raw, request, "prepared", key)
	require.NoError(t, e)
	assert.Equal(t, prepared.CapsuleID, published.CapsuleID)
	assert.Equal(t, "appended", published.State)
}

func TestMySQLCheckpointCommandsAndRetry(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, key := mysqlProfile(t)
	p.Checkpoint.Endpoint = "https://127.0.0.1:1"
	p.Checkpoint.PublicKey = p.TrustedKeys[0]
	target, e := openTarget(t.Context(), p, useInitialization)
	require.NoError(t, e)
	defer func() { require.NoError(t, target.close()) }()
	p.StoreID = target.storeID
	target.profile = p
	require.NoError(t, saveProfile(p, false))
	raw := requestFixture(t)
	request, e := parseRequest(raw)
	require.NoError(t, e)
	_, e = target.publish(t.Context(), raw, request, "checkpoint-input", key)
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
	service, e := serviceID(p)
	require.NoError(t, e)
	state, e := target.log.GetWitness(t.Context(), service, 1)
	require.NoError(t, e)
	assert.NotContains(t, state.LastError, "127.0.0.1")
	assert.Contains(t, state.LastError, "suppressed")
	// Backoff state survives a new CLI invocation and must not make another call.
	out, e = invoke(t, "", "cll", "checkpoint", "publish", "--profile", p.Name, "--checkpoint", "1")
	require.ErrorIs(t, e, ErrPending)
	assert.Contains(t, out, `"attempts":1`)
	p.Checkpoint.Endpoint = "https://other.invalid"
	require.NoError(t, saveProfile(p, true))
	_, e = invoke(t, "", "cll", "checkpoint", "publish", "--profile", p.Name, "--checkpoint", "1")
	require.ErrorIs(t, e, ErrConflict)
}

func TestMySQLCheckpointNewServiceDoesNotRedirectOld(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, key := mysqlProfile(t)
	target, e := openTarget(t.Context(), p, useInitialization)
	require.NoError(t, e)
	defer func() { require.NoError(t, target.close()) }()
	p.StoreID = target.storeID
	require.NoError(t, saveProfile(p, false))
	raw := requestFixture(t)
	request, e := parseRequest(raw)
	require.NoError(t, e)
	_, e = target.publish(t.Context(), raw, request, "first", key)
	require.NoError(t, e)
	_, e = invoke(t, "", "cll", "checkpoint", "create", "--profile", p.Name)
	require.NoError(t, e)
	p.Checkpoint.Endpoint = "https://127.0.0.1:1"
	p.Checkpoint.PublicKey = p.TrustedKeys[0]
	require.NoError(t, saveProfile(p, true))
	request.Capsule.ActionID = "second-checkpoint-action"
	raw, e = json.Marshal(request)
	require.NoError(t, e)
	_, e = target.publish(t.Context(), raw, request, "second", key)
	require.NoError(t, e)
	out, e := invoke(t, "", "cll", "checkpoint", "create", "--profile", p.Name)
	require.NoError(t, e)
	assert.Contains(t, out, `"checkpoint":3`)
	_, oldService, e := target.savedCheckpoint(t.Context(), 1)
	require.NoError(t, e)
	assert.Empty(t, oldService)
	_, newService, e := target.savedCheckpoint(t.Context(), 3)
	require.NoError(t, e)
	expected, e := serviceID(p)
	require.NoError(t, e)
	assert.Equal(t, expected, newService)
}

func TestMySQLPublishMissingEffectOriginalRollsBack(t *testing.T) {
	p, key := mysqlProfile(t)
	target, e := openTarget(t.Context(), p, useInitialization)
	require.NoError(t, e)
	defer func() { require.NoError(t, target.close()) }()
	request, e := parseRequest(requestFixture(t))
	require.NoError(t, e)
	request.Capsule.Effect = &emit.Effect{Type: "urn:test:publication:v1", Status: emit.EffectConfirmed, IrreversibilityClass: emit.IrreversibilityTwoWay, EffectAttestation: emit.AttestationRuntimeClaimed, RequestDigest: strings.Repeat("a", 64), ResponseDigest: strings.Repeat("b", 64)}
	raw, e := json.Marshal(request)
	require.NoError(t, e)
	_, e = target.publish(t.Context(), raw, request, "missing-effect-originals", key)
	require.ErrorIs(t, e, ErrPartial)
	var count int
	e = target.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM capsule_cli_operations WHERE namespace=?", p.Namespace).Scan(&count)
	require.NoError(t, e)
	assert.Zero(t, count)
	entries, e := target.log.ScanEntries(t.Context(), 0, 10)
	require.NoError(t, e)
	assert.Empty(t, entries)
}

func TestMySQLCLLCommandsNeedNoProducerKeys(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, key := mysqlProfile(t)
	target, e := openTarget(t.Context(), p, useInitialization)
	require.NoError(t, e)
	defer func() { require.NoError(t, target.close()) }()
	p.StoreID = target.storeID
	raw := requestFixture(t)
	request, e := parseRequest(raw)
	require.NoError(t, e)
	_, e = target.publish(t.Context(), raw, request, "existing-entry", key)
	require.NoError(t, e)
	p.TrustedKeys = nil
	p.Signing = Secret{}
	require.NoError(t, saveProfile(p, false))
	out, e := invoke(t, "", "cll", "list", "--profile", p.Name)
	require.NoError(t, e)
	assert.Contains(t, out, `"sequence":1`)
	out, e = invoke(t, "", "cll", "checkpoint", "create", "--profile", p.Name)
	require.NoError(t, e)
	assert.Contains(t, out, `"checkpoint":1`)
}

func TestMySQLPendingPurgeFailsWithoutAppendOrResurrection(t *testing.T) {
	p, key := mysqlProfile(t)
	target, e := openTarget(t.Context(), p, useInitialization)
	require.NoError(t, e)
	defer func() { require.NoError(t, target.close()) }()
	raw := requestFixture(t)
	request, e := parseRequest(raw)
	require.NoError(t, e)
	prepared, e := target.preparePublication(t.Context(), raw, request, "purged-before-delivery", key)
	require.NoError(t, e)
	require.NoError(t, target.artifacts.Purge(t.Context(), prepared.CapsuleID))
	_, e = target.publish(t.Context(), raw, request, "purged-before-delivery", key)
	require.ErrorIs(t, e, ErrPartial)
	entries, e := target.log.ScanEntries(t.Context(), 0, 10)
	require.NoError(t, e)
	assert.Empty(t, entries)
	record, e := target.artifacts.Get(t.Context(), prepared.CapsuleID)
	require.NoError(t, e)
	for _, a := range record.Artifacts {
		assert.Equal(t, artifact.Purged, a.State)
		assert.Nil(t, a.Content)
	}
}

type forbidAppend struct {
	cll.Backend
	called bool
}

func (f *forbidAppend) Append(context.Context, cll.AppendInput) (cll.AppendResult, error) {
	f.called = true
	return cll.AppendResult{}, errors.New("unexpected append during completion reconciliation")
}
func TestMySQLCompletedAndLostACKPurgeRetriesOnlyReconcile(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(strconv.FormatBool(lost), func(t *testing.T) {
			p, key := mysqlProfile(t)
			target, e := openTarget(t.Context(), p, useInitialization)
			require.NoError(t, e)
			defer func() { require.NoError(t, target.close()) }()
			raw := requestFixture(t)
			request, e := parseRequest(raw)
			require.NoError(t, e)
			if lost {
				target.log = &lostAck{Backend: target.log}
			}
			first, e := target.publish(t.Context(), raw, request, "completed-then-purged", key)
			if lost {
				require.ErrorIs(t, e, ErrPending)
			} else {
				require.NoError(t, e)
			}
			require.NoError(t, target.artifacts.Purge(t.Context(), first.CapsuleID))
			guard := &forbidAppend{Backend: target.log}
			target.log = guard
			resumed, e := target.publish(t.Context(), raw, request, "completed-then-purged", key)
			require.NoError(t, e)
			assert.Equal(t, "appended", resumed.State)
			assert.Equal(t, uint64(1), resumed.Sequence)
			assert.False(t, guard.called)
		})
	}
}

type failFirstLookup struct {
	cll.Backend
	failed bool
}

func (f *failFirstLookup) GetEntry(ctx context.Context, id []byte) (cll.Entry, error) {
	if !f.failed {
		f.failed = true
		return cll.Entry{}, errors.New("transient lookup failure")
	}
	return f.Backend.GetEntry(ctx, id)
}
func TestMySQLPreAppendLookupFailureRemainsResumable(t *testing.T) {
	p, key := mysqlProfile(t)
	target, e := openTarget(t.Context(), p, useInitialization)
	require.NoError(t, e)
	defer func() { require.NoError(t, target.close()) }()
	target.log = &failFirstLookup{Backend: target.log}
	raw := requestFixture(t)
	request, e := parseRequest(raw)
	require.NoError(t, e)
	first, e := target.publish(t.Context(), raw, request, "lookup-failure", key)
	require.ErrorIs(t, e, ErrPending)
	assert.NotEmpty(t, first.CapsuleID)
	second, e := target.publish(t.Context(), raw, request, "lookup-failure", key)
	require.NoError(t, e)
	assert.Equal(t, first.CapsuleID, second.CapsuleID)
	assert.Equal(t, "appended", second.State)
}
