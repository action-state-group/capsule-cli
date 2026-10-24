package deal

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/action-state-group/capsule-cli/cli"
	"github.com/action-state-group/evidencebook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests that keep what capsulectl-book and capsulectl-deal shared before the
// plugins split (capsule-cli#326) checked from this side.

// --policy is the evidence book's share policy: `deal respond` refuses it
// before reading anything, whatever the file holds.
func TestDealRespondRefusesAPolicy(t *testing.T) {
	dealFixture(t)
	out := filepath.Join(t.TempDir(), "reply.json")
	_, err := invoke(t, "", "--profile", "deal", "deal", "respond", "--request", writeJSONFile(t, "req.json", map[string]any{}),
		"--requester", "x", "--output", out, "--policy", filepath.Join(t.TempDir(), "no-such-policy.json"))
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Contains(t, cli.SafeError(err), "--policy is the evidence book's share policy")
	assert.NoFileExists(t, out)
}

// The fields a backfill row shares with deal-execution-record/v0 pass the
// execution record's own checks (capsulectl-book's
// TestBackfillSharesDealExecutionFieldsButNotAmount checks the row's).
func TestDealExecutionRecordTakesTheBackfillSharedFields(t *testing.T) {
	shared := `{"id":"sa-1","at":"2026-09-27T18:00:40Z","task":"agent-worker","parent_task":"agent-main","tool":"spend_request","merchant_domain":"www.stickers.example","currency":"usd"`
	exec := shared + `,"action":"pay","observed":"approval","amount_minor":1100,"amount_kind":"ceiling","status":"unknown"}`
	var rec dealExecutionRecord
	require.NoError(t, cli.DecodeJSON([]byte(exec), &rec))
	require.NoError(t, rec.validate())
}

// capsulectl-book's TestResultCommandsAndTheDealReconcileContract reads a
// Result `deal terms-result` wrote (its testdata/deal-reconcile/result.json).
// This holds terms-result to the shape that copy has: the reconcile claims,
// by id, under ec:deal-reconcile@1. When this changes, regenerate that file
// from a fresh terms-result (closedPair, then runTermsResult's --out).
func TestTermsResultKeepsTheShapeTheBookFixtureCopies(t *testing.T) {
	pair := closedPair(t, received, received)
	_, _, resultPath, err := runTermsResult(t, pair.id, pair.response, "--openings", pair.sellerFile)
	require.NoError(t, err)
	var doc struct {
		Claims []struct {
			ID          string `json:"id"`
			ContractRef string `json:"contract_ref"`
		} `json:"claims"`
	}
	require.NoError(t, json.Unmarshal(mustRead(t, resultPath), &doc))
	var ids []string
	for _, c := range doc.Claims {
		ids = append(ids, c.ID)
		assert.Equal(t, evidencebook.DealReconcileContractRef, c.ContractRef)
	}
	assert.Equal(t, []string{"reconcile-amount", "reconcile-currency"}, ids)
	assert.Equal(t, "ec:deal-reconcile@1", evidencebook.DealReconcileContractRef)
}

// The book's own refusals stay input errors in deal's copy of bookError, as
// in capsulectl-book's.
func TestDealBookErrorKeepsTheBooksRefusalsInputErrors(t *testing.T) {
	for _, base := range []error{evidencebook.ErrInvalid, evidencebook.ErrNotFound} {
		err := bookError(fmt.Errorf("append: %w", base))
		assert.ErrorIs(t, err, cli.ErrInput)
		assert.ErrorIs(t, err, base)
	}
	other := errors.New("disk full")
	assert.Equal(t, other, bookError(other))
	assert.NoError(t, bookError(nil))
}
