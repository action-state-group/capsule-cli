package cli

import (
	"fmt"
	"html"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every receipt states its own scope on its face, and nothing claims to be a
// complete record.
func TestDealReceiptsStateTheirScope(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(retailDemo, "open.json"))["deal_id"].(string)
	dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(retailDemo, "check-pay.json"))
	dir := t.TempDir()
	page, eml := filepath.Join(dir, "receipt.html"), filepath.Join(dir, "receipt.eml")
	dealRun(t, "report", "--deal", dealID, "--html", page, "--email", eml)

	const scope = "This receipt covers this one deal. It is not a record of everything the agent did."
	assert.Equal(t, scope, dealScopeLine)
	assert.Contains(t, string(mustRead(t, page)), scope, "the receipt page header")
	_, bodies, _ := emailParts(t, mustRead(t, eml))
	require.Len(t, bodies, 2)
	for kind, body := range bodies {
		assert.Contains(t, strings.ReplaceAll(body, "\r\n", "\n"), scope, kind)
		assert.Contains(t, html.UnescapeString(body), "What the agent did is the agent's own report", kind)
	}

	path := writeJSON(t, `{"id":"c1","at":"2026-09-27T18:05:00Z","tool":"make_payment","action":"pay","amount_minor":627,"currency":"USD","status":"succeeded"}`)
	result, err := reconcileRun(t, "--executions", path, "--to", "2026-09-28T00:00:00Z")
	require.NoError(t, err)
	assert.Equal(t, "This pass covers the execution records it was given, for this period. It is not a record of everything the agent did.", result["scope"])

	for _, text := range append([]string{string(mustRead(t, page)), fmt.Sprint(result)}, bodies["text/plain"], bodies["text/html"]) {
		lower := strings.ToLower(text)
		for _, claim := range []string{"all activity", "complete history", "complete record", "full history", "everything the agent did."} {
			if claim == "everything the agent did." {
				assert.NotContains(t, strings.ReplaceAll(lower, "not a record of everything the agent did.", ""), claim)
				continue
			}
			assert.NotContains(t, lower, claim)
		}
	}
}

// A host that records only a spend approval with a ceiling, not the payment
// or the charged amount: the deal still accounts for it, and the output says
// on its face what the source cannot show.
func TestDealReconcileSaysWhatASpendApprovalCannotShow(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(retailDemo, "open.json"))["deal_id"].(string)
	dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(retailDemo, "check-pay.json"))
	path := filepath.Join(bookingFixture, "..", "executions-spend-approval.jsonl")
	result, err := reconcileRun(t, "--executions", path, "--from", "2026-09-27T00:00:00Z", "--to", "2026-09-28T00:00:00Z")
	require.NoError(t, err)
	recorded := result["recorded"].([]any)
	require.Len(t, recorded, 1, "a $10.00 ceiling covers the $6.27 the deal checked")
	row := recorded[0].(map[string]any)
	assert.Equal(t, "ceiling", row["amount_kind"])
	assert.Equal(t, "approval", row["observed"])
	gaps := result["coverage"].(map[string]any)["cannot_see"].([]any)
	assert.Contains(t, gaps, dealCeilingGap)
	assert.Contains(t, gaps, dealApprovalOnlyGap)
	assert.Contains(t, gaps, dealNoApprovalsGap, "the user's authority is not shown without the host's approval history")

	// A ceiling below what the deal checked does not cover it.
	low := writeJSON(t, `{"id":"spend-2","at":"2026-09-27T18:00:40Z","tool":"spend_request","action":"pay","amount_minor":500,"amount_kind":"ceiling","currency":"USD","observed":"approval","status":"unknown"}`)
	_, err = reconcileRun(t, "--executions", low, "--from", "2026-09-27T00:00:00Z", "--to", "2026-09-28T00:00:00Z")
	require.ErrorIs(t, err, ErrPartial)
}

// Worker rows can hold filled form values in cleartext. None of it may reach
// reconcile's output, its errors, or the store, wherever a reader puts it.
func TestDealReconcileNeverCarriesPersonalDetails(t *testing.T) {
	dir := dealFixture(t)
	const email, phone, street = "jane.doe@example.com", "+15550100123", "12 Main Street"
	good := `{"id":"c1","at":"2026-09-27T18:05:00Z","tool":"browser_automation","action":"other","status":"succeeded"}`
	for _, bad := range []string{
		`{"id":"c2","at":"2026-09-27T18:05:00Z","tool":"browser_automation","action":"other","status":"succeeded","text_content":"email ` + email + ` phone ` + phone + ` address ` + street + `"}`,
		`{"id":"c2","at":"2026-09-27T18:05:00Z","tool":"browser_automation","action":"other","status":"succeeded","form_values":{"email":"` + email + `","phone":"` + phone + `","street":"` + street + `"}}`,
		`{"id":"` + email + `","at":"2026-09-27T18:05:00Z","tool":"browser_automation","action":"other","status":"succeeded"}`,
		`{"id":"c2","at":"2026-09-27T18:05:00Z","task":"` + phone + `","tool":"browser_automation","action":"other","status":"succeeded"}`,
		`{"id":"c2","at":"2026-09-27T18:05:00Z","tool":"` + street + `","action":"other","status":"succeeded"}`,
		`{"id":"c2","at":"2026-09-27T18:05:00Z","tool":"browser_automation","action":"pay","status":"unknown","merchant_domain":"` + email + `"}`,
		`{"id":"call` + phone + `","at":"2026-09-27T18:05:00Z","tool":"browser_automation","action":"other","status":"succeeded"}`,
		`{"id":"c2","at":"2026-09-27T18:05:00Z","parent_task":"jane.doe@example","tool":"browser_automation","action":"other","status":"succeeded"}`,
	} {
		out, err := invoke(t, "", "--profile", "deal", "deal", "reconcile", "--executions", writeJSON(t, good+"\n"+bad))
		require.ErrorIs(t, err, ErrInput, bad)
		for _, secret := range []string{email, phone, street, "jane.doe"} {
			assert.NotContains(t, out, secret, bad)
			assert.NotContains(t, err.Error(), secret, bad)
		}
	}
	raw := string(mustRead(t, filepath.Join(dir, "deal.db")))
	for _, secret := range []string{email, phone, street} {
		assert.NotContains(t, raw, secret)
	}
}

// The approval records take the same guard.
func TestDealReconcileApprovalsNeverCarryPersonalDetails(t *testing.T) {
	dealFixture(t)
	const email, phone, street = "jane.doe@example.com", "+15550100123", "12 Main Street"
	executions := writeJSON(t, `{"id":"c1","at":"2026-09-27T18:05:00Z","tool":"browser_automation","action":"other","status":"succeeded"}`)
	for _, bad := range []string{
		`{"id":"a1","at":"2026-09-27T18:00:00Z","decision":"approved","task":"` + email + `"}`,
		`{"id":"a` + phone + `","at":"2026-09-27T18:00:00Z","decision":"approved"}`,
		`{"id":"a1","at":"2026-09-27T18:00:00Z","decision":"approved","execution_id":"` + street + `"}`,
		`{"id":"a1","at":"2026-09-27T18:00:00Z","decision":"approved","note":"ship to ` + street + `"}`,
	} {
		out, err := invoke(t, "", "--profile", "deal", "deal", "reconcile", "--executions", executions, "--approvals", writeJSON(t, bad))
		require.ErrorIs(t, err, ErrInput, bad)
		for _, secret := range []string{email, phone, street, "jane.doe"} {
			assert.NotContains(t, out, secret, bad)
			assert.NotContains(t, err.Error(), secret, bad)
		}
	}
}

func TestDealDidLineNamesAnIndependentSourceOnlyWhenOneIsAttached(t *testing.T) {
	assert.Contains(t, dealDidLine(nil), "What the agent did is the agent's own report: no independent source, such as the merchant's own email, is attached.")
	with := dealDidLine([]string{"the merchant's own email"})
	assert.Contains(t, with, "with an independent source attached: the merchant's own email.")
	assert.NotContains(t, with, "no independent source")
	assert.Equal(t, dealDidLine(nil), dealDidLineOf(map[string]interface{}{}))
	b := map[string]interface{}{"extensions": map[string]interface{}{"x-deal-v0": map[string]interface{}{"did_line": with}}}
	assert.Equal(t, with, dealDidLineOf(b))
	assert.Contains(t, dealAssurance(b)["text"], with)
}
