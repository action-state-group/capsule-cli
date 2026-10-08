package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// phoneDeal is a sticker deal whose counterparty gave a phone number.
func phoneDeal(t *testing.T) string {
	t.Helper()
	dealFixture(t)
	return dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"buy me a funny otter sticker for at most 50 dollars","max_total_minor":5000,"allowed":["pay"]},
		"who":{"name":"Sticker Marketplace","domain":"stickers.example","phone":"+1 415 555 0199"},
		"terms":{"item":"otter sticker","price_minor":454,"currency":"USD"},
		"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
}

// checkWithFinding runs a check whose pinned checker allows with one
// finding carrying limit and value as given (raw JSON).
func checkWithFinding(t *testing.T, id, limit, value string) map[string]any {
	t.Helper()
	finding := `{"id":"purchase-over-limit","check":"caps/5.0.0","verdict":"pass","limit":` + limit + `,"value":` + value + `}`
	pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules", checkerPrints("allow", finding))}})
	return dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 480)))
}

// A checker answer that cannot be sealed never fails the check: the check
// pauses, saying the rules were not checked and why, and that is sealed
// (cause unreadable). That covers the value a real checker printed on a
// second purchase in a week, and every phone shape the personal-data scan
// refuses, which it still refuses.
func TestAnUnsealableCheckerAnswerPauses(t *testing.T) {
	for name, value := range map[string]string{
		"a week total":           "2000 authorised (capture 2000); 7d total 4000 (2000 earlier)",
		"a UK number":            "+44 20 7946 0958",
		"a German number":        "+49 30 12345678",
		"a short number":         "555 0123",
		"a split number":         "415 5550123",
		"a national number":      "020 7946 0958",
		"a call":                 "call 30123456",
		"the counterparty's own": "+1 415 555 0199",
		"an email address":       "ask orders@shop.example",
	} {
		t.Run(name, func(t *testing.T) {
			id := phoneDeal(t)
			check := checkWithFinding(t, id, `"2500 USD minor per purchase"`, `"`+value+`"`)
			assert.Equal(t, "pause", check["verdict"], "a pause, not a failed check")
			rules := check["rules"].(map[string]any)
			assert.Equal(t, "not_evaluated", rules["status"])
			assert.Equal(t, "unreadable", rules["cause"])
			assert.Contains(t, approvalText(check), "Your rules were not checked: the rules checker's answer could not be sealed (")
			assert.NotContains(t, check["card"], "anyway", "no rule found anything: Pay, not Pay anyway")
			records, export := exportRecords(t, id)
			for _, r := range records {
				if r["x-deal-v0"].(map[string]any)["record_type"] == "verdict" {
					sealed := r["body"].(map[string]any)["rules"].(map[string]any)
					assert.Equal(t, "unreadable", sealed["cause"])
					assert.Nil(t, sealed["findings"], "nothing the checker said is sealed")
				}
			}
			checkProfile(t, export)
		})
	}
}

// Numeric limits and values are never read as text: they seal as given.
func TestNumericLimitAndValueSeal(t *testing.T) {
	id := phoneDeal(t)
	check := checkWithFinding(t, id, `10000`, `4000`)
	rules := check["rules"].(map[string]any)
	assert.Equal(t, "evaluated", rules["status"])
	records, export := exportRecords(t, id)
	for _, r := range records {
		if r["x-deal-v0"].(map[string]any)["record_type"] == "verdict" {
			f := r["body"].(map[string]any)["rules"].(map[string]any)["findings"].([]any)[0].(map[string]any)
			assert.Equal(t, float64(10000), f["limit"])
			assert.Equal(t, float64(4000), f["value"])
		}
	}
	checkProfile(t, export)
}
