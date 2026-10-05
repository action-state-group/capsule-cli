package cli

import (
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
		intent(t, id, `,"max_total_minor":5000,"allowed":["pay"]`)
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
}
