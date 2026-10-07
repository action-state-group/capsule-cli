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

// cancelAfterPurchase runs a purchase, then the user's "cancel this ticket"
// and an authorized cancel that returns the payment, and returns the deal id.
func cancelAfterPurchase(t *testing.T) string {
	t.Helper()
	dealFixture(t)
	const item = "Southwest WN 1234 HOU-SJC, Oct 21 to Oct 24, Basic"
	id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"book me Southwest HOU to SJC Oct 21 to Oct 24, Basic fare","asked":{"item":"`+item+`"},"allowed":["pay"]},
		"who":{"name":"Southwest Airlines","domain":"southwest.example"},
		"terms":{"item":"`+item+`","price_minor":55880,"currency":"USD"},
		"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	answer := func(check map[string]any, said string) {
		if check["verdict"] != "pass" {
			dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", said)
		}
	}
	answer(dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":55880,"authorized_max_minor":55880,"terms":{"item":"`+item+`","price_minor":55880},"recourse":{"rail":"card","refundable":true}}`)), "yes, book it")
	dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":55880,"currency":"USD","rail":"card"}`))
	dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"cancel this ticket for me please","allowed":["pay","cancel"]}`))
	answer(dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"cancel","description":"cancel the ticket","amount_minor":55880,"recourse":{"rail":"card","refundable":true}}`)), "yes, cancel it")
	act := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"cancel","amount_minor":55880,"currency":"USD","rail":"card"}`))
	require.Equal(t, false, act["unchecked"], "the cancel is authorized by the user's answer")
	return id
}

// A cancel after a purchase is a reversal: the receipt says the money came
// back, the money nets to zero, the deal is not open, and the cancel the user
// asked for and approved is no anomaly.
func TestCancelAfterPurchaseIsAReversal(t *testing.T) {
	id := cancelAfterPurchase(t)
	report := dealRun(t, "report", "--deal", id)
	var did []string
	for _, d := range report["did"].([]any) {
		did = append(did, d.(map[string]any)["text"].(string))
	}
	t.Logf("did: %q", did)
	t.Logf("anomalies: %v", report["anomalies"])
	assert.Contains(t, did, "Did: pay $558.80 by card")
	assert.Contains(t, did, "Did: cancel: $558.80 back to you on the card, reversing the payment")
	money, _ := report["money"].(map[string]any)
	require.NotNil(t, money, "the report states what the deal moved")
	assert.EqualValues(t, 55880, money["paid_minor"])
	assert.EqualValues(t, 55880, money["returned_minor"])
	assert.EqualValues(t, 0, money["net_minor"], "a pay and its reversal net to zero, not 1117.60")
	assert.Equal(t, "cancelled", report["lifecycle"].(map[string]any)["state"])
	assert.Empty(t, report["anomalies"], "the cancel was asked for and approved by the user")
	assert.Equal(t, true, report["cancellations"].([]any)[0].(map[string]any)["authorized"])

	// The record carries the direction and the pay it reverses, and the
	// profile checker accepts it.
	export := filepath.Join(t.TempDir(), "deal.json")
	dealRun(t, "export", "--deal", id, "--output", export)
	records, err := os.ReadFile(export)
	require.NoError(t, err)
	assert.Contains(t, string(records), `"direction":"out"`)
	assert.Contains(t, string(records), `"direction":"in"`)
	assert.Equal(t, 1, strings.Count(string(records), `"rel":"reverses"`))
	if python := profilePython(t); python != "" {
		result, err := exec.Command(python, dealProfileDir+"/check_profile.py", export).CombinedOutput()
		require.NoError(t, err, string(result))
		assert.Contains(t, string(result), "ALL OK")
	}
}
