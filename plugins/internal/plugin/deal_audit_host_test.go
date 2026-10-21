package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const retailDemo = "../../../skills/deal/demo/retail-checkout"

var procedureStep = regexp.MustCompile(`^\d+\. (.+)$`)

// procedureSteps returns the numbered steps of one procedure in SKILL.md.
func procedureSteps(t *testing.T, name string) []string {
	t.Helper()
	raw, err := os.ReadFile("../../../skills/deal/SKILL.md")
	require.NoError(t, err)
	var steps []string
	in := false
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case line == "### "+name:
			in = true
		case in && strings.HasPrefix(line, "#"):
			return steps
		case in:
			if m := procedureStep.FindStringSubmatch(line); m != nil {
				steps = append(steps, m[1])
			}
		}
	}
	require.NotEmpty(t, steps, name)
	return steps
}

// auditHostRun runs `deal audit-host` and decodes its result, which is printed
// whether or not anything is unrecorded.
func auditHostRun(t *testing.T, args ...string) (map[string]any, error) {
	t.Helper()
	out, err := invoke(t, "", append([]string{"--profile", "deal", "deal", "audit-host"}, args...)...)
	var m map[string]any
	require.NoError(t, json.NewDecoder(strings.NewReader(out)).Decode(&m), out)
	return m, err
}

func TestDealProceduresEndInTheFinalReview(t *testing.T) {
	for _, name := range []string{"Purchase", "Booking", "Signature", "Disclosure"} {
		steps := procedureSteps(t, name)
		review, act, open := -1, -1, -1
		for i, step := range steps {
			// One deal per errand (capsule-cli#340): the deal opens at the
			// user's first ask, before the final review checks against it.
			if strings.Contains(step, "`deal open`") && open == -1 {
				open = i
			}
			if strings.HasPrefix(step, "**Final review:**") {
				assert.Equal(t, -1, review, "%s has one final review", name)
				review = i
				assert.Contains(t, step, "`deal check`", name)
			}
			if strings.Contains(step, "On `\"proceed\": true`") {
				act = i
				assert.Contains(t, step, "`approval_text`", "%s asks with the check's own text", name)
			}
		}
		require.NotEqual(t, -1, review, "%s has a final review step", name)
		require.NotEqual(t, -1, open, "%s opens the deal", name)
		assert.Less(t, open, review, "%s opens the deal before the final review", name)
		require.NotEqual(t, -1, act, "%s has an acting step", name)
		assert.Less(t, review, act, "%s reviews before it acts", name)
	}
}

func TestDealSkillSaysInvocationIsAdvisoryAndRetailIsInScope(t *testing.T) {
	raw, err := os.ReadFile("../../../skills/deal/SKILL.md")
	require.NoError(t, err)
	skill := strings.Join(strings.Fields(string(raw)), " ")
	assert.Contains(t, skill, "## Invocation is advisory")
	assert.Contains(t, skill, "Invocation is advisory, not enforced.")
	assert.Contains(t, skill, "A known merchant at a fixed price is in scope.")
	assert.Contains(t, skill, "A retail checkout")
	assert.Contains(t, skill, "It does not depend on who the other side is.")
}

// A scripted agent loop walks the Purchase procedure as SKILL.md writes it,
// for a request that never names the deal skill. The host side of each step
// (filling the form, the payment) is written as the host's execution records,
// as a sub-task would leave them; the pass reads those records.
func TestDealRetailCheckoutFollowsTheProcedure(t *testing.T) {
	dealFixture(t)
	request, err := os.ReadFile(filepath.Join(retailDemo, "request.txt"))
	require.NoError(t, err)
	for _, word := range []string{"deal", "skill", "capsulectl", "check", "seal"} {
		require.NotContains(t, strings.ToLower(string(request)), word, "the user does not name the skill")
	}

	var executions bytes.Buffer
	host := func(id, task, tool, action, extra string) {
		fmt.Fprintf(&executions, `{"id":%q,"at":"2026-09-27T18:00:00Z","task":%q,"parent_task":"t-main","tool":%q,"action":%q,"status":"succeeded"%s}`+"\n", id, task, tool, action, extra)
	}
	host("c1", "t-main", "spawn_task", "other", "")
	var dealID string
	proceed := false
	for _, step := range procedureSteps(t, "Purchase") {
		switch {
		case strings.HasPrefix(step, "Fill"):
			host("c2", "t-browser", "fill_form", "other", "")
		case strings.Contains(step, "`deal open`") && dealID == "":
			opened := dealRun(t, "open", "--input", filepath.Join(retailDemo, "open.json"))
			dealID = opened["deal_id"].(string)
		case strings.HasPrefix(step, "**Final review:**"):
			require.NotEmpty(t, dealID, "the deal opened at the first ask")
			require.Contains(t, step, "`deal check`")
			check := dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(retailDemo, "check-pay.json"))
			assert.Equal(t, "pass", check["verdict"], "a known merchant at the agreed price passes quietly")
			assert.Equal(t, "", check["card"])
			assert.Equal(t, "Pay $6.27 to Example Stickers (stickers.example) for the cat sticker?\n"+
				"$6.27 · by card · refundable", check["approval_text"])
			assert.Equal(t, "2026-09-27T18:00:00Z", check["checked_at"])
			proceed = check["proceed"] == true
		case strings.Contains(step, "place the order"):
			require.True(t, proceed, "the order is placed only after the final review")
			host("c3", "t-browser", "make_payment", "pay", `,"amount_minor":627,"currency":"USD","merchant_domain":"stickers.example","reference_sha256":"71c270b6a6356140ce4d74549522926ab8c7f3f5f2a65d0589f99364bb823ab0"`)
		case strings.Contains(step, "`deal note --kind act`"):
			act := dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", filepath.Join(retailDemo, "act-pay.json"))
			assert.Equal(t, false, act["unchecked"])
		}
	}
	require.NotEmpty(t, dealID, "the procedure sealed a deal")
	report := dealRun(t, "report", "--deal", dealID)
	assert.Equal(t, strings.TrimSpace(string(request)), report["asked"])
	assert.Empty(t, report["anomalies"])

	path := filepath.Join(t.TempDir(), "executions.jsonl")
	require.NoError(t, os.WriteFile(path, executions.Bytes(), 0o600))
	result, err := auditHostRun(t, "--executions", path, "--from", "2026-09-27T00:00:00Z", "--to", "2026-09-28T00:00:00Z")
	require.NoError(t, err)
	assert.Empty(t, result["unrecorded"])
	assert.Equal(t, "report", result["voice"])
	assert.Equal(t, "Every action on this device has a record.", result["say"])
	recorded := result["recorded"].([]any)
	require.Len(t, recorded, 1)
	assert.Equal(t, dealID, recorded[0].(map[string]any)["deal_id"])
	assert.Equal(t, "act", recorded[0].(map[string]any)["matched_by"])
	assert.Equal(t, float64(3), result["records_read"])
	assert.True(t, strings.HasPrefix(result["summary"].(string), "0 of 1 consequential actions have no deal record"))
	assert.Contains(t, result["summary"], "cannot prove that nothing else happened")
	coverage := result["coverage"].(map[string]any)
	assert.Equal(t, map[string]any{"available": false, "read": float64(0)}, coverage["host_approvals"])
	gaps := coverage["cannot_see"].([]any)
	assert.Len(t, gaps, len(dealCoverageGaps)+2)
	assert.Contains(t, gaps, dealAuthorityGap)
	assert.Equal(t, "not established", coverage["authority"])
	assert.Contains(t, gaps, dealNoApprovalsGap, "the output says when approvals were not available")
	assert.NotContains(t, recorded[0].(map[string]any), "host_approval")

	approvals := filepath.Join(t.TempDir(), "approvals.jsonl")
	require.NoError(t, os.WriteFile(approvals, []byte(`{"id":"a1","at":"2026-09-27T18:00:00Z","task":"t-browser","amount_minor":627,"currency":"USD","decision":"approved"}`+"\n"), 0o600))
	result, err = auditHostRun(t, "--executions", path, "--approvals", approvals, "--from", "2026-09-27T00:00:00Z", "--to", "2026-09-28T00:00:00Z")
	require.NoError(t, err)
	coverage = result["coverage"].(map[string]any)
	assert.Equal(t, map[string]any{"available": true, "read": float64(1)}, coverage["host_approvals"])
	assert.NotContains(t, coverage["cannot_see"], dealNoApprovalsGap)
	assert.Contains(t, coverage["cannot_see"], dealAuthorityGap, "approval ids do not establish authority")
	assert.Equal(t, "not established", coverage["authority"])
	assert.Equal(t, map[string]any{"id": "a1", "at": "2026-09-27T18:00:00Z", "decision": "approved"}, result["recorded"].([]any)[0].(map[string]any)["host_approval"])
}

// The approval text is the card: the tag, the question, the facts and the
// reasons. When the check goes stale is in the check's output and its sealed
// record, not in the prompt.
func TestDealCheckApprovalTextCarriesTheFindingAndWhenItGoesStale(t *testing.T) {
	dealFixture(t)
	dealID := openJetSki(t)
	check := dealRun(t, "check", "--deal", dealID, "--stale-after", "5m", "--input", filepath.Join(jetSkiDemo, "06-check-pay.json"))
	assert.Equal(t, "DEMO · STOP: CHECK THIS\n"+
		"Pay $200 to M. Torres (coastal-jet-rentals.example) for the jet ski rental?\n"+
		"$200 · by Zelle · not refundable\n"+
		"Why I'm asking: the payee or contact changed since you first talked; the way to get your money back changed.\n"+
		"Good to know: they have 2 jet skis for Saturday (not checked).", check["approval_text"])
	assert.NotContains(t, check["approval_text"], "5m", "when the check goes stale is not in the prompt")
	assert.Equal(t, check["approval_text"], check["card"], "one block: the card sealed is the text shown")
	assert.Equal(t, float64(5), check["stale_after_minutes"])
	assert.Equal(t, "2026-09-27T18:00:00Z", check["checked_at"])
	_, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", dealID, "--stale-after", "10s", "--input", filepath.Join(jetSkiDemo, "06-check-pay.json"))
	require.ErrorIs(t, err, cli.ErrInput)
}

func TestDealAuditHostListsAPaymentWithNoDeal(t *testing.T) {
	dealFixture(t)
	result, err := auditHostRun(t, "--executions", filepath.Join(bookingFixture, "..", "executions-unrecorded.jsonl"), "--from", "2026-09-27T00:00:00Z", "--to", "2026-09-28T00:00:00Z")
	require.ErrorIs(t, err, cli.ErrPartial, "anything unrecorded exits partial")
	unrecorded := result["unrecorded"].([]any)
	require.Len(t, unrecorded, 1)
	assert.Equal(t, "1 action on this device has no record.", result["say"])
	assert.Equal(t, "c4", unrecorded[0].(map[string]any)["id"], "the sub-task's payment, not the parent's spawn and steer calls")
	assert.Equal(t, "t-browser", unrecorded[0].(map[string]any)["task"])
	assert.Len(t, result["failed_attempts"], 1)
	assert.Equal(t, float64(1), result["outside_period"])
	assert.Equal(t, float64(3), result["not_consequential"])
	assert.True(t, strings.HasPrefix(result["summary"].(string), "1 of 1 consequential actions have no deal record"))
	assert.NotEmpty(t, result["coverage"].(map[string]any)["cannot_see"])
}

func TestDealAuditHostMatchesAChecksDealAndRefusesBadRecords(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(retailDemo, "open.json"))["deal_id"].(string)
	dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(retailDemo, "check-pay.json"))
	path := writeJSON(t, `{"id":"c1","at":"2026-09-27T18:05:00Z","tool":"make_payment","action":"pay","amount_minor":627,"currency":"USD","status":"unknown","deal_id":"`+dealID+`"}`)
	result, err := auditHostRun(t, "--executions", path, "--to", "2026-09-28T00:00:00Z")
	require.NoError(t, err)
	assert.Equal(t, "check", result["recorded"].([]any)[0].(map[string]any)["matched_by"])

	// A different amount is not the checked payment.
	path = writeJSON(t, `{"id":"c1","at":"2026-09-27T18:05:00Z","tool":"make_payment","action":"pay","amount_minor":9999,"status":"succeeded"}`)
	_, err = auditHostRun(t, "--executions", path, "--to", "2026-09-28T00:00:00Z")
	require.ErrorIs(t, err, cli.ErrPartial)

	for _, bad := range []string{
		`{"id":"c1","at":"2026-09-27T18:05:00Z","tool":"make_payment","action":"pay","status":"succeeded","extra":1}`,
		`{"id":"c1","at":"yesterday","tool":"make_payment","action":"pay","status":"succeeded"}`,
		`{"id":"c1","at":"2026-09-27T18:05:00Z","tool":"make_payment","action":"refund","status":"succeeded"}`,
		`{"id":"c1","at":"2026-09-27T18:05:00Z","tool":"make_payment","action":"pay","status":"done"}`,
	} {
		_, err := invoke(t, "", "--profile", "deal", "deal", "audit-host", "--executions", writeJSON(t, bad))
		require.ErrorIs(t, err, cli.ErrInput, bad)
	}
}

// The reader takes metadata and digests only. A card number, a code or form
// content in an execution or approval record is refused, and never reaches
// the output, the error, or the store.
func TestDealAuditHostNeverCarriesACardNumber(t *testing.T) {
	dir := dealFixture(t)
	const pan = "4111111111111111"
	spaced := "4111 1111 1111 1111"
	good := `{"id":"c1","at":"2026-09-27T18:05:00Z","task":"t-browser","tool":"make_payment","action":"pay","amount_minor":627,"currency":"USD","status":"succeeded"`
	for _, bad := range []string{
		`{"id":"c1","at":"2026-09-27T18:05:00Z","tool":"make_payment","action":"pay","status":"succeeded","card_number":"` + pan + `"}`,
		`{"id":"c1","at":"2026-09-27T18:05:00Z","tool":"make_payment","action":"pay","status":"succeeded","form_body":{"card":"` + spaced + `","cvc":"123"}}`,
		`{"id":"c1","at":"2026-09-27T18:05:00Z","tool":"make_payment","action":"pay","status":"succeeded","verification_code":"482913"}`,
		`{"id":"` + pan + `","at":"2026-09-27T18:05:00Z","tool":"make_payment","action":"pay","status":"succeeded"}`,
		`{"id":"c1","at":"2026-09-27T18:05:00Z","tool":"make_payment card ` + spaced + `","action":"pay","status":"succeeded"}`,
		`{"id":"c1","at":"2026-09-27T18:05:00Z","task":"t-` + pan + `","tool":"make_payment","action":"pay","status":"succeeded"}`,
		`{"id":"c1","at":"2026-09-27T18:05:00Z","tool":"make_payment","action":"pay","status":"succeeded","merchant_domain":"pay.example/` + pan + `"}`,
		`{"id":"c1","at":"2026-09-27T18:05:00Z","tool":"make_payment","action":"pay","status":"succeeded","reference_sha256":"` + pan + `"}`,
	} {
		out, err := invoke(t, "", "--profile", "deal", "deal", "audit-host", "--executions", writeJSON(t, good+"}\n"+bad))
		require.ErrorIs(t, err, cli.ErrInput, bad)
		assert.NotContains(t, out, pan, bad)
		assert.NotContains(t, out, spaced, bad)
		assert.NotContains(t, cli.SafeError(err), pan, bad)
		assert.NotContains(t, err.Error(), pan, bad)
		assert.NotContains(t, err.Error(), "482913", bad)
	}
	for _, bad := range []string{
		`{"id":"a1","at":"2026-09-27T18:00:00Z","execution_id":"` + pan + `","decision":"approved"}`,
		`{"id":"a1","at":"2026-09-27T18:00:00Z","decision":"approved","card_last4":"1111","card":"` + pan + `"}`,
	} {
		out, err := invoke(t, "", "--profile", "deal", "deal", "audit-host", "--executions", writeJSON(t, good+"}"), "--approvals", writeJSON(t, bad))
		require.ErrorIs(t, err, cli.ErrInput, bad)
		assert.NotContains(t, out, pan, bad)
		assert.NotContains(t, err.Error(), pan, bad)
	}
	// Nothing was written to the store either: audit-host seals nothing.
	raw, err := os.ReadFile(filepath.Join(dir, "deal.db"))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), pan)
	assert.NotContains(t, string(raw), spaced)
}

// `deal reconcile`, the name this command shipped under, still runs it for
// one release: the same result on stdout, a warning on stderr only, and no
// entry in the help.
func TestDealReconcileIsADeprecatedAliasOfAuditHost(t *testing.T) {
	dealFixture(t)
	args := []string{"--executions", filepath.Join(bookingFixture, "..", "executions-unrecorded.jsonl"), "--from", "2026-09-27T00:00:00Z", "--to", "2026-09-28T00:00:00Z"}
	run := func(verb string) (string, string, error) {
		c := testCommand()
		var stdout, stderr bytes.Buffer
		c.SetOut(&stdout)
		c.SetErr(&stderr)
		c.SetArgs(append([]string{"--profile", "deal", "deal", verb}, args...))
		err := c.ExecuteContext(t.Context())
		return stdout.String(), stderr.String(), err
	}
	newOut, newErr, err := run("audit-host")
	require.ErrorIs(t, err, cli.ErrPartial)
	assert.NotContains(t, newErr, dealReconcileDeprecation)
	oldOut, oldErr, err := run("reconcile")
	require.ErrorIs(t, err, cli.ErrPartial)
	assert.Equal(t, newOut, oldOut, "the same stdout, byte for byte")
	assert.Contains(t, oldErr, dealReconcileDeprecation)

	help, err := invoke(t, "", "--profile", "deal", "deal", "--help")
	require.NoError(t, err)
	assert.Contains(t, help, "audit-host")
	assert.Contains(t, help, "List host actions that have no deal record; seals nothing")
	assert.NotRegexp(t, `(?m)^\s+reconcile\s`, help, "the alias is hidden")
}

// The plugin's subcommand list is the deal command tree's, hidden alias
// included, so `capsulectl plugin ls` names every verb the plugin answers.
func TestDealMetadataListsEveryDealSubcommand(t *testing.T) {
	var names []string
	for _, c := range dealCommands().Commands() {
		names = append(names, c.Name())
	}
	assert.ElementsMatch(t, DealMetadata.Subcommands, names)
}
