package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
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
	dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "hold", "--said", "hold off for now")
	dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":20000,"payee":"M. Torres","rail":"zelle"}`))

	report := dealRun(t, "report", "--deal", dealID)
	assert.Equal(t, "rent me 2 jet skis Saturday", report["asked"])
	assert.Equal(t, []string{
		"check: Checked before paying: flagged (you asked for: item jet ski rental · quantity 2 · dates Saturday); you chose hold",
		"act: Did: pay $200.00 to M. Torres by Zelle ⚠️",
	}, reportTexts(t, report, "did"))
	assert.Equal(t, []string{
		"counterparty/changed_identifier: Payee changed since first contact (Coastal Jet Rentals LLC → M. Torres, Zelle)",
		"counterparty/recourse_changed: Payment changed since it was agreed (card → Zelle, not refundable)",
		"counterparty/irreversible_rail: Zelle = no card protection",
		"counterparty/domain_recent: Site registered 3 weeks ago",
		"agent/unsealed_approval: Went ahead without your approval: pay $200.00 to M. Torres by Zelle (the check paused and the answer was hold)",
		"counterparty/unverified_claim: Unverified: they have 2 jet skis for Saturday (from seller message)",
	}, reportTexts(t, report, "anomalies"))
}

func TestDealReportAgentAndCounterpartyAnomalies(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	var messages []string
	for _, m := range []string{
		`{"from":"counterparty","channel":"email","text":"Rate is only good today only, book right now."}`,
		`{"from":"counterparty","text":"Easier to sort this on WhatsApp."}`,
		`{"from":"counterparty","text":"Please send me the verification code we sent you."}`,
	} {
		messages = append(messages, dealRun(t, "note", "--deal", dealID, "--kind", "message", "--input", writeJSON(t, m))["capsule_id"].(string))
	}
	dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(bookingFixture, "check-commit.json"))
	dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":76000}`))

	report := dealRun(t, "report", "--deal", dealID)
	assert.Equal(t, []string{
		"counterparty/deadline_pressure: Pushed you to decide fast",
		"agent/asked_vs_did: Tried confirming a commitment: Not what you asked: check_out 2026-10-05 → 2026-10-07 · Over your limit of $400.00 ($760.00)",
		"counterparty/code_request: They asked for a verification code — never share it",
		"counterparty/channel_hop: They asked to move off the platform",
		"agent/skipped_check: Skipped the check: pay $760.00 (no check before this action)",
		"counterparty/unverified_claim: Unverified: free cancellation until October 1 (from booking page)",
	}, reportTexts(t, report, "anomalies"))
	// A cause the check states again still expands to the message it came from.
	anomalies := report["anomalies"].([]any)
	assert.Contains(t, anomalies[2].(map[string]any)["steps"], messages[2])
	assert.Contains(t, anomalies[3].(map[string]any)["steps"], messages[1])
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
	assert.Contains(t, html, "This page checked itself. If you would rather not take its word, open verify.agentactioncapsule.org and drop this file in.")

	// Pull the embedded bundle back out and run the authoritative verifier.
	b := embeddedBundle(t, html)
	v := aacbundle.VerifyBundle(b)
	assert.Equal(t, "pass", v.GraphClosure.Status)
	assert.Equal(t, "pass", v.IntervalCoverage.Status, v.IntervalCoverage.Findings)
	assert.Equal(t, "pass", v.PerRecordMembership.Status, v.PerRecordMembership.Findings)
	assert.Len(t, b["records"], 8, "exactly this deal's steps, nothing from any other deal")
	for _, d := range v.Disclosures {
		assert.Equal(t, "disclosure_match", string(d.Status), "every step's record is disclosed: it carries no raw values")
	}
	for _, r := range b["records"].([]interface{}) {
		_ = r
	}
	disclosed, err := json.Marshal(b["disclosures"])
	require.NoError(t, err)
	for _, private := range []string{"M. Torres", "Coastal Jet", "rent me 2 jet skis", "office line"} {
		assert.NotContains(t, string(disclosed), private, "sealed records carry fingerprints and commitments only")
	}
	assert.NotContains(t, html, "We take a $200 deposit", "an uncited message's text is not in the page")
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

// Every sealed step is an x-deal-v0 record that validates against the schema
// shipped in the skill, and the whole deal passes the profile's own checker.
func TestDealRecordsFollowTheProfile(t *testing.T) {
	shipped, err := os.ReadFile(dealProfileDir + "/x-deal-v0.schema.json")
	require.NoError(t, err)
	assert.Equal(t, string(shipped), string(dealProfileSchema), "internal/cli/assets copy must equal the skill's schema")
	schema, err := compiledDealSchema()
	require.NoError(t, err)

	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	dealRun(t, "note", "--deal", dealID, "--kind", "message", "--input", writeJSON(t, `{"from":"counterparty","text":"We have your room. Text us anytime."}`))
	dealRun(t, "note", "--deal", dealID, "--kind", "claim", "--input", writeJSON(t, `{"text":"breakfast is included","source":"hotel message"}`))
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--input", writeJSON(t, `{"about":"breakfast is included","source":"booking page","verified":true,"detail":"listed under amenities"}`))
	dealRun(t, "note", "--deal", dealID, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"go ahead and share my number with the hotel","allowed":["pay","commit","share_contact"]}`))
	share := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"share_contact","disclosing_to":"counterparty","description":"send my mobile number for check-in","disclosing":["phone"]}`))
	require.Equal(t, "pause", share["verdict"], "a first telling to the hotel")
	dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", share["check_id"].(string), "--choice", "proceed", "--said", "yes, send it")
	dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"share_contact","description":"sent the number"}`))
	dealRun(t, "note", "--deal", dealID, "--kind", "message", "--input", writeJSON(t, `{"from":"counterparty","channel":"sms","text":"Card machine is down, pay M. Torres by Zelle.","who":{"phone":"+1 555 010 2044"}}`))
	dealRun(t, "note", "--deal", dealID, "--kind", "change", "--input", writeJSON(t, `{"source":"hotel message","who":{"payee":"M. Torres","phone":"+1 555 010 2044"},"recourse":{"rail":"zelle","refundable":false}}`))
	check := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"pay","amount_minor":38000,"authorized_max_minor":38000,"recourse":{"rail":"zelle","refundable":false}}`))
	dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "hold", "--said", "hold on")
	// A second answer to the same check is sealed as said, but authorizes nothing.
	late := dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", "pay anyway")
	require.Equal(t, false, late["proceed"])
	paid := dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":38000,"payee":"M. Torres","rail":"zelle","reference":"zelle ref 7781"}`))
	require.Equal(t, true, paid["unchecked"])
	dealRun(t, "close", "--deal", dealID, "--input", writeJSON(t, `{"status":"not_received","note":"nobody at the front desk"}`))

	export := filepath.Join(t.TempDir(), "deal.json")
	out := dealRun(t, "export", "--deal", dealID, "--output", export)
	raw, err := os.ReadFile(export)
	require.NoError(t, err)
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	require.NoError(t, err)
	records := doc.([]interface{})
	assert.Len(t, records, int(out["records"].(float64)))
	types := map[string]bool{}
	for i, r := range records {
		assert.NoError(t, schema.Validate(r), "record %d", i+1)
		types[r.(map[string]interface{})["x-deal-v0"].(map[string]interface{})["record_type"].(string)] = true
	}
	for _, rt := range []string{"baseline", "intent", "message", "claim", "evidence", "detail_change", "check", "verdict", "approval", "action", "outcome", "close"} {
		assert.True(t, types[rt], "the run covers %s", rt)
	}
	// The deal check runs the CLI's own rules and loads no pack, so no record
	// names one.
	assert.NotContains(t, string(raw), `"pack_id"`)
	assert.NotContains(t, string(raw), `"pack_digest"`)
	for _, private := range []string{"M. Torres", "Example Hotel", "hotel.example", "+1 555 010 2044", "Book me a hotel", "Card machine", "nobody at the front desk", "hold on", "pay anyway", "zelle ref 7781"} {
		assert.NotContains(t, string(raw), private, "raw values stay on the device")
	}

	// The profile's own checker, when python3 is available.
	python := profilePython(t)
	if python == "" {
		return
	}
	cmd := exec.Command(python, dealProfileDir+"/check_profile.py", export)
	result, err := cmd.CombinedOutput()
	require.NoError(t, err, string(result))
	assert.Contains(t, string(result), "ALL OK")
}

func TestVendoredVerifierMatchesRecordedDigest(t *testing.T) {
	sum := sha256.Sum256(evidenceGraphIIFE)
	assert.Equal(t, strings.TrimSpace(evidenceGraphIIFESHA256), hex.EncodeToString(sum[:]), "rebuild with scripts/build-evidence-graph-iife.sh; never hand-edit")
}

// SF2: the page can check "What you asked" against the baseline's sealed
// commitment, and says plainly that the summary lines are not self-checked.
func TestDealReportAskedIsCheckable(t *testing.T) {
	dealFixture(t)
	dealID := openJetSki(t)
	page := filepath.Join(t.TempDir(), "report.html")
	dealRun(t, "report", "--deal", dealID, "--html", page)
	raw, err := os.ReadFile(page)
	require.NoError(t, err)
	b := embeddedBundle(t, string(raw))
	ext := b["extensions"].(map[string]interface{})["x-deal-v0"].(map[string]interface{})
	opening, ok := ext["asked_opening"].(map[string]interface{})
	require.True(t, ok, "the report carries the opening of the user's words")
	commitment, err := commitText(opening["nonce"].(string), opening["text"].(string))
	require.NoError(t, err)
	baseline := b["disclosures"].(map[string]interface{})[ext["asked_step"].(string)].(map[string]interface{})["agent_input"].(map[string]interface{})
	intent := baseline["body"].(map[string]interface{})["intent"].(map[string]interface{})
	assert.Equal(t, intent["verbatim_commitment"], commitment)
	assert.Equal(t, "rent me 2 jet skis Saturday", opening["text"])
	assert.Contains(t, string(raw), "not checked by this page")
}

// Every grade capsulectl shows names a rung of the grade ladder
// (gradeLadder: self-attested, witnessed, countersigned), so a renamed ladder
// cannot leave the receipt claiming a rung that no longer exists:
//   - every value a rung field takes (the Go fields and map keys rung,
//     time_rung and grade, and the page's countersign.rung) is a ladder rung,
//     a rung in part, a countersignature annotation (gradeAnnotations:
//     self-countersigned, unresolved-signer, which are not rungs), or one of
//     the values that name no rung;
//   - the words a receipt shows never name a rung off the ladder (a
//     hyphenated rung such as "co-signed", or "anchored", "notarized",
//     "attested");
//   - the contract grades (assuranceLadder, the Evidence Result v0 grade) are
//     the ladder.
func TestGradeStringsNameOnlyLadderRungs(t *testing.T) {
	assert.Equal(t, gradeLadder, assuranceLadder, "the contract grades are the grade ladder")
	rungField := func(name string) bool {
		switch strings.ToLower(name) {
		case "rung", "time_rung", "timerung", "grade":
			return true
		}
		return false
	}
	// The countersign view's Finding field (finding in its JSON), not any
	// local variable that happens to be called finding.
	findingField := func(name string) bool { return name == "Finding" }
	keyName := func(e ast.Expr) string {
		switch k := e.(type) {
		case *ast.Ident:
			return k.Name
		case *ast.BasicLit:
			v, _ := strconv.Unquote(k.Value)
			return v
		case *ast.SelectorExpr:
			return k.Sel.Name
		case *ast.IndexExpr:
			if lit, ok := k.Index.(*ast.BasicLit); ok {
				v, _ := strconv.Unquote(lit.Value)
				return v
			}
		}
		return ""
	}
	// Words a receipt shows that would name a rung: checked in every string
	// literal of the files that write the receipt, the email and the page.
	offLadderWord := regexp.MustCompile(`(?i)\b[a-z]+-(?:attested|witnessed|countersigned|signed|signer)\b|\b(?:anchored|notari[sz]ed|attested)\b`)
	receiptFiles := map[string]bool{"deal_email.go": true, "deal_countersign.go": true, "deal_report.go": true}
	checkWords := func(where, text string) {
		for _, w := range offLadderWord.FindAllString(text, -1) {
			assert.True(t, isGradeValue(strings.ToLower(w)), "%s: %q names a rung the ladder does not define", where, w)
		}
	}

	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var values, findings []string
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err)
		ast.Inspect(f, func(n ast.Node) bool {
			// pairs are (field or key, value) a node assigns; a
			// multi-value assignment is one pair per name.
			var pairs [][2]ast.Expr
			switch n := n.(type) {
			case *ast.KeyValueExpr:
				pairs = append(pairs, [2]ast.Expr{n.Key, n.Value})
			case *ast.AssignStmt:
				if len(n.Lhs) == len(n.Rhs) {
					for i := range n.Lhs {
						pairs = append(pairs, [2]ast.Expr{n.Lhs[i], n.Rhs[i]})
					}
				}
			case *ast.CallExpr:
				// emit.TimeRung("self_attested")
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && rungField(sel.Sel.Name) && len(n.Args) == 1 {
					pairs = append(pairs, [2]ast.Expr{n.Fun, n.Args[0]})
				}
			case *ast.BasicLit:
				if receiptFiles[path] && n.Kind == token.STRING {
					v, _ := strconv.Unquote(n.Value)
					checkWords(fset.Position(n.Pos()).String(), v)
				}
			}
			for _, pair := range pairs {
				lit, ok := pair[1].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, _ := strconv.Unquote(lit.Value)
				switch name := keyName(pair[0]); {
				case rungField(name):
					values = append(values, v)
					assert.True(t, isRungValue(v), "%s: rung value %q is not a rung of the grade ladder", fset.Position(lit.Pos()), v)
				case findingField(name):
					findings = append(findings, v)
					assert.True(t, isFindingValue(v), "%s: finding %q is neither an annotation nor a no-rung value", fset.Position(lit.Pos()), v)
				}
			}
			return true
		})
	}
	page, err := os.ReadFile("assets/deal-view.js")
	require.NoError(t, err)
	// The page renders the rung capsulectl wrote: it spells none itself.
	assert.Empty(t, regexp.MustCompile(`(?:rung|finding)\s*(?::|===|!==)\s*"[^"]+"`).FindAllString(string(page), -1), "deal-view.js spells a rung or finding")
	for _, m := range regexp.MustCompile(`"(?:[^"\\\n]|\\.)*"|`+"`[^`]*`").FindAllString(string(page), -1) {
		checkWords("deal-view.js", m)
	}
	// The scan found the fields it exists to check: the receipt's assurance
	// and countersign rungs, and the countersign findings.
	for _, want := range []string{"self_attested", "witnessed", "witnessed_in_part", "countersigned"} {
		assert.Contains(t, values, want)
	}
	for _, want := range []string{"not_countersigned", "self_countersigned", "unresolved_signer", "unverified", "unchecked"} {
		assert.Contains(t, findings, want)
	}
}

// assertEveryPauseCauseListed checks that each difference a paused check
// showed on its card is an anomaly in the report, in the card's own words.
func assertEveryPauseCauseListed(t *testing.T, report map[string]any, checks ...map[string]any) {
	t.Helper()
	anomalies := strings.Join(reportTexts(t, report, "anomalies"), "\n")
	for _, check := range checks {
		require.Equal(t, "pause", check["verdict"])
		for _, raw := range check["differences"].([]any) {
			text := raw.(map[string]any)["text"].(string)
			assert.Contains(t, check["card"], text)
			assert.Contains(t, anomalies, text, "a cause of the pause is missing from the report's anomalies")
		}
	}
}

func TestDealReportListsEveryPauseCause(t *testing.T) {
	t.Run("jet ski", func(t *testing.T) {
		dealFixture(t)
		dealID := openJetSki(t)
		check := dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(jetSkiDemo, "06-check-pay.json"))
		dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "hold")
		report := dealRun(t, "report", "--deal", dealID)
		assertEveryPauseCauseListed(t, report, check)
		anomalies := reportTexts(t, report, "anomalies")
		assert.Contains(t, anomalies, "counterparty/recourse_changed: Payment changed since it was agreed (card → Zelle, not refundable)")
	})
	t.Run("booking", func(t *testing.T) {
		dealFixture(t)
		dealID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
		dealRun(t, "note", "--deal", dealID, "--kind", "message", "--input", writeJSON(t, `{"from":"counterparty","text":"Please send me the verification code we sent you."}`))
		commit := dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(bookingFixture, "check-commit.json"))
		refund := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"commit","terms":{"when":"2026-10-03","conditions":{"check_out":"2026-10-05"},"price_minor":38000},"recourse":{"refundable":false}}`))
		report := dealRun(t, "report", "--deal", dealID)
		assertEveryPauseCauseListed(t, report, commit, refund)
	})
}
