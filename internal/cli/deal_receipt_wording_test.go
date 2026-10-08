package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// closedMerchantDeal is a merchant deal that approved approvedMinor, paid
// it, and closed as received; then the merchant's confirmation ($64.00)
// arrives after the close.
func closedMerchantDeal(t *testing.T, approvedMinor string) string {
	t.Helper()
	dealFixture(t)
	setDealClock(t, "2026-10-03T21:00:00Z")
	stubDNS(t, map[string]string{merchantSelector + "._domainkey.shop.example": merchantKeyTXT(t), dmarcName: merchantDMARC(t)})
	dealID := dealRun(t, "open", "--input", writeJSON(t, `{
		"type": "purchase", "channel": "web",
		"intent": {"verbatim": "Buy the 3-design sticker pack from Shop Example, no more than $100 all in", "max_total_minor": 10000, "allowed": ["pay"]},
		"who": {"name": "Shop Example", "domain": "shop.example"},
		"terms": {"item": "sticker pack", "quantity": 1, "price_minor": `+approvedMinor+`, "currency": "USD"},
		"recourse": {"rail": "card", "refundable": true}
	}`))["deal_id"].(string)
	check := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"pay","amount_minor":`+approvedMinor+`,"authorized_max_minor":`+approvedMinor+`}`))
	require.Equal(t, "pass", check["verdict"], check["card"])
	dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":`+approvedMinor+`,"rail":"card","reference":"card ending 4242"}`))
	dealRun(t, "close", "--deal", dealID, "--input", writeJSON(t, `{"status":"received"}`))
	setDealClock(t, "2026-10-03T22:15:00Z")
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--email", filepath.Join(merchantFixture, "confirmation.eml"))
	return dealID
}

// The receipt's own words, on the page (the bundle's sealed report, which
// the page renders as written) and in the emailed receipt: a completed close
// is "matched, nothing left to match"; the merchant's email after the close
// is "The merchant's receipt arrived." and whether it matches what the user
// approved, with both amounts when it differs.
func TestTheReceiptSaysWhetherTheMerchantsReceiptMatches(t *testing.T) {
	for name, tc := range map[string]struct{ approved, line string }{
		"matches": {"6400", "The merchant's receipt arrived. It matches what you approved."},
		"differs": {"4800", "The merchant's receipt arrived. It differs: you approved $48.00; their receipt says $64.00."},
	} {
		t.Run(name, func(t *testing.T) {
			dealID := closedMerchantDeal(t, tc.approved)
			bundle := filepath.Join(t.TempDir(), "receipt.json")
			eml := filepath.Join(t.TempDir(), "receipt.eml")
			dealRun(t, "report", "--deal", dealID, "--bundle", bundle, "--email", eml)

			raw, err := os.ReadFile(bundle)
			require.NoError(t, err)
			var b map[string]any
			require.NoError(t, json.Unmarshal(raw, &b))
			life := dealReportOf(b)["lifecycle"].(map[string]any)
			assert.Equal(t, "Closed at 2026-10-03T21:00:00Z: matched, nothing left to match. 1 record sealed after the close is linked to it (it confirms the close).", life["text"])
			assert.Equal(t, tc.line, life["later"].([]any)[0].(map[string]any)["text"])

			email, err := os.ReadFile(eml)
			require.NoError(t, err)
			text := strings.ReplaceAll(string(email), "=\r\n", "")
			assert.Contains(t, text, "matched, nothing left to match")
			assert.Contains(t, text, "The merchant's receipt arrived.")
		})
	}
}

// An open deal with no merchant email yet is waiting for the merchant's
// receipt; a close other than completed keeps its outcome.
func TestAnOpenDealIsWaitingForTheMerchantsReceipt(t *testing.T) {
	dealFixture(t)
	setDealClock(t, "2026-10-03T21:00:00Z")
	dealID := openMerchantDeal(t)
	life := dealRun(t, "report", "--deal", dealID)["lifecycle"].(map[string]any)
	assert.True(t, strings.HasPrefix(life["text"].(string), "Open: waiting for the merchant's receipt."), life["text"])

	dealRun(t, "close", "--deal", dealID, "--input", writeJSON(t, `{"status":"not_received"}`))
	life = dealRun(t, "report", "--deal", dealID)["lifecycle"].(map[string]any)
	assert.NotContains(t, life["text"], "matched", "only a completed close reads matched")
}

// The receipt names the option the user chose as they saw it: "Pay anyway"
// over a finding (the approval of a paused check is the override, on
// record), and "Hold".
func TestTheReceiptNamesTheChoiceAsShown(t *testing.T) {
	id := stickerDeal(t, "card", false)
	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 954)))
	require.Equal(t, "pause", check["verdict"], check["card"])
	require.Contains(t, check["card"], "[Pay anyway]")
	dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", "yes, pay it")
	report := dealRun(t, "report", "--deal", id)
	raw, err := json.Marshal(report["did"])
	require.NoError(t, err)
	assert.Contains(t, string(raw), "; you chose Pay anyway")
}

// The trap: "receipt" is consumer wording only. No deal schema, typed record
// schema or external-check schema has a member named for it (a witness's
// COSE Receipt is witnesses[].receipt_b64, and the rendered report's wire
// name is action-report/v0).
func TestNoSchemaHasAReceiptKey(t *testing.T) {
	var files []string
	for _, dir := range []string{dealProfileDir, dealProfileDir + "/records", "assets", "assets/records"} {
		found, err := filepath.Glob(filepath.Join(dir, "*.json"))
		require.NoError(t, err)
		files = append(files, found...)
	}
	require.NotEmpty(t, files)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		require.NoError(t, err)
		var doc any
		require.NoError(t, json.Unmarshal(raw, &doc))
		for _, key := range schemaKeys(doc) {
			assert.NotContains(t, strings.ToLower(key), "receipt", "%s: a member named %q", f, key)
		}
	}
}

// schemaKeys is every member name a schema declares or requires.
func schemaKeys(v any) []string {
	var out []string
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if k == "properties" || k == "patternProperties" || k == "$defs" {
				if m, ok := child.(map[string]any); ok {
					for name := range m {
						out = append(out, name)
					}
				}
			}
			if k == "required" {
				if list, ok := child.([]any); ok {
					for _, name := range list {
						if s, ok := name.(string); ok {
							out = append(out, s)
						}
					}
				}
			}
			out = append(out, schemaKeys(child)...)
		}
	case []any:
		for _, child := range x {
			out = append(out, schemaKeys(child)...)
		}
	}
	return out
}
