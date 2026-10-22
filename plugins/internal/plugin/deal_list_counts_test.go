package plugin

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The check and anomaly counts of a range (capsule-cli#385), read from built
// logs: each step is only what the counts read.

func countsStep(day int, ev dealEvent) sealedEvent {
	ev.At = time.Date(2026, 10, day, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	return sealedEvent{Event: ev}
}

func countsOpen(day int) sealedEvent {
	return countsStep(day, dealEvent{Kind: "open", Open: &dealOpen{}})
}

func countsCheck(day int, verdict string, rules *dealRules) sealedEvent {
	return countsStep(day, dealEvent{Kind: "check", Check: &dealCheckResult{Verdict: verdict, Rules: rules}})
}

func countsAct(day int, unchecked bool, rule string) sealedEvent {
	return countsStep(day, dealEvent{Kind: "act", Act: &dealAct{Action: "pay", Unchecked: unchecked, Rule: rule}})
}

func countsTold(day int, authorizedBy string) sealedEvent {
	return countsStep(day, dealEvent{Kind: "disclosure", Disclosure: &dealDisclosure{To: "counterparty", AuthorizedBy: authorizedBy}})
}

func countsClaim(day int, unchecked bool) sealedEvent {
	return countsStep(day, dealEvent{Kind: "claim", Claim: &dealClaim{Text: "covered for 90 days", SourceKind: "agent", Unchecked: unchecked}})
}

func evaluated(id, digest, verdict string, findings ...dealRulesFinding) *dealRules {
	return &dealRules{Status: "evaluated", RulesetID: id, DefinitionDigest: digest, Verdict: verdict, Findings: findings}
}

// October 10 to 20.
var countsRange = dealListBounds{since: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), until: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)}

func TestDealListCountsEmptyRange(t *testing.T) {
	r, err := dealListInRange(nil, countsRange)
	require.NoError(t, err)
	raw, err := json.Marshal(r.Totals)
	require.NoError(t, err)
	assert.JSONEq(t, `{"deals":0,"sales":0,"outcomes":{"bought":0,"sold":0,"called_off":0,"not_chosen":0,"open":0},"money":[],
		"checks":{"verdicts":{"pass":0,"pause":0,"deny":0},
			"rules":{"evaluated":0,"partly_unanswered":0,"not_evaluated":0,"not_configured":0,"not_applicable":0,"unrecorded":0},
			"rulesets":[]},
		"anomalies":{"acted_against_answer":0,"skipped_check":0,"unsealed_approval":0,"differs_from_check":0,"unapproved_disclosure":0,"unapproved_representation":0}}`, string(raw))
}

func TestDealListCountsChecks(t *testing.T) {
	declared := dealRulesFinding{ID: "r9", Verdict: "not_evaluable", Reason: rulesDeclaredNotMeasured}
	logs := []dealLog{
		{id: "a", events: []sealedEvent{
			countsOpen(1),
			// Before the range: not counted.
			countsCheck(5, "pass", evaluated("asg/everyday/0.3.7", "d1", "allow")),
			// An older check, sealed before checks carried rules.
			countsCheck(11, "pass", nil),
			countsCheck(12, "pass", evaluated("asg/everyday/0.3.7", "d1", "allow", declared)),
			countsCheck(12, "pause", evaluated("asg/everyday/0.3.7", "d1", "escalate", dealRulesFinding{ID: "r1", Verdict: "fail"})),
			// Upgraded mid-range: a ruleset of its own.
			countsCheck(13, "pause", evaluated("asg/everyday/0.3.8", "d2", "not_evaluable", dealRulesFinding{ID: "r2", Verdict: "not_evaluable", Reason: "no history"})),
			// Overall not_evaluable with no single rule marked.
			countsCheck(13, "pause", evaluated("asg/everyday/0.3.8", "d2", "not_evaluable")),
			// Overall not_evaluable with only a failing rule named.
			countsCheck(13, "pause", evaluated("asg/everyday/0.3.8", "d2", "not_evaluable", dealRulesFinding{ID: "r5", Verdict: "fail"})),
			countsCheck(14, "deny", evaluated("asg/everyday/0.3.7", "d1", "deny", dealRulesFinding{ID: "r3", Verdict: "fail"})),
			// A deny that also left a measured rule undecided.
			countsCheck(14, "deny", evaluated("asg/everyday/0.3.7", "d1", "deny",
				dealRulesFinding{ID: "r3", Verdict: "fail"}, dealRulesFinding{ID: "r4", Verdict: "not_evaluable", Reason: "no history"})),
			// After the range: not counted.
			countsCheck(21, "deny", nil),
		}},
		{id: "b", events: []sealedEvent{
			countsOpen(15),
			countsCheck(15, "pause", &dealRules{Status: "not_evaluated", Cause: "refused"}),
			countsCheck(16, "pass", &dealRules{Status: "not_configured"}),
		}},
	}
	r, err := dealListInRange(logs, countsRange)
	require.NoError(t, err)
	c := r.Totals.Checks
	assert.Equal(t, map[string]int{"pass": 3, "pause": 5, "deny": 2}, c.Verdicts)
	assert.Equal(t, map[string]int{"evaluated": 3, "partly_unanswered": 4, "not_evaluated": 1, "not_configured": 1, "not_applicable": 0, "unrecorded": 1}, c.Rules)
	assert.Equal(t, []dealListRuleset{
		{RulesetID: "asg/everyday/0.3.7", DefinitionDigest: "d1", Checks: 4},
		{RulesetID: "asg/everyday/0.3.8", DefinitionDigest: "d2", Checks: 3},
	}, c.Rulesets)
	// Each partitions the checks; the rulesets sum to the evaluated ones.
	total := func(m map[string]int) (n int) {
		for _, v := range m {
			n += v
		}
		return n
	}
	assert.Equal(t, 10, total(c.Verdicts))
	assert.Equal(t, 10, total(c.Rules))
	assert.Equal(t, c.Rules["evaluated"]+c.Rules["partly_unanswered"], c.Rulesets[0].Checks+c.Rulesets[1].Checks)
}

// A verdict word or rules status outside the known set keeps its own key,
// so the counts still partition the checks.
func TestDealListCountsUnknownWords(t *testing.T) {
	logs := []dealLog{{id: "a", events: []sealedEvent{
		countsOpen(11),
		countsCheck(11, "hold", &dealRules{Status: "suspended"}),
	}}}
	r, err := dealListInRange(logs, countsRange)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"pass": 0, "pause": 0, "deny": 0, "hold": 1}, r.Totals.Checks.Verdicts)
	assert.Equal(t, 1, r.Totals.Checks.Rules["suspended"])
	assert.Empty(t, r.Totals.Checks.Rulesets)
}

func TestDealListCountsAnomalies(t *testing.T) {
	logs := []dealLog{
		{id: "a", events: []sealedEvent{
			countsOpen(1),
			// Before the range: not counted.
			countsAct(5, true, "no_check"),
			// A skipped check, then an act: one skipped_check.
			countsAct(11, true, "no_check"),
			countsAct(11, false, ""),
			countsAct(12, true, "differs_from_check"),
			countsAct(12, true, "answer_was_not_proceed"),
			countsAct(12, true, "no_sealed_approval"),
			countsAct(13, true, "approval_already_used"),
			countsTold(13, ""),
			countsTold(13, "approval-1"),
			countsClaim(13, true),
			countsClaim(13, false),
		}},
		{id: "b", events: []sealedEvent{
			countsOpen(15),
			// A check in the range whose act falls after it: the check
			// counts here, the act in the range it is sealed in.
			countsCheck(19, "pass", nil),
			countsAct(22, true, "differs_from_check"),
		}},
	}
	r, err := dealListInRange(logs, countsRange)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"acted_against_answer": 1, "skipped_check": 1, "unsealed_approval": 2, "differs_from_check": 1, "unapproved_disclosure": 1, "unapproved_representation": 1}, r.Totals.Anomalies)
	assert.Equal(t, 1, r.Totals.Checks.Verdicts["pass"])

	later, err := dealListInRange(logs, dealListBounds{since: countsRange.until})
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"acted_against_answer": 0, "skipped_check": 0, "unsealed_approval": 0, "differs_from_check": 1, "unapproved_disclosure": 0, "unapproved_representation": 0}, later.Totals.Anomalies)
	assert.Equal(t, 0, later.Totals.Checks.Verdicts["pass"])
}

// The act kinds `deal list` counts are the ones `deal report` names.
func TestActAnomalyKind(t *testing.T) {
	for rule, want := range map[string]string{
		"differs_from_check":     "differs_from_check",
		"answer_was_not_proceed": "acted_against_answer",
		"no_sealed_approval":     "unsealed_approval",
		"approval_already_used":  "unsealed_approval",
		// The check asked, and only the agent's card answered it.
		"ask_needs_your_words":            "unsealed_approval",
		"no_check":                        "skipped_check",
		"stale_check":                     "skipped_check",
		"changed_after_check":             "skipped_check",
		"required_representation_missing": "skipped_check",
		"no_accepted_offer":               "skipped_check",
		"offer_not_accepted":              "skipped_check",
		"changed_after_acceptance":        "skipped_check",
		// A rule this release does not name is one with no check in force.
		"":        "skipped_check",
		"expired": "skipped_check",
	} {
		assert.Equal(t, want, actAnomalyKind(dealAct{Unchecked: true, Rule: rule}), rule)
	}
	assert.Empty(t, actAnomalyKind(dealAct{Rule: "differs_from_check"}))
}
