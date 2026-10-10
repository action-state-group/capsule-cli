package cli

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every check that moves money to the merchant names the payee, not only a
// pay: a booking's commit, a rental's sign, a cancel with an amount. So its
// check carries the per-deal payee fingerprint and is followed by the payee
// keyed per profile, and a rule keyed on who was paid sees the merchant the
// same way on every such check, in one deal and across deals.

func hotelOpen(name, domain string) string {
	return `{"type":"booking","channel":"web",
		"intent":{"verbatim":"book me a double room at ` + name + ` for October 3","max_total_minor":50000,"allowed":["commit"]},
		"who":{"name":"` + name + `","domain":"` + domain + `"},
		"terms":{"item":"double room","price_minor":38000,"currency":"USD","when":"2026-10-03"},
		"recourse":{"rail":"card","refundable":true}}`
}

const hotelCommit = `{"action":"commit","amount_minor":38000,"terms":{"item":"double room","price_minor":38000,"when":"2026-10-03"},"recourse":{"rail":"card","refundable":true}}`

func openHotel(t *testing.T, typed bool, name, domain string) string {
	t.Helper()
	args := []string{"open", "--input", writeJSON(t, hotelOpen(name, domain))}
	if typed {
		args = append(args, "--records", "typed")
	}
	return dealRun(t, args...)["deal_id"].(string)
}

// book checks a commit (answering it in the user's words when it pauses)
// and records the commit act; it returns the act.
func book(t *testing.T, dealID string) map[string]any {
	t.Helper()
	check := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, hotelCommit))
	if check["verdict"] != "pass" {
		_, err := answerCheck(t, dealID, check["check_id"].(string), "yes, book it", check["card"].(string))
		require.NoError(t, err)
	}
	act := dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"commit","amount_minor":38000}`))
	require.Equal(t, false, act["unchecked"])
	return act
}

// checkerInput is what the pinned stub checker was last given.
func checkerInput(t *testing.T, checker string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(checker + ".input")
	require.NoError(t, err)
	var input map[string]any
	require.NoError(t, json.Unmarshal(raw, &input))
	return input
}

func pinStubChecker(t *testing.T) string {
	t.Helper()
	checker := stubChecker(t, "rules", checkerPrints("allow", passFinding))
	pinChecker(t, map[string]any{"command": []string{checker}})
	return checker
}

func eachRecordSet(t *testing.T, fn func(t *testing.T, typed bool)) {
	for name, typed := range map[string]bool{"x-deal-v0": false, "typed": true} {
		t.Run(name, func(t *testing.T) {
			dealFixture(t)
			fn(t, typed)
		})
	}
}

// A repeated booking in one deal: the second check and the first booking,
// as the rules checker is given them, name the same payee (the key a dedupe
// rule compares), and both checks seal the same per-deal fingerprint.
func TestARepeatedBookingInOneDealNamesTheSamePayee(t *testing.T) {
	eachRecordSet(t, func(t *testing.T, typed bool) {
		checker := pinStubChecker(t)
		id := openHotel(t, typed, "Example Hotel Shinjuku", "hotel.example")
		first := book(t, id)
		dealRun(t, "check", "--deal", id, "--input", writeJSON(t, hotelCommit))
		input := checkerInput(t, checker)
		now := wantProfileShape(t, input["record"].(map[string]any)["counterparty_profile"])
		history := historyByID(input)
		require.Contains(t, history, first["capsule_id"])
		assert.Equal(t, now, wantProfileShape(t, history[first["capsule_id"].(string)]["counterparty_profile"]), "the repeat is the same payee")
		steps := profileSteps(t, id)
		require.Len(t, steps, 2, "each commit check is followed by its companion")
		assert.Equal(t, steps[0].checkPayee, steps[1].checkPayee, "one deal, one per-deal fingerprint")
		assert.Equal(t, steps[0].payee, steps[1].payee)
	})
}

// After a booking, a later check at the same merchant in another deal
// carries the same profile value (a rule reading earlier dealings sees it);
// a booking at a different merchant carries a different one.
func TestABookedMerchantIsTheSamePayeeInAnotherDealAndNoOtherIs(t *testing.T) {
	eachRecordSet(t, func(t *testing.T, typed bool) {
		checker := pinStubChecker(t)
		first := openHotel(t, typed, "Example Hotel Shinjuku", "hotel.example")
		booked := book(t, first)
		bookedProfile := profileSteps(t, first)[0].payee

		again := openHotel(t, typed, "Example Hotel Shinjuku", "hotel.example")
		dealRun(t, "check", "--deal", again, "--input", writeJSON(t, hotelCommit))
		input := checkerInput(t, checker)
		now := wantProfileShape(t, input["record"].(map[string]any)["counterparty_profile"])
		assert.Equal(t, bookedProfile, now, "the same merchant, the same profile value")
		assert.Equal(t, now, wantProfileShape(t, historyByID(input)[booked["capsule_id"].(string)]["counterparty_profile"]))

		other := openHotel(t, typed, "Another Example Inn", "inn.example")
		dealRun(t, "check", "--deal", other, "--input", writeJSON(t, hotelCommit))
		input = checkerInput(t, checker)
		elsewhere := wantProfileShape(t, input["record"].(map[string]any)["counterparty_profile"])
		assert.NotEqual(t, bookedProfile, elsewhere, "another merchant is another payee: no over-match")
	})
}

// The companion of a booking's check is in no shared copy, for any audience.
func TestNoShareCarriesABookingsProfileFingerprint(t *testing.T) {
	eachRecordSet(t, func(t *testing.T, typed bool) {
		id := openHotel(t, typed, "Example Hotel Shinjuku", "hotel.example")
		book(t, id)
		steps := profileSteps(t, id)
		require.Len(t, steps, 1)
		for _, audience := range []string{dealAudienceCounterparty, dealAudienceAdjudicator} {
			_, raw := sharedCopy(t, id, audience, "x")
			assert.NotContains(t, raw, steps[0].payee, audience)
			assert.NotContains(t, raw, dealProfileFPAlg, audience)
		}
	})
}

// Which checks name the payee: every pay; where the user buys, a commit, a
// sign or a cancel that states an amount; never a check without an amount,
// a share, or a seller's commit or offer (money from the buyer).
func TestWhichChecksNameThePayee(t *testing.T) {
	amount := int64(100)
	for _, tc := range []struct {
		action string
		amount *int64
		role   string
		want   bool
	}{
		{"pay", nil, dealRoleBuyer, true},
		{"pay", &amount, dealRoleBuyer, true},
		{"commit", &amount, dealRoleBuyer, true},
		{"sign", &amount, dealRoleBuyer, true},
		{"cancel", &amount, dealRoleBuyer, true},
		{"commit", nil, dealRoleBuyer, false},
		{"cancel", nil, dealRoleBuyer, false},
		{"share_contact", nil, dealRoleBuyer, false},
		{"commit", &amount, dealRoleSeller, false},
		{"offer", &amount, dealRoleSeller, false},
	} {
		got := checkNamesPayee(dealSnapshot{Action: tc.action, AmountMinor: tc.amount}, tc.role)
		assert.Equal(t, tc.want, got, "%s amount=%v role=%s", tc.action, tc.amount != nil, tc.role)
	}
}

// A booking checked by a release from before, when only a pay named the
// payee, re-derives byte for byte under this one; a new check in the same
// deal names it.
func TestABookingCheckedBeforeReDerivesUnchanged(t *testing.T) {
	eachRecordSet(t, func(t *testing.T, typed bool) {
		var id string
		dealNamesPayeeOnAmountChecks = false
		func() {
			defer func() { dealNamesPayeeOnAmountChecks = true }()
			id = openHotel(t, typed, "Example Hotel Shinjuku", "hotel.example")
			book(t, id)
		}()
		require.Empty(t, profileSteps(t, id), "the earlier release sealed no companion for it")
		events := chainSteps(t, id)
		p, err := loadProfile("deal")
		require.NoError(t, err)
		s, err := openDealSession(t.Context(), p)
		require.NoError(t, err)
		require.NoError(t, s.useDeal(t.Context(), id, false))
		for i, se := range events {
			_, digest, err := encodeDealRecord(se.Event, events[:i], s.dkey)
			require.NoError(t, err, "step %d", se.Event.N)
			assert.Equal(t, se.Digest, digest, "step %d (%s) re-derives byte for byte", se.Event.N, se.Event.Kind)
		}
		require.NoError(t, s.close())
		dealRun(t, "check", "--deal", id, "--input", writeJSON(t, hotelCommit))
		assert.Len(t, profileSteps(t, id), 1, "a new check names the payee")
	})
}
