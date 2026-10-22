package plugin

import (
	"strings"
	"testing"
	"time"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A buyer's acceptance releases the seller's details only while the deal is
// live, and capsulectl says why the user's rules found no acceptance when
// it is older than the history they are given (capsule-engine#179).

const phoneShare = `{"action":"share_contact","disclosing_to":"counterparty","description":"to arrange pickup","disclosing":["phone"]}`

// acceptedThread is a sale's buyer thread where the buyer accepted the
// seller's offer and the seller committed to it.
func acceptedThread(t *testing.T) string {
	t.Helper()
	saleID, _ := newSale(t)
	id := buyerThread(t, saleID, "buyer.example")
	_, err := acceptOffer(t, id, makeOffer(t, id, "190000"))
	require.NoError(t, err)
	commit := commitNow(t, id, "190000")
	require.Equal(t, false, commit["unchecked"])
	return id
}

func hasDifference(t *testing.T, check map[string]any, rule string) bool {
	t.Helper()
	return strings.Contains(mustJSONString(t, check["differences"]), `"rule":"`+rule+`"`)
}

func cancelAct(t *testing.T, dealID string) {
	t.Helper()
	dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"cancel","description":"the buyer withdrew"}`))
}

func TestASellersCancelAfterAcceptanceMakesEveryLaterShareAsk(t *testing.T) {
	dealFixture(t)
	id := acceptedThread(t)
	before := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, phoneShare))
	assert.False(t, hasDifference(t, before, "acceptance_called_off"), "a live acceptance changes nothing")

	cancelAct(t, id)
	for _, share := range []string{phoneShare, `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["pickup_area"]}`} {
		after := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, share))
		assert.True(t, hasDifference(t, after, "acceptance_called_off"), share)
		assert.Equal(t, "pause", after["verdict"])
		assert.Contains(t, after["card"], "the buyer's acceptance was called off")
		assert.Contains(t, differenceTexts(after), "The buyer's acceptance was called off")
	}
	// Only a share asks for it: an offer is not about the acceptance.
	offer := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, offerInput))
	assert.False(t, hasDifference(t, offer, "acceptance_called_off"))
	_, export := exportRecords(t, id)
	checkProfile(t, export)
}

func TestACancelWithNoAcceptanceChangesNoShare(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	id := buyerThread(t, saleID, "buyer.example")
	makeOffer(t, id, "190000")
	cancelAct(t, id)
	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, phoneShare))
	assert.False(t, hasDifference(t, check, "acceptance_called_off"))
}

// A buyer's deal that is not a sale has no acceptance to call off.
func TestACancelOutsideASaleChangesNoShare(t *testing.T) {
	s := dealState{open: dealOpen{Intent: dealIntent{PartyRole: dealRoleSeller}}}
	assert.False(t, s.saleThread(), "no item reference: not a sale's thread")
	s.open.ItemRef = strings.Repeat("a", 64)
	assert.True(t, s.saleThread())
	s.open.Intent.PartyRole = dealRoleBuyer
	assert.False(t, s.saleThread())
}

func TestAnAcceptanceOutsideTheHistoryWindowIsTold(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	at := func(days int) string { return now.Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339) }
	for _, tc := range []struct {
		name    string
		commits []string
		old     bool
	}{
		{"no commit: no acceptance to tell of", nil, false},
		{"within 31 days", []string{at(30)}, false},
		{"older than 31 days", []string{at(32)}, true},
		{"an older commit and one within the window", []string{at(40), at(3)}, false},
		{"a time that does not read is never called old", []string{"yesterday"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := dealState{commitsAt: tc.commits}
			assert.Equal(t, tc.old, s.acceptanceOutsideHistory(now))
		})
	}
}

func TestTheCardSaysWhyTheRulesFoundNoAcceptance(t *testing.T) {
	w := cli.BuiltinWording()
	share := dealSnapshot{Action: "share_contact"}
	old := cardFacts{acceptanceOld: true}
	assert.Equal(t, []string{"the buyer's acceptance is older than 31 days; ask them to accept again", "your rules don't allow this"},
		cardWhy(w, dealCheckResult{Differences: diffs("rules_deny")}, share, old))
	assert.Equal(t, []string{"your rules don't allow this"}, cardWhy(w, dealCheckResult{Differences: diffs("rules_deny")}, share, cardFacts{}),
		"within the window the rules' own reason stands alone")
	assert.Equal(t, []string{"first time they get your details"}, cardWhy(w, dealCheckResult{Differences: diffs("first_disclosure")}, share, old),
		"rules that allowed it need no reason about the acceptance")
	assert.Equal(t, []string{"the buyer's acceptance was called off"}, cardWhy(w, dealCheckResult{Differences: diffs("acceptance_called_off")}, share, cardFacts{}))
}

// End to end: a share 32 days after the commit, refused by the user's rules,
// says why; at 30 days the card says nothing about the acceptance's age.
func TestAShareAfterTheWindowSaysTheAcceptanceIsTooOld(t *testing.T) {
	dealFixture(t)
	id := acceptedThread(t)
	pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules-deny", checkerPrints("deny",
		`{"id":"s13","check":"seller.release_on_acceptance/1.2.0","verdict":"fail","reason":"no acceptance in the history"}`))}})
	start := cli.DealClock()
	old := cli.DealClock
	t.Cleanup(func() { cli.DealClock = old })
	for _, tc := range []struct {
		days int
		told bool
	}{{30, false}, {32, true}} {
		cli.DealClock = func() time.Time { return start.Add(time.Duration(tc.days) * 24 * time.Hour) }
		check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, phoneShare))
		require.Equal(t, "deny", check["verdict"], tc.days)
		card := check["card"].(string)
		assert.Contains(t, card, "your rules don't allow this", tc.days)
		if tc.told {
			assert.Contains(t, card, "Why I stopped: the buyer's acceptance is older than 31 days; ask them to accept again; your rules don't allow this.")
		} else {
			assert.NotContains(t, card, "older than", tc.days)
		}
		assert.False(t, hasDifference(t, check, "acceptance_called_off"))
	}
	// A platform's share is not the acceptance's to release.
	platform := dealRun(t, "check", "--deal", id, "--input", writeJSON(t,
		`{"action":"share_contact","disclosing_to":"platform","recipient":{"name":"Example Market","domain":"market.example"},"disclosing":["email"]}`))
	assert.NotContains(t, platform["card"], "older than")
}
