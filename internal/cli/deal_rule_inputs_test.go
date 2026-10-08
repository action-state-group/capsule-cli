package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The two field lists' basis digests, computed independently: lowercase hex
// SHA-256 of the JCS bytes of each list, in order.
func TestTheFieldListBasesArePinned(t *testing.T) {
	assert.Equal(t, "effa16bb95192a3208f24daa127a4f0ac387095e139856b7ce390ebd96b772c3", materialFieldsBasis)
	assert.Equal(t, "52ddf74622010caba9ef3baf4d9600365e8670169fb4a56a3c5a5a1cea93739d", offerFieldsBasis)
	assert.Len(t, materialFields, 11)
	assert.Len(t, offerFields, 6)
}

func TestChangedFieldsCounting(t *testing.T) {
	price := func(v int64) *int64 { return &v }
	yes, no := true, false
	base := dealFieldValues(dealTerms{Item: "otter sticker", PriceMinor: price(454), Currency: "USD", Conditions: map[string]string{"size": "small", "colour": "blue"}},
		&dealRecourse{Rail: "card", Refundable: &yes}, "Sticker Marketplace")
	for name, tc := range map[string]struct {
		other map[string]interface{}
		want  int
	}{
		"the same":            {base, 0},
		"one value differs":   {dealFieldValues(dealTerms{Item: "otter sticker", PriceMinor: price(500), Currency: "USD", Conditions: map[string]string{"size": "small", "colour": "blue"}}, &dealRecourse{Rail: "card", Refundable: &yes}, "Sticker Marketplace"), 1},
		"present vs absent":   {dealFieldValues(dealTerms{Item: "otter sticker", PriceMinor: price(454), Currency: "USD", DepositMinor: price(100), Conditions: map[string]string{"size": "small", "colour": "blue"}}, &dealRecourse{Rail: "card", Refundable: &yes}, "Sticker Marketplace"), 1},
		"absent vs present":   {dealFieldValues(dealTerms{Item: "otter sticker", PriceMinor: price(454), Conditions: map[string]string{"size": "small", "colour": "blue"}}, &dealRecourse{Rail: "card", Refundable: &yes}, "Sticker Marketplace"), 1},
		"conditions are one":  {dealFieldValues(dealTerms{Item: "otter sticker", PriceMinor: price(454), Currency: "USD", Conditions: map[string]string{"size": "large", "colour": "red", "finish": "gloss"}}, &dealRecourse{Rail: "card", Refundable: &yes}, "Sticker Marketplace"), 1},
		"conditions in order": {dealFieldValues(dealTerms{Item: "otter sticker", PriceMinor: price(454), Currency: "USD", Conditions: map[string]string{"colour": "blue", "size": "small"}}, &dealRecourse{Rail: "card", Refundable: &yes}, "Sticker Marketplace"), 0},
		"case matters":        {dealFieldValues(dealTerms{Item: "Otter Sticker", PriceMinor: price(454), Currency: "USD", Conditions: map[string]string{"size": "small", "colour": "blue"}}, &dealRecourse{Rail: "card", Refundable: &yes}, "Sticker Marketplace"), 1},
		"rail, refund, payee": {dealFieldValues(dealTerms{Item: "otter sticker", PriceMinor: price(454), Currency: "USD", Conditions: map[string]string{"size": "small", "colour": "blue"}}, &dealRecourse{Rail: "zelle", Refundable: &no}, "M. Torres"), 3},
		"everything":          {dealFieldValues(dealTerms{Item: "x", Quantity: 2, PriceMinor: price(1), DepositMinor: price(1), Currency: "EUR", When: "Sat", Place: "Pier", Conditions: map[string]string{"a": "b"}}, &dealRecourse{Rail: "wire", Refundable: &no}, "Someone"), 11},
		"nothing on one side": {map[string]interface{}{}, 7},
	} {
		t.Run(name, func(t *testing.T) {
			n := changedFields(materialFields, base, tc.other)
			assert.Equal(t, tc.want, n)
			assert.LessOrEqual(t, n, len(materialFields), "bounded by the list")
		})
	}
	assert.Equal(t, 0, changedFields(offerFields, map[string]interface{}{}, map[string]interface{}{}), "both absent is unchanged")
}

// ruleInputs is the sealed check body of the deal's latest check, and what
// the pinned checker was given.
func ruleInputs(t *testing.T, id, check string) (sealed, given, input map[string]any) {
	t.Helper()
	checker := stubChecker(t, "rules", checkerPrints("allow", passFinding))
	pinChecker(t, map[string]any{"command": []string{checker}})
	dealRun(t, "check", "--deal", id, "--input", writeJSON(t, check))
	records, export := exportRecords(t, id)
	checkProfile(t, export)
	for _, r := range records {
		if typeOf(r) == typeProposedAction {
			sealed = r["body"].(map[string]any)
		} else if b, ok := r["x-deal-v0"].(map[string]any); ok && b["record_type"] == "check" {
			sealed = r["body"].(map[string]any)
		}
	}
	raw, err := os.ReadFile(checker + ".input")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &input))
	given = input["record"].(map[string]any)["agent_input"].(map[string]any)["body"].(map[string]any)
	return sealed, given, input
}

const stickerPay = `{"action":"pay","amount_minor":454,"authorized_max_minor":454,"terms":{"item":"otter sticker","price_minor":454},"recourse":{"rail":"card","refundable":true}}`

// Each field, in both record sets: emitted where the deal holds it, as the
// checker is given it.
func TestACheckCarriesTheRuleInputs(t *testing.T) {
	for name, typed := range map[string]bool{"x-deal-v0": false, "typed": true} {
		t.Run(name, func(t *testing.T) {
			id := stickerDeal(t, "card", typed)
			sealed, given, input := ruleInputs(t, id, stickerPay)
			for _, b := range []map[string]any{sealed, given} {
				assert.Equal(t, "web", b["channel"])
				assert.Equal(t, "web", b["first_contact_channel"])
				assert.Equal(t, float64(0), b["material_fields_changed"], "what was agreed")
				assert.Equal(t, materialFieldsBasis, b["material_fields_basis"])
				assert.Equal(t, offerFieldsBasis, b["offer_fields_basis"])
				assert.NotContains(t, b, "recipient_role", "a pay has no recipient role")
				assert.NotContains(t, b, "upfront_amount_minor", "no deposit stated")
			}
			// The user's words state no terms here, so every offer field the
			// proposal states counts: item, price and refundability.
			assert.Equal(t, float64(3), sealed["offer_fields_changed"])
			if !typed {
				assert.NotContains(t, sealed, "task_authority_ref", "no task-authority record in an x-deal-v0 deal")
				assert.NotContains(t, input, "task_authority")
				return
			}
			ref := sealed["task_authority_ref"].(map[string]any)
			ta := input["task_authority"].(map[string]any)
			rawTA, err := json.Marshal(ta["agent_input"])
			require.NoError(t, err)
			decoder := json.NewDecoder(bytes.NewReader(rawTA))
			decoder.UseNumber()
			var record any
			require.NoError(t, decoder.Decode(&record))
			digest, err := canonical.JSONDigest(record)
			require.NoError(t, err)
			assert.Equal(t, ref["digest"], digest, "the task authority given is the one the ref names")
			body := ta["agent_input"].(map[string]any)["body"].(map[string]any)
			assert.Equal(t, []any{"pay"}, body["allowed_actions"])
			assert.Equal(t, body["allowed"], body["allowed_actions"])
			assert.Equal(t, []any{}, body["preconditions"])
		})
	}
}

// A changed channel, a changed price and refundability, and terms the user's
// own words state.
func TestTheRuleInputsFollowTheDeal(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"marketplace",
		"intent":{"verbatim":"buy me a funny otter sticker for 4.54","asked":{"item":"otter sticker","price_minor":454},"max_total_minor":500,"allowed":["pay"]},
		"who":{"name":"Sticker Marketplace","domain":"stickers.example"},
		"terms":{"item":"otter sticker","price_minor":454,"currency":"USD"},
		"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	dealRun(t, "note", "--deal", id, "--kind", "message", "--input", writeJSON(t, `{"from":"counterparty","channel":"whatsapp","text":"message me here instead"}`))
	sealed, _, _ := ruleInputs(t, id, `{"action":"pay","amount_minor":480,"authorized_max_minor":480,"terms":{"item":"otter sticker","price_minor":480},"recourse":{"rail":"card","refundable":false}}`)
	assert.Equal(t, "whatsapp", sealed["channel"], "the channel in use now")
	assert.Equal(t, "marketplace", sealed["first_contact_channel"])
	assert.Equal(t, float64(2), sealed["material_fields_changed"], "price and refundability")
	// Against the user's words (item, price): the price differs, and
	// refundability is stated only by the proposal.
	assert.Equal(t, float64(2), sealed["offer_fields_changed"])
}

// A share says who receives it, by role.
func TestAShareCarriesTheRecipientRole(t *testing.T) {
	for check, role := range map[string]string{
		`{"action":"share_contact","disclosing_to":"counterparty","disclosing":["phone"]}`:                                                            "fulfilling_merchant",
		`{"action":"share_contact","disclosing":["address"],"disclosing_to":"other","recipient":{"name":"Quick Couriers","phone":"+1 555 010 7777"}}`: "third_party",
	} {
		t.Run(role, func(t *testing.T) {
			dealFixture(t)
			id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"marketplace",
				"intent":{"verbatim":"buy the bike and arrange pickup","allowed":["pay","share_contact"]},
				"who":{"name":"Pat Seller"},"terms":{"item":"bike","price_minor":12000,"currency":"USD"},"recourse":{"rail":"cash","refundable":false}}`))["deal_id"].(string)
			sealed, given, _ := ruleInputs(t, id, check)
			assert.Equal(t, role, sealed["recipient_role"])
			assert.Equal(t, role, given["recipient_role"])
			raw, err := json.Marshal(sealed)
			require.NoError(t, err)
			for _, never := range []string{"Quick Couriers", "Pat Seller", "555 010 7777"} {
				assert.NotContains(t, string(raw), never, "a role, never a name or a number")
			}
		})
	}
}

// A step sealed before records carried the rule inputs re-derives without
// them: its record's bytes do not change.
func TestAStepWithoutRuleInputsReDerivesUnchanged(t *testing.T) {
	id := stickerDeal(t, "card", false)
	dealRun(t, "check", "--deal", id, "--input", writeJSON(t, stickerPay))
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	require.NoError(t, s.useDeal(t.Context(), id, false))
	events, err := s.load(t.Context(), id)
	require.NoError(t, err)
	for i, se := range events {
		if se.Event.Kind != "snapshot" {
			continue
		}
		ev := se.Event
		ev.RuleInputs = ""
		raw, _, err := encodeDealRecord(ev, events[:i], s.dkey)
		require.NoError(t, err)
		for _, field := range []string{"channel", "material_fields_changed", "offer_fields_basis", "upfront_amount_minor", "recipient_role"} {
			assert.False(t, strings.Contains(string(raw), `"`+field+`"`), field)
		}
	}
}
