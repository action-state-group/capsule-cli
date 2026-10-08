package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func checkWithValue(t *testing.T, id, value string) map[string]any {
	t.Helper()
	finding := `{"id":"purchase-over-limit","check":"caps/5.0.0","verdict":"pass","limit":"2500 USD minor per purchase; 10000 per 7d","value":"` + value + `"}`
	pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules", checkerPrints("allow", finding))}})
	return dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 480)))
}

// The value a real checker printed on a second purchase in a week seals:
// neighbouring amounts are not a phone number.
func TestAWeekTotalValueSeals(t *testing.T) {
	id := phoneDeal(t)
	value := "2000 authorised (capture 2000); 7d total 4000 (2000 earlier)"
	check := checkWithValue(t, id, value)
	rules := check["rules"].(map[string]any)
	assert.Equal(t, "evaluated", rules["status"])
	assert.Equal(t, value, rules["findings"].([]any)[0].(map[string]any)["value"])
	records, export := exportRecords(t, id)
	var sealed string
	for _, r := range records {
		if r["x-deal-v0"].(map[string]any)["record_type"] == "verdict" {
			sealed = r["body"].(map[string]any)["rules"].(map[string]any)["findings"].([]any)[0].(map[string]any)["value"].(string)
		}
	}
	assert.Equal(t, value, sealed)
	checkProfile(t, export)

	// The profile checker reads it the same way: the sealed value passes,
	// and the same record carrying a phone number there does not.
	python := profilePython(t)
	if python == "" {
		return
	}
	raw, err := os.ReadFile(export)
	require.NoError(t, err)
	edited := filepath.Join(t.TempDir(), "edited.json")
	require.NoError(t, os.WriteFile(edited, []byte(strings.Replace(string(raw), value, "call 415-555-0123 first", 1)), 0o600))
	out, err := exec.Command(python, dealProfileDir+"/check_profile.py", edited).CombinedOutput()
	require.Error(t, err, string(out))
	assert.Contains(t, string(out), "raw phone number at $.body.rules.findings[0].value")
}

// A checker answer that cannot be sealed never fails the check: the check
// pauses, saying the rules were not checked and why, and that is sealed. A
// phone-shaped token, an email address, or the counterparty's own number
// written any way is still refused.
func TestAnUnsealableCheckerAnswerPauses(t *testing.T) {
	for name, value := range map[string]string{
		"a phone number":                   "call 415-555-0123 first",
		"a short phone number":             "555-0123",
		"a phone number with a plus":       "+14155550123",
		"a long run of digits":             "4155550123",
		"the counterparty's number, split": "ref 4155 550 199",
		"an email address":                 "ask orders@shop.example",
	} {
		t.Run(name, func(t *testing.T) {
			id := phoneDeal(t)
			check := checkWithValue(t, id, value)
			assert.Equal(t, "pause", check["verdict"])
			rules := check["rules"].(map[string]any)
			assert.Equal(t, "not_evaluated", rules["status"])
			assert.Equal(t, "unsealable", rules["cause"])
			assert.Contains(t, approvalText(check), "Your rules were not checked: the rules checker's answer could not be sealed (")
			assert.NotContains(t, check["card"], "anyway", "no rule found anything: Pay, not Pay anyway")
			records, export := exportRecords(t, id)
			for _, r := range records {
				if r["x-deal-v0"].(map[string]any)["record_type"] == "verdict" {
					assert.Equal(t, "unsealable", r["body"].(map[string]any)["rules"].(map[string]any)["cause"])
				}
			}
			checkProfile(t, export)
		})
	}
}

func TestScanCheckerScalar(t *testing.T) {
	local := []string{"+1 415 555 0199"}
	for _, ok := range []string{
		"2000 authorised (capture 2000); 7d total 4000 (2000 earlier)",
		"954 authorised (capture 454); 7d total 454 (no earlier spend)",
		"2500 USD minor per purchase; 10000 per 7d",
		"55880 authorised (capture 55880); 7d total 55880",
		"123456789",
		"45.40 USD",
	} {
		require.NoError(t, scanCheckerScalar(ok, local), ok)
	}
	for _, bad := range []string{"415-555-0123", "(415) 555-0123", "415 555 0123", "555-0123", "+14155550123", "4155550123", "415.555.0123", "x 4155550199 y", "4155 550 199"} {
		assert.Error(t, scanCheckerScalar(bad, local), bad)
	}
}
