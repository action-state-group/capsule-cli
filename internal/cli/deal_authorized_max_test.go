package cli

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stickerDeal opens a sticker purchase with a $5.00 limit, paid by rail, at
// a price of 454.
func stickerDeal(t *testing.T, rail string, typed bool) string {
	t.Helper()
	dealFixture(t)
	args := []string{"open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"buy me a funny otter sticker for at most 5 dollars","max_total_minor":500,"allowed":["pay"]},
		"who":{"name":"Sticker Marketplace","domain":"stickers.example"},
		"terms":{"item":"otter sticker","price_minor":454,"currency":"USD"},
		"recourse":{"rail":"`+rail+`","refundable":true}}`)}
	if typed {
		args = append(args, "--records", "typed")
	}
	return dealRun(t, args...)["deal_id"].(string)
}

// payCheck is a pay check of amount on rail, with authorized when it is not
// negative.
func payCheck(rail string, amount, authorized int) string {
	body := `{"action":"pay","amount_minor":` + strconv.Itoa(amount) + `,"terms":{"item":"otter sticker","price_minor":454},"recourse":{"rail":"` + rail + `","refundable":true}`
	if authorized >= 0 {
		body += `,"authorized_max_minor":` + strconv.Itoa(authorized)
	}
	return body + `}`
}

func differenceFields(check map[string]any) map[string]string {
	out := map[string]string{}
	for _, d := range check["differences"].([]any) {
		d := d.(map[string]any)
		field, _ := d["field"].(string)
		out[d["rule"].(string)] = field
	}
	return out
}

// The pre-authorisation buffer: the expected charge is 454, under the $5.00
// limit, but the approval lets the merchant take up to 954. A limit binds
// what may be taken, so the check pauses on the authorised maximum, and the
// difference names the field it evaluated.
func TestALimitBindsTheAuthorisedMaximumNotTheExpectedCharge(t *testing.T) {
	id := stickerDeal(t, "card", false)
	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 954)))
	assert.Equal(t, "pause", check["verdict"])
	assert.Equal(t, "authorized_max_minor", differenceFields(check)["over_limit"], "the evidence names the field evaluated")
	assert.Contains(t, check["card"], "Over your limit of $5.00")
	assert.Contains(t, check["card"], "up to $9.54 may be taken")
	assert.Contains(t, check["card"], "$4.54")
}

// Within the limit, the authorised maximum passes, and the sealed check
// carries it beside the expected charge: authorized_max_minor as given and
// spend_authorized_minor beside spend_minor.
func TestAnAuthorisedMaximumWithinTheLimitIsSealed(t *testing.T) {
	id := stickerDeal(t, "card", false)
	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 480)))
	assert.NotContains(t, differenceFields(check), "over_limit")
	records, export := exportRecords(t, id)
	var sealed map[string]any
	for _, r := range records {
		if r["x-deal-v0"].(map[string]any)["record_type"] == "check" {
			sealed = r["body"].(map[string]any)
		}
	}
	require.NotNil(t, sealed)
	assert.EqualValues(t, 454, sealed["amount_minor"])
	assert.EqualValues(t, 480, sealed["authorized_max_minor"])
	assert.EqualValues(t, 454, sealed["spend_minor"], "the expected charge, as before")
	assert.EqualValues(t, 480, sealed["spend_authorized_minor"], "what a per-action cap reads")
	checkProfile(t, export)
}

// Fail safe: a pay on a rail that can hold more than it charges (a card, a
// wallet, or any rail not known to be holdless) must state its authorised
// maximum; a pay on a holdless rail defaults it to the amount.
func TestACardPayMustStateItsAuthorisedMaximum(t *testing.T) {
	for _, rail := range []string{"card", "apple_pay", "paypal"} {
		id := stickerDeal(t, rail, false)
		_, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", id, "--input", writeJSON(t, payCheck(rail, 454, -1)))
		require.ErrorIs(t, err, ErrInput, rail)
		assert.Equal(t, 2, ExitCode(err), rail)
		assert.Contains(t, err.Error(), "authorized_max_minor", rail)
	}
	id := stickerDeal(t, "bank_transfer", false)
	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("bank_transfer", 454, -1)))
	assert.NotContains(t, differenceFields(check), "over_limit", "a holdless rail's amount is its maximum")
}

func TestAnAuthorisedMaximumBelowTheAmountIsRefused(t *testing.T) {
	id := stickerDeal(t, "card", false)
	_, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 400)))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, err.Error(), "authorized_max_minor")
}

// What was taken is measured against what may be taken: a capture above the
// authorised maximum differs from the check; one below it, though not the
// estimate, is what a hold is for.
func TestACaptureIsHeldToTheAuthorisedMaximum(t *testing.T) {
	act := func(t *testing.T, captured int) map[string]any {
		id := stickerDeal(t, "card", false)
		dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 480)))
		return dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":`+strconv.Itoa(captured)+`,"rail":"card"}`))
	}
	under := act(t, 470)
	assert.Equal(t, false, under["unchecked"], "a capture under the authorised maximum is covered by the check")
	over := act(t, 481)
	assert.Equal(t, true, over["unchecked"])
	assert.Equal(t, "differs_from_check", over["rule"])
	assert.Contains(t, over["reason"], "more than the authorized maximum")
}

// In a deal sealed in typed records, the proposed action carries the
// authorised maximum, so the evaluation's proposed_action_digest covers it.
func TestTheProposedActionCarriesTheAuthorisedMaximum(t *testing.T) {
	id := stickerDeal(t, "card", true)
	dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 480)))
	_, records := recordsOf(t, id)
	found := false
	for _, r := range records {
		if typeOf(r) == typeProposedAction {
			found = true
			assert.EqualValues(t, 480, r["body"].(map[string]any)["authorized_max_minor"])
		}
	}
	assert.True(t, found, "a proposed-action record")
	_, export := exportRecords(t, id)
	checkProfile(t, export)
}
