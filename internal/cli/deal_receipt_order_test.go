package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The same words again are not a change of what was asked; different words
// are.
func TestRestatedIntentIsNotAChange(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", filepath.Join(otterFixture, "open.json"))["deal_id"].(string)
	opening := "buy me a sticker for less than 8 bucks about otters that is funny"
	dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"`+opening+`"}`))
	dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"the Otterly Chaos one"}`))
	trail := dealRun(t, "report", "--deal", id)["trail"].(string)
	assert.Contains(t, trail, `you said again what you asked, unchanged: "`+opening+`"`)
	assert.Contains(t, trail, `you changed what you asked: "the Otterly Chaos one"`)
	assert.Equal(t, 1, strings.Count(trail, "you changed what you asked"))
}

// Two standing-intent cycles in the same words are told apart by when they
// were sealed.
func TestRepeatedCyclesCarryTheirTimes(t *testing.T) {
	dealFixture(t)
	now := clockAt(t, time.Date(2026, 10, 4, 18, 24, 0, 0, time.UTC))
	id := retailDeal(t)
	*now = now.Add(13 * time.Minute)
	dealRun(t, "check", "--deal", id, "--input", filepath.Join(retailDemo, "check-pay.json"))
	var checks []map[string]any
	for _, d := range dealRun(t, "report", "--deal", id)["did"].([]any) {
		if item := d.(map[string]any); item["kind"] == "check" {
			checks = append(checks, item)
		}
	}
	require.Len(t, checks, 2)
	assert.Equal(t, checks[0]["text"], checks[1]["text"], "the same words")
	assert.Equal(t, "2026-10-04T18:24:00Z", checks[0]["at"])
	assert.Equal(t, "2026-10-04T18:37:00Z", checks[1]["at"])
}

// What the user's opening sentence named (dates, place, fare) reads as
// theirs in the report; a flight number they never said is the agent's. The
// check itself keeps every attribute the agent picked: the report re-reads
// it, the check's verdict is untouched.
func TestAttributesNamedInTheOpeningAreTheUsers(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"book me Southwest HOU to SJC Oct 21 to Oct 24, Basic fare","allowed":["pay"]},
		"who":{"name":"Southwest Airlines","domain":"southwest.example"},
		"terms":{"item":"WN 1234","when":"Oct 21 to Oct 24","place":"HOU/SJC","price_minor":55880,"currency":"USD","conditions":{"fare":"Basic"}},
		"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":55880,"terms":{"item":"WN 1234","when":"Oct 21 to Oct 24","place":"HOU/SJC","price_minor":55880,"conditions":{"fare":"Basic"}},"recourse":{"rail":"card","refundable":true}}`))
	var picked []string
	for _, a := range check["picked_by_agent"].([]any) {
		picked = append(picked, a.(map[string]any)["field"].(string))
	}
	assert.ElementsMatch(t, []string{"item", "when", "place", "conditions.fare"}, picked, "the check keeps what it judged")
	var text string
	for _, d := range dealRun(t, "report", "--deal", id)["did"].([]any) {
		if item := d.(map[string]any); item["kind"] == "check" {
			text = item["text"].(string)
		}
	}
	assert.Contains(t, text, "you asked for: dates Oct 21 to Oct 24 · place HOU/SJC · fare Basic")
	assert.Contains(t, text, "the agent picked, not you: item WN 1234")
}

// A number the agent picked is never made the user's by a digit that only
// happens to be in their words: the opening says "Oct 2", the agent picked
// quantity 2, and the check still pauses for the user's nod.
func TestADigitInADateDoesNotSettleAMaterialPick(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"get me tickets for the Oct 2 show","allowed":["pay"]},
		"who":{"name":"Example Tickets","domain":"tickets.example"},
		"terms":{"item":"show ticket","when":"Oct 2","price_minor":9000,"currency":"USD"},
		"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":9000,"terms":{"item":"show ticket","quantity":2,"when":"Oct 2","price_minor":9000},"recourse":{"rail":"card","refundable":true}}`))
	assert.Equal(t, "pause", check["verdict"])
	assert.Equal(t, false, check["proceed"])
	assert.Contains(t, check["card"], "I picked quantity 2")
}

// Facts before epistemics: on the page, what was asked, what was done and
// where the deal stands come before what the receipt can prove and how to
// check it; in the email, what the agent did comes before the assurance.
func TestReceiptPutsFactsFirst(t *testing.T) {
	page, err := os.ReadFile("assets/deal-view.js")
	require.NoError(t, err)
	order := []string{`el("h2", "What you asked")`, `el("h2", "What the agent did")`, `el("h2", "Where this deal stands")`, `host.append(header, provenance)`, `el("h2", "What your agent told whom")`}
	last := -1
	for _, marker := range order {
		at := strings.Index(string(page), marker)
		require.GreaterOrEqual(t, at, 0, marker)
		assert.Greater(t, at, last, "%s comes after the one before it", marker)
		last = at
	}

	dealFixture(t)
	id := retailDeal(t)
	email := filepath.Join(t.TempDir(), "receipt.eml")
	dealRun(t, "report", "--deal", id, "--email", email)
	raw, err := os.ReadFile(email)
	require.NoError(t, err)
	text := string(raw)
	did, sealed := strings.Index(text, "What the agent did:"), strings.Index(text, "Sealed by my agent")
	require.Greater(t, did, 0)
	require.Greater(t, sealed, 0)
	assert.Less(t, did, sealed, "what the agent did comes before the assurance")
}

// A deal witnessed in part says, in the user's terms, what the witnessed
// steps hold and what is still witness-pending: the payment is witnessed,
// the cancellation sealed after the tick is not yet.
func TestPartlyWitnessedCancelNamesWhatIsCovered(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	endpoint, _ := countingWitness(t)
	p := cadenceFixture(t, endpoint, public, "1h", "0s", 0)
	now := clockAt(t, time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC))
	id := retailDeal(t)
	require.Equal(t, "ticked", dealRun(t, "tick")["state"])
	deliver(t, p, cadenceSize(t, p), key)

	*now = now.Add(10 * time.Minute)
	dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"cancel this order please","allowed":["pay","cancel"]}`))
	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"cancel","amount_minor":627,"recourse":{"rail":"card","refundable":true}}`))
	if check["verdict"] != "pass" {
		dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", "yes")
	}
	dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"cancel","amount_minor":627,"currency":"USD","rail":"card"}`))

	html := filepath.Join(t.TempDir(), "part.html")
	assurance := dealRun(t, "report", "--deal", id, "--html", html)["assurance"].(map[string]any)
	require.Equal(t, "witnessed_in_part", assurance["rung"])
	k := int(assurance["steps_witnessed"].(float64))
	assert.Contains(t, assurance["text"], "Witnessed, steps 1 to "+strconv.Itoa(k)+": pay $6.27 by card.")
	assert.Contains(t, assurance["text"], "Witness pending, steps "+strconv.Itoa(k+1)+" to ")
	assert.Contains(t, assurance["text"], ": cancel: $6.27 back to you on the card, reversing the payment.")
	page, err := os.ReadFile(html)
	require.NoError(t, err)
	assert.Contains(t, string(page), `"witness_coverage"`)
	assert.Contains(t, string(page), `"pending_acts":"cancel: $6.27 back to you on the card, reversing the payment"`)
}
