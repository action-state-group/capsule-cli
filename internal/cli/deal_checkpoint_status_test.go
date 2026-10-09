package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/action-state-group/checkpointed-local-log/go/checkpoint"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storeFiles is every file under the deal store's directory, by path, with
// the SHA-256 of its bytes.
func storeFiles(t *testing.T, p Profile) map[string]string {
	t.Helper()
	dir := filepath.Dir(p.Connection.Database)
	files := map[string]string{}
	require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		files[path] = hex.EncodeToString(sum[:])
		return nil
	}))
	require.NotEmpty(t, files)
	return files
}

// checkpointStatusOf runs the status read and checks it changed no byte of
// the store.
func checkpointStatusOf(t *testing.T, dealID string) map[string]any {
	t.Helper()
	p, err := loadProfile("deal")
	require.NoError(t, err)
	before := storeFiles(t, p)
	out := dealRun(t, "checkpoint", "status", "--deal", dealID)
	assert.Equal(t, before, storeFiles(t, p), "the read changes no file under the store")
	return out
}

// latestDealCheckpoint is the deal log's latest stored checkpoint.
func latestDealCheckpoint(t *testing.T, p Profile, dealID string) (string, checkpoint.Record) {
	t.Helper()
	p.LogID = dealLogID(dealID)
	target, err := openTarget(t.Context(), p, useCLLRead)
	require.NoError(t, err)
	defer func() { require.NoError(t, target.close()) }()
	state, err := target.log.LoadCLL(t.Context())
	require.NoError(t, err)
	require.NotNil(t, state.Checkpoint)
	record, err := checkpoint.ParseRecord(state.Checkpoint.Bytes)
	require.NoError(t, err)
	sum := sha256.Sum256(state.Checkpoint.Bytes)
	return hex.EncodeToString(sum[:]), record
}

// The status follows the deal checkpoint through the cadence: scheduled,
// pending while the witness is down, then witnessed once a receipt is
// stored. It never reaches the witness, never cuts a checkpoint, and changes
// no byte of the store.
func TestDealCheckpointStatusFollowsTheWitness(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	endpoint, connections := countingWitness(t)
	p := cadenceFixture(t, endpoint, public, "1h", "0s", 0)
	dealID := retailDeal(t)

	status := checkpointStatusOf(t, dealID)
	id, record := latestDealCheckpoint(t, p, dealID)
	assert.Equal(t, dealID, status["deal_id"])
	assert.Equal(t, "deal/"+dealID, status["log_id"])
	cp := status["checkpoint"].(map[string]any)
	assert.Equal(t, id, cp["id"], "the checkpoint is named by the SHA-256 of its signed statement")
	assert.EqualValues(t, record.MMRSize, cp["mmr_size"])
	assert.Equal(t, record.Root, cp["root"])
	assert.Equal(t, record.Timestamp.UTC().Format("2006-01-02T15:04:05Z"), cp["at"])
	assert.EqualValues(t, mmrLeafCount(record.MMRSize), cp["entries"], "the log entries the checkpoint covers")
	assert.GreaterOrEqual(t, status["log_entries"].(float64), cp["entries"].(float64), "the log head is at or past the checkpoint")
	assert.Equal(t, "scheduled", status["witness"].(map[string]any)["state"], "not in a tick yet")

	tick := dealRun(t, "tick")
	require.Equal(t, "ticked", tick["state"])
	reached := connections.Load()
	id, record = latestDealCheckpoint(t, p, dealID)
	status = checkpointStatusOf(t, dealID)
	assert.Equal(t, id, status["checkpoint"].(map[string]any)["id"])
	witness := status["witness"].(map[string]any)
	assert.Equal(t, "pending", witness["state"], "the witness is down")
	assert.NotEmpty(t, witness["reason"])
	assert.NotEmpty(t, witness["text"])

	deliver(t, p, cadenceSize(t, p), key)
	status = checkpointStatusOf(t, dealID)
	witness = status["witness"].(map[string]any)
	assert.Equal(t, "witnessed", witness["state"])
	assert.Equal(t, "all", witness["extent"], "the tick holds this very checkpoint")
	assert.EqualValues(t, record.MMRSize, witness["witnessed_mmr_size"])
	assert.NotEmpty(t, witness["tick_at"])
	assert.Equal(t, reached, connections.Load(), "the status reads never contact the witness")
	again, _ := latestDealCheckpoint(t, p, dealID)
	assert.Equal(t, id, again, "the status reads never cut a checkpoint")
}

// Without a witness the state says so.
func TestDealCheckpointStatusWithoutAWitness(t *testing.T) {
	dealFixture(t)
	dealID := retailDeal(t)
	status := checkpointStatusOf(t, dealID)
	require.NotNil(t, status["checkpoint"])
	assert.Equal(t, map[string]any{"state": "not_configured"}, status["witness"])
}

// A deal whose log has no checkpoint yet: null fields, not an error.
func TestDealCheckpointStatusWithNoCheckpoint(t *testing.T) {
	dealFixture(t)
	p, err := loadProfile("deal")
	require.NoError(t, err)
	p.Checkpoint.Signing = Secret{}
	require.NoError(t, saveProfile(p, true))
	dealID := dealRun(t, "open", "--input", filepath.Join(retailDemo, "open.json"))["deal_id"].(string)
	status := checkpointStatusOf(t, dealID)
	assert.Nil(t, status["checkpoint"])
	assert.Nil(t, status["witness"])
	assert.EqualValues(t, 1, status["log_entries"], "the log holds the baseline")
}

// A read_only profile reads the status; the same profile cannot seal.
func TestDealCheckpointStatusOnAReadOnlyProfile(t *testing.T) {
	dealFixture(t)
	dealID := retailDeal(t)
	writable := checkpointStatusOf(t, dealID)
	p, err := loadProfile("deal")
	require.NoError(t, err)
	p.ReadOnly = true
	require.NoError(t, saveProfile(p, true))
	assert.Equal(t, writable, checkpointStatusOf(t, dealID))
	_, err = invoke(t, "", "--profile", "deal", "deal", "note", "--deal", dealID, "--kind", "message", "--input", writeJSON(t, `{"from":"user","text":"hi"}`))
	assert.ErrorIs(t, err, ErrReadOnlyCLL, "the profile is read-only")
}

// An unknown deal is an input error, and the read still writes nothing.
func TestDealCheckpointStatusOfAnUnknownDeal(t *testing.T) {
	dealFixture(t)
	retailDeal(t)
	p, err := loadProfile("deal")
	require.NoError(t, err)
	before := storeFiles(t, p)
	_, err = invoke(t, "", "--profile", "deal", "deal", "checkpoint", "status", "--deal", "deal-0000000000000000")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown deal")
	assert.Equal(t, before, storeFiles(t, p))
}

// Opening a log that does not exist fails with the log's error; its cleanup
// never calls Close on the failed open's typed nil.
func TestOpenTargetOnAMissingLogFailsCleanly(t *testing.T) {
	dealFixture(t)
	p, err := loadProfile("deal")
	require.NoError(t, err)
	p.LogID = dealLogID("deal-0000000000000000")
	require.NotPanics(t, func() {
		_, err = openTarget(t.Context(), p, useCLLRead)
	})
	require.Error(t, err)
}
