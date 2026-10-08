package cli

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The golden task-authority record a rules checker binds a plan to: a real
// sealed typed task-authority record (synthetic deal), its exact JCS bytes,
// and the task_authority_ref that names it. A rules engine tests against the
// same bytes, so a drift on either side fails.
const taskAuthorityGoldenDir = "testdata/task-authority-golden"

// taskAuthorityGolden is what re-derives the record: the steps sealed before
// it and its own step, and the deal's key.
type taskAuthorityGolden struct {
	DealKey string        `json:"deal_key_hex"`
	Events  []sealedEvent `json:"events"`
}

func TestRegenerateTaskAuthorityGolden(t *testing.T) {
	if os.Getenv("CAPSULECTL_REGEN_TASK_AUTHORITY_GOLDEN") == "" {
		t.Skip("set CAPSULECTL_REGEN_TASK_AUTHORITY_GOLDEN=1 to regenerate the golden task-authority record")
	}
	id := stickerDeal(t, "card", true)
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	require.NoError(t, s.useDeal(t.Context(), id, false))
	events, err := s.load(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "task_authority", events[1].Event.Kind)
	record, digest, err := encodeDealRecord(events[1].Event, events[:1], s.dkey)
	require.NoError(t, err)
	require.Equal(t, events[1].Digest, digest)

	require.NoError(t, os.MkdirAll(taskAuthorityGoldenDir, 0o755))
	inputs, err := json.MarshalIndent(taskAuthorityGolden{DealKey: hex.EncodeToString(s.dkey), Events: events[:2]}, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(taskAuthorityGoldenDir, "inputs.json"), append(inputs, '\n'), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(taskAuthorityGoldenDir, "task-authority-record.json"), record, 0o644))
	ref, err := json.Marshal(typedRef(digest))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(taskAuthorityGoldenDir, "task_authority_ref.json"), append(ref, '\n'), 0o644))
}

// The golden record re-derives to the same bytes; SHA-256 over those JCS
// bytes is the ref's digest; and the plan is at body.
func TestTheTaskAuthorityGoldenHolds(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(taskAuthorityGoldenDir, "inputs.json"))
	require.NoError(t, err)
	var golden taskAuthorityGolden
	require.NoError(t, json.Unmarshal(raw, &golden))
	key, err := hex.DecodeString(golden.DealKey)
	require.NoError(t, err)
	record, err := os.ReadFile(filepath.Join(taskAuthorityGoldenDir, "task-authority-record.json"))
	require.NoError(t, err)
	var ref map[string]string
	raw, err = os.ReadFile(filepath.Join(taskAuthorityGoldenDir, "task_authority_ref.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &ref))

	payload, digest, err := encodeDealRecord(golden.Events[1].Event, golden.Events[:1], key)
	require.NoError(t, err)
	assert.Equal(t, string(record), string(payload), "the record re-derives to the golden bytes")
	assert.Equal(t, ref["digest"], digest)
	assert.Equal(t, map[string]string{"type": typedRecordRef, "digest_alg": "SHA-256", "digest": golden.Events[1].Digest}, ref)

	// What a rules engine does: SHA-256 over the record's JCS bytes, then
	// the plan at body.
	decoder := json.NewDecoder(bytes.NewReader(record))
	decoder.UseNumber()
	var doc map[string]any
	require.NoError(t, decoder.Decode(&doc))
	jcs, err := canonical.JCS(doc)
	require.NoError(t, err)
	assert.Equal(t, string(record), string(jcs), "the golden file is the JCS bytes themselves")
	recomputed, err := canonical.JSONDigest(doc)
	require.NoError(t, err)
	assert.Equal(t, ref["digest"], recomputed)
	assert.Equal(t, "task-authority/v0", doc["type"])
	plan := doc["body"].(map[string]any)
	assert.Equal(t, "capsulectl.deal.purchase/1.0.0", plan["outcome_id"])
	assert.Equal(t, []any{"pay"}, plan["allowed_actions"])
	assert.Equal(t, []any{}, plan["preconditions"])
}
