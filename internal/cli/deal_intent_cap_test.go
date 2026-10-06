package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An intent note is written by the agent. A later one may narrow what the
// user set (a lower limit, fewer allowed actions) but never widen it: the
// user said "less than 8 bucks" (max_total_minor 800, allowed pay), and a
// later note with a higher limit or more actions must not let a 3000
// purchase or a commitment through on its own. Going over stays the user's
// call one step at a time: the check pauses, and their sealed approval
// covers that step.
func TestDealLaterIntentCannotRaiseTheLimit(t *testing.T) {
	const item = "Otterly Chaos - Unsupervised and Thriving Funny Otter Design Sticker"
	asked := `"asked":{"item":"` + item + `","conditions":{"size":"small 3.8in x 2.4in"}}`
	// The price is already agreed at 3000, so the limit is the only thing a
	// 3000 payment can differ on.
	open := func(t *testing.T) string {
		dealFixture(t)
		return dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
			"intent":{"verbatim":"buy me a sticker for less than 8 bucks about otters that is funny",`+asked+`,"max_total_minor":800,"allowed":["pay"]},
			"who":{"name":"Sticker Marketplace","domain":"stickers.example"},
			"terms":{"item":"`+item+`","price_minor":3000,"currency":"USD","conditions":{"size":"small 3.8in x 2.4in"}},
			"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	}
	intent := func(t *testing.T, dealID, rest string) {
		dealRun(t, "note", "--deal", dealID, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"the Otterly Chaos one",`+asked+rest+`}`))
	}
	check := func(t *testing.T, dealID, action string, amount int) map[string]any {
		body := `{"action":"` + action + `","terms":{"item":"` + item + `","price_minor":` + strconv.Itoa(amount) + `,"conditions":{"size":"small 3.8in x 2.4in"}},"recourse":{"rail":"card","refundable":true}`
		if action == "pay" {
			body += `,"amount_minor":` + strconv.Itoa(amount)
		}
		return dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, body+`}`))
	}
	paused := func(t *testing.T, c map[string]any, card string) {
		t.Helper()
		assert.Equal(t, "pause", c["verdict"])
		assert.Equal(t, false, c["proceed"])
		assert.Nil(t, c["approval_id"], "no standing approval for a paused check")
		assert.Contains(t, c["card"], card)
	}

	t.Run("a higher limit in a later note does not raise it", func(t *testing.T) {
		id := open(t)
		note := dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"the Otterly Chaos one",`+asked+`,"max_total_minor":5000,"allowed":["pay"]}`))
		// The note is sealed as a proposal, and says so.
		assert.Equal(t, map[string]any{"max_total_minor": float64(5000), "allowed": []any{"pay"}}, note["proposed"])
		assert.Equal(t, map[string]any{"max_total_minor": float64(800), "allowed": []any{"pay"}}, note["in_force"])
		assert.Contains(t, note["next"], "--choice confirm_limits")
		c := check(t, id, "pay", 3000)
		paused(t, c, "Over your limit of $8.00 ($30.00)")
		// The user can still say yes to this one step: their sealed approval
		// covers it, as for any paused check.
		approval := dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", c["check_id"].(string), "--choice", "proceed", "--said", "yes, 30 is fine for this one")
		require.NotEmpty(t, approval["capsule_id"])
		act := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":3000}`))
		assert.Equal(t, approval["capsule_id"], act["authorized_by"])
	})
	t.Run("a note without a limit keeps the user's", func(t *testing.T) {
		id := open(t)
		intent(t, id, `,"allowed":["pay"]`)
		paused(t, check(t, id, "pay", 3000), "Over your limit of $8.00")
	})
	t.Run("more actions in a later note do not widen what was allowed", func(t *testing.T) {
		id := open(t)
		intent(t, id, `,"max_total_minor":5000,"allowed":["pay","commit"]`)
		paused(t, check(t, id, "commit", 300), "You didn't ask for this: confirming a commitment")
	})
	t.Run("a note without a list keeps the user's", func(t *testing.T) {
		id := open(t)
		intent(t, id, `,"max_total_minor":800`)
		paused(t, check(t, id, "commit", 300), "You didn't ask for this: confirming a commitment")
	})
	t.Run("a lower limit and fewer actions do apply", func(t *testing.T) {
		id := open(t)
		intent(t, id, `,"max_total_minor":200,"allowed":["pay"]`)
		paused(t, check(t, id, "pay", 300), "Over your limit of $2.00 ($3.00)")
		intent(t, id, `,"max_total_minor":800,"allowed":[]`)
		c := check(t, id, "pay", 100)
		paused(t, c, "You didn't ask for this: paying")
		assert.NotContains(t, c["card"], "$8.00", "a later note cannot raise the lowered limit back either")
	})
	t.Run("the user's confirmation seals a new version of the limits", func(t *testing.T) {
		id := open(t)
		note := dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"ok, up to 50 dollars",`+asked+`,"max_total_minor":5000}`))
		paused(t, check(t, id, "pay", 3000), "Over your limit of $8.00")
		confirmed := dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", note["capsule_id"].(string), "--choice", "confirm_limits", "--said", "yes, 50 is my new limit")
		assert.Equal(t, true, confirmed["proceed"])
		assert.Equal(t, map[string]any{
			"previous": map[string]any{"max_total_minor": float64(800), "allowed": []any{"pay"}},
			"new":      map[string]any{"max_total_minor": float64(5000), "allowed": []any{"pay"}},
		}, confirmed["limits"], "the confirmation names the version it replaces and the new one")
		c := check(t, id, "pay", 3000)
		assert.Equal(t, "pass", c["verdict"], c["card"])
		paused(t, check(t, id, "pay", 6000), "Over your limit of $50.00")
		// Confirmed once: a second answer is sealed but changes nothing.
		again := dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", note["capsule_id"].(string), "--choice", "confirm_limits", "--said", "yes")
		assert.Equal(t, false, again["proceed"])
		assert.Equal(t, "this proposal was already confirmed", again["reason"])
		// The previous version stays readable: in the trail, and in the
		// exported records the profile checker accepts.
		report := dealRun(t, "report", "--deal", id)
		assert.Contains(t, report["trail"], "you confirmed new limits: limit 800 (minor units), pay → limit 5000 (minor units), pay")
		export := filepath.Join(t.TempDir(), "deal.json")
		dealRun(t, "export", "--deal", id, "--output", export)
		records, err := os.ReadFile(export)
		require.NoError(t, err)
		assert.Contains(t, string(records), `"choice":"confirm_limits","limits":{"new":{"allowed":["pay"],"max_total_minor":5000},"previous":{"allowed":["pay"],"max_total_minor":800}}`)
		if python, err := exec.LookPath("python3"); err == nil {
			result, err := exec.Command(python, dealProfileDir+"/check_profile.py", export).CombinedOutput()
			require.NoError(t, err, string(result))
			assert.Contains(t, string(result), "ALL OK")
		}
	})
	t.Run("only an intent that asks for more can be confirmed, and only by confirm_limits", func(t *testing.T) {
		id := open(t)
		lower := dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"keep it under 5",`+asked+`,"max_total_minor":500}`))
		_, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", id, "--kind", "approval", "--check", lower["capsule_id"].(string), "--choice", "confirm_limits", "--said", "yes")
		require.ErrorIs(t, err, ErrInput, "nothing to confirm")
		more := dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"fine, 50",`+asked+`,"max_total_minor":5000}`))
		_, err = invoke(t, "", "--profile", "deal", "deal", "note", "--deal", id, "--kind", "approval", "--check", more["capsule_id"].(string), "--choice", "proceed", "--said", "yes")
		require.ErrorIs(t, err, ErrInput, "an intent is answered only with confirm_limits")
		c := check(t, id, "pay", 300)
		_, err = invoke(t, "", "--profile", "deal", "deal", "note", "--deal", id, "--kind", "approval", "--check", c["check_id"].(string), "--choice", "confirm_limits", "--said", "yes")
		require.ErrorIs(t, err, ErrInput, "a check is not answered with confirm_limits")
		// A later intent replaces an unconfirmed proposal.
		dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"actually 40",`+asked+`,"max_total_minor":4000}`))
		stale := dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", more["capsule_id"].(string), "--choice", "confirm_limits", "--said", "yes")
		assert.Equal(t, false, stale["proceed"])
		paused(t, check(t, id, "pay", 3000), "Over your limit of $5.00")
	})
}
