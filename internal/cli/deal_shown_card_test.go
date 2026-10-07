package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dealRecords is each sealed step's x-deal-v0 record, by capsule_id.
func dealRecords(t *testing.T, dealID string) map[string]map[string]any {
	t.Helper()
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.NoError(t, s.useDeal(t.Context(), dealID, false))
	events, err := s.load(t.Context(), dealID)
	require.NoError(t, err)
	out := map[string]map[string]any{}
	for i, se := range events {
		raw, _, err := encodeDealRecord(se.Event, events[:i], s.dkey)
		require.NoError(t, err)
		var record map[string]any
		require.NoError(t, json.Unmarshal(raw, &record))
		out[se.CapsuleID] = record
	}
	return out
}

func shownCard(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "card.txt")
	require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
	return path
}

func answer(t *testing.T, dealID, check, said, cardFile string) (map[string]any, error) {
	t.Helper()
	args := []string{"--profile", "deal", "deal", "note", "--deal", dealID, "--kind", "approval", "--check", check, "--choice", "proceed"}
	if said != "" {
		args = append(args, "--said", said)
	}
	if cardFile != "" {
		args = append(args, "--shown-card", cardFile)
	}
	out, err := invoke(t, "", args...)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m), out)
	return m, nil
}

// An answer given on the card the check rendered commits to it under the
// check's own card nonce: its card_commitment equals the verdict's, so a
// verifier recomputes that what was shown is what was checked, without the
// text. The user's words and a card click can both carry it.
func TestDealApprovalCommitsToTheCardShown(t *testing.T) {
	for _, said := range []string{"yes, 30 is fine", ""} {
		dealFixture(t)
		id := openSticker(t, true)
		c := checkAct(t, id, "pay", 3000)
		card, _ := c["card"].(string)
		require.NotEmpty(t, card, "a paused check renders a card")
		// A file ending in a newline still holds exactly the card.
		approval, err := answer(t, id, c["check_id"].(string), said, shownCard(t, card+"\n"))
		require.NoError(t, err)

		records := dealRecords(t, id)
		verdict := records[c["check_id"].(string)]["body"].(map[string]any)
		body := records[approval["capsule_id"].(string)]["body"].(map[string]any)
		assert.Regexp(t, `^[0-9a-f]{64}$`, body["card_commitment"])
		assert.Equal(t, verdict["card_commitment"], body["card_commitment"], "the card shown is the card checked")
		want := map[bool]string{true: "user", false: "agent_card"}[said != ""]
		assert.Equal(t, want, body["approver"])

		if python := profilePython(t); python != "" {
			export := filepath.Join(t.TempDir(), "deal.json")
			dealRun(t, "export", "--deal", id, "--output", export)
			result, err := exec.Command(python, dealProfileDir+"/check_profile.py", export).CombinedOutput()
			require.NoError(t, err, string(result))
			assert.Contains(t, string(result), "ALL OK")
		}
	}
}

// A card other than the one the check rendered is refused, and nothing is
// sealed: a card_commitment must never differ from the verdict's.
func TestDealApprovalOnAnotherCardIsRefused(t *testing.T) {
	dealFixture(t)
	id := openSticker(t, true)
	c := checkAct(t, id, "pay", 3000)
	before := len(dealRecords(t, id))
	_, err := answer(t, id, c["check_id"].(string), "yes", shownCard(t, "Pay $30.00 to Sticker Marketplace: all checks passed."))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "the card shown is not the card checked")
	assert.Len(t, dealRecords(t, id), before, "nothing sealed")
}

// confirm_limits answers an intent, not a check's card.
func TestDealConfirmLimitsTakesNoShownCard(t *testing.T) {
	dealFixture(t)
	id := openSticker(t, true)
	note := dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"up to 50","max_total_minor":5000,"allowed":["pay"]}`))
	_, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", id, "--kind", "approval", "--check", note["capsule_id"].(string),
		"--choice", "confirm_limits", "--said", "yes", "--shown-card", shownCard(t, "anything"))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "--shown-card goes with an answer to a check's card")
}
