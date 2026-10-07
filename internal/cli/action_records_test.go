package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The merchant-changed counterexample: the merchant changes from Example Air
// to travel-super-discount.example, so the deal check asks; the platform's own
// gate asks "Approve $558.80 purchase" and the user approves it there. That is
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

func openTyped(t *testing.T, input string) string {
	t.Helper()
	return dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, input))["deal_id"].(string)
}

func openTypedFlight(t *testing.T) string {
	t.Helper()
	id := openTyped(t, flightOpen)
	dealRun(t, "note", "--deal", id, "--kind", "change", "--input", writeJSON(t,
		`{"source":"checkout page","who":{"domain":"travel-super-discount.example","payee":"Travel Super Discount"}}`))
	return id
}

func openTypedSticker(t *testing.T) string {
	t.Helper()
	return openTyped(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"buy me an otter sticker under 8 bucks","max_total_minor":800,"allowed":["pay","commit","cancel"]},
		"who":{"name":"Sticker Marketplace","domain":"stickers.example"},
		"terms":{"item":"otter sticker","price_minor":600,"currency":"USD"},
		"recourse":{"rail":"card","refundable":true}}`)
}

func checkPay(t *testing.T, dealID string, amount int) map[string]any {
	t.Helper()
	return dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t,
		`{"action":"pay","amount_minor":`+strconv.Itoa(amount)+`,"terms":{"item":"otter sticker","price_minor":`+strconv.Itoa(amount)+`}}`))
}

func payNow(t *testing.T, dealID string, amount int) map[string]any {
	t.Helper()
	return dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":`+strconv.Itoa(amount)+`}`))
}

func platformApproval(t *testing.T, dealID, check, text, amount string) map[string]any {
	t.Helper()
	args := []string{"note", "--deal", dealID, "--kind", "platform_approval", "--check", check,
		"--provider", "example-platform", "--mechanism", "native-gate", "--text", text}
	if amount != "" {
		args = append(args, "--amount-minor", amount, "--currency", "USD")
	}
	return dealRun(t, args...)
}

func shownCardFile(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "card.txt")
	require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
	return path
}

// answerCheck seals an answer to a check; said "" is a card answer, card "" passes no shown card.
func answerCheck(t *testing.T, dealID, check, said, card string) (map[string]any, error) {
	t.Helper()
	args := []string{"--profile", "deal", "deal", "note", "--deal", dealID, "--kind", "approval", "--check", check, "--choice", "proceed"}
	if said != "" {
		args = append(args, "--said", said)
	}
	if card != "" {
		args = append(args, "--shown-card", shownCardFile(t, card))
	}
	out, err := invoke(t, "", args...)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m), out)
	return m, nil
}

// chainSteps is a deal's sealed steps.
func chainSteps(t *testing.T, dealID string) []sealedEvent {
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

// chainRecords is each step's sealed record, by capsule_id, as re-derived.
func chainRecords(t *testing.T, dealID string) map[string]map[string]any {
	t.Helper()
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.NoError(t, s.useDeal(t.Context(), dealID, false))
	events, err := s.load(t.Context(), dealID)
	require.NoError(t, err)
	out := map[string]map[string]any{}
	for i, se := range events {
		raw, _, err := encodeDealRecord(se.Event, events[:i], s.dkey)
		require.NoError(t, err)
		var record map[string]any
		require.NoError(t, json.Unmarshal(raw, &record))
		out[se.CapsuleID] = record
	}
	return out
}

func typeOf(record map[string]any) string {
	if t, ok := record["type"].(string); ok {
		return t
	}
	return "x-deal-v0:" + record["x-deal-v0"].(map[string]any)["record_type"].(string)
}

func bodyOf(record map[string]any) map[string]any { return record["body"].(map[string]any) }

func digestOfRef(v any) string { return v.(map[string]any)["digest"].(string) }

func stepDigest(t *testing.T, dealID, capsuleID string) string {
	t.Helper()
	for _, se := range chainSteps(t, dealID) {
		if se.CapsuleID == capsuleID {
			return se.Digest
		}
	}
	t.Fatalf("no step %s", capsuleID)
	return ""
}

func checkerPasses(t *testing.T, dealID string) {
	t.Helper()
	if python := profilePython(t); python != "" {
		export := filepath.Join(t.TempDir(), "chain.json")
		dealRun(t, "export", "--deal", dealID, "--output", export)
		result, err := exec.Command(python, dealProfileDir+"/check_profile.py", export).CombinedOutput()
		require.NoError(t, err, string(result))
		assert.Contains(t, string(result), "ALL OK")
	}
}

func TestTypedMerchantChangedPlatformApprovalDoesNotAnswerTheAsk(t *testing.T) {
	dealFixture(t)
	id := openTypedFlight(t)
	c := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, flightCheck))
	require.Equal(t, "pause", c["verdict"], "the merchant changed")
	check := c["check_id"].(string)
	assert.Nil(t, c["approval_id"])

	platform := platformApproval(t, id, check, "Approve $558.80 purchase", "55880")
	assert.Equal(t, false, platform["answers_check"], "a platform's approval never answers the deal check")

	paid := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, flightPay))
	require.Equal(t, true, paid["unchecked"], "the platform click alone authorizes nothing")
	assert.Equal(t, "no_sealed_approval", paid["rule"])
	assert.Contains(t, paid["reason"], "a platform's approval does not answer the deal check")

	approval, err := answerCheck(t, id, check, "yes, the new site is fine", c["card"].(string))
	require.NoError(t, err)
	paid = dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, flightPay))
	require.Equal(t, false, paid["unchecked"])
	require.Equal(t, approval["capsule_id"], paid["authorized_by"])

	steps := chainSteps(t, id)
	records := chainRecords(t, id)
	types := make([]string, len(steps))
	for i, se := range steps {
		types[i] = typeOf(records[se.CapsuleID])
	}
	assert.Equal(t, []string{"x-deal-v0:baseline", "task-authority/v0", "x-deal-v0:detail_change", "proposed-action/v0",
		"action-evaluation/v0", "action-approval/v0", "action-outcome/v0", "action-approval/v0", "action-record/v0"}, types,
		"one chain: evidence records and the typed action records")
	for _, r := range records {
		if tn, ok := r["type"].(string); ok {
			assert.Equal(t, strings.TrimPrefix(steps[0].Event.DealID, ""), r["chain_id"], tn)
			assert.NotContains(t, tn, "deal")
		}
	}

	ta := steps[1].Digest
	evaluation := bodyOf(records[check])
	assert.Equal(t, "ASK", evaluation["disposition"])
	assert.Equal(t, steps[3].Digest, evaluation["proposed_action_digest"], "the ProposedAction as checked")
	assert.Equal(t, ta, digestOfRef(evaluation["task_authority_ref"]))
	assert.Equal(t, "2026-09-27T18:15:00Z", evaluation["valid_until"], "15 minutes from the evaluation")
	assert.Equal(t, materialityPredicateDigest, evaluation["materiality_digest"])
	rules, err := rulesetDigest(materialityPredicateDigest)
	require.NoError(t, err)
	assert.Equal(t, rules, evaluation["ruleset_digest"])
	assert.Equal(t, []any{map[string]any{"type": "task_authority", "ref": evaluation["task_authority_ref"]}}, evaluation["authority_basis"])

	answerBody := bodyOf(records[approval["capsule_id"].(string)])
	assert.Equal(t, "action_state_approval", answerBody["authority"])
	assert.Equal(t, evaluation["rendering_commitment"], answerBody["rendering_commitment"], "what was shown is what was checked")
	platformBody := bodyOf(records[platform["capsule_id"].(string)])
	assert.Equal(t, "platform_approval", platformBody["authority"])
	assert.Equal(t, "user", platformBody["actor"])
	assert.EqualValues(t, 55880, platformBody["amount_minor"])

	action := bodyOf(records[paid["capsule_id"].(string)])
	assert.Equal(t, stepDigest(t, id, check), digestOfRef(action["evaluation_ref"]))
	assert.Equal(t, []any{
		map[string]any{"type": "task_authority", "ref": typedRef(ta)},
		map[string]any{"type": "action_state_approval", "ref": typedRef(stepDigest(t, id, approval["capsule_id"].(string)))},
		map[string]any{"type": "platform_approval", "ref": typedRef(stepDigest(t, id, platform["capsule_id"].(string)))},
	}, toPlain(t, action["authority_basis"]))

	// The check response is the sealed evaluation.
	resp := c["check_response"].(map[string]any)
	for _, k := range []string{"disposition", "valid_until", "proposed_action_digest", "ruleset_digest", "materiality_digest", "authority_basis", "task_authority_ref"} {
		assert.Equal(t, toPlain(t, evaluation[k]), toPlain(t, resp[k]), k)
	}
	assert.Equal(t, stepDigest(t, id, check), digestOfRef(resp["evaluation_ref"]))
	checkerPasses(t, id)
}

func toPlain(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	var out any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

// A card answered with no words of the user's satisfies no ask.
func TestTypedCardAnswerDoesNotAnswerAnAsk(t *testing.T) {
	dealFixture(t)
	id := openTypedFlight(t)
	c := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, flightCheck))
	card, err := answerCheck(t, id, c["check_id"].(string), "", c["card"].(string))
	require.NoError(t, err)
	paid := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, flightPay))
	assert.Equal(t, true, paid["unchecked"])
	assert.Equal(t, "ask_needs_your_words", paid["rule"])
	assert.Equal(t, "card_answer", bodyOf(chainRecords(t, id)[card["capsule_id"].(string)])["authority"])
	checkerPasses(t, id)
}

// The user's answer to an ask is bound to what they were shown.
func TestTypedAnswerToAnAskNeedsTheShownCard(t *testing.T) {
	dealFixture(t)
	id := openTypedFlight(t)
	c := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, flightCheck))
	_, err := answerCheck(t, id, c["check_id"].(string), "yes", "")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "--shown-card")
	_, err = answerCheck(t, id, c["check_id"].(string), "yes", "Pay Example Air $558.80 · [Pay]")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "the card shown is not the card checked")
}

// DO, within your rules: no approval is sealed; the action rests on the task
// authority, with a platform approval observed for the same evaluation beside it.
func TestTypedDORestsOnTaskAuthority(t *testing.T) {
	dealFixture(t)
	id := openTypedSticker(t)
	c := checkPay(t, id, 600)
	require.Equal(t, "pass", c["verdict"])
	assert.Nil(t, c["approval_id"], "a DO seals no approval")
	assert.Equal(t, "DO", c["check_response"].(map[string]any)["disposition"])
	platform := platformApproval(t, id, c["check_id"].(string), "Buy otter sticker for $6.00", "600")
	paid := payNow(t, id, 600)
	require.Equal(t, c["check_id"], paid["authorized_by"], "the evaluation itself authorizes")

	steps := chainSteps(t, id)
	action := bodyOf(chainRecords(t, id)[paid["capsule_id"].(string)])
	assert.Equal(t, []any{
		map[string]any{"type": "task_authority", "ref": typedRef(steps[1].Digest)},
		map[string]any{"type": "platform_approval", "ref": typedRef(stepDigest(t, id, platform["capsule_id"].(string)))},
	}, toPlain(t, action["authority_basis"]))
	// One evaluation covers one step.
	again := payNow(t, id, 600)
	assert.Equal(t, "approval_already_used", again["rule"])
	checkerPasses(t, id)
}

// A platform approval stating another amount is a scope mismatch; one
// observed for an earlier evaluation of the action is not listed.
func TestTypedPlatformApprovalScope(t *testing.T) {
	dealFixture(t)
	id := openTypedSticker(t)
	old := checkPay(t, id, 600)
	platformApproval(t, id, old["check_id"].(string), "Buy otter sticker for $6.00", "600")
	c := checkPay(t, id, 600)
	mismatch := platformApproval(t, id, c["check_id"].(string), "Buy otter sticker for $9.99", "999")
	paid := payNow(t, id, 600)
	basis := toPlain(t, bodyOf(chainRecords(t, id)[paid["capsule_id"].(string)])["authority_basis"]).([]any)
	require.Len(t, basis, 2, "the one observed for the earlier evaluation is not listed")
	assert.Equal(t, map[string]any{"type": "platform_approval", "scope": "mismatch",
		"ref": toPlain(t, typedRef(stepDigest(t, id, mismatch["capsule_id"].(string))))}, basis[1])
	checkerPasses(t, id)
}

// The task authority in force: after the user confirms new limits, the new
// version, which names the one it replaces and carries their words.
func TestTypedTaskAuthorityIsTheLimitsInForce(t *testing.T) {
	dealFixture(t)
	id := openTypedSticker(t)
	note := dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"up to 50","max_total_minor":5000,"allowed":["pay","commit","cancel"]}`))
	confirmed := dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", note["capsule_id"].(string), "--choice", "confirm_limits", "--said", "yes, up to 50")
	records := chainRecords(t, id)
	version := records[confirmed["capsule_id"].(string)]
	assert.Equal(t, "task-authority/v0", version["type"])
	assert.Equal(t, chainSteps(t, id)[1].Digest, digestOfRef(bodyOf(version)["previous_ref"]))
	assert.Regexp(t, `^[0-9a-f]{64}$`, bodyOf(version)["said_commitment"])
	assert.EqualValues(t, 5000, bodyOf(version)["max_total_minor"])

	c := checkPay(t, id, 600)
	paid := payNow(t, id, 600)
	evaluation := bodyOf(chainRecords(t, id)[c["check_id"].(string)])
	assert.Equal(t, stepDigest(t, id, confirmed["capsule_id"].(string)), digestOfRef(evaluation["task_authority_ref"]))
	basis := toPlain(t, bodyOf(chainRecords(t, id)[paid["capsule_id"].(string)])["authority_basis"]).([]any)
	assert.Equal(t, stepDigest(t, id, confirmed["capsule_id"].(string)), digestOfRef(basis[0].(map[string]any)["ref"]))
	checkerPasses(t, id)
}

// A check older than its valid_until is refused.
func TestTypedStaleCheckIsRefused(t *testing.T) {
	dealFixture(t)
	id := openTypedSticker(t)
	require.Equal(t, "pass", checkPay(t, id, 600)["verdict"])
	later := dealClock().Add(16 * time.Minute)
	old := dealClock
	dealClock = func() time.Time { return later }
	t.Cleanup(func() { dealClock = old })
	paid := payNow(t, id, 600)
	assert.Equal(t, true, paid["unchecked"])
	assert.Equal(t, "stale_check", paid["rule"])
	checkerPasses(t, id)
}

// An approval covers exactly the ProposedAction its evaluation names: once the
// action changes, the new evaluation has another proposed_action_digest, and
// the old answer covers nothing.
func TestTypedApprovalDoesNotSurviveAChangedProposedAction(t *testing.T) {
	dealFixture(t)
	id := openTypedFlight(t)
	first := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, flightCheck))
	_, err := answerCheck(t, id, first["check_id"].(string), "yes, the new site is fine", first["card"].(string))
	require.NoError(t, err)
	changed := strings.Replace(flightCheck, "55880", "61000", 1)
	second := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, changed))
	require.Equal(t, "pause", second["verdict"])
	a := first["check_response"].(map[string]any)["proposed_action_digest"]
	b := second["check_response"].(map[string]any)["proposed_action_digest"]
	assert.NotEqual(t, a, b)
	paid := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, strings.Replace(flightPay, "55880", "61000", 1)))
	assert.Equal(t, true, paid["unchecked"], "the earlier answer was for another ProposedAction")
	assert.Equal(t, "no_sealed_approval", paid["rule"])
	checkerPasses(t, id)
}

// check-request/v0 and check-response/v0 round-trip every field of the
// contract, and each validates against its schema.
func TestCheckContractRoundTrips(t *testing.T) {
	ref := func(c string) *CheckRef {
		return &CheckRef{Type: typedRecordRef, DigestAlg: "SHA-256", Digest: strings.Repeat(c, 64)}
	}
	req := CheckRequest{
		Type: typeCheckRequest, Phase: "proposed", Platform: &CheckPlatform{ID: "example-platform", DisplayName: "Example Platform"},
		RulesetRef: "rules/everyday", RulesetDigest: strings.Repeat("1", 64), TaskAuthorityRef: ref("2"), RootIntentRef: ref("3"),
		ParentDelegationRef: nil, ProposedAction: map[string]interface{}{"action": "pay", "amount_minor": float64(55880)},
		HistoryRefs: []CheckRef{*ref("4")}, AgentAssessment: map[string]interface{}{"note": "merchant changed"},
		PlatformApprovalRefs: []CheckRef{*ref("5")},
	}
	resp := CheckResponse{
		Type: typeCheckResponse, Disposition: "ASK", ValidUntil: "2026-10-06T09:28:00Z", ProposedActionDigest: strings.Repeat("6", 64),
		Findings:       []CheckFinding{{Question: "who", Rule: "payee_or_contact_changed", Field: "payee"}},
		AuthorityBasis: []CheckAuthority{{Type: "task_authority", Ref: *ref("2")}},
		EvaluationRef:  *ref("7"), RulesetDigest: strings.Repeat("1", 64), TaskAuthorityRef: *ref("2"), MaterialityDigest: strings.Repeat("8", 64),
	}
	for typeName, v := range map[string]any{typeCheckRequest: &req, typeCheckResponse: &resp} {
		raw, err := json.Marshal(v)
		require.NoError(t, err)
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		require.NoError(t, err)
		schema, err := recordSchema(typeName)
		require.NoError(t, err)
		require.NoError(t, schema.Validate(doc), typeName)
	}
	raw, err := json.Marshal(resp)
	require.NoError(t, err)
	var back CheckResponse
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, resp, back)
	for _, field := range []string{"valid_until", "authority_basis", "proposed_action_digest", "evaluation_ref", "ruleset_digest", "task_authority_ref", "materiality_digest"} {
		assert.Contains(t, string(raw), `"`+field+`"`)
	}
	raw, err = json.Marshal(req)
	require.NoError(t, err)
	var backReq CheckRequest
	require.NoError(t, json.Unmarshal(raw, &backReq))
	assert.Equal(t, req, backReq)
}

// No new record type, schema file or field name carries "deal", and the
// schemas ship identically with the skill.
func TestNoNewRecordTypeCarriesDeal(t *testing.T) {
	entries, err := recordSchemaFiles.ReadDir("assets/records")
	require.NoError(t, err)
	key := regexp.MustCompile(`"([a-z_]+)":`)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), "deal")
		raw, err := recordSchemaFiles.ReadFile("assets/records/" + e.Name())
		require.NoError(t, err)
		for _, m := range key.FindAllStringSubmatch(string(raw), -1) {
			assert.NotContains(t, m[1], "deal", "%s: field %s", e.Name(), m[1])
		}
		shipped, err := os.ReadFile(filepath.Join(dealProfileDir, "records", e.Name()))
		require.NoError(t, err)
		assert.Equal(t, string(shipped), string(raw), "the skill ships %s unchanged", e.Name())
	}
	for _, n := range []string{typeTaskAuthority, typeProposedAction, typeActionEvaluation, typeActionApproval, typeActionRecord, typeActionOutcome, typeActionReport} {
		assert.NotContains(t, n, "deal")
	}
}

// The rule table an evaluation's ruleset_digest names is exactly the rules
// evaluateDeal emits, and ruleset_digest covers the materiality predicate.
func TestRulesetDigestCoversRulesAndMateriality(t *testing.T) {
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
	a, err := rulesetDigest(strings.Repeat("a", 64))
	require.NoError(t, err)
	b, err := rulesetDigest(strings.Repeat("b", 64))
	require.NoError(t, err)
	assert.NotEqual(t, a, b, "a different materiality predicate is a different ruleset")
}

// A policy-change confirmation binds what the user was shown, the policy
// that takes effect and the semantic diff.
func TestPolicyChangeApprovalShape(t *testing.T) {
	h := strings.Repeat("c", 64)
	record := map[string]interface{}{
		"type": typeActionApproval, "canonicalization": "jcs", "chain_id": "chain-" + strings.Repeat("d", 16), "seq": 2,
		"at": "2026-10-07T09:00:00Z", "prev": typedRef(h), "chain_root": typedRef(h),
		"body": policyChangeApproval(strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64), strings.Repeat("4", 64)),
	}
	schema, err := recordSchema(typeActionApproval)
	require.NoError(t, err)
	require.NoError(t, schema.Validate(toPlain(t, record)))
	delete(record["body"].(map[string]interface{}), "semantic_diff_digest")
	assert.Error(t, schema.Validate(toPlain(t, record)), "a policy change binds its semantic diff")
}

// A deal opened without --records typed keeps x-deal-v0 records throughout,
// as before: a standing approval on a pass, no typed record.
func TestDealsStayXDealV0ByDefault(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"buy me an otter sticker under 8 bucks","max_total_minor":800,"allowed":["pay"]},
		"who":{"name":"Sticker Marketplace","domain":"stickers.example"},
		"terms":{"item":"otter sticker","price_minor":600,"currency":"USD"},"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	c := checkPay(t, id, 600)
	require.NotNil(t, c["approval_id"])
	assert.Nil(t, c["check_response"])
	paid := payNow(t, id, 600)
	require.Equal(t, c["approval_id"], paid["authorized_by"])
	for _, r := range chainRecords(t, id) {
		assert.Nil(t, r["type"], "x-deal-v0 throughout")
	}
	checkerPasses(t, id)
}

// The profile checker's typed fixtures name the materiality predicate and rule
// table capsulectl seals: regenerate them when either changes.
func TestTypedFixturesNameTheSealedRuleset(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(dealProfileDir, "fixtures", "positive-typed", "06-action-evaluation.json"))
	require.NoError(t, err)
	var f struct {
		Record struct {
			Body map[string]any `json:"body"`
		} `json:"record"`
	}
	require.NoError(t, json.Unmarshal(raw, &f))
	assert.Equal(t, materialityPredicateDigest, f.Record.Body["materiality_digest"])
	rules, err := rulesetDigest(materialityPredicateDigest)
	require.NoError(t, err)
	assert.Equal(t, rules, f.Record.Body["ruleset_digest"])
}

// Several findings are one prompt: one evaluation lists them all, one card
// renders them, and the user's one answer, citing that evaluation, covers
// every one.
func TestTypedOnePromptForSeveralFindings(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, flightOpen)
	check := strings.NewReplacer(`"price_minor":55880}`, `"price_minor":57900}`, `"amount_minor":55880`, `"amount_minor":57900`,
		`"refundable":true}}`, `"refundable":false}}`, `"domain":"travel-super-discount.example",`, ``).Replace(flightCheck)
	c := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, check))
	require.Equal(t, "pause", c["verdict"])
	approval, err := answerCheck(t, id, c["check_id"].(string), "yes to all of it", c["card"].(string))
	require.NoError(t, err)
	paid := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, strings.Replace(flightPay, "55880", "57900", 1)))
	require.Equal(t, false, paid["unchecked"])

	records := chainRecords(t, id)
	var evaluations, prompts []map[string]any
	for _, r := range records {
		switch {
		case r["type"] == typeActionEvaluation:
			evaluations = append(evaluations, r)
		case r["type"] == typeActionApproval && bodyOf(r)["authority"] == "action_state_approval":
			prompts = append(prompts, r)
		}
	}
	require.Len(t, evaluations, 1)
	require.Len(t, prompts, 1, "one prompt")
	findings := bodyOf(evaluations[0])["findings"].([]any)
	questions := map[string]bool{}
	for _, f := range findings {
		questions[f.(map[string]any)["question"].(string)] = true
	}
	assert.Len(t, findings, 3, "%v", findings)
	assert.Equal(t, map[string]bool{"who": true, "terms": true, "recourse": true}, questions)
	assert.Equal(t, stepDigest(t, id, c["check_id"].(string)), digestOfRef(prompts[0]["refs"].([]any)[0]),
		"the one answer cites the evaluation that lists every finding")
	assert.Equal(t, approval["capsule_id"], paid["authorized_by"])
	checkerPasses(t, id)
}
