package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const retailDemo = "../../skills/deal/demo/retail-checkout"

var procedureStep = regexp.MustCompile(`^\d+\. (.+)$`)

// procedureSteps returns the numbered steps of one procedure in SKILL.md.
func procedureSteps(t *testing.T, name string) []string {
	t.Helper()
	raw, err := os.ReadFile("../../skills/deal/SKILL.md")
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

// reconcileRun runs `deal reconcile` and decodes its result, which is printed
// whether or not anything is unrecorded.
func reconcileRun(t *testing.T, args ...string) (map[string]any, error) {
	t.Helper()
	out, err := invoke(t, "", append([]string{"--profile", "deal", "deal", "reconcile"}, args...)...)
	var m map[string]any
	require.NoError(t, json.NewDecoder(strings.NewReader(out)).Decode(&m), out)
	return m, err
}

func TestDealProceduresEndInTheFinalReview(t *testing.T) {
	for _, name := range []string{"Purchase", "Booking", "Signature", "Disclosure"} {
		steps := procedureSteps(t, name)
		review, act := -1, -1
		for i, step := range steps {
			if strings.HasPrefix(step, "**Final review:**") {
				assert.Equal(t, -1, review, "%s has one final review", name)
				review = i
				assert.Contains(t, step, "`deal open`", name)
				assert.Contains(t, step, "`deal check`", name)
			}
			if strings.HasPrefix(step, "On `\"proceed\": true`") {
				act = i
			}
		}
		require.NotEqual(t, -1, review, "%s has a final review step", name)
		require.NotEqual(t, -1, act, "%s has an acting step", name)
		assert.Less(t, review, act, "%s reviews before it acts", name)
	}
}

func TestDealSkillSaysInvocationIsAdvisoryAndRetailIsInScope(t *testing.T) {
	raw, err := os.ReadFile("../../skills/deal/SKILL.md")
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
// as a sub-task would leave them; the reconciliation reads those records.
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
		case strings.HasPrefix(step, "**Final review:**"):
			require.Contains(t, step, "`deal open`")
			opened := dealRun(t, "open", "--input", filepath.Join(retailDemo, "open.json"))
			dealID = opened["deal_id"].(string)
			require.Contains(t, step, "`deal check`")
			check := dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(retailDemo, "check-pay.json"))
			assert.Equal(t, "pass", check["verdict"], "a known merchant at the agreed price passes quietly")
			assert.Equal(t, "", check["card"])
			proceed = check["proceed"] == true
		case strings.Contains(step, "place the order"):
			require.True(t, proceed, "the order is placed only after the final review")
			host("c3", "t-browser", "make_payment", "pay", `,"amount_minor":627,"currency":"USD","merchant_domain":"stickers.example","reference":"ORDER-1001"`)
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
	result, err := reconcileRun(t, "--executions", path, "--from", "2026-09-27T00:00:00Z", "--to", "2026-09-28T00:00:00Z")
	require.NoError(t, err)
	assert.Empty(t, result["unrecorded"])
	recorded := result["recorded"].([]any)
	require.Len(t, recorded, 1)
	assert.Equal(t, dealID, recorded[0].(map[string]any)["deal_id"])
	assert.Equal(t, "act", recorded[0].(map[string]any)["matched_by"])
	assert.Equal(t, float64(3), result["records_read"])
	assert.True(t, strings.HasPrefix(result["summary"].(string), "0 of 1 consequential actions have no deal record"))
	assert.Contains(t, result["summary"], "cannot prove that nothing else happened")
	gaps := result["coverage"].(map[string]any)["cannot_see"].([]any)
	assert.Len(t, gaps, len(dealCoverageGaps))
}

func TestDealReconcileListsAPaymentWithNoDeal(t *testing.T) {
	dealFixture(t)
	result, err := reconcileRun(t, "--executions", filepath.Join(bookingFixture, "..", "executions-unrecorded.jsonl"), "--from", "2026-09-27T00:00:00Z", "--to", "2026-09-28T00:00:00Z")
	require.ErrorIs(t, err, ErrPartial, "anything unrecorded exits partial")
	unrecorded := result["unrecorded"].([]any)
	require.Len(t, unrecorded, 1)
	assert.Equal(t, "c4", unrecorded[0].(map[string]any)["id"], "the sub-task's payment, not the parent's spawn and steer calls")
	assert.Equal(t, "t-browser", unrecorded[0].(map[string]any)["task"])
	assert.Len(t, result["failed_attempts"], 1)
	assert.Equal(t, float64(1), result["outside_period"])
	assert.Equal(t, float64(3), result["not_consequential"])
	assert.True(t, strings.HasPrefix(result["summary"].(string), "1 of 1 consequential actions have no deal record"))
	assert.NotEmpty(t, result["coverage"].(map[string]any)["cannot_see"])
}

func TestDealReconcileMatchesAChecksDealAndRefusesBadRecords(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(retailDemo, "open.json"))["deal_id"].(string)
	dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(retailDemo, "check-pay.json"))
	path := writeJSON(t, `{"id":"c1","at":"2026-09-27T18:05:00Z","tool":"make_payment","action":"pay","amount_minor":627,"currency":"USD","status":"unknown","deal_id":"`+dealID+`"}`)
	result, err := reconcileRun(t, "--executions", path, "--to", "2026-09-28T00:00:00Z")
	require.NoError(t, err)
	assert.Equal(t, "check", result["recorded"].([]any)[0].(map[string]any)["matched_by"])

	// A different amount is not the checked payment.
	path = writeJSON(t, `{"id":"c1","at":"2026-09-27T18:05:00Z","tool":"make_payment","action":"pay","amount_minor":9999,"status":"succeeded"}`)
	_, err = reconcileRun(t, "--executions", path, "--to", "2026-09-28T00:00:00Z")
	require.ErrorIs(t, err, ErrPartial)

	for _, bad := range []string{
		`{"id":"c1","at":"2026-09-27T18:05:00Z","tool":"make_payment","action":"pay","status":"succeeded","extra":1}`,
		`{"id":"c1","at":"yesterday","tool":"make_payment","action":"pay","status":"succeeded"}`,
		`{"id":"c1","at":"2026-09-27T18:05:00Z","tool":"make_payment","action":"refund","status":"succeeded"}`,
		`{"id":"c1","at":"2026-09-27T18:05:00Z","tool":"make_payment","action":"pay","status":"done"}`,
	} {
		_, err := invoke(t, "", "--profile", "deal", "deal", "reconcile", "--executions", writeJSON(t, bad))
		require.ErrorIs(t, err, ErrInput, bad)
	}
}
