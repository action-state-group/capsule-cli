package cli

import (
	"encoding/json"
	"fmt"
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
		// A currency code inside a run of hex is part of a digest, not money.
		"1700aed04c2", "\"1700cad9f\"", "f01700bbd2", "4cad1700",
	} {
		assert.NotContains(t, moneyAmounts(text), int64(170000), text)
	}
}

// A digest that happens to hold a digit and then a currency code spelled in hex
// letters ("8aed", "4cad") is never an amount: this witness hash once read as
// 8 AED, a deal's 8.00 limit, and refused the deal's share.
func TestADigestIsNeverMoney(t *testing.T) {
	for _, text := range []string{
		`"witness":["8aed04c28dc0b81b61966fc58565d2bbad26412fc9294e6f9d5ef4cb26f12885"]`,
		"e3d8aed0", "0x8cad00", "8aed1", "1aed8", "7a3cad8e1f", "e1cad800f", "0b3cad8000", "deal-3cad800000000000",
	} {
		assert.Empty(t, moneyAmounts(text), text)
	}
	// A whole word still reads as money, glued or spaced.
	assert.Contains(t, moneyAmounts("8aed"), int64(800))
	assert.Contains(t, moneyAmounts("aed8"), int64(800))
	assert.Contains(t, moneyAmounts("pay 8 aed"), int64(800))
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

// The spending-limit gate reads past the deal's own ids and every digest: a
// deal whose id holds "3cad8", or a copy whose digests hold "cad800", shares
// normally, and the prose forms still trip it.
func TestTheCeilingGateReadsPastIDsAndDigests(t *testing.T) {
	for _, limit := range []int64{800, 80000} {
		events := []sealedEvent{{Event: dealEvent{Kind: "open", DealID: "deal-3cad800000000000", Open: &dealOpen{
			Intent: dealIntent{MaxTotalMinor: &limit}, Terms: dealTerms{Currency: "USD"}}}}}
		copyText := `{"deal_id":"deal-3cad800000000000","log":"deal/deal-3cad800000000000:1:0",` +
			`"capsule_id":"7a3cad8e1f00c0ffee000000000000000000000000000000000000000000cad800",` +
			`"witness":["8aed04c28dc0b81b61966fc58565d2bbad26412fc9294e6f9d5ef4cb26f12885"],` +
			`"key":"0b3cad8000aed8004cad8001234567890abcdef0123456789abcdef01234567"}`
		assert.NoError(t, dealCeilingGate([]byte(copyText), events, dealAudienceCounterparty), "limit %d", limit)

		// The planted controls: the limit as prose still trips the gate.
		major := limit / 100
		for _, prose := range []string{fmt.Sprintf("my limit is CAD %d", major), fmt.Sprintf("up to %d cad", major), fmt.Sprintf("$%d.00 at most", major)} {
			err := dealCeilingGate([]byte(copyText+" "+prose), events, dealAudienceCounterparty)
			assert.ErrorContains(t, err, "your spending limit", prose)
		}
	}
}

// The ceiling gate blanks every whole hex token before it reads a copy: a
// digest or key that happens to be all digits ("0000000000000800") after a
// money word reads to the money matcher as 800.00, and only the strip keeps
// it from refusing the share. The limit written as prose still trips it.
func TestTheCeilingGateBlanksHexTokens(t *testing.T) {
	limit := int64(80000)
	events := []sealedEvent{{Event: dealEvent{Kind: "open", DealID: "deal-0123456789abcdef", Open: &dealOpen{
		Intent: dealIntent{MaxTotalMinor: &limit}, Terms: dealTerms{Currency: "USD"}}}}}
	copyText := `{"pay":"0000000000000800","for":"0000000000000800"}`
	require.Contains(t, moneyAmounts(copyText), limit, "the matcher alone reads the digest as the limit")
	assert.NoError(t, dealCeilingGate([]byte(copyText), events, dealAudienceCounterparty))
	assert.ErrorContains(t, dealCeilingGate([]byte(copyText+" I will pay 800 for it"), events, dealAudienceCounterparty), "your spending limit")
}
