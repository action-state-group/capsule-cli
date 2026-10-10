package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// closeBodies are the bodies of a deal's last outcome and close records.
func closeBodies(t *testing.T, id string) (outcome, closing map[string]any) {
	t.Helper()
	for _, r := range records(t, id) {
		kind := typeOf(r)
		if b, ok := r["x-deal-v0"].(map[string]any); ok {
			kind = b["record_type"].(string)
		}
		switch kind {
		case "outcome", typeActionOutcome:
			outcome = r["body"].(map[string]any)
		case "close":
			closing = r["body"].(map[string]any)
		}
	}
	require.NotNil(t, outcome)
	require.NotNil(t, closing)
	return outcome, closing
}

func closeDealWith(t *testing.T, id, input string) map[string]any {
	t.Helper()
	return dealRun(t, "close", "--deal", id, "--input", writeJSON(t, input))
}

func didTexts(t *testing.T, id string) []string {
	t.Helper()
	report, _ := ownReportAndChain(t, id)
	return reportTexts(t, report, "did")
}

// A deal the user closed and one the agent abandoned seal different actors
// and reasons, on the outcome and the close alike, and the report says who
// ended each and why, as an actor-marked line.
func TestAUserCloseAndAnAgentAbandonSealDifferentActorsAndReasons(t *testing.T) {
	dealFixture(t)
	byUser := dealRun(t, "open", "--input", writeJSON(t, stickerOpen))["deal_id"].(string)
	out := closeDealWith(t, byUser, `{"status":"not_received","reason":"user_closed"}`)
	assert.Equal(t, "user", out["actor"], "user_closed is the user's")
	assert.Equal(t, "user_closed", out["reason"])

	byAgent := dealRun(t, "open", "--input", writeJSON(t, stickerOpen))["deal_id"].(string)
	out = closeDealWith(t, byAgent, `{"status":"not_received","reason":"agent_could_not_complete"}`)
	assert.Equal(t, "agent", out["actor"])

	for id, want := range map[string][2]string{byUser: {"user", "user_closed"}, byAgent: {"agent", "agent_could_not_complete"}} {
		outcome, closing := closeBodies(t, id)
		for _, b := range []map[string]any{outcome, closing} {
			assert.Equal(t, want[0], b["actor"])
			assert.Equal(t, want[1], b["reason"])
		}
		_, export := exportRecords(t, id)
		checkProfile(t, export)
	}
	assert.Contains(t, didTexts(t, byUser), "close: You closed this deal")
	assert.Contains(t, didTexts(t, byAgent), "close: The agent could not complete this deal")
}

// Where the status says why, the reason follows from it; the actor is the
// agent, which runs the close, unless stated.
func TestACloseReasonFollowsFromItsStatus(t *testing.T) {
	dealFixture(t)
	open := func() string { return dealRun(t, "open", "--input", writeJSON(t, stickerOpen))["deal_id"].(string) }
	for _, c := range []struct{ input, actor, reason, line string }{
		{`{"status":"received","delivered":{"item":"otter sticker","price_minor":454}}`, "agent", "completed", "The agent closed this deal: completed as agreed"},
		{`{"status":"received","delivered":{"item":"beaver sticker","price_minor":454}}`, "agent", "delivered_mismatch", "The agent closed this deal: what was delivered differs from what was agreed"},
		{`{"status":"received","actor":"user","delivered":{"item":"otter sticker","price_minor":454}}`, "user", "completed", "You closed this deal: completed as agreed"},
		{`{"status":"not_selected"}`, "agent", "not_selected", "The agent closed this deal: the other side was not chosen"},
		{`{"status":"not_received","reason":"payment_failed","actor":"platform"}`, "platform", "payment_failed", "The platform closed this deal: the payment failed"},
	} {
		id := open()
		out := closeDealWith(t, id, c.input)
		assert.Equal(t, c.actor, out["actor"], c.input)
		assert.Equal(t, c.reason, out["reason"], c.input)
		assert.Contains(t, didTexts(t, id), "close: "+c.line, c.input)
	}
	pending := open()
	out := closeDealWith(t, pending, `{"status":"pending"}`)
	assert.Equal(t, "awaiting_delivery", out["reason"])
	_, export := exportRecords(t, pending)
	checkProfile(t, export)
}

// A reason or an actor outside the closed sets, a missing reason where the
// status cannot say it, and a reason or actor the close contradicts are
// refused, and nothing is sealed.
func TestACloseActorAndReasonAreFromTheirSetsAndAgree(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, stickerOpen))["deal_id"].(string)
	before := len(chainSteps(t, id))
	for _, input := range []string{
		`{"status":"not_received"}`,
		`{"status":"not_received","reason":"the seller ghosted us"}`,
		`{"status":"not_received","reason":"completed"}`,
		`{"status":"received","reason":"payment_failed"}`,
		`{"status":"pending","reason":"user_closed"}`,
		`{"status":"not_received","reason":"user_closed","actor":"agent"}`,
		`{"status":"not_received","reason":"agent_could_not_complete","actor":"user"}`,
		`{"status":"not_received","reason":"other","actor":"merchant"}`,
	} {
		_, err := invoke(t, "", "--profile", "deal", "deal", "close", "--deal", id, "--input", writeJSON(t, input))
		require.ErrorIs(t, err, ErrInput, input)
	}
	assert.Len(t, chainSteps(t, id), before, "nothing sealed")
}

// A close sealed before closes carried an actor and a reason re-derives
// unchanged, with neither, and reads as it did.
func TestACloseSealedBeforeActorsReDerivesUnchanged(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, stickerOpen))["deal_id"].(string)
	closeDealWith(t, id, `{"status":"not_received","reason":"user_closed"}`)
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.NoError(t, s.useDeal(t.Context(), id, false))
	events, err := s.load(t.Context(), id)
	require.NoError(t, err)
	seen := 0
	for i, se := range events {
		ev := se.Event
		var c *dealCloseResult
		switch ev.Kind {
		case "outcome":
			o := *ev.Outcome
			c, ev.Outcome = &o, &o
		case "close":
			o := *ev.Close
			c, ev.Close = &o, &o
		default:
			continue
		}
		c.Actor, c.Reason = "", ""
		raw, _, err := encodeDealRecord(ev, events[:i], s.dkey)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), `"actor"`)
		assert.NotContains(t, string(raw), `"reason"`)
		if ev.Kind == "close" {
			assert.Equal(t, "Closed: mismatch", closeLine(*c))
		}
		seen++
	}
	assert.Equal(t, 2, seen)
}

// In typed records the outcome is an action-outcome/v0 carrying the same
// actor and reason.
func TestATypedOutcomeCarriesTheClosesActorAndReason(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, stickerOpen))["deal_id"].(string)
	closeDealWith(t, id, `{"status":"not_received","reason":"merchant_rejected"}`)
	var typed map[string]any
	for _, r := range records(t, id) {
		if typeOf(r) == typeActionOutcome {
			typed = r["body"].(map[string]any)
		}
	}
	require.NotNil(t, typed)
	assert.Equal(t, "agent", typed["actor"])
	assert.Equal(t, "merchant_rejected", typed["reason"])
	_, export := exportRecords(t, id)
	checkProfile(t, export)
}

// The profile checker holds a close to its outcome's actor and reason.
func TestTheProfileCheckerHoldsACloseToItsOutcomesReason(t *testing.T) {
	python := profilePython(t)
	if python == "" {
		return
	}
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, stickerOpen))["deal_id"].(string)
	closeDealWith(t, id, `{"status":"not_received","reason":"user_closed"}`)
	recs, _ := exportRecords(t, id)
	last := recs[len(recs)-1]
	require.Equal(t, "close", last["x-deal-v0"].(map[string]any)["record_type"])
	last["body"].(map[string]any)["reason"] = "not_delivered"
	last["body"].(map[string]any)["actor"] = "agent"
	raw, err := json.Marshal(recs)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "tampered.json")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	out, err := exec.Command(python, dealProfileDir+"/check_profile.py", path).CombinedOutput()
	require.Error(t, err, string(out))
	assert.Contains(t, string(out), "close actor differs from the referenced outcome's")
}

// The page shows who ended the deal and why.
func TestTheDealPageShowsWhoClosedTheDeal(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, stickerOpen))["deal_id"].(string)
	closeDealWith(t, id, `{"status":"not_received","reason":"agent_could_not_complete"}`)
	page := filepath.Join(t.TempDir(), "page.html")
	dealRun(t, "report", "--deal", id, "--html", page)
	assert.True(t, strings.Contains(string(mustRead(t, page)), "The agent could not complete this deal"))
}
