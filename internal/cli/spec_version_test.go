package cli

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	emit "github.com/action-state-group/capsule-emit-go"
	"github.com/action-state-group/capsule-emit-go/artifact"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	specVersion04           = "draft-mih-scitt-agent-action-capsule-04"
	specVersion05           = "draft-mih-scitt-agent-action-capsule-05"
	unrecognizedSpecVersion = "draft-mih-scitt-agent-action-capsule-99"
)

// emitModuleDir resolves the capsule-emit-go module pinned in go.mod, so tests
// replay its committed vectors instead of keeping copies in this repository.
func emitModuleDir(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/action-state-group/capsule-emit-go").Output()
	require.NoError(t, err)
	dir := strings.TrimSpace(string(out))
	require.NotEmpty(t, dir, "capsule-emit-go module is not downloaded")
	return dir
}

// jsonlArtifactTarget initializes a jsonl profile trusting keys and opens its
// artifact store the way every capsulectl artifact command does.
func jsonlArtifactTarget(t *testing.T, keys ...string) *target {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := profileFixture(t)
	p.Type = "jsonl"
	p.Connection.Database = filepath.Join(t.TempDir(), "store")
	p.TrustedKeys = keys
	require.NoError(t, saveProfile(p, false))
	_, err := invoke(t, "", "store", "init", "--profile", p.Name)
	require.NoError(t, err)
	tg, err := openTarget(t.Context(), p, useArtifacts)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tg.close()) })
	return tg
}

func sealedSpecVersion(t *testing.T, capsule []byte) any {
	t.Helper()
	payload, err := emit.DecodePayload(capsule)
	require.NoError(t, err)
	return payload["spec_version"]
}

// TestCapsulectlSealStampsSpecVersion05: the seal verb writes a record whose
// Capsule carries spec_version -05, the revision the pinned emit library and
// the AAC reference verifier both name as current.
func TestCapsulectlSealStampsSpecVersion05(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := profileFixture(t)
	require.NoError(t, saveProfile(p, false))
	dir := t.TempDir()
	request := filepath.Join(dir, "request.json")
	require.NoError(t, os.WriteFile(request, requestFixture(t), 0600))
	output := filepath.Join(dir, "record.json")

	_, err := invoke(t, "", "seal", "--profile", p.Name, "--request", request, "--output", output)
	require.NoError(t, err)
	record, err := readRecord(output)
	require.NoError(t, err)

	assert.Equal(t, specVersion05, emit.SpecVersion)
	assert.Equal(t, specVersion05, sealedSpecVersion(t, record.Capsule))
}

type interopExpected struct {
	CapsuleID    string `json:"capsule_id"`
	PublicKeyHex string `json:"public_key_hex"`
}

// TestV04RecordAndV05TwinLoadAndVerifyThroughArtifactStore replays
// capsule-emit-go's committed format-4 interop packs: every released -04
// record and its -05 twin is stored, read back byte-for-byte, and verified
// (identity, Producer Envelope, trusted signer) through capsule-cli's
// artifact store. The twins differ only by the spec_version the ID commits to.
func TestV04RecordAndV05TwinLoadAndVerifyThroughArtifactStore(t *testing.T) {
	root := filepath.Join(emitModuleDir(t), "testdata", "capsule-emit")
	packs := []struct {
		dir         string
		specVersion string
	}{
		{"format4-interop", specVersion04},
		{"format4-interop-v05", specVersion05},
	}
	for _, name := range []string{"authored", "received", "who", "did", "composition"} {
		t.Run(name, func(t *testing.T) {
			ids := make([]string, 0, len(packs))
			for _, pack := range packs {
				caseDir := filepath.Join(root, pack.dir, "valid", name)
				capsule, err := os.ReadFile(filepath.Join(caseDir, "capsule.detached.jcs"))
				require.NoError(t, err)
				envelope, err := os.ReadFile(filepath.Join(caseDir, "envelope.cose"))
				require.NoError(t, err)
				raw, err := os.ReadFile(filepath.Join(caseDir, "expected.json"))
				require.NoError(t, err)
				var expected interopExpected
				require.NoError(t, json.Unmarshal(raw, &expected))
				require.Equal(t, pack.specVersion, sealedSpecVersion(t, capsule))

				tg := jsonlArtifactTarget(t, expected.PublicKeyHex)
				record := artifact.Record{CapsuleID: expected.CapsuleID, Capsule: capsule, ProducerEnvelope: envelope}
				require.NoError(t, tg.artifacts.Put(t.Context(), record), pack.specVersion)
				got, err := tg.artifacts.Get(t.Context(), expected.CapsuleID)
				require.NoError(t, err, pack.specVersion)
				assert.Equal(t, capsule, got.Capsule)
				assert.Equal(t, envelope, got.ProducerEnvelope)

				keys, err := parseKeys([]string{expected.PublicKeyHex})
				require.NoError(t, err)
				_, err = artifact.Verify(got, keys)
				require.NoError(t, err, pack.specVersion)
				ids = append(ids, got.CapsuleID)
			}
			assert.NotEqual(t, ids[0], ids[1])
		})
	}
}

// TestUnrecognizedSpecVersionIsNotRejected: a signed record stamped with a
// spec_version this build has never heard of is stored, read back and passes
// `capsulectl verify` like any other ("verifiers accept every published
// spec_version"; an unknown one is informational, never a rejection).
func TestUnrecognizedSpecVersionIsNotRejected(t *testing.T) {
	_, key := profileFixture(t)
	public, ok := key.Public().(ed25519.PublicKey)
	require.True(t, ok)
	request := `{"spec_version":"capsule-seal-request/v1","capsule":{"ActionID":"future-rev","ActionType":"fyi","Operator":"test-operator","Developer":"test-developer","Timestamp":"2026-09-08T00:00:00Z"}}`
	r, err := parseRequest([]byte(request))
	require.NoError(t, err)
	sealed, err := seal(r, key)
	require.NoError(t, err)

	value, err := emit.DecodePayload(sealed.Capsule)
	require.NoError(t, err)
	value["spec_version"] = unrecognizedSpecVersion
	delete(value, "capsule_id")
	id, err := canonical.ComputeCapsuleID(value)
	require.NoError(t, err)
	value["capsule_id"] = id
	capsule, err := canonical.JCS(value)
	require.NoError(t, err)
	identity, err := emit.NewEd25519SigningIdentity(key)
	require.NoError(t, err)
	envelope, err := emit.Sign(emit.BuiltPayload{CapsuleID: id, Value: value, JSON: capsule}, identity)
	require.NoError(t, err)

	tg := jsonlArtifactTarget(t, hex.EncodeToString(public))
	require.NoError(t, tg.artifacts.Put(t.Context(), artifact.Record{CapsuleID: id, Capsule: capsule, ProducerEnvelope: envelope}))
	got, err := tg.artifacts.Get(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, unrecognizedSpecVersion, sealedSpecVersion(t, got.Capsule))

	path := filepath.Join(t.TempDir(), "record.json")
	b, err := json.Marshal(got)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, b, 0600))
	out, err := invoke(t, "", "verify", "--profile", "test", "--capsule", path)
	require.NoError(t, err)
	assert.Contains(t, out, `"capsule_identity":"passed"`)
	assert.Contains(t, out, `"producer_signature_and_trust":"passed"`)
}
