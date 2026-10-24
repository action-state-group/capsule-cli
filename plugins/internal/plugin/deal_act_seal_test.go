package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/action-state-group/capsule-cli/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An offer, commit, sign or cancel act seals the counterparty of the check it
// rests on, in the record itself, so a replay of the sealed records keys an
// earlier act on who it was with exactly as a live check does.

// sealedActCounterparty is the counterparty a history act's own record
// seals, where its record set seals it; the test fails when it seals none.
func sealedActCounterparty(t *testing.T, act map[string]any) map[string]any {
	t.Helper()
	input := act["agent_input"].(map[string]any)
	holder, ok := input["x-deal-v0"].(map[string]any)
	if !ok {
		holder = input["body"].(map[string]any)
	}
	cp, ok := holder["counterparty"].(map[string]any)
	require.True(t, ok, "the act's own record seals a counterparty")
	return cp
}

// A repeated booking in one deal: the earlier act's own sealed record names
// the new check's counterparty (a dedupe rule replayed from the records
// sees the repeat), and the history supplies the same value beside it.
func TestARepeatedBookingsActSealsTheChecksCounterparty(t *testing.T) {
	eachRecordSet(t, func(t *testing.T, typed bool) {
		checker := pinStubChecker(t)
		id := openHotel(t, typed, "Example Hotel Shinjuku", "hotel.example")
		first := book(t, id)
		dealRun(t, "check", "--deal", id, "--input", writeJSON(t, hotelCommit))
		input := checkerInput(t, checker)

		now := sealedCheckCounterparty(t, input["record"].(map[string]any))
		act := historyByID(input)[first["capsule_id"].(string)]
		require.NotNil(t, act)
		sealed := sealedActCounterparty(t, act)
		assert.Equal(t, now, sealed, "the act's own record seals who its check named")
		assert.Equal(t, sealed, act["counterparty"], "the history's value beside it is the same")
	})
}

// A merchant booked in one deal, checked in another: the act's sealed
// counterparty is its own deal's (per-deal keys never match across deals),
// and the profile value beside it is the one that matches.
func TestABookedMerchantsSealedActCounterpartyIsPerDeal(t *testing.T) {
	eachRecordSet(t, func(t *testing.T, typed bool) {
		checker := pinStubChecker(t)
		first := openHotel(t, typed, "Example Hotel Shinjuku", "hotel.example")
		booked := book(t, first)
		again := openHotel(t, typed, "Example Hotel Shinjuku", "hotel.example")
		dealRun(t, "check", "--deal", again, "--input", writeJSON(t, hotelCommit))
		input := checkerInput(t, checker)

		record := input["record"].(map[string]any)
		act := historyByID(input)[booked["capsule_id"].(string)]
		require.NotNil(t, act)
		assert.NotEqual(t, sealedCheckCounterparty(t, record)["ids"], sealedActCounterparty(t, act)["ids"])
		assert.Equal(t, record["counterparty_profile"], act["counterparty_profile"])
	})
}

// A step sealed before acts carried their check's counterparty re-derives
// without it, so its bytes are unchanged.
func TestAnActSealedBeforeReDerivesWithoutACounterparty(t *testing.T) {
	dealFixture(t)
	id := openHotel(t, false, "Example Hotel Shinjuku", "hotel.example")
	booked := book(t, id)

	p, err := cli.LoadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.close() })
	require.NoError(t, s.useDeal(t.Context(), id, false))
	events, err := s.load(t.Context(), id)
	require.NoError(t, err)
	found := false
	for i, se := range events {
		if se.CapsuleID != booked["capsule_id"] {
			continue
		}
		found = true
		require.Equal(t, dealActCounterpartyVersion, se.Event.ActCounterparty)
		before := se.Event
		before.ActCounterparty = ""
		raw, _, err := encodeDealRecord(before, events[:i], s.dkey)
		require.NoError(t, err)
		var record map[string]any
		require.NoError(t, json.Unmarshal(raw, &record))
		assert.NotContains(t, record[cli.DealProfile], "counterparty")
	}
	require.True(t, found)
}

// The two-thread sale, B's commit after A's: on each thread, the offer and
// the commit acts are decide Capsules with an accept disposition, and carry the task authority they acted under
// (authority_basis), from which their disposition derives, and the buyer
// that thread was opened for, with exactly the fingerprints its opening
// sealed; the two buyers' never match.
func TestASalesOfferAndCommitActsSealAuthorityAndTheirBuyer(t *testing.T) {
	_, a, b := twoThreadSale(t)
	buyers := map[string]bool{}
	for _, id := range []string{a, b} {
		records, _ := exportRecords(t, id)
		acts := map[string]bool{}
		for _, r := range records {
			if r["type"] != cli.TypeActionRecord {
				continue
			}
			body := r["body"].(map[string]any)
			action := body["action"].(string)
			acts[action] = true
			basis := body["authority_basis"].([]any)
			require.NotEmpty(t, basis, action)
			assert.Equal(t, "task_authority", basis[0].(map[string]any)["type"], action)
			cp, ok := body["counterparty"].(map[string]any)
			require.True(t, ok, "%s: the act names the buyer it is with", action)
			assert.Equal(t, cli.TypedFPAlg, cp["fp_alg"], action)
			ids := cp["ids"].(map[string]any)
			assert.Equal(t, baselineIDs(t, id), ids, action)
			buyers[ids["domain"].(string)] = true
		}
		assert.Equal(t, map[string]bool{offerAction: true, "commit": true}, acts)
		effects := map[string]string{}
		for _, c := range dealCapsules(t, id) {
			if e, ok := c["effect"].(map[string]any); ok {
				effects[e["type"].(string)] = c["disposition"].(map[string]any)["decision"].(string)
			}
		}
		assert.Equal(t, map[string]string{"make_offer": "accept", "accept_agreement": "accept"}, effects,
			"each act is a decide Capsule with its disposition: an accepted commit is told from a repeat")
	}
	assert.Len(t, buyers, 2, "A's and B's acts name different buyers")
}

// Every effect type an act other than a pay seals is provisional, and the
// deal profile labels each one so.
func TestActEffectTypesAreLabelledProvisional(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(dealProfileDir, "PROFILE.md"))
	require.NoError(t, err)
	doc := strings.Join(strings.Fields(string(raw)), " ")
	require.NotEmpty(t, dealActEffectTypes)
	for action, effect := range dealActEffectTypes {
		assert.Contains(t, doc, "`"+action+"` as `"+effect+"` (provisional)", "PROFILE.md labels %s provisional", effect)
	}
}

// openSignAndCancel opens a refundable rental (a lease to sign, which may be
// cancelled) that allows a commit, a sign and a cancel.
func openSignAndCancel(t *testing.T, typed bool) string {
	t.Helper()
	args := []string{"open", "--input", writeJSON(t, `{"type":"rental","channel":"web",
		"intent":{"verbatim":"rent me a one-bedroom at Example Lettings, up to 500","max_total_minor":50000,"allowed":["commit","sign","cancel"]},
		"who":{"name":"Example Lettings","domain":"lettings.example"},
		"terms":{"item":"one-bedroom flat","price_minor":38000,"currency":"USD"},
		"recourse":{"rail":"card","refundable":true}}`)}
	if typed {
		args = append(args, "--records", "typed")
	}
	return dealRun(t, args...)["deal_id"].(string)
}

// actOn checks action for amount (answering it in the user's words when it
// pauses) and records its act; it returns the act and the check input.
func actOn(t *testing.T, dealID, action string, amount int) (map[string]any, string) {
	t.Helper()
	input := `{"action":"` + action + `","amount_minor":` + strconv.Itoa(amount) + `,"recourse":{"rail":"card","refundable":true}}`
	check := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, input))
	if check["verdict"] != "pass" {
		_, err := answerCheck(t, dealID, check["check_id"].(string), "yes, go ahead", check["card"].(string))
		require.NoError(t, err)
	}
	act := dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"`+action+`","amount_minor":`+strconv.Itoa(amount)+`}`))
	require.Equal(t, false, act["unchecked"])
	return act, input
}

// actSealsItsChecksCounterparty: the act's own sealed record names the
// counterparty its check sealed (the same payee a later check of the same
// action names, keyed per deal), in both record sets.
func actSealsItsChecksCounterparty(t *testing.T, action string) {
	eachRecordSet(t, func(t *testing.T, typed bool) {
		checker := pinStubChecker(t)
		id := openSignAndCancel(t, typed)
		act, input := actOn(t, id, action, 38000)
		dealRun(t, "check", "--deal", id, "--input", writeJSON(t, input))
		given := checkerInput(t, checker)

		now := sealedCheckCounterparty(t, given["record"].(map[string]any))
		entry := historyByID(given)[act["capsule_id"].(string)]
		require.NotNil(t, entry, "the %s act is in the history", action)
		sealed := sealedActCounterparty(t, entry)
		assert.Equal(t, now, sealed, "the %s act's own record seals who its check named", action)
		assert.Contains(t, sealed["ids"], "payee")
	})
}

func TestASignActSealsItsChecksCounterparty(t *testing.T) {
	actSealsItsChecksCounterparty(t, "sign")
}

func TestACancelActSealsItsChecksCounterparty(t *testing.T) {
	actSealsItsChecksCounterparty(t, "cancel")
}

// An approved sign is a decide Capsule, as a pay is: sign_document, accepted
// and executed, one_way_recoverable on this refundable deal.
func TestASignActIsDecideWithItsDisposition(t *testing.T) {
	eachRecordSet(t, func(t *testing.T, typed bool) {
		id := openSignAndCancel(t, typed)
		act, _ := actOn(t, id, "sign", 38000)
		capsule := dealCapsules(t, id)[act["capsule_id"].(string)]
		require.NotNil(t, capsule)
		assert.Equal(t, "decide", capsule["action_type"])
		disposition, ok := capsule["disposition"].(map[string]any)
		require.True(t, ok, "the sign act seals a disposition")
		assert.Equal(t, "accept", disposition["decision"])
		assert.Equal(t, "executed", disposition["verdict_class"])
		effect := capsule["effect"].(map[string]any)
		assert.Equal(t, "sign_document", effect["type"])
		assert.Equal(t, "one_way_recoverable", effect["irreversibility_class"])
	})
}
