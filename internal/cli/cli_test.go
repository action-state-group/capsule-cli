package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/action-state-group/capsule-emit-go/artifact"
	"github.com/action-state-group/cll-go/checkpoint"
	"github.com/action-state-group/cll-go/cll"
	"github.com/action-state-group/cll-go/store/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func profileFixture(t *testing.T) (Profile, ed25519.PrivateKey) {
	t.Helper()
	public, private, e := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, e)
	p := Profile{Name: "test", Type: "mysql", LogID: "test-log", Namespace: "capsule"}
	p.Connection.Host = "127.0.0.1"
	p.Connection.Port = 3306
	p.Connection.Database = "capsule_cli_test"
	p.Connection.TLS = "false"
	p.Credentials.Username = "root"
	p.Signing.Value = hex.EncodeToString(private.Seed())
	p.TrustedKeys = []string{hex.EncodeToString(public)}
	p.Checkpoint.Signing = p.Signing
	p.Checkpoint.TrustedKeys = p.TrustedKeys
	return p, private
}
func requestFixture(t *testing.T) []byte {
	t.Helper()
	return []byte(`{"spec_version":"capsule-seal-request/v1","capsule":{"ActionID":"test-action","ActionType":"fyi","Operator":"test-operator","Developer":"test-developer","Timestamp":"2026-09-08T00:00:00Z"},"payload":{"a": 1},"agent_output":{"ok":true}}`)
}
func invoke(t *testing.T, input string, args ...string) (string, error) {
	t.Helper()
	c := NewCommand()
	var out bytes.Buffer
	c.SetIn(strings.NewReader(input))
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs(args)
	e := c.ExecuteContext(t.Context())
	return out.String(), e
}

func TestProfiles(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CAPSULE_PROFILE", "implicit")
	out, e := invoke(t, "", "profile", "list")
	require.NoError(t, e)
	assert.JSONEq(t, `{"spec_version":"capsule-cli-result/v1","profiles":[]}`, out)
	_, e = invoke(t, "", "profile", "create", "--name", "one", "--type", "mysql", "--mysql-host", "localhost", "--mysql-database", "local", "--mysql-tls", "false", "--log-id", "log-a", "--mysql-password", "top-secret")
	require.NoError(t, e)
	p, e := loadProfile("one")
	require.NoError(t, e)
	assert.Equal(t, "mysql", p.Type)
	assert.Equal(t, "capsule", p.Namespace)
	path, e := profilePath("one")
	require.NoError(t, e)
	info, e := os.Stat(path)
	require.NoError(t, e)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
	_, e = invoke(t, "", "profile", "create", "--name", "one", "--mysql-host", "localhost", "--mysql-database", "local", "--log-id", "other")
	require.Error(t, e)
	out, e = invoke(t, "", "profile", "show", "one")
	require.NoError(t, e)
	assert.NotContains(t, out, "top-secret")
	assert.Contains(t, out, "[redacted]")
	out, e = invoke(t, "", "profile", "list")
	require.NoError(t, e)
	assert.JSONEq(t, `{"spec_version":"capsule-cli-result/v1","profiles":["one"]}`, out)
	dir, e := profilesDir()
	require.NoError(t, e)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ignored.txt"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "-invalid.yaml"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "one-two.yaml"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "one_two.yaml"), nil, 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "directory.yaml"), 0o700))
	outside := filepath.Join(t.TempDir(), "outside.yaml")
	require.NoError(t, os.WriteFile(outside, nil, 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "symlink.yaml")))
	out, e = invoke(t, "", "profile", "list")
	require.NoError(t, e)
	assert.JSONEq(t, `{"spec_version":"capsule-cli-result/v1","profiles":["one","one-two","one_two"]}`, out)
	_, e = invoke(t, "", "profile", "list", "extra")
	require.Error(t, e)
	_, e = invoke(t, "", "profile", "show", "--profile", "one")
	require.Error(t, e)
	_, e = invoke(t, "", "profile", "update", "--profile", "one", "--mysql-host", "127.0.0.1")
	require.NoError(t, e)
	p, e = loadProfile("one")
	require.NoError(t, e)
	assert.Equal(t, "127.0.0.1", p.Connection.Host)
	assert.Equal(t, "top-secret", p.Credentials.Password.Value)
	_, e = invoke(t, "", "profile", "update", "--profile", "one", "--mysql-password-file", "/missing")
	require.Error(t, e)
	_, e = invoke(t, "", "profile", "update", "--profile", "one", "--mysql-password", "", "--mysql-password-env", "TEST_SECRET")
	require.NoError(t, e)
	p, e = loadProfile("one")
	require.NoError(t, e)
	assert.Empty(t, p.Credentials.Password.Value)
	assert.Equal(t, "TEST_SECRET", p.Credentials.Password.Env)
	_, e = invoke(t, "", "get", "--capsule-id", strings.Repeat("a", 64))
	require.Error(t, e)
	_, e = invoke(t, "", "profile", "create", "--name", "../../escape")
	require.Error(t, e)
}
func TestGuidedAndUnknownConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, e := invoke(t, "guided\nlocalhost\nlocal\nlog-a\nroot\n", "profile", "create", "--interactive")
	require.NoError(t, e)
	p, e := loadProfile("guided")
	require.NoError(t, e)
	assert.Equal(t, "log-a", p.LogID)
	path, e := profilePath("guided")
	require.NoError(t, e)
	f, e := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	require.NoError(t, e)
	_, e = f.WriteString("unknown: true\n")
	require.NoError(t, e)
	require.NoError(t, f.Close())
	_, e = loadProfile("guided")
	require.Error(t, e)
}

func TestSQLiteProfileCreate(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dbPath := filepath.Join(t.TempDir(), "store.db")
	trusted := strings.Repeat("ab", 32)
	// Non-interactive: SQLite is a peer type selected with --type sqlite and a
	// file path; no MySQL host/port/TLS is required or consulted.
	_, e := invoke(t, "", "profile", "create", "--name", "sq", "--type", "sqlite",
		"--sqlite-path", dbPath, "--namespace", "demo", "--log-id", "log-s", "--trusted-key", trusted)
	require.NoError(t, e)
	p, e := loadProfile("sq")
	require.NoError(t, e)
	assert.Equal(t, "sqlite", p.Type)
	assert.Equal(t, dbPath, p.Connection.Database)
	assert.Equal(t, "demo", p.Namespace)

	// Interactive SQLite prompts only for Name, the SQLite path, and Log ID —
	// never MySQL host/user. Empty stdin after those three still saves.
	dbPath2 := filepath.Join(t.TempDir(), "store2.db")
	_, e = invoke(t, "sqguided\n"+dbPath2+"\nlog-s2\n", "profile", "create", "--interactive", "--type", "sqlite")
	require.NoError(t, e)
	p2, e := loadProfile("sqguided")
	require.NoError(t, e)
	assert.Equal(t, "sqlite", p2.Type)
	assert.Equal(t, dbPath2, p2.Connection.Database)
	assert.Equal(t, "log-s2", p2.LogID)
	assert.Empty(t, p2.Connection.Host)
	assert.Empty(t, p2.Credentials.Username)
}
func TestRequiredProfileAndNoRuntimeOverrides(t *testing.T) {
	for _, args := range [][]string{{"seal"}, {"get"}, {"verify"}, {"publish"}, {"store", "init"}, {"cll", "list"}, {"cll", "append"}, {"cll", "verify"}, {"cll", "checkpoint", "create"}, {"cll", "checkpoint", "publish"}, {"cll", "checkpoint", "status"}, {"profile", "show"}, {"profile", "update"}} {
		t.Run(strings.Join(args, "-"), func(t *testing.T) { _, e := invoke(t, "", args...); require.Error(t, e) })
	}
	_, e := invoke(t, "", "get", "--profile", "test", "--mysql-password", "should-not-be-accepted")
	require.Error(t, e)
	assert.NotContains(t, SafeError(e), "should-not-be-accepted")
	_, e = invoke(t, "", "--help")
	require.NoError(t, e)
	_, e = invoke(t, "", "--version")
	require.NoError(t, e)
}
func TestSecretProtection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(path, []byte("sensitive\n"), 0644))
	_, e := (Secret{File: path}).resolve()
	require.Error(t, e)
	require.NoError(t, os.Chmod(path, 0600))
	v, e := (Secret{File: path}).resolve()
	require.NoError(t, e)
	assert.Equal(t, "sensitive", v)
	t.Setenv("EXPLICIT_SECRET", "env-secret")
	v, e = (Secret{Env: "EXPLICIT_SECRET"}).resolve()
	require.NoError(t, e)
	assert.Equal(t, "env-secret", v)
	_, e = (Secret{Value: "literal", Env: "EXPLICIT_SECRET"}).resolve()
	require.Error(t, e)
	_, e = (Secret{Env: "UNSET_CAPSULE_TEST_SECRET"}).resolve()
	require.Error(t, e)
}
func TestSealVerifyExactOriginals(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := profileFixture(t)
	require.NoError(t, saveProfile(p, false))
	dir := t.TempDir()
	request := filepath.Join(dir, "request.json")
	recordPath := filepath.Join(dir, "capsule.json")
	require.NoError(t, os.WriteFile(request, requestFixture(t), 0600))
	_, e := invoke(t, "", "seal", "--profile", p.Name, "--request", request, "--output", recordPath)
	require.NoError(t, e)
	record, e := readRecord(recordPath)
	require.NoError(t, e)
	require.Len(t, record.Artifacts, 2)
	assert.Equal(t, `{"a": 1}`, string(record.Artifacts[0].Content))
	out, e := invoke(t, "", "verify", "--profile", p.Name, "--capsule", recordPath)
	require.NoError(t, e)
	assert.Contains(t, out, "passed")
	assert.Contains(t, out, "not_performed")
	_, e = invoke(t, "", "seal", "--profile", p.Name, "--request", request, "--output", recordPath)
	require.Error(t, e)
	keys, e := parseKeys(p.TrustedKeys)
	require.NoError(t, e)
	record.Artifacts[0].Content = []byte(`{"a":2}`)
	_, e = artifact.Verify(record, keys)
	require.Error(t, e)
	p.TrustedKeys = nil
	require.NoError(t, saveProfile(p, true))
	_, e = invoke(t, "", "verify", "--profile", p.Name, "--capsule", recordPath)
	require.ErrorIs(t, e, ErrPartial)
}
func TestInputBoundsAndUnknownFields(t *testing.T) {
	var r Request
	require.Error(t, decodeJSON([]byte(`{"unexpected":true}`), &r))
	require.Error(t, decodeJSON(append(requestFixture(t), []byte(` {}`)...), &r))
	require.Error(t, decodeJSON(bytes.Repeat([]byte(" "), maxInput+1), &r))
	_, e := parseRequest([]byte(`{"spec_version":"old"}`))
	require.Error(t, e)
}
func TestReadOnlyFailsBeforeConnection(t *testing.T) {
	p, _ := profileFixture(t)
	p.ReadOnly = true
	p.Connection.Host = "unreachable.invalid"
	_, e := openTarget(t.Context(), p, useCLL)
	require.ErrorIs(t, e, ErrReadOnlyCLL)
	_, e = openTarget(t.Context(), p, useInitialization)
	require.ErrorIs(t, e, ErrReadOnlyCLL)
}
func TestProfileTargetCanBeUpdated(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := profileFixture(t)
	require.NoError(t, saveProfile(p, false))
	_, e := invoke(t, "", "profile", "update", "--profile", p.Name, "--log-id", "other")
	require.NoError(t, e)
}
func TestCheckpointProof(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, key := profileFixture(t)
	require.NoError(t, saveProfile(p, false))
	backend := memory.New()
	defer func() { require.NoError(t, backend.Close()) }()
	value := bytes.Repeat([]byte{1}, 32)
	_, e := backend.Append(t.Context(), cll.AppendInput{Value: value, AppendedAt: time.Now().UTC()})
	require.NoError(t, e)
	signer, e := checkpoint.NewEd25519Signer(key)
	require.NoError(t, e)
	cfg := checkpoint.DefaultRunnerConfig(p.LogID)
	cfg.Cadence.CadenceEntries = 1
	runner, e := checkpoint.NewRunner(cfg, backend, signer)
	require.NoError(t, e)
	_, e = runner.RunOnce(t.Context(), time.Now().UTC())
	require.NoError(t, e)
	state, e := backend.LoadCLL(t.Context())
	require.NoError(t, e)
	require.NotNil(t, state.Checkpoint)
	raw, e := json.Marshal(Proof{Checkpoint: state.Checkpoint.Bytes, CapsuleID: hex.EncodeToString(value)})
	require.NoError(t, e)
	path := filepath.Join(t.TempDir(), "proof.json")
	require.NoError(t, os.WriteFile(path, raw, 0600))
	_, e = invoke(t, "", "cll", "verify", "--profile", p.Name, "--proof", path)
	require.NoError(t, e)
	p.LogID = "wrong-log"
	_, e = verifyCheckpoint(p, state.Checkpoint.Bytes)
	require.ErrorIs(t, e, ErrConflict)
}

func TestVerifyMissingOriginalsIsPartial(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, key := profileFixture(t)
	require.NoError(t, saveProfile(p, false))
	request, e := parseRequest(requestFixture(t))
	require.NoError(t, e)
	r, e := seal(request, key)
	require.NoError(t, e)
	assert.Empty(t, missingBindings(r))
	r.Artifacts = nil
	assert.Len(t, missingBindings(r), 2)
	raw, e := json.Marshal(r)
	require.NoError(t, e)
	path := filepath.Join(t.TempDir(), "missing.json")
	require.NoError(t, os.WriteFile(path, raw, 0600))
	out, e := invoke(t, "", "verify", "--profile", p.Name, "--capsule", path)
	require.ErrorIs(t, e, ErrPartial)
	assert.Contains(t, out, "missing_originals")
}

func TestKeyGenerateWritesSeedAndPrintsMatchingPublicKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signer.ed25519")
	out, e := invoke(t, "", "key", "generate", "--output", path)
	require.NoError(t, e)
	var result struct {
		SpecVersion    string `json:"spec_version"`
		PublicKey      string `json:"public_key"`
		SigningKeyFile string `json:"signing_key_file"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	assert.Equal(t, "capsule-cli-result/v1", result.SpecVersion)
	assert.Equal(t, path, result.SigningKeyFile)

	// The file holds the seed in the exact format --signing-key-file reads.
	info, e := os.Stat(path)
	require.NoError(t, e)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	seedHex, e := os.ReadFile(path)
	require.NoError(t, e)
	priv, e := privateKey(Secret{Value: string(seedHex)})
	require.NoError(t, e)

	// The printed public key is the seed's real Ed25519 public key.
	public, ok := priv.Public().(ed25519.PublicKey)
	require.True(t, ok)
	assert.Equal(t, result.PublicKey, hex.EncodeToString(public))
	assert.NotContains(t, out, string(seedHex)) // the secret seed is never printed

	// It round-trips as a usable producer key in a profile.
	_, e = parseKeys([]string{result.PublicKey})
	require.NoError(t, e)
}

func TestKeyGenerateRequiresOutput(t *testing.T) {
	_, e := invoke(t, "", "key", "generate")
	require.ErrorIs(t, e, ErrInput)
	assert.Equal(t, 2, ExitCode(e))
}

func TestKeyGenerateRefusesToClobber(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k.ed25519")
	_, e := invoke(t, "", "key", "generate", "--output", path)
	require.NoError(t, e)
	before, e := os.ReadFile(path)
	require.NoError(t, e)
	// A second generate to the same path must not destroy the existing key.
	_, e = invoke(t, "", "key", "generate", "--output", path)
	require.ErrorIs(t, e, ErrInput)
	assert.Equal(t, 2, ExitCode(e))
	after, e := os.ReadFile(path)
	require.NoError(t, e)
	assert.Equal(t, before, after)
}

func TestKeyShowPublicDerivesFromSeedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.ed25519")
	gen, e := invoke(t, "", "key", "generate", "-o", path) // also exercises the -o shorthand
	require.NoError(t, e)
	var g struct {
		PublicKey string `json:"public_key"`
	}
	require.NoError(t, json.Unmarshal([]byte(gen), &g))

	shown, e := invoke(t, "", "key", "show-public", path)
	require.NoError(t, e)
	var s struct {
		PublicKey string `json:"public_key"`
	}
	require.NoError(t, json.Unmarshal([]byte(shown), &s))
	assert.Equal(t, g.PublicKey, s.PublicKey) // seed file alone yields the same public key

	// a missing seed file names the path (input-file error, exit 2)
	_, e = invoke(t, "", "key", "show-public", filepath.Join(t.TempDir(), "nope.ed25519"))
	require.ErrorIs(t, e, ErrInput)
	assert.Equal(t, 2, ExitCode(e))
}

func TestInputFileErrorNamesPathButProfileErrorStaysGeneric(t *testing.T) {
	// A missing --request/--proof/--capsule file names the caller-supplied path
	// (which discloses nothing sensitive) instead of the profile-conflated
	// message, but keeps ErrInput / exit code 2.
	missing := filepath.Join(t.TempDir(), "nope.json")
	_, e := readInput(missing)
	require.ErrorIs(t, e, ErrInput)
	assert.Equal(t, 2, ExitCode(e))
	assert.Equal(t, "input file not found: "+missing, SafeError(e))

	// A non-not-found open failure still names the path but marks it unreadable.
	regular := filepath.Join(t.TempDir(), "afile")
	require.NoError(t, os.WriteFile(regular, []byte("x"), 0600))
	notDir := filepath.Join(regular, "child.json") // parent is a file: ENOTDIR, not ErrNotExist
	_, e = readInput(notDir)
	require.ErrorIs(t, e, ErrInput)
	assert.Equal(t, "cannot read input file: "+notDir, SafeError(e))

	// A directory opens but fails to read (EISDIR); it stays exit 2 + named path,
	// not the generic exit-1 operational catch-all.
	dir := t.TempDir()
	_, e = readInput(dir)
	require.ErrorIs(t, e, ErrInput)
	assert.Equal(t, 2, ExitCode(e))
	assert.Equal(t, "cannot read input file: "+dir, SafeError(e))

	// A profile/configuration failure stays generic and leaks no detail.
	profErr := errors.Join(ErrInput, errors.New("dial tcp 10.0.0.1:3306: connection refused"))
	assert.Equal(t, "invalid input or profile configuration", SafeError(profErr))
}

func TestInputExitCodesAreDistinctAndRedacted(t *testing.T) {
	_, e := invoke(t, "", "get")
	require.Error(t, e)
	assert.Equal(t, 2, ExitCode(e))
	assert.Equal(t, "invalid input or profile configuration", SafeError(e))
	_, e = invoke(t, "", "get", "--mysql-password", "do-not-print-me")
	require.Error(t, e)
	assert.Equal(t, 2, ExitCode(e))
	assert.NotContains(t, SafeError(e), "do-not-print-me")
	var value Request
	e = decodeJSON([]byte(`{"unknown":true}`), &value)
	require.Error(t, e)
	assert.Equal(t, 2, ExitCode(e))
}

func TestInputTaxonomyAcrossCommands(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := profileFixture(t)
	require.NoError(t, saveProfile(p, false))
	for _, args := range [][]string{{"seal"}, {"seal", "--output", "unused"}, {"seal", "--request", "does-not-exist", "--output", "unused"}, {"get"}, {"publish"}, {"cll", "list", "--limit", "0"}, {"cll", "checkpoint", "publish"}, {"cll", "checkpoint", "publish", "--checkpoint", "1"}} {
		t.Run(strings.Join(args, "-"), func(t *testing.T) {
			_, e := invoke(t, "", append(args, "--profile", p.Name)...)
			require.Error(t, e)
			assert.Equal(t, 2, ExitCode(e))
		})
	}
	_, e := parseRequest([]byte(`{"spec_version":"wrong"}`))
	require.Error(t, e)
	assert.Equal(t, 2, ExitCode(e))
}
func TestOneSidedOriginalCoverage(t *testing.T) {
	p, key := profileFixture(t)
	for _, kind := range []string{"payload", "output", "neither"} {
		t.Run(kind, func(t *testing.T) {
			r, e := parseRequest(requestFixture(t))
			require.NoError(t, e)
			if kind != "payload" {
				r.Payload = nil
			}
			if kind != "output" {
				r.AgentOutput = nil
			}
			record, e := seal(r, key)
			require.NoError(t, e)
			assert.Empty(t, missingBindings(record))
			keys, e := parseKeys(p.TrustedKeys)
			require.NoError(t, e)
			_, e = artifact.Verify(record, keys)
			require.NoError(t, e)
		})
	}
}
func TestAppendRejectsStrippedOriginalsBeforeConnection(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, key := profileFixture(t)
	p.Connection.Host = "unreachable.invalid"
	require.NoError(t, saveProfile(p, false))
	r, e := parseRequest(requestFixture(t))
	require.NoError(t, e)
	record, e := seal(r, key)
	require.NoError(t, e)
	record.Artifacts = nil
	raw, e := json.Marshal(record)
	require.NoError(t, e)
	path := filepath.Join(t.TempDir(), "stripped.json")
	require.NoError(t, os.WriteFile(path, raw, 0600))
	_, e = invoke(t, "", "cll", "append", "--profile", p.Name, "--capsule", path)
	require.ErrorIs(t, e, ErrPartial)
}
func TestPublishRequiresItsOwnTrustedKeyBeforeConnection(t *testing.T) {
	p, key := profileFixture(t)
	require.NoError(t, requirePublisherKey(p, key))
	p.TrustedKeys = nil
	e := requirePublisherKey(p, key)
	require.ErrorIs(t, e, artifact.ErrUntrustedSigner)
	assert.Equal(t, 2, ExitCode(e))
	assert.Contains(t, SafeError(e), "trusted_keys")
}

func TestCheckpointHexKeyCaseIsNotDifferentIdentity(t *testing.T) {
	p, _ := profileFixture(t)
	key := p.Checkpoint.TrustedKeys[0]
	p.Checkpoint.TrustedKeys = []string{strings.ToUpper(key)}
	assert.True(t, checkpointSignerTrusted(p, key))
	p.Checkpoint.Endpoint = "https://example.invalid"
	p.Checkpoint.PublicKey = key
	a, e := serviceID(p)
	require.NoError(t, e)
	p.Checkpoint.PublicKey = strings.ToUpper(key)
	b, e := serviceID(p)
	require.NoError(t, e)
	assert.Equal(t, a, b)
}
