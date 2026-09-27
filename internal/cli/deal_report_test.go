package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/emitter"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reportTexts returns the report's items as "side/kind: text" lines.
func reportTexts(t *testing.T, report map[string]any, key string) []string {
	t.Helper()
	var out []string
	for _, raw := range report[key].([]any) {
		item := raw.(map[string]any)
		prefix := item["kind"].(string)
		if side, ok := item["side"].(string); ok {
			prefix = side + "/" + prefix
		}
		out = append(out, prefix+": "+item["text"].(string))
	}
	return out
}

func TestDealReportThreeParts(t *testing.T) {
	dealFixture(t)
	dealID := openJetSki(t)
	check := dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(jetSkiDemo, "06-check-pay.json"))
	dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "hold")
	dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":20000,"payee":"M. Torres","rail":"zelle"}`))

	report := dealRun(t, "report", "--deal", dealID)
	assert.Equal(t, "rent me 2 jet skis Saturday", report["asked"])
	assert.Equal(t, []string{
		"check: Checked before paying: paused; you chose hold",
		"act: Did: pay $200.00 to M. Torres by Zelle ⚠️",
	}, reportTexts(t, report, "did"))
	assert.Equal(t, []string{
		"counterparty/changed_identifier: Payee changed: Coastal Jet Rentals LLC → M. Torres",
		"counterparty/domain_recent: Website registered 3 weeks ago",
		"agent/unsealed_approval: Went ahead without your approval: pay $200.00 to M. Torres by Zelle (the check paused and the answer was hold)",
		"counterparty/unverified_claim: Unverified: they have 2 jet skis for Saturday (from seller message)",
	}, reportTexts(t, report, "anomalies"))
}

func TestDealReportAgentAndCounterpartyAnomalies(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	for _, m := range []string{
		`{"from":"counterparty","channel":"email","text":"Rate is only good today only, book right now."}`,
		`{"from":"counterparty","text":"Easier to sort this on WhatsApp."}`,
		`{"from":"counterparty","text":"Please send me the verification code we sent you."}`,
	} {
		dealRun(t, "note", "--deal", dealID, "--kind", "message", "--input", writeJSON(t, m))
	}
	dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(bookingFixture, "check-commit.json"))
	dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":76000}`))

	report := dealRun(t, "report", "--deal", dealID)
	assert.Equal(t, []string{
		"counterparty/deadline_pressure: Pushed you to decide fast",
		"counterparty/channel_hop: Asked to move off the platform",
		"counterparty/code_request: Asked for a verification code",
		"agent/asked_vs_did: Tried confirming a commitment: Not what you asked: dates 2026-10-03/2026-10-05 → 2026-10-03/2026-10-07 · Over your limit of $400.00 ($760.00)",
		"agent/skipped_check: Skipped the check: pay $760.00 (no check before this action)",
		"counterparty/unverified_claim: Unverified: free cancellation until October 1 (from booking page)",
	}, reportTexts(t, report, "anomalies"))
}

func TestDealReportIsOneLocalVerifyingPage(t *testing.T) {
	dealFixture(t)
	dealID := openJetSki(t)
	check := dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(jetSkiDemo, "06-check-pay.json"))
	dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "hold")
	page := filepath.Join(t.TempDir(), "report.html")
	report := dealRun(t, "report", "--deal", dealID, "--html", page)
	assert.Equal(t, page, report["html"])
	assert.NotContains(t, report, "fragment", "no links or hosting: the page is the report")

	raw, err := os.ReadFile(page)
	require.NoError(t, err)
	html := string(raw)
	assert.NotRegexp(t, regexp.MustCompile(`(?i)<(script|link|img|iframe)[^>]+(src|href)=`), html)
	assert.Contains(t, html, "<title>Deal report</title>")
	assert.Contains(t, html, string(evidenceGraphIIFE))

	// Pull the embedded bundle back out and run the authoritative verifier.
	b := embeddedBundle(t, html)
	v := aacbundle.VerifyBundle(b)
	assert.Equal(t, "pass", v.GraphClosure.Status)
	assert.Equal(t, "pass", v.IntervalCoverage.Status, v.IntervalCoverage.Findings)
	assert.Equal(t, "pass", v.PerRecordMembership.Status, v.PerRecordMembership.Findings)
	assert.Len(t, b["records"], 8, "exactly this deal's steps, nothing from any other deal")
	ext := b["extensions"].(map[string]interface{})["x-deal-v0"].(map[string]interface{})
	cited := map[string]bool{}
	for _, a := range ext["anomalies"].([]interface{}) {
		for _, id := range a.(map[string]interface{})["steps"].([]interface{}) {
			cited[id.(string)] = true
		}
	}
	kinds := map[string]string{}
	for _, s := range ext["steps"].([]interface{}) {
		step := s.(map[string]interface{})
		kinds[step["capsule_id"].(string)] = step["kind"].(string)
	}
	for _, d := range v.Disclosures {
		if kinds[d.CapsuleID] == "message" && !cited[d.CapsuleID] {
			assert.Equal(t, "withheld", string(d.Status), "uncited message text stays on the machine")
		} else {
			assert.Equal(t, "disclosure_match", string(d.Status), kinds[d.CapsuleID])
		}
	}
	assert.NotContains(t, html, "We take a $200 deposit", "an uncited message is not in the page")
}

// embeddedBundle recovers the bundle the emitter embedded in the page.
func embeddedBundle(t *testing.T, html string) map[string]interface{} {
	t.Helper()
	const start = "window.__BUNDLE__ = "
	i := strings.Index(html, start)
	require.GreaterOrEqual(t, i, 0)
	rest := html[i+len(start):]
	j := strings.Index(rest, ";</script>")
	require.GreaterOrEqual(t, j, 0)
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(rest[:j])))
	require.NoError(t, err)
	b, ok := v.(map[string]interface{})
	require.True(t, ok)
	// Round-trip check: re-emitting the same bundle gives the same page.
	again, err := emitter.EmitEvidenceGraphHTML(b, evidenceGraphIIFE)
	require.NoError(t, err)
	assert.Contains(t, html, again[strings.Index(again, start):strings.Index(again, ";</script>")])
	return b
}

// Every sealed step validates against the schema that ships in the skill.
func TestDealRecordsMatchShippedSchema(t *testing.T) {
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	schema, err := c.Compile("../../skills/deal/schema/x-deal-v0.schema.json")
	require.NoError(t, err)
	dealFixture(t)
	dealID := openJetSki(t)
	check := dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(jetSkiDemo, "06-check-pay.json"))
	dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", "ok")
	dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":20000,"payee":"M. Torres","rail":"zelle","reference":"r1"}`))
	dealRun(t, "note", "--deal", dealID, "--kind", "claim", "--input", writeJSON(t, `{"text":"skis are serviced","source":"seller message"}`))
	dealRun(t, "close", "--deal", dealID, "--input", writeJSON(t, `{"status":"received","delivered":{"quantity":1},"note":"one ski short"}`))

	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.NoError(t, s.useDeal(t.Context(), dealID, false))
	events, err := s.load(t.Context(), dealID)
	require.NoError(t, err)
	seen := map[string]bool{}
	for _, se := range events {
		raw, err := encodeDealRecord(se.Event)
		require.NoError(t, err)
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		require.NoError(t, err)
		assert.NoError(t, schema.Validate(doc), "step %d (%s)", se.Event.N, se.Event.Kind)
		seen[se.Event.Kind] = true
	}
	for _, kind := range []string{"open", "message", "claim", "evidence", "change", "snapshot", "check", "approval", "act", "close"} {
		assert.True(t, seen[kind], "fixture covers %s", kind)
	}
}

func TestVendoredVerifierMatchesRecordedDigest(t *testing.T) {
	sum := sha256.Sum256(evidenceGraphIIFE)
	assert.Equal(t, strings.TrimSpace(evidenceGraphIIFESHA256), hex.EncodeToString(sum[:]), "rebuild with scripts/build-evidence-graph-iife.sh; never hand-edit")
}
