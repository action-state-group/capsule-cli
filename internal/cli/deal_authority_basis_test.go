package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The merchant-changed counterexample: the merchant changes from Example Air to
// travel-super-discount.example, so the deal check asks; the platform's own gate
// asks "Approve $558.80 purchase" and the user clicks it. That click is
// recorded as the platform's approval and does not answer the deal check's
// question; only the user's own answer, on the card the check showed, does.

const flightOpen = `{"type":"purchase","channel":"web",
	"intent":{"verbatim":"book the SJC to HOU flight under $600","max_total_minor":60000,"allowed":["pay"]},
	"who":{"name":"Example Air","domain":"air.example","payee":"Example Air"},
	"terms":{"item":"SJC-HOU flight","price_minor":55880,"currency":"USD"},
	"recourse":{"rail":"card","refundable":true}}`

const flightCheck = `{"action":"pay","amount_minor":55880,
	"who":{"domain":"travel-super-discount.example","payee":"Travel Super Discount"},
	"terms":{"item":"SJC-HOU flight","price_minor":55880},
	"recourse":{"rail":"card","refundable":true}}`

const flightPay = `{"action":"pay","amount_minor":55880,"payee":"Travel Super Discount","rail":"card"}`

func openFlight(t *testing.T) string {
	t.Helper()
	id := dealRun(t, "open", "--input", writeJSON(t, flightOpen))["deal_id"].(string)
	dealRun(t, "note", "--deal", id, "--kind", "change", "--input", writeJSON(t,
		`{"source":"checkout page","who":{"domain":"travel-super-discount.example","payee":"Travel Super Discount"}}`))
	return id
}

func platformApproval(t *testing.T, dealID, check, text string, amount string) map[string]any {
	t.Helper()
	args := []string{"note", "--deal", dealID, "--kind", "platform_approval", "--check", check,
		"--provider", "example-platform", "--mechanism", "native-gate", "--text", text}
	if amount != "" {
		args = append(args, "--amount-minor", amount, "--currency", "USD")
	}
	return dealRun(t, args...)
}

func basisOf(t *testing.T, record map[string]any) []map[string]any {
	t.Helper()
	raw, ok := record["body"].(map[string]any)["authority_basis"].([]any)
	require.True(t, ok, "the record carries authority_basis")
	out := make([]map[string]any, len(raw))
	for i, e := range raw {
		out[i] = e.(map[string]any)
	}
	return out
}

func recordDigestOf(t *testing.T, dealID, capsuleID string) string {
	t.Helper()
	for _, se := range dealSteps(t, dealID) {
		if se.CapsuleID == capsuleID {
			return se.Digest
		}
	}
	t.Fatalf("no step %s", capsuleID)
	return ""
}

func dealSteps(t *testing.T, dealID string) []sealedEvent {
	t.Helper()
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.NoError(t, s.useDeal(t.Context(), dealID, false))
	events, err := s.load(t.Context(), dealID)
	require.NoError(t, err)
	return events
}

func profileChecks(t *testing.T, dealID string) {
	t.Helper()
	if python := profilePython(t); python != "" {
		export := filepath.Join(t.TempDir(), "deal.json")
		dealRun(t, "export", "--deal", dealID, "--output", export)
		result, err := exec.Command(python, dealProfileDir+"/check_profile.py", export).CombinedOutput()
		require.NoError(t, err, string(result))
		assert.Contains(t, string(result), "ALL OK")
	}
}

func TestDealMerchantChangedPlatformApprovalDoesNotAnswerTheAsk(t *testing.T) {
	dealFixture(t)
	id := openFlight(t)
	c := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, flightCheck))
	require.Equal(t, "pause", c["verdict"], "the merchant changed")
	check := c["check_id"].(string)

	platform := platformApproval(t, id, check, "Approve $558.80 purchase", "55880")
	assert.Equal(t, false, platform["answers_check"], "a platform's approval never answers the deal check")

	// The platform click alone: the act is not authorized.
	paid := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, flightPay))
	require.Equal(t, true, paid["unchecked"])
	assert.Equal(t, "no_sealed_approval", paid["rule"])
	assert.Contains(t, paid["reason"], "a platform's approval does not answer the deal check")

	// The user's own answer, on the card the check showed.
	approval, err := answer(t, id, check, "yes, the new site is fine", shownCard(t, c["card"].(string)))
	require.NoError(t, err)
	paid = dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, flightPay))
	require.Equal(t, false, paid["unchecked"])
	require.Equal(t, approval["capsule_id"], paid["authorized_by"])

	records := dealRecords(t, id)
	steps := dealSteps(t, id)
	basis := basisOf(t, records[paid["capsule_id"].(string)])
	require.Len(t, basis, 3)
	assert.Equal(t, "task_authority", basis[0]["type"])
	assert.Equal(t, steps[0].Digest, basis[0]["ref"].(map[string]any)["digest"], "the opening ask")
	assert.Equal(t, "action_state_approval", basis[1]["type"])
	assert.Equal(t, recordDigestOf(t, id, approval["capsule_id"].(string)), basis[1]["ref"].(map[string]any)["digest"])
	verdict := records[check]["body"].(map[string]any)
	assert.Equal(t, verdict["card_commitment"], basis[1]["binding"].(map[string]any)["card_commitment"], "bound to the card checked")
	assert.Equal(t, "platform_approval", basis[2]["type"])
	assert.Equal(t, "example-platform", basis[2]["provider"])
	assert.Equal(t, "native-gate", basis[2]["mechanism"])
	assert.Equal(t, recordDigestOf(t, id, platform["capsule_id"].(string)), basis[2]["ref"].(map[string]any)["digest"])
	assert.Nil(t, basis[2]["scope"], "same amount: no mismatch")

	// The PRD section 13 check contract on the evaluation, and the payment's
	// evaluation_ref back to it.
	var checkRecord string
	for i, se := range steps {
		if se.CapsuleID == check {
			checkRecord = steps[i-1].Digest // the ProposedAction as checked, sealed just before its verdict
		}
	}
	rules, err := dealRulesetDigest()
	require.NoError(t, err)
	assert.Equal(t, "ASK", verdict["disposition"])
	assert.Equal(t, checkRecord, verdict["proposed_action_digest"])
	assert.Equal(t, steps[0].Digest, verdict["task_authority_ref"].(map[string]any)["digest"])
	assert.Equal(t, "2026-09-27T18:15:00Z", verdict["valid_until"], "the default 15 minutes from the evaluation")
	assert.Equal(t, rules, verdict["ruleset_digest"])
	assert.Equal(t, []any{map[string]any{"type": "task_authority", "ref": verdict["task_authority_ref"]}}, verdict["authority_basis"])
	assert.Equal(t, recordDigestOf(t, id, check), records[paid["capsule_id"].(string)]["body"].(map[string]any)["evaluation_ref"].(map[string]any)["digest"])

	p := records[platform["capsule_id"].(string)]
	assert.Equal(t, "platform_approval", p["x-deal-v0"].(map[string]any)["record_type"])
	body := p["body"].(map[string]any)
	assert.Equal(t, "user", body["actor"])
	assert.Regexp(t, `^[0-9a-f]{64}$`, body["approval_text_commitment"])
	assert.EqualValues(t, 55880, body["amount_minor"])
	profileChecks(t, id)
}

// A click on a card answers no ASK: an ASK needs the user's own approval.
func TestDealV1CardClickDoesNotAnswerAnAsk(t *testing.T) {
	dealFixture(t)
	id := openFlight(t)
	c := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, flightCheck))
	_, err := answer(t, id, c["check_id"].(string), "", shownCard(t, c["card"].(string)))
	require.NoError(t, err)
	paid := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, flightPay))
	assert.Equal(t, true, paid["unchecked"])
	assert.Equal(t, "ask_needs_your_words", paid["rule"])
	profileChecks(t, id)
}

// In v1 the user's answer to an ASK is bound to the card it was given on.
func TestDealV1AnswerToAnAskNeedsTheShownCard(t *testing.T) {
	dealFixture(t)
	id := openFlight(t)
	c := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, flightCheck))
	_, err := answer(t, id, c["check_id"].(string), "yes", "")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "--shown-card")
}

// DO, within your rules: on a pass the act rests on the task authority alone,
// and a platform approval observed for the same action is listed beside it.
func TestDealV1PassRestsOnTaskAuthority(t *testing.T) {
	dealFixture(t)
	id := openSticker(t, true)
	c := checkAct(t, id, "pay", 600)
	require.Equal(t, "pass", c["verdict"])
	platform := platformApproval(t, id, c["check_id"].(string), "Buy otter sticker for $6.00", "600")
	paid := act(t, id, "pay", 600)
	require.Equal(t, c["approval_id"], paid["authorized_by"])

	basis := basisOf(t, dealRecords(t, id)[paid["capsule_id"].(string)])
	require.Len(t, basis, 2)
	assert.Equal(t, "task_authority", basis[0]["type"])
	assert.Equal(t, "platform_approval", basis[1]["type"])
	assert.Equal(t, recordDigestOf(t, id, platform["capsule_id"].(string)), basis[1]["ref"].(map[string]any)["digest"])
	profileChecks(t, id)
}

// A platform approval for a different amount is listed as a scope mismatch;
// one observed for an earlier check of the action is stale and not listed.
func TestDealV1PlatformApprovalScope(t *testing.T) {
	dealFixture(t)
	id := openSticker(t, true)
	old := checkAct(t, id, "pay", 600)
	platformApproval(t, id, old["check_id"].(string), "Buy otter sticker for $6.00", "600")
	c := checkAct(t, id, "pay", 600)
	require.Equal(t, "pass", c["verdict"])
	mismatch := platformApproval(t, id, c["check_id"].(string), "Buy otter sticker for $9.99", "999")
	paid := act(t, id, "pay", 600)

	basis := basisOf(t, dealRecords(t, id)[paid["capsule_id"].(string)])
	require.Len(t, basis, 2, "the stale one, for the earlier check, is not listed")
	assert.Equal(t, recordDigestOf(t, id, mismatch["capsule_id"].(string)), basis[1]["ref"].(map[string]any)["digest"])
	assert.Equal(t, "mismatch", basis[1]["scope"])
	profileChecks(t, id)
}

// The task authority is the limits in force: after a confirm_limits answer,
// that answer.
func TestDealV1TaskAuthorityIsTheLimitsInForce(t *testing.T) {
	dealFixture(t)
	id := openSticker(t, true)
	note := dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"up to 50","max_total_minor":5000,"allowed":["pay","commit","cancel"]}`))
	confirmed := dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", note["capsule_id"].(string), "--choice", "confirm_limits", "--said", "yes, up to 50")
	c := checkAct(t, id, "pay", 600)
	require.Equal(t, "pass", c["verdict"])
	paid := act(t, id, "pay", 600)
	basis := basisOf(t, dealRecords(t, id)[paid["capsule_id"].(string)])
	assert.Equal(t, recordDigestOf(t, id, confirmed["capsule_id"].(string)), basis[0]["ref"].(map[string]any)["digest"])
	profileChecks(t, id)
}

// New deals are x-deal-v1; a deal opened as v0 keeps the v0 records (no
// authority_basis) and both read back and pass the profile checker.
func TestDealProfileVersionPerDeal(t *testing.T) {
	dealFixture(t)
	v1 := openSticker(t, true)
	v0 := dealRun(t, "open", "--profile-version", "x-deal-v0", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"buy me an otter sticker under 8 bucks","max_total_minor":800,"allowed":["pay"]},
		"who":{"name":"Sticker Marketplace","domain":"stickers.example"},
		"terms":{"item":"otter sticker","price_minor":600,"currency":"USD"},"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	for id, want := range map[string]string{v1: "x-deal-v1", v0: "x-deal-v0"} {
		c := checkAct(t, id, "pay", 600)
		paid := act(t, id, "pay", 600)
		require.Equal(t, c["approval_id"], paid["authorized_by"])
		for _, r := range dealRecords(t, id) {
			assert.Equal(t, want, r["x-deal-v0"].(map[string]any)["profile"])
		}
		_, has := dealRecords(t, id)[paid["capsule_id"].(string)]["body"].(map[string]any)["authority_basis"]
		assert.Equal(t, want == "x-deal-v1", has)
		dealRun(t, "report", "--deal", id)
		profileChecks(t, id)
	}
}

// v1 compares terms and refundability too: an approval covers the action
// only under the terms and refund terms in force when it was checked.
func TestDealV1ScopeIncludesTermsAndRefundability(t *testing.T) {
	refundable, nonRefundable := true, false
	price, other := int64(600), int64(900)
	atCheck := dealState{terms: dealTerms{Item: "otter sticker", PriceMinor: &price}, recourse: dealRecourse{Rail: "card", Refundable: &refundable}}
	assert.Equal(t, "", scopeMismatch(atCheck, atCheck))
	terms := atCheck
	terms.terms = dealTerms{Item: "otter sticker", PriceMinor: &other}
	assert.Equal(t, "the terms differ from the ones checked", scopeMismatch(atCheck, terms))
	refund := atCheck
	refund.recourse = dealRecourse{Rail: "card", Refundable: &nonRefundable}
	assert.Equal(t, "the refund terms differ from the ones checked", scopeMismatch(atCheck, refund))
}

// PRD section 14: an evaluation covers the action until its valid_until.
func TestDealV1StaleCheckDoesNotCover(t *testing.T) {
	dealFixture(t)
	id := openSticker(t, true)
	c := checkAct(t, id, "pay", 600)
	require.Equal(t, "pass", c["verdict"])
	later := dealClock().Add(16 * time.Minute)
	old := dealClock
	dealClock = func() time.Time { return later }
	t.Cleanup(func() { dealClock = old })
	paid := act(t, id, "pay", 600)
	assert.Equal(t, true, paid["unchecked"])
	assert.Equal(t, "stale_check", paid["rule"])
	profileChecks(t, id)
}

// The built-in rule table whose digest a verdict names is exactly the rules
// evaluateDeal emits, and the profile checker's fixture names the same digest.
func TestDealBuiltinRuleTableIsTheRulesEvaluated(t *testing.T) {
	src, err := os.ReadFile("deal_rules.go")
	require.NoError(t, err)
	body := string(src[strings.Index(string(src), "func evaluateDeal("):])
	body = body[:strings.Index(body, "\n}\n")]
	emitted := map[string]bool{}
	for _, m := range regexp.MustCompile(`add\("([a-z]+)", "([a-z_]+)"`).FindAllStringSubmatch(body, -1) {
		emitted[m[1]+"/"+m[2]] = true
	}
	listed := map[string]bool{}
	for _, q := range dealBuiltinRules {
		for _, r := range q.Rules {
			listed[q.Question+"/"+r] = true
		}
	}
	assert.Equal(t, emitted, listed)

	raw, err := os.ReadFile(dealProfileDir + "/fixtures/positive-v1/05-verdict-pause.json")
	require.NoError(t, err)
	var fixture struct {
		Record struct {
			Body struct {
				RulesetDigest string `json:"ruleset_digest"`
			} `json:"body"`
		} `json:"record"`
	}
	require.NoError(t, json.Unmarshal(raw, &fixture))
	digest, err := dealRulesetDigest()
	require.NoError(t, err)
	assert.Equal(t, fixture.Record.Body.RulesetDigest, digest, "Go and the profile checker digest the same table")
}
