package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The floor of 1700.00 as prose writes it: each form is a planted positive
// control, read as 170000 minor units.
var floorForms = []string{
	"$1,700.00", "$1700", "$ 1,700", "1.700,00 €", "€1.700", "1 700 €", "1 700,00 €", "1 700 €",
	"1'700.00 CHF", "1’700 chf", "USD 1,700", "usd 1700.00", "1700 usd", "1,700 USD", "1700,00 EUR",
	"1,700 dollars", "1700 bucks", "£1,700", "1.7k dollars", "$1.7k",
	"the lowest I'll take is 1700", "asking 1,700 for it", "I could go down to 1700.00", "won't sell under 1700",
	// A code or a currency word written against the number.
	"USD1,700", "EUR1700", "1700usd", "$1700USD", "1,700.00EUR", "GBP1.700,00", "1700dollars",
}

func TestMoneyAmountsReadsEveryForm(t *testing.T) {
	for _, form := range floorForms {
		assert.Contains(t, moneyAmounts("Ok: "+form+", final."), int64(170000), form)
	}
}

// What is not money is not read as an amount: dates, times, ids, digests,
// and a bare number with no money beside it.
func TestMoneyAmountsLeavesOtherNumbersAlone(t *testing.T) {
	for _, text := range []string{
		"on 2026-10-09 at 17:00", "2026-10-09T17:00:00Z", "deal-ab1700cd00112233", "ab1700cd",
		"order 1700-22", "1700 Main Street", "I took 1700 photos", "call 555-1700",
		// Letters against the number that are not a whole currency code.
		"xusd1700", "1700usdx", "abc1700", "1700abc",
	} {
		assert.NotContains(t, moneyAmounts(text), int64(170000), text)
	}
}

func agentSays(t *testing.T, dealID, text string) error {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"from": "agent", "text": text})
	require.NoError(t, err)
	body := string(raw)
	_, err = invoke(t, "", "--profile", "deal", "deal", "note", "--deal", dealID, "--kind", "message", "--input", writeJSON(t, body))
	return err
}

// A seller's agent never tells the buyer the floor, in any form: the
// message is refused before it is sealed, naming the field, never the value.
func TestAnAgentMessageNeverStatesTheFloor(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, sellerWithFloor))["deal_id"].(string)
	before := len(chainSteps(t, id))
	for _, form := range floorForms {
		err := agentSays(t, id, "Honestly, "+form+" is fine by me.")
		require.ErrorIs(t, err, ErrInput, form)
		assert.Contains(t, err.Error(), "intent.min_total_minor", form)
		for _, v := range []string{"1700", "1,700", "1.700", "170000", "1.7k"} {
			assert.NotContains(t, err.Error(), v, "the refusal names the field, never the value")
		}
	}
	assert.Len(t, chainSteps(t, id), before, "nothing was sealed")

	// The asking price, any other amount, and the user's own words pass.
	require.NoError(t, agentSays(t, id, "The asking price is $1,900.00."))
	require.NoError(t, agentSays(t, id, "Shipping would be $25 extra; it has 1700 miles on it."))
	dealRun(t, "note", "--deal", id, "--kind", "message", "--input", writeJSON(t, `{"from": "user", "text": "I'd take 1700 if I must"}`))

	// Nor does an agent's claim, which the buyer's copy opens.
	_, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", id, "--kind", "claim", "--input",
		writeJSON(t, `{"text": "Price is firm above $1,700", "source_kind": "agent", "class": "other"}`))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, err.Error(), "intent.min_total_minor")
}

// A buyer's agent never tells the merchant the spending limit either.
func TestAnAgentMessageNeverStatesTheLimit(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, merchantOpen)
	err := agentSays(t, id, "My budget tops out at USD 600.")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, err.Error(), "intent.max_total_minor")
	require.NoError(t, agentSays(t, id, "Is $558.80 the final price?"))
}

// The share gate reads the same forms: a shared copy that states the floor
// in any of them is refused, for every shared audience.
func TestTheShareGateReadsEveryFormOfTheFloor(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, sellerWithFloor))["deal_id"].(string)
	events := chainSteps(t, id)
	for _, audience := range []string{dealAudienceCounterparty, dealAudienceAdjudicator} {
		for _, form := range floorForms {
			assert.Error(t, dealCeilingGate([]byte(`<p>`+form+`</p>`), events, audience), audience+": "+form)
		}
		assert.NoError(t, dealCeilingGate([]byte(`<p>About to offer $1,900.00 on 2026-10-09T17:00:00Z</p>`), events, audience), audience)
	}
	assert.False(t, strings.Contains(strings.Join(floorForms, " "), "170000"), "the controls are written forms, not minor units")
}
