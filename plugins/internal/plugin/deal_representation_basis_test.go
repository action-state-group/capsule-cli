package plugin

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A seller's representations (Seller PRD §3.5, §9 step 2; capsule-cli#269).
// The fixture is a sale whose task authority orders a condition statement
// ("tell them about the scratch"), with a thread per buyer.
const repFixture = "testdata/deal/seller-authority-basis"

func repFile(name string) string { return filepath.Join(repFixture, name) }

// repSale opens the fixture's sale and its two threads.
func repSale(t *testing.T) (sale, a, b string) {
	t.Helper()
	sale = dealRun(t, "sale", "new", "--input", repFile("sale.json"))["sale_id"].(string)
	a = dealRun(t, "open", "--sale", sale, "--input", repFile("thread-a.json"))["deal_id"].(string)
	b = dealRun(t, "open", "--sale", sale, "--input", repFile("thread-b.json"))["deal_id"].(string)
	return sale, a, b
}

// sellerRepOpen is a standalone seller's deal that orders the given classes.
func sellerRepOpen(t *testing.T, ordered string) string {
	t.Helper()
	return openTyped(t, `{"type": "purchase", "demo": true, "channel": "web",
 "intent": {"verbatim": "Sell my example bicycle; ask 1900", "party_role": "seller",
            "asked": {"item": "example bicycle", "quantity": 1}, "allowed": ["offer", "commit", "share_contact", "cancel"],
            "ordered_representations": `+ordered+`},
 "who": {"name": "Example Buyer", "domain": "buyer.example"},
 "terms": {"item": "example bicycle", "quantity": 1, "price_minor": 190000, "currency": "USD"},
 "recourse": {"rail": "card", "refundable": false}}`)
}

func represent(t *testing.T, dealID, input string) map[string]any {
	t.Helper()
	return dealRun(t, "check", "--deal", dealID, "--input", input)
}

func claimWith(t *testing.T, dealID, check, input string) map[string]any {
	t.Helper()
	args := []string{"note", "--deal", dealID, "--kind", "claim", "--input", input}
	if check != "" {
		args = append(args, "--check", check)
	}
	return dealRun(t, args...)
}

// recordsOfType are the deal's exported records of a typed type, or of an
// x-deal-v0 record type.
func recordsOfType(t *testing.T, dealID, typ string) []map[string]any {
	t.Helper()
	records, _ := exportRecords(t, dealID)
	var out []map[string]any
	for _, r := range records {
		if r["type"] == typ {
			out = append(out, r)
		}
		if b, ok := r["x-deal-v0"].(map[string]any); ok && b["record_type"] == typ {
			out = append(out, r)
		}
	}
	return out
}

// claimRecord is the deal's sealed claim record of the given class (the
// last one).
func claimRecord(t *testing.T, dealID, class string) map[string]any {
	t.Helper()
	var out map[string]any
	for _, r := range recordsOfType(t, dealID, "claim") {
		if r["body"].(map[string]any)["class"] == class {
			out = r
		}
	}
	require.NotNil(t, out, "a %s claim is sealed", class)
	return out
}

// digestOfType is the digest of the deal's last record of a type.
func digestOfType(t *testing.T, dealID, typ string) string {
	t.Helper()
	rs := recordsOfType(t, dealID, typ)
	require.NotEmpty(t, rs)
	return recordDigest(t, rs[len(rs)-1])
}

func basisOf(r map[string]any) []any {
	b, _ := r["body"].(map[string]any)["authority_basis"].([]any)
	return b
}

func ruleOf(t *testing.T, check map[string]any, rule string) bool {
	t.Helper()
	for _, d := range check["differences"].([]any) {
		if d.(map[string]any)["rule"] == rule {
			return true
		}
	}
	return false
}

func optionIDs(check map[string]any) []string {
	var out []string
	for _, o := range check["options"].([]any) {
		out = append(out, o.(map[string]any)["id"].(string))
	}
	return out
}

// A representation the task authority orders is checked (DO) and sealed with
// authority_basis [task_authority], the thread's own task authority, in
// each thread, and no action-approval/v0 is made for it.
func TestAnOrderedRepresentationNeedsNoApproval(t *testing.T) {
	dealFixture(t)
	_, a, b := repSale(t)
	for _, thread := range []string{a, b} {
		before := len(recordsOfType(t, thread, cli.TypeActionApproval))
		ck := represent(t, thread, repFile("represent-condition.json"))
		require.Equal(t, "pass", ck["verdict"], "the task authority orders it: DO")
		assert.Contains(t, ck["approval_text"], `about the condition: "The frame has one small scratch by the seat"`)
		claim := claimWith(t, thread, ck["check_id"].(string), repFile("claim-condition.json"))
		assert.Equal(t, ck["check_id"], claim["authorized_by"], "authorized by its evaluation")
		assert.Equal(t, false, claim["unchecked"])

		assert.Len(t, recordsOfType(t, thread, cli.TypeActionApproval), before, "no approval event is manufactured for an ordered representation")
		r := claimRecord(t, thread, "condition")
		basis := basisOf(r)
		require.Len(t, basis, 1, "the task authority alone")
		entry := basis[0].(map[string]any)
		assert.Equal(t, "task_authority", entry["type"])
		assert.Equal(t, digestOfType(t, thread, cli.TypeTaskAuthority), entry["ref"].(map[string]any)["digest"], "this thread's own task authority")
		assert.Equal(t, digestOfType(t, thread, cli.TypeActionEvaluation), r["body"].(map[string]any)["evaluation_ref"].(map[string]any)["digest"])
		assert.Equal(t, digestOfType(t, thread, cli.TypeActionEvaluation), relDigest(r, "authorized_by"))
		assert.NotContains(t, r["body"], "unchecked")
		checked := recordsOfType(t, thread, cli.TypeProposedAction)
		representing := checked[len(checked)-1]["body"].(map[string]any)["representing"].(map[string]any)
		assert.Equal(t, representing["text_commitment"], r["body"].(map[string]any)["text_commitment"], "sealed under the check's nonce: the words said are the words checked")
		_, export := exportRecords(t, thread)
		checkProfile(t, export)
	}
}

// A representation outside the authority asks; only the user's own approval
// of those exact words authorizes it, and its basis carries that approval.
func TestARepresentationOutsideTheAuthorityNeedsTheUsersApproval(t *testing.T) {
	dealFixture(t)
	_, a, _ := repSale(t)
	ck := represent(t, a, repFile("represent-warranty.json"))
	require.Equal(t, "pause", ck["verdict"])
	assert.True(t, ruleOf(t, ck, ruleRepresentationOutside))
	assert.Contains(t, optionIDs(ck), "proceed")
	assert.Contains(t, ck["card"], `Tell Example Buyer A about the warranty: "I will cover the gears for 30 days"?`)
	assert.Contains(t, ck["card"], "Why I'm asking: it goes beyond what you asked me to tell them.")
	_, err := answerCheck(t, a, ck["check_id"].(string), "yes, offer the 30 days on the gears", ck["card"].(string))
	require.NoError(t, err)
	claim := claimWith(t, a, ck["check_id"].(string), repFile("claim-warranty.json"))
	require.Equal(t, false, claim["unchecked"])

	r := claimRecord(t, a, "warranty")
	basis := basisOf(r)
	require.Len(t, basis, 2, "the task authority, then the user's approval")
	assert.Equal(t, "task_authority", basis[0].(map[string]any)["type"])
	assert.Equal(t, "user_approval", basis[1].(map[string]any)["type"])
	approval := digestOfType(t, a, cli.TypeActionApproval)
	assert.Equal(t, approval, basis[1].(map[string]any)["ref"].(map[string]any)["digest"])
	assert.Equal(t, approval, relDigest(r, "authorized_by"))
	_, export := exportRecords(t, a)
	checkProfile(t, export)

	// The same approval covers one claim only.
	again := claimWith(t, a, ck["check_id"].(string), repFile("claim-warranty.json"))
	assert.Equal(t, true, again["unchecked"])
	assert.Equal(t, "approval_used", again["rule"])
}

// A claim nothing authorized is still sealed, unchecked, with the rule why,
// and carries no basis.
func TestAnUnauthorizedRepresentationIsSealedUnchecked(t *testing.T) {
	warranty := repFile("claim-warranty.json")
	other := writeJSON(t, `{"text": "I will cover the gears for 90 days", "source_kind": "agent", "class": "warranty"}`)
	refund := writeJSON(t, `{"text": "I will cover the gears for 30 days", "source_kind": "agent", "class": "refund_terms"}`)
	for name, tc := range map[string]struct {
		setup func(t *testing.T, a, b string) string // the check to name
		claim string
		rule  string
	}{
		"no check": {func(t *testing.T, a, b string) string { return "" }, warranty, "no_check"},
		"an unanswered ask": {func(t *testing.T, a, b string) string {
			return represent(t, a, repFile("represent-warranty.json"))["check_id"].(string)
		}, warranty, "not_approved"},
		"a declined ask": {func(t *testing.T, a, b string) string {
			ck := represent(t, a, repFile("represent-warranty.json"))
			dealRun(t, "note", "--deal", a, "--kind", "approval", "--check", ck["check_id"].(string), "--choice", "hold", "--said", "no, don't promise that",
				"--shown-card", shownCardFile(t, ck["card"].(string)))
			return ck["check_id"].(string)
		}, warranty, "not_approved"},
		"other words":            {func(t *testing.T, a, b string) string { return approvedWarranty(t, a) }, other, "differs_from_check"},
		"another class":          {func(t *testing.T, a, b string) string { return approvedWarranty(t, a) }, refund, "differs_from_check"},
		"another thread's check": {func(t *testing.T, a, b string) string { return approvedWarranty(t, b) }, warranty, "no_check"},
		"a stale check": {func(t *testing.T, a, b string) string {
			id := approvedWarranty(t, a)
			setDealClock(t, "2026-09-27T19:00:00Z")
			return id
		}, warranty, "check_expired"},
	} {
		t.Run(name, func(t *testing.T) {
			dealFixture(t)
			_, a, b := repSale(t)
			check := tc.setup(t, a, b)
			before := len(recordsOfType(t, a, "claim"))
			out := claimWith(t, a, check, tc.claim)
			assert.Equal(t, true, out["unchecked"])
			assert.Equal(t, tc.rule, out["rule"])
			assert.Contains(t, out["next"], "represent")
			claims := recordsOfType(t, a, "claim")
			require.Len(t, claims, before+1, "sealed: the words may already have been said")
			body := claims[len(claims)-1]["body"].(map[string]any)
			assert.Equal(t, true, body["unchecked"])
			assert.Equal(t, tc.rule, body["rule"])
			assert.NotContains(t, body, "authority_basis")
			_, export := exportRecords(t, a)
			checkProfile(t, export)
			// A period counts it once, whatever the rule.
			r, _ := listRange(t, "--since", "2000-01-01")
			assert.Equal(t, 1, r.Totals.Anomalies["unapproved_representation"])
		})
	}
}

// approvedWarranty is a warranty represent check the user approved.
func approvedWarranty(t *testing.T, dealID string) string {
	t.Helper()
	ck := represent(t, dealID, repFile("represent-warranty.json"))
	_, err := answerCheck(t, dealID, ck["check_id"].(string), "yes, say that", ck["card"].(string))
	require.NoError(t, err)
	return ck["check_id"].(string)
}

// An ordered representation comes before any offer, commit or share in its
// own thread, and nothing approves past it: the check asks with no way to
// proceed. Another thread's statement does not count. A cancel is never held.
func TestAnOrderedRepresentationComesFirstInEachThread(t *testing.T) {
	dealFixture(t)
	_, a, b := repSale(t)
	held := dealRun(t, "check", "--deal", b, "--input", repFile("check-offer.json"))
	require.Equal(t, "pause", held["verdict"])
	assert.True(t, ruleOf(t, held, ruleRepresentationMissing))
	assert.Equal(t, []string{"hold"}, optionIDs(held), "no way to proceed past it")
	_, err := answerCheck(t, b, held["check_id"].(string), "go ahead anyway", held["card"].(string))
	require.ErrorIs(t, err, cli.ErrInput, "a transaction approval never waives it")

	// Thread A states it; B is still held.
	ck := represent(t, a, repFile("represent-condition.json"))
	claimWith(t, a, ck["check_id"].(string), repFile("claim-condition.json"))
	assert.Equal(t, "pass", dealRun(t, "check", "--deal", a, "--input", repFile("check-offer.json"))["verdict"])
	still := dealRun(t, "check", "--deal", b, "--input", repFile("check-offer.json"))
	assert.True(t, ruleOf(t, still, ruleRepresentationMissing), "a statement to buyer A is not one to buyer B")

	// An unchecked condition claim does not count either.
	claimWith(t, b, "", repFile("claim-condition.json"))
	assert.True(t, ruleOf(t, dealRun(t, "check", "--deal", b, "--input", repFile("check-offer.json")), ruleRepresentationMissing))
	ck = represent(t, b, repFile("represent-condition.json"))
	claimWith(t, b, ck["check_id"].(string), repFile("claim-condition.json"))
	assert.Equal(t, "pass", dealRun(t, "check", "--deal", b, "--input", repFile("check-offer.json"))["verdict"])

	// The seller can always walk away.
	dealFixture(t)
	id := sellerRepOpen(t, `["condition"]`)
	cancel := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"cancel"}`))
	assert.False(t, ruleOf(t, cancel, ruleRepresentationMissing))
}

// What the user orders changes only on their confirmation, in both
// directions; a check made under the old order no longer covers a claim, and
// an act checked before a new order is held when it is noted.
func TestOrderedRepresentationsChangeOnlyOnTheUsersConfirmation(t *testing.T) {
	dealFixture(t)
	checker := pinStubChecker(t)
	id := sellerRepOpen(t, `["condition"]`)

	// Adding warranty is a proposal: until confirmed, warranty still asks.
	note := dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t,
		`{"verbatim": "Also offer the 30 days on the gears", "ordered_representations": ["condition", "warranty"]}`))
	require.NotNil(t, note["proposed"])
	assert.Equal(t, "pause", represent(t, id, repFile("represent-warranty.json"))["verdict"])
	dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", note["capsule_id"].(string), "--choice", "confirm_limits", "--said", "yes, add the gears warranty")
	ck := represent(t, id, repFile("represent-warranty.json"))
	require.Equal(t, "pass", ck["verdict"], "now ordered: DO")
	var input map[string]any
	require.NoError(t, json.Unmarshal([]byte(rawCheckerInput(t, checker)), &input))
	ta := input["task_authority_record"].(map[string]any)
	assert.Equal(t, []any{"condition", "warranty"}, ta["body"].(map[string]any)["ordered_representations"], "the checker gets the confirmed task authority")
	claimWith(t, id, ck["check_id"].(string), repFile("claim-warranty.json"))
	assert.Equal(t, taskAuthorityDigest(t, id), basisOf(claimRecord(t, id, "warranty"))[0].(map[string]any)["ref"].(map[string]any)["digest"], "the confirmed task authority")

	// A condition check under this order, then the user drops condition: the
	// check no longer covers a claim.
	cond := represent(t, id, repFile("represent-condition.json"))
	require.Equal(t, "pass", cond["verdict"])
	drop := dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t,
		`{"verbatim": "Don't mention the scratch", "ordered_representations": ["warranty"]}`))
	require.NotNil(t, drop["proposed"], "dropping an ordered statement is a proposal too")
	assert.True(t, ruleOf(t, dealRun(t, "check", "--deal", id, "--input", repFile("check-offer.json")), ruleRepresentationMissing), "still ordered until confirmed")
	dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", drop["capsule_id"].(string), "--choice", "confirm_limits", "--said", "right, leave the scratch out")
	stale := claimWith(t, id, cond["check_id"].(string), repFile("claim-condition.json"))
	assert.Equal(t, "authority_changed", stale["rule"])
	assert.Equal(t, "pass", dealRun(t, "check", "--deal", id, "--input", repFile("check-offer.json"))["verdict"], "no longer ordered")
	_, export := exportRecords(t, id)
	checkProfile(t, export)
}

func taskAuthorityDigest(t *testing.T, dealID string) string {
	t.Helper()
	return digestOfType(t, dealID, cli.TypeTaskAuthority)
}

// An act checked before the user ordered a new statement is held when it is
// noted, and so is an address telling approved before it: the order is read
// again when the step is noted.
func TestANewOrderHoldsStepsCheckedBeforeIt(t *testing.T) {
	dealFixture(t)
	id := sellerRepOpen(t, `[]`)
	offer := dealRun(t, "check", "--deal", id, "--input", repFile("check-offer.json"))
	require.Equal(t, "pass", offer["verdict"])
	order := dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t,
		`{"verbatim": "Tell them about the scratch", "ordered_representations": ["condition"]}`))
	dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", order["capsule_id"].(string), "--choice", "confirm_limits", "--said", "yes, tell them")
	act := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"offer","amount_minor":190000}`))
	assert.Equal(t, true, act["unchecked"])
	assert.Equal(t, ruleRepresentationMissing, act["rule"])
}

// A seller's address, approved before a new order, is held as a first
// telling when it is noted: the order clears the approval first.
func TestANewOrderHoldsAnApprovedAddress(t *testing.T) {
	dealFixture(t)
	id := sellerRepOpen(t, `[]`)
	_, err := acceptOffer(t, id, makeOffer(t, id, "190000"))
	require.NoError(t, err)
	share := dealRun(t, "check", "--deal", id, "--input", writeJSON(t,
		`{"action":"share_contact","disclosing":["address"],"disclosing_to":"counterparty"}`))
	approved(t, id, share)
	order := dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t,
		`{"verbatim": "Tell them about the scratch first", "ordered_representations": ["condition"]}`))
	dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", order["capsule_id"].(string), "--choice", "confirm_limits", "--said", "yes, the scratch first")
	before := len(recordsOfType(t, id, cli.TypeActionRecord))
	err = dealRunErr(t, "note", "--deal", id, "--kind", "disclosure", "--input", writeJSON(t,
		`{"to":"counterparty","fields":[{"class":"address","value":"1 Example Street"}]}`))
	require.ErrorIs(t, err, cli.ErrPaused, "a first telling nothing authorizes is held")
	assert.Len(t, recordsOfType(t, id, cli.TypeActionRecord), before, "nothing is sealed")
}

// A seller ruleset decides from the basis: with the built-in gate off
// (nothing ordered), a stub that holds an offer unless a condition statement
// was authorized pauses on an unchecked one and passes on an approved one.
// It also reads the proposed words of a represent check, and denies a
// promise it never allows.
func TestTheSellerRulesetReadsTheBasis(t *testing.T) {
	// The checker runs from a private copy of the stub, so the input file the
	// stub saved is read from the stub's own second line.
	stub := `python3 - "$(sed -n '2s/^cat > "\(.*\)"$/\1/p' "$0")" <<'PY'
import json, sys
inp = json.load(open(sys.argv[1]))
rec = inp["record"]
action = rec["agent_input"]["body"]["action"]
verdict = "allow"
if action == "offer" and not any(c["class"] == "condition" and "authority_basis" in c for c in rec.get("deal_claims", [])):
    verdict = "escalate"
if action == "represent" and "authentic" in rec.get("representing_opening", {}).get("text", ""):
    verdict = "deny"
print(json.dumps({"schema": "external-check-result/v0", "ruleset_id": "example-rules/1.0.0", "definition_digest": "` + exampleRulesDigest + `",
                  "verdict": verdict, "findings": [{"id": "s07-condition", "verdict": "pass" if verdict == "allow" else "fail"}]}))
PY`
	for name, approve := range map[string]bool{"unchecked condition": false, "approved condition": true} {
		t.Run(name, func(t *testing.T) {
			dealFixture(t)
			checker := stubChecker(t, "seller-rules", stub)
			pinChecker(t, map[string]any{"command": []string{checker}})
			id := sellerRepOpen(t, `[]`)
			check := ""
			if approve {
				ck := represent(t, id, repFile("represent-condition.json"))
				require.Equal(t, "pause", ck["verdict"], "not ordered: ASK")
				_, err := answerCheck(t, id, ck["check_id"].(string), "yes, mention the scratch", ck["card"].(string))
				require.NoError(t, err)
				check = ck["check_id"].(string)
				raw := rawCheckerInput(t, checker)
				inputFitsSchema(t, raw)
				var input map[string]any
				require.NoError(t, json.Unmarshal([]byte(raw), &input))
				opening := input["record"].(map[string]any)["representing_opening"].(map[string]any)
				assert.Equal(t, "The frame has one small scratch by the seat", opening["text"])
				sealed := input["record"].(map[string]any)["agent_input"].(map[string]any)["body"].(map[string]any)["representing"].(map[string]any)
				commitment, err := cli.CommitText(opening["nonce"].(string), opening["text"].(string))
				require.NoError(t, err)
				assert.Equal(t, sealed["text_commitment"], commitment, "the opening opens the sealed commitment")
			}
			claimWith(t, id, check, repFile("claim-condition.json"))
			offer := dealRun(t, "check", "--deal", id, "--input", repFile("check-offer.json"))
			raw := rawCheckerInput(t, checker)
			inputFitsSchema(t, raw)
			rules := offer["rules"].(map[string]any)
			if approve {
				assert.Equal(t, "allow", rules["verdict"])
				assert.Equal(t, "pass", offer["verdict"])
				assert.Contains(t, raw, `"authority_basis"`)
			} else {
				assert.Equal(t, "escalate", rules["verdict"])
				assert.Equal(t, "pause", offer["verdict"])
				assert.Contains(t, raw, `"unchecked":true`)
			}
		})
	}
	t.Run("never", func(t *testing.T) {
		dealFixture(t)
		checker := stubChecker(t, "seller-rules", stub)
		pinChecker(t, map[string]any{"command": []string{checker}})
		id := sellerRepOpen(t, `["authenticity"]`)
		ck := represent(t, id, writeJSON(t, `{"action":"represent","representing":{"class":"authenticity","text":"This is an authentic vintage frame"}}`))
		assert.Equal(t, "deny", ck["verdict"], "even an ordered class: the ruleset never allows the promise")
		out := claimWith(t, id, ck["check_id"].(string), writeJSON(t, `{"text":"This is an authentic vintage frame","source_kind":"agent","class":"authenticity"}`))
		assert.Equal(t, "denied", out["rule"])
	})
}

// Where the representation rules apply, inputs that would get around them
// are refused before anything is sealed.
func TestRepresentationInputsAreRefused(t *testing.T) {
	dealFixture(t)
	id := sellerRepOpen(t, `["condition"]`)
	buyer := openTypedSticker(t)
	for name, args := range map[string][]string{
		"an unclassed agent claim":              {"note", "--deal", id, "--kind", "claim", "--input", writeJSON(t, `{"text":"Lovely bike","source_kind":"agent"}`)},
		"an outside claim with no source":       {"note", "--deal", id, "--kind", "claim", "--input", writeJSON(t, `{"text":"Bikes like this sell fast","source_kind":"external"}`)},
		"a buyer's claim with no source":        {"note", "--deal", id, "--kind", "claim", "--input", writeJSON(t, `{"text":"The gears are covered","source_kind":"merchant"}`)},
		"a platform claim with no source":       {"note", "--deal", id, "--kind", "claim", "--input", writeJSON(t, `{"text":"The gears are covered","source_kind":"platform"}`)},
		"--check on someone else's claim":       {"note", "--deal", id, "--kind", "claim", "--check", "x", "--input", writeJSON(t, `{"text":"I want it","source_kind":"merchant","source":"buyer_message"}`)},
		"represent on a buyer's deal":           {"check", "--deal", buyer, "--input", repFile("represent-condition.json")},
		"represent with an amount":              {"check", "--deal", id, "--input", writeJSON(t, `{"action":"represent","amount_minor":5,"representing":{"class":"price","text":"It is 1900"}}`)},
		"represent with no words":               {"check", "--deal", id, "--input", writeJSON(t, `{"action":"represent","representing":{"class":"price","text":" "}}`)},
		"represent with no class":               {"check", "--deal", id, "--input", writeJSON(t, `{"action":"represent","representing":{"text":"It rides well"}}`)},
		"representing on another check":         {"check", "--deal", id, "--input", writeJSON(t, `{"action":"cancel","representing":{"class":"price","text":"x"}}`)},
		"represent as an act":                   {"note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"represent"}`)},
		"represent in allowed":                  {"note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"x","allowed":["represent"]}`)},
		"an unknown ordered class":              {"note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"x","ordered_representations":["provenance"]}`)},
		"ordered representations for a buyer":   {"note", "--deal", buyer, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"x","ordered_representations":["condition"]}`)},
		"the floor in the words to the buyer":   {"check", "--deal", sellerFloorDeal(t), "--input", writeJSON(t, `{"action":"represent","representing":{"class":"price","text":"I can go down to 1700"}}`)},
		"agent claims in a seller's open input": {"open", "--records", "typed", "--input", writeJSON(t, strings.Replace(sellerTyped, `"recourse"`, `"claims": [{"text": "One small scratch", "source_kind": "agent", "class": "condition"}], "recourse"`, 1))},
	} {
		require.ErrorIs(t, dealRunErr(t, args...), cli.ErrInput, name)
	}
	_, err := invoke(t, "", "--profile", "deal", "deal", "open", "--records", "typed", "--input", writeJSON(t,
		`{"type":"purchase","channel":"web","intent":{"verbatim":"buy a sticker","ordered_representations":["condition"]},"who":{"name":"Shop"},"terms":{"item":"sticker","price_minor":600,"currency":"USD"},"recourse":{"rail":"card"}}`))
	require.ErrorIs(t, err, cli.ErrInput, "ordered representations go with a seller's deal")
	// Nothing was sealed on the seller's deal by the refusals.
	assert.Empty(t, recordsOfType(t, id, "claim"))
}

func sellerFloorDeal(t *testing.T) string {
	t.Helper()
	return openTyped(t, `{"type": "purchase", "channel": "web",
 "intent": {"verbatim": "Sell my bike; not under 1700", "party_role": "seller", "min_total_minor": 170000,
            "asked": {"item": "example bicycle", "quantity": 1}, "allowed": ["offer", "commit"]},
 "who": {"name": "Example Buyer", "domain": "buyer.example"},
 "terms": {"item": "example bicycle", "quantity": 1, "price_minor": 190000, "currency": "USD"},
 "recourse": {"rail": "card", "refundable": false}}`)
}

// A seller's deal opened since the rules records its order, empty when
// nothing is ordered, and a sale's thread carries its sale's; a deal or sale
// opened before has none and keeps its rules, its threads included.
func TestTheRepresentationRulesFollowTheOpening(t *testing.T) {
	dealFixture(t)
	id := sellerRepOpen(t, `["warranty", "condition", "condition"]`)
	ta := recordsOfType(t, id, cli.TypeTaskAuthority)[0]["body"].(map[string]any)
	assert.Equal(t, []any{"condition", "warranty"}, ta["ordered_representations"], "unique and sorted")
	plain := openTyped(t, sellerTyped)
	ta = recordsOfType(t, plain, cli.TypeTaskAuthority)[0]["body"].(map[string]any)
	assert.Equal(t, []any{}, ta["ordered_representations"], "a seller's deal records an empty order")
	_, a, _ := repSale(t)
	ta = recordsOfType(t, a, cli.TypeTaskAuthority)[0]["body"].(map[string]any)
	assert.Equal(t, []any{"condition"}, ta["ordered_representations"], "a thread carries its sale's order")
	buyer := openTypedSticker(t)
	assert.NotContains(t, recordsOfType(t, buyer, cli.TypeTaskAuthority)[0]["body"], "ordered_representations")

	// A deal opened before: its opening intent has no order.
	legacy := []sealedEvent{{Event: dealEvent{Kind: "open", Open: &dealOpen{Intent: dealIntent{PartyRole: dealRoleSeller}}}}}
	assert.False(t, representationRules(legacy))
	empty := []string{}
	legacy[0].Event.Open.Intent.OrderedRepresentations = &empty
	assert.True(t, representationRules(legacy))
	// A thread under a sale opened before copies its sale's intent, with no
	// order: it stays under the old rules.
	o := dealOpen{Type: "purchase"}
	sale := dealOpen{Type: "purchase", Intent: dealIntent{Verbatim: "sell", PartyRole: dealRoleSeller}, Terms: dealTerms{Item: "bike", Currency: "USD"}}
	require.NoError(t, underSale(&o, sale, "sale-0000000000000000", "ab"))
	require.NoError(t, normalizeOpen(&o))
	assert.Nil(t, o.Intent.OrderedRepresentations)
	// On a deal under the old rules an agent claim is sealed as before.
	assert.Nil(t, checkSellerClaim(legacy[:0], &dealClaim{Text: "x", SourceKind: "agent"}))
}

// checkProfileRejects runs the profile checker on records and wants it to
// fail with want in its output.
func checkProfileRejects(t *testing.T, records []map[string]any, want string) {
	t.Helper()
	python := profilePython(t)
	if python == "" {
		return
	}
	raw, err := json.Marshal(records)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "edited.json")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	out, err := exec.Command(python, dealProfileDir+"/check_profile.py", path).CombinedOutput()
	require.Error(t, err, string(out))
	assert.Contains(t, string(out), want)
}

// The profile checker holds a representation's basis to the chain: each
// hand edit of the last record (so no later prev moves) is rejected.
func TestTheProfileCheckerHoldsTheRepresentationRules(t *testing.T) {
	dealFixture(t)
	_, a, _ := repSale(t)
	ck := represent(t, a, repFile("represent-condition.json"))
	claimWith(t, a, ck["check_id"].(string), repFile("claim-condition.json"))
	records, export := exportRecords(t, a)
	checkProfile(t, export)
	last := func(edit func(body map[string]any, block map[string]any)) []map[string]any {
		var copied []map[string]any
		raw, err := json.Marshal(records)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &copied))
		r := copied[len(copied)-1]
		edit(r["body"].(map[string]any), r["x-deal-v0"].(map[string]any))
		return copied
	}
	checkProfileRejects(t, last(func(body, _ map[string]any) {
		delete(body, "authority_basis")
		delete(body, "evaluation_ref")
	}), "exactly one of authority_basis or unchecked")
	checkProfileRejects(t, last(func(body, _ map[string]any) {
		body["unchecked"], body["rule"] = true, "no_check"
	}), "authority")
	checkProfileRejects(t, last(func(body, _ map[string]any) {
		body["authority_basis"].([]any)[0].(map[string]any)["ref"].(map[string]any)["digest"] = recordDigest(t, records[0])
	}), "task_authority is not the evaluation's task_authority_ref")
	checkProfileRejects(t, last(func(body, _ map[string]any) {
		body["class"] = "warranty"
	}), "class differs from the one checked")
	checkProfileRejects(t, last(func(body, block map[string]any) {
		delete(block, "refs")
	}), "exactly one 'authorized_by' ref")
	checkProfileRejects(t, last(func(body, _ map[string]any) {
		body["text_commitment"] = strings.Repeat("ab", 32)
	}), "the representation's words differ from the ones checked")

	// An unchecked representation names nothing that authorized it.
	claimWith(t, a, "", repFile("claim-warranty.json"))
	records, export = exportRecords(t, a)
	checkProfile(t, export)
	checkProfileRejects(t, last(func(_, block map[string]any) {
		block["refs"] = []any{map[string]any{"rel": "authorized_by", "type": "deal-record", "digest_alg": "SHA-256", "digest": recordDigest(t, records[len(records)-2])}}
	}), "names nothing that authorized it")
}

// A change of details after a represent check leaves the claim unchecked, on
// a DO and on an approved ASK alike, and the chain still verifies.
func TestAChangeAfterARepresentCheckLeavesTheClaimUnchecked(t *testing.T) {
	for name, input := range map[string]string{"ordered": "represent-condition.json", "approved": "represent-warranty.json"} {
		t.Run(name, func(t *testing.T) {
			dealFixture(t)
			_, a, _ := repSale(t)
			ck := represent(t, a, repFile(input))
			if ck["verdict"] != "pass" {
				_, err := answerCheck(t, a, ck["check_id"].(string), "yes, say that", ck["card"].(string))
				require.NoError(t, err)
			}
			dealRun(t, "note", "--deal", a, "--kind", "change", "--input", writeJSON(t, `{"source":"buyer_message","who":{"name":"Another Buyer"}}`))
			claim := "claim-condition.json"
			if name == "approved" {
				claim = "claim-warranty.json"
			}
			out := claimWith(t, a, ck["check_id"].(string), repFile(claim))
			assert.Equal(t, true, out["unchecked"])
			assert.Equal(t, "changed_after_check", out["rule"])
			_, export := exportRecords(t, a)
			checkProfile(t, export)
		})
	}
}

// Approving what the agent tells the buyer approves those words only: a
// change of price before it is still a change at the next offer.
func TestApprovingARepresentationAcceptsNoChangeOfTerms(t *testing.T) {
	dealFixture(t)
	id := sellerRepOpen(t, `[]`)
	dealRun(t, "note", "--deal", id, "--kind", "change", "--input", writeJSON(t, `{"source":"buyer_message","terms":{"price_minor":150000}}`))
	approvedWarranty(t, id)
	offer := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"offer","amount_minor":150000,"terms":{"item":"example bicycle","quantity":1,"price_minor":150000}}`))
	assert.True(t, ruleOf(t, offer, "terms_changed") || ruleOf(t, offer, "price_changed"), "the price change still needs its own answer: %v", offer["differences"])
}
