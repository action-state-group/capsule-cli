package plugin

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/action-state-group/capsule-cli/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A buyer's acceptance releases the seller's details only while the deal is
// live, and capsulectl says why the user's rules found no acceptance when
// it is older than the history they are given (capsule-engine#179).

const phoneShare = `{"action":"share_contact","disclosing_to":"counterparty","description":"to arrange pickup","disclosing":["phone"]}`

// acceptOnThread has the buyer accept a new offer on the thread and the
// seller commit to it.
func acceptOnThread(t *testing.T, id string) {
	t.Helper()
	_, err := acceptOffer(t, id, makeOffer(t, id, "190000"))
	require.NoError(t, err)
	commit := commitNow(t, id, "190000")
	require.Equal(t, false, commit["unchecked"])
}

// acceptedThread is a sale's buyer thread where the buyer accepted the
// seller's offer and the seller committed to it.
func acceptedThread(t *testing.T) string {
	t.Helper()
	saleID, _ := newSale(t)
	id := buyerThread(t, saleID, "buyer.example")
	acceptOnThread(t, id)
	return id
}

func hasDifference(t *testing.T, check map[string]any, rule string) bool {
	t.Helper()
	return strings.Contains(mustJSONString(t, check["differences"]), `"rule":"`+rule+`"`)
}

func dealCheckOf(t *testing.T, id, input string) map[string]any {
	t.Helper()
	return dealRun(t, "check", "--deal", id, "--input", writeJSON(t, input))
}

func cancelAct(t *testing.T, dealID string) map[string]any {
	t.Helper()
	return dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"cancel"}`))
}

func TestASellersCancelAfterAcceptanceMakesEveryLaterShareAsk(t *testing.T) {
	dealFixture(t)
	id := acceptedThread(t)
	assert.False(t, hasDifference(t, dealCheckOf(t, id, phoneShare), "acceptance_called_off"), "a live acceptance changes nothing")

	cancelAct(t, id)
	for _, share := range []string{phoneShare, `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["pickup_area"]}`} {
		after := dealCheckOf(t, id, share)
		assert.True(t, hasDifference(t, after, "acceptance_called_off"), share)
		assert.Equal(t, "pause", after["verdict"])
		assert.Contains(t, after["card"], "the buyer's acceptance was called off")
		assert.Contains(t, differenceTexts(after), "The buyer's acceptance was called off")
	}
	// Only a share asks for it: an offer is not about the acceptance.
	assert.False(t, hasDifference(t, dealCheckOf(t, id, offerInput), "acceptance_called_off"))

	// The buyer accepting again makes the deal live again: re-offer,
	// acceptance and commit in the same thread, and a share no longer asks
	// for the called-off acceptance.
	acceptOnThread(t, id)
	assert.False(t, hasDifference(t, dealCheckOf(t, id, phoneShare), "acceptance_called_off"))
	// A cancel after that calls the new acceptance off in turn.
	cancelAct(t, id)
	assert.True(t, hasDifference(t, dealCheckOf(t, id, phoneShare), "acceptance_called_off"))
	_, export := exportRecords(t, id)
	checkProfile(t, export)
}

func TestACancelWithNoAcceptanceChangesNoShare(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	id := buyerThread(t, saleID, "buyer.example")
	makeOffer(t, id, "190000")
	cancelAct(t, id)
	assert.False(t, hasDifference(t, dealCheckOf(t, id, phoneShare), "acceptance_called_off"))
}

// A cancel checked, even approved, but never recorded calls nothing off.
func TestACheckedCancelNeverRecordedLeavesTheAcceptanceLive(t *testing.T) {
	dealFixture(t)
	id := acceptedThread(t)
	approved(t, id, dealCheckOf(t, id, `{"action":"cancel"}`))
	assert.False(t, hasDifference(t, dealCheckOf(t, id, phoneShare), "acceptance_called_off"))
}

// The skill's path: check the cancel (the card says what the user
// confirms), record it once it proceeds, and the next share asks.
func TestTheSkillsCancelPathCallsTheAcceptanceOff(t *testing.T) {
	dealFixture(t)
	id := acceptedThread(t)
	check := dealCheckOf(t, id, `{"action":"cancel"}`)
	require.Equal(t, "pause", check["verdict"])
	assert.Contains(t, check["card"], "\nThe buyer backed out: record their acceptance as called off?\n")
	approved(t, id, check)
	act := cancelAct(t, id)
	assert.Equal(t, false, act["unchecked"], "the approved check covers the cancel")
	share := dealCheckOf(t, id, phoneShare)
	assert.True(t, hasDifference(t, share, "acceptance_called_off"))
	assert.Contains(t, share["card"], "the buyer's acceptance was called off")
	// Once called off, a cancel check is an ordinary cancel again.
	assert.NotContains(t, dealCheckOf(t, id, `{"action":"cancel"}`)["card"], "backed out")
}

func TestOnlyASellersSaleThreadHasAnAcceptanceToCallOff(t *testing.T) {
	s := dealState{open: dealOpen{Intent: dealIntent{PartyRole: dealRoleSeller}}}
	assert.False(t, s.saleThread(), "no item reference: not a sale's thread")
	s.open.ItemRef = strings.Repeat("a", 64)
	assert.True(t, s.saleThread())
	s.open.Intent.PartyRole = dealRoleBuyer
	assert.False(t, s.saleThread())
}

func TestAnAcceptanceOutsideTheHistoryWindowIsTold(t *testing.T) {
	since := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return since.Add(d).Format(time.RFC3339) }
	for _, tc := range []struct {
		name    string
		commits []string
		since   time.Time
		old     bool
	}{
		{"no commit: no acceptance to tell of", nil, since, false},
		{"no checker ran", []string{at(-time.Hour)}, time.Time{}, false},
		{"within the window", []string{at(24 * time.Hour)}, since, false},
		{"exactly at the cutoff: the history carries it", []string{at(0)}, since, false},
		{"a second before the cutoff", []string{at(-time.Second)}, since, true},
		{"an older commit and one within the window", []string{at(-9 * 24 * time.Hour), at(time.Hour)}, since, false},
		{"a time that does not read is never called old", []string{"yesterday"}, since, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := dealState{commitsAt: tc.commits}
			assert.Equal(t, tc.old, s.acceptanceOutsideHistory(tc.since))
		})
	}
}

func TestTheCardSaysWhyTheRulesFoundNoAcceptance(t *testing.T) {
	w := cli.BuiltinWording()
	share := dealSnapshot{Action: "share_contact"}
	old := cardFacts{acceptanceOld: true, acceptanceRefused: true}
	assert.Equal(t, []string{"your rules don't allow this, because the buyer's acceptance is older than 31 days. Ask them to accept again"},
		cardWhy(w, dealCheckResult{Differences: diffs("rules_deny", "first_disclosure")}, share, old), "the refusal first, then why, and nothing after")
	assert.Equal(t, []string{"your rules don't allow this"}, cardWhy(w, dealCheckResult{Differences: diffs("rules_deny")}, share, cardFacts{}),
		"within the window the rules' own reason stands alone")
	assert.Equal(t, []string{"the buyer's acceptance is older than 31 days; ask them to accept again", "I couldn't check this against your rules"},
		cardWhy(w, dealCheckResult{Differences: diffs("rules_not_evaluable")}, share, old))
	assert.Equal(t, []string{"your rules ask for your OK on this"}, cardWhy(w, dealCheckResult{Differences: diffs("rules_escalate")}, share, old),
		"an escalation asks for something else")
	assert.Equal(t, []string{"first time they get your details"}, cardWhy(w, dealCheckResult{Differences: diffs("first_disclosure")}, share, old),
		"rules that allowed it need no reason about the acceptance")
	assert.Equal(t, []string{"the buyer's acceptance was called off"}, cardWhy(w, dealCheckResult{Differences: diffs("acceptance_called_off")}, share, cardFacts{}))
	// The refusal is never dropped by the two-reason limit: higher rows may
	// come first, and it takes the second place.
	refusal := "your rules don't allow this, because the buyer's acceptance is older than 31 days. Ask them to accept again"
	assert.Equal(t, []string{"the buyer's acceptance was called off", refusal},
		cardWhy(w, dealCheckResult{Differences: diffs("acceptance_called_off", "rules_deny")}, share, old))
	assert.Equal(t, []string{"the payee or contact changed since you first talked", refusal},
		cardWhy(w, dealCheckResult{Differences: diffs("payee_or_contact_changed", "acceptance_called_off", "rules_deny")}, share, old))
}

// The card names the acceptance only when the rule that releases on it is
// the one that failed or could not be evaluated.
func TestOnlyTheAcceptanceRuleFailingNamesTheAcceptance(t *testing.T) {
	rules := func(findings ...dealRulesFinding) *dealRules {
		return &dealRules{Status: "evaluated", Verdict: "deny", Findings: findings}
	}
	s13 := dealRulesFinding{ID: "s13", Check: "seller.release_on_acceptance/1.2.0", Verdict: "fail"}
	assert.Equal(t, "fail", acceptanceRuleVerdict(rules(s13)))
	assert.Equal(t, "not_evaluable", acceptanceRuleVerdict(rules(dealRulesFinding{ID: "s13", Check: "release_on_acceptance", Verdict: "not_evaluable", Reason: "no history"})))
	assert.Equal(t, "", acceptanceRuleVerdict(rules(dealRulesFinding{ID: "s13", Check: s13.Check, Verdict: "pass"})), "it passed")
	assert.Equal(t, "", acceptanceRuleVerdict(rules(dealRulesFinding{ID: "s13", Check: s13.Check, Verdict: "not_evaluable", Reason: rulesDeclaredNotMeasured})), "declared, not measured")
	assert.Equal(t, "", acceptanceRuleVerdict(rules(dealRulesFinding{ID: "r01", Check: "caps/1", Verdict: "fail"})), "another rule")
	assert.Equal(t, "", acceptanceRuleVerdict(rules(dealRulesFinding{ID: "x", Check: "seller.release_on_acceptance_extra/1", Verdict: "fail"})))
	assert.Equal(t, "", acceptanceRuleVerdict(&dealRules{Status: "not_evaluated"}))
	assert.Equal(t, "", acceptanceRuleVerdict(nil))
	notEvaluable := dealRulesFinding{ID: "s13", Check: s13.Check, Verdict: "not_evaluable", Reason: "no history"}
	assert.Equal(t, "fail", acceptanceRuleVerdict(rules(notEvaluable, s13)), "a fail wins, whatever the order")
	assert.Equal(t, "fail", acceptanceRuleVerdict(rules(s13, notEvaluable)), "a fail wins, whatever the order")
	// A not_evaluable s13 beside another rule's deny did not cause the deny,
	// so the card gives only the plain deny reason.
	w := cli.BuiltinWording()
	f := cardFacts{acceptanceOld: true, acceptanceRefused: false}
	assert.Equal(t, []string{"your rules don't allow this"},
		cardWhy(w, dealCheckResult{Differences: diffs("rules_deny")}, dealSnapshot{Action: "share_contact"}, f))
}

// setClock replaces the deal clock for the rest of the test.
func setClock(t *testing.T, clock func() time.Time) {
	t.Helper()
	old := cli.DealClock
	t.Cleanup(func() { cli.DealClock = old })
	cli.DealClock = clock
}

// frozenAt is a clock that always reads at.
func frozenAt(at time.Time) func() time.Time { return func() time.Time { return at } }

const (
	deniedOld = "Why I stopped: your rules don't allow this, because the buyer's acceptance is older than 31 days. Ask them to accept again."
	s13Fail   = `{"id":"s13","check":"seller.release_on_acceptance/1.2.0","verdict":"fail","reason":"no acceptance in the history"}`
)

// End to end, against a denying checker: the reason appears only past the
// cutoff the checker's history was read from, only on a refusal of a share
// to the buyer the acceptance releases, and goes once the buyer accepts
// again and the seller commits in the window.
func TestAShareAfterTheWindowSaysTheAcceptanceIsTooOld(t *testing.T) {
	dealFixture(t)
	t0 := time.Now().UTC().Truncate(time.Second)
	setClock(t, frozenAt(t0))
	id := acceptedThread(t)
	deny := []string{stubChecker(t, "rules-deny", checkerPrints("deny", s13Fail))}
	pinChecker(t, map[string]any{"command": deny})
	window := cli.RulesHistoryDays * 24 * time.Hour

	cli.DealClock = frozenAt(t0.Add(window))
	at := dealCheckOf(t, id, phoneShare)
	require.Equal(t, "deny", at["verdict"])
	assert.Contains(t, at["card"], "Why I stopped: your rules don't allow this", "exactly 31 days: the history still carries the commit")
	assert.NotContains(t, at["card"], "older than")

	cli.DealClock = frozenAt(t0.Add(window + time.Second))
	past := dealCheckOf(t, id, phoneShare)
	require.Equal(t, "deny", past["verdict"])
	assert.Contains(t, past["card"], "\n"+deniedOld)
	assert.False(t, hasDifference(t, past, "acceptance_called_off"))
	// The public pickup area needs no acceptance, and a platform's share is
	// not the acceptance's to release; a share naming another class with it
	// is.
	assert.NotContains(t, dealCheckOf(t, id, `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["pickup_area"]}`)["card"], "older than")
	assert.NotContains(t, dealCheckOf(t, id, `{"action":"share_contact","disclosing_to":"platform","recipient":{"name":"Example Market","domain":"market.example"},"disclosing":["email"]}`)["card"], "older than")
	assert.Contains(t, dealCheckOf(t, id, `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["pickup_area","phone"]}`)["card"], deniedOld)

	// Past the window, a refusal or a gap from another rule is not the
	// acceptance's: the rules' own reason stands.
	pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules-other-deny", checkerPrints("deny", denyFinding))}})
	other := dealCheckOf(t, id, phoneShare)
	require.Equal(t, "deny", other["verdict"])
	assert.Contains(t, other["card"], "Why I stopped: your rules don't allow this")
	assert.NotContains(t, other["card"], "older than")
	pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules-other-gap", checkerPrints("not_evaluable",
		`{"id":"weekly","check":"window","verdict":"not_evaluable","reason":"the record carries no week of history"}`))}})
	gap := dealCheckOf(t, id, phoneShare)
	require.Equal(t, "pause", gap["verdict"])
	assert.Contains(t, gap["card"], "I couldn't check this against your rules")
	assert.NotContains(t, gap["card"], "older than")
	// The acceptance rule not evaluated is the acceptance's.
	pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules-s13-gap", checkerPrints("not_evaluable",
		`{"id":"s13","check":"seller.release_on_acceptance/1.2.0","verdict":"not_evaluable","reason":"no acceptance in the history"}`))}})
	assert.Contains(t, dealCheckOf(t, id, phoneShare)["card"], "the buyer's acceptance is older than 31 days; ask them to accept again")

	// An unrelated ask from the rules says nothing about the acceptance.
	pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules-ask", checkerPrints("escalate",
		`{"id":"first-share","check":"ask/1","verdict":"fail","reason":"a first share asks"}`))}})
	asked := dealCheckOf(t, id, phoneShare)
	require.Equal(t, "pause", asked["verdict"])
	assert.NotContains(t, asked["card"], "older than")

	// The recovery the skill gives: offer again, record the acceptance,
	// check and record the commit, then share.
	// (A checker that allows them: the stub denies every check.)
	pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules-allow", checkerPrints("allow", ""))}})
	acceptOnThread(t, id)
	pinChecker(t, map[string]any{"command": deny})
	again := dealCheckOf(t, id, phoneShare)
	require.Equal(t, "deny", again["verdict"], "the stub still denies")
	assert.NotContains(t, again["card"], "older than", "a fresh commit is in the window")
}

// The card and the checker's history read one cutoff, even when the clock
// moves past the boundary during the check.
func TestTheCardUsesTheHistorysCutoff(t *testing.T) {
	dealFixture(t)
	t0 := time.Now().UTC().Truncate(time.Second)
	setClock(t, frozenAt(t0))
	id := acceptedThread(t)
	path := stubChecker(t, "rules-deny", checkerPrints("deny", s13Fail))
	pinChecker(t, map[string]any{"command": []string{path}})
	window := cli.RulesHistoryDays * 24 * time.Hour
	var told, untold int
	for start := -60 * time.Second; start <= 0; start += 5 * time.Second {
		// Every read of the clock moves it a second on, so the samples of
		// one check span the cutoff.
		var mu sync.Mutex
		next := t0.Add(window + start)
		cli.DealClock = func() time.Time { mu.Lock(); defer mu.Unlock(); next = next.Add(time.Second); return next }
		check := dealCheckOf(t, id, phoneShare)
		var input struct {
			History []json.RawMessage `json:"history"`
		}
		require.NoError(t, json.Unmarshal(mustRead(t, path+".input"), &input))
		old := strings.Contains(check["card"].(string), "older than")
		assert.Equal(t, len(input.History) == 0, old, "start %s: the card says the acceptance is too old exactly when the checker's history left it out", start)
		if old {
			told++
		} else {
			untold++
		}
	}
	assert.Positive(t, told, "some check fell past the cutoff")
	assert.Positive(t, untold, "some check fell within it")
}
