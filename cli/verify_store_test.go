package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	aacverify "github.com/action-state-group/agent-action-capsule/go/verify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// verifyProfileStore runs `verify --profile rep` and decodes its JSON.
func verifyProfileStore(t *testing.T) (map[string]any, error) {
	t.Helper()
	out, err := invoke(t, "", "verify", "--profile", "rep")
	var v map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &v), out)
	return v, err
}

// publishChild publishes a capsule whose chain names parent.
func publishChild(d *disclosureProfile, name, parent string) string {
	d.t.Helper()
	path := filepath.Join(d.dir, name+".json")
	require.NoError(d.t, os.WriteFile(path, []byte(fmt.Sprintf(`{"spec_version":"capsule-seal-request/v1","capsule":{"ActionID":%q,"ActionType":"fyi","Operator":"example-operator","Developer":"example-developer","Timestamp":"2026-10-07T00:00:00Z"},"chain":{"parent_capsule_id":%q,"relation":"confirms"},"payload":{"n":2}}`, name, parent)), 0o600))
	return d.run("publish", "--profile", "rep", "--request", path)["capsule_id"].(string)
}

func openRep(t *testing.T, use TargetUse) *Target {
	t.Helper()
	p, err := LoadProfile("rep")
	require.NoError(t, err)
	target, err := OpenTarget(t.Context(), p, use)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	return target
}

func TestVerifyProfileStorePasses(t *testing.T) {
	d := newDisclosureProfile(t)
	v, err := verifyProfileStore(t)
	require.NoError(t, err)
	assert.EqualValues(t, 0, v["entries"], "an empty log verifies")

	// One page per entry: the parent is on an earlier page than its child,
	// and still found, since the store checks run once on the whole log.
	old := storeScanPage
	storeScanPage = 1
	t.Cleanup(func() { storeScanPage = old })
	parent := d.publish("parent", `{"n":1}`)
	publishChild(d, "child", parent)
	v, err = verifyProfileStore(t)
	require.NoError(t, err)
	assert.EqualValues(t, 2, v["entries"])
	assert.EqualValues(t, 2, v["records_passed"])
	assert.Equal(t, "passed", v["store_checks"])
	assert.Empty(t, v["failed"])
	assert.Empty(t, v["could_not_check"])
	assert.Equal(t, "rep-log", v["log_id"])
}

func TestVerifyProfileStoreChainParentMissing(t *testing.T) {
	d := newDisclosureProfile(t)
	absent := strings.Repeat("ab", 32)
	child := publishChild(d, "orphan", absent)
	v, err := verifyProfileStore(t)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrPartial)
	assert.Equal(t, "failed", v["store_checks"])
	failed := v["failed"].([]any)
	require.Len(t, failed, 1)
	got := failed[0].(map[string]any)
	assert.Equal(t, child, got["capsule_id"])
	assert.EqualValues(t, 1, got["sequence"])
	assert.Equal(t, "store_checks", got["check"])
	assert.Contains(t, got["reason"], "chain_parent_missing")
}

func TestVerifyProfileStoreEntryWithoutRecord(t *testing.T) {
	d := newDisclosureProfile(t)
	d.publish("kept", `{"n":1}`)
	id := strings.Repeat("cd", 32)
	_, err := appendRecord(t.Context(), openRep(t, UseCLL).Log, id)
	require.NoError(t, err)
	v, err := verifyProfileStore(t)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrPartial)
	assert.EqualValues(t, 2, v["entries"])
	assert.EqualValues(t, 1, v["records_passed"])
	assert.Equal(t, []any{map[string]any{"sequence": float64(2), "capsule_id": id, "check": "record", "reason": "the artifact store holds no record for this entry"}}, v["failed"])
}

func TestVerifyProfileStoreUntrustedProducer(t *testing.T) {
	d := newDisclosureProfile(t)
	id := d.publish("signed", `{"n":1}`)
	other, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	d.run("profile", "update", "--profile", "rep", "--trusted-key", hex.EncodeToString(other))
	v, err := verifyProfileStore(t)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrPartial)
	failed := v["failed"].([]any)
	require.Len(t, failed, 1)
	assert.Equal(t, id, failed[0].(map[string]any)["capsule_id"])
	assert.Equal(t, "record", failed[0].(map[string]any)["check"])
	assert.Equal(t, "incomplete", v["store_checks"], "the refused record's capsule was not checked")
}

// A refused parent stays in the log, so its intact child is no orphan.
func TestVerifyProfileStoreRefusedParentIntactChild(t *testing.T) {
	d := newDisclosureProfile(t)
	parent := d.publish("parent", `{"n":1}`)
	p, err := LoadProfile("rep")
	require.NoError(t, err)
	oldKey := p.TrustedKeys[0]
	seed := filepath.Join(d.dir, "seed2")
	newKey := d.run("key", "generate", "--output", seed)["public_key"].(string)
	d.run("profile", "update", "--profile", "rep", "--signing-key-file", seed, "--trusted-key", oldKey+","+newKey)
	publishChild(d, "child", parent)
	d.run("profile", "update", "--profile", "rep", "--trusted-key", newKey)
	v, err := verifyProfileStore(t)
	require.Error(t, err)
	failed := v["failed"].([]any)
	require.Len(t, failed, 1, "only the parent's record fails, not the child's store checks")
	assert.Equal(t, parent, failed[0].(map[string]any)["capsule_id"])
	assert.Equal(t, "record", failed[0].(map[string]any)["check"])
	assert.EqualValues(t, 1, v["records_passed"])
	assert.Equal(t, "incomplete", v["store_checks"])
}

// An edited capsule in the store is a failed record, exit 1.
func TestVerifyProfileStoreEditedRecord(t *testing.T) {
	d := newDisclosureProfile(t)
	id := d.publish("edited", `{"n":1}`)
	target := openRep(t, UseArtifacts)
	_, err := target.db.ExecContext(t.Context(), `UPDATE capsule_store_capsules SET capsule_bytes = CAST(REPLACE(CAST(capsule_bytes AS TEXT), 'edited', 'editeD') AS BLOB) WHERE capsule_id = ?`, id)
	require.NoError(t, err)
	v, err := verifyProfileStore(t)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrPartial)
	failed := v["failed"].([]any)
	require.Len(t, failed, 1)
	assert.Equal(t, id, failed[0].(map[string]any)["capsule_id"])
	assert.Equal(t, "record", failed[0].(map[string]any)["check"])
}

func TestStoreFindingsSeparatesVerifierErrors(t *testing.T) {
	reason, internal := storeFindings(aacverify.VerificationResult{Findings: []aacverify.Finding{{Code: "verifier_internal_error", Severity: "error"}}})
	assert.True(t, internal)
	assert.Equal(t, "verifier_internal_error", reason)
	reason, internal = storeFindings(aacverify.VerificationResult{Findings: []aacverify.Finding{{Code: "verifier_internal_error", Severity: "error"}, {Code: "chain_parent_missing", Severity: "error"}, {Code: "note", Severity: "info"}}})
	assert.False(t, internal, "a real failure beside an internal error is a failure")
	assert.Equal(t, "verifier_internal_error, chain_parent_missing", reason)
}

func TestVerifyProfileStorePurgedOriginalsCouldNotCheck(t *testing.T) {
	d := newDisclosureProfile(t)
	id := d.publish("purged", `{"n":1}`)
	require.NoError(t, openRep(t, UseArtifacts).Artifacts.Purge(t.Context(), id))
	v, err := verifyProfileStore(t)
	require.ErrorIs(t, err, ErrPartial, "a missing original is not a failed check")
	assert.Empty(t, v["failed"])
	unchecked := v["could_not_check"].([]any)
	require.NotEmpty(t, unchecked)
	for _, item := range unchecked {
		got := item.(map[string]any)
		assert.Equal(t, id, got["capsule_id"])
		assert.Equal(t, "originals", got["check"])
	}
	assert.EqualValues(t, 0, v["records_passed"])
}

func TestVerifyProfileStoreRefusals(t *testing.T) {
	newDisclosureProfile(t)
	_, err := invoke(t, "", "verify", "--profile", "rep", "--witness-directory", "witnesses.json")
	require.ErrorIs(t, err, ErrInput)
	c := NewCommand()
	c.SetContext(t.Context())
	owned := Profile{Name: "owned", Type: "jsonl", LogID: "l", Log: "example"}
	require.ErrorContains(t, verifyStore(c, owned), "capsulectl-example plugin")
}
