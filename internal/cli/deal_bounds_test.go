package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sellerWithFloor = `{"type": "purchase", "demo": true, "channel": "web",
 "intent": {"verbatim": "Sell my example bicycle; ask 1900, go down to 1700", "party_role": "seller",
            "min_total_minor": 170000, "asked": {"item": "example bicycle", "quantity": 1}, "allowed": ["commit"]},
 "who": {"name": "Example Buyer", "domain": "buyer.example"},
 "terms": {"item": "example bicycle", "quantity": 1, "price_minor": 190000, "currency": "USD"},
 "recourse": {"rail": "card", "refundable": false}}`

// The cross-implementation vector: the profile checker writes it, and Go
// recomputes every text and bounds_commitment with its own JCS and commitText.
func TestCommercialBoundsVectorsMatch(t *testing.T) {
	var vec struct {
		Vectors []struct {
			Document         map[string]any `json:"document"`
			Nonce            string         `json:"nonce"`
			Text             string         `json:"text"`
			BoundsCommitment string         `json:"bounds_commitment"`
		} `json:"vectors"`
	}
	require.NoError(t, json.Unmarshal(mustRead(t, "../../skills/deal/profile/fixtures/commercial-bounds-vectors.json"), &vec))
	require.Len(t, vec.Vectors, 3)
	schema, err := recordSchema(typeCommercialBounds)
	require.NoError(t, err)
	for _, x := range vec.Vectors {
		raw, err := json.Marshal(x.Document)
		require.NoError(t, err)
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		require.NoError(t, err)
		require.NoError(t, schema.Validate(doc), "a commercial-bounds/v0 document fits its schema")
		var min int64
		require.NoError(t, json.Unmarshal(mustJSON(t, x.Document["min_total_minor"]), &min))
		assert.Equal(t, x.Text, commercialBoundsText(min))
		got, err := commitText(x.Nonce, x.Text)
		require.NoError(t, err)
		assert.Equal(t, x.BoundsCommitment, got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// A seller's floor: never in a sealed record, opened in the user's own copy,
// asked about when an offer goes below it, and kept from the buyer.
func TestASellersFloorStaysPrivate(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, sellerWithFloor))["deal_id"].(string)

	// Every deal record carries the commitment, never the floor; the own copy's
	// sealed report opens it, and the opening recomputes the commitment.
	report, chain := ownReportAndChain(t, id)
	assert.NotContains(t, chain, "min_total_minor")
	assert.NotContains(t, chain, "170000")
	assert.Contains(t, chain, "bounds_commitment")
	openings := report["bounds_openings"].([]any)
	require.Len(t, openings, 1)
	o := openings[0].(map[string]any)
	doc := o["document"].(map[string]any)
	assert.Equal(t, "commercial-bounds/v0", doc["type"])
	assert.Equal(t, float64(170000), doc["min_total_minor"])
	c, err := commitText(o["nonce"].(string), commercialBoundsText(170000))
	require.NoError(t, err)
	assert.Contains(t, chain, `"bounds_commitment":"`+c+`"`)

	// An offer below the floor is asked about; one at or above it is not.
	below := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"commit","terms":{"item":"example bicycle","price_minor":165000}}`))
	assert.Equal(t, "pause", below["verdict"])
	assert.Contains(t, mustJSONString(t, below["differences"]), `"rule":"under_floor"`)
	at := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"commit","terms":{"item":"example bicycle","price_minor":175000}}`))
	assert.NotContains(t, mustJSONString(t, at["differences"]), "under_floor")

	// The buyer's copy carries neither the floor nor its amount.
	_, shared := sharedCopy(t, id, dealAudienceCounterparty, "the buyer")
	assert.NotContains(t, shared, "min_total_minor")
	assert.NotContains(t, shared, "1700.00")
	assert.NotContains(t, shared, "bounds_openings")
}

func mustJSONString(t *testing.T, v any) string {
	t.Helper()
	return string(mustJSON(t, v))
}

// Raising the floor narrows what the agent may accept and applies; lowering
// it asks for more and is only a proposal until the user confirms it.
func TestRaisingTheFloorAppliesLoweringItIsAProposal(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, sellerWithFloor))["deal_id"].(string)

	raised := dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim": "Not under 1800 now", "min_total_minor": 180000, "allowed": ["commit"]}`))
	assert.Nil(t, raised["proposed"], "a higher floor narrows and applies")
	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"commit","terms":{"item":"example bicycle","price_minor":175000}}`))
	assert.Contains(t, mustJSONString(t, check["differences"]), "under_floor", "1,750 is under the raised floor")

	lowered := dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim": "Fine, 1600 is ok", "min_total_minor": 160000, "allowed": ["commit"]}`))
	require.NotNil(t, lowered["proposed"], "a lower floor asks for more")
	check = dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"commit","terms":{"item":"example bicycle","price_minor":175000}}`))
	assert.Contains(t, mustJSONString(t, check["differences"]), "under_floor", "unconfirmed, the floor in force stays 1,800")

	dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", lowered["capsule_id"].(string), "--choice", "confirm_limits", "--said", "Yes, 1600 is fine")
	check = dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"commit","terms":{"item":"example bicycle","price_minor":175000}}`))
	assert.NotContains(t, mustJSONString(t, check["differences"]), "under_floor", "confirmed, the floor is 1,600")

	// The confirmation seals the floor as commitments only.
	_, chain := ownReportAndChain(t, id)
	assert.NotContains(t, chain, "min_total_minor")
}

// ownReportAndChain is the own copy's sealed report, and every other record
// and disclosure of the deal as JSON text.
func ownReportAndChain(t *testing.T, dealID string) (map[string]any, string) {
	t.Helper()
	var own map[string]any
	require.NoError(t, json.Unmarshal([]byte(dealOwnBundle(t, dealID)), &own))
	reportID := own["extensions"].(map[string]any)["x-deal-v0"].(map[string]any)["sealed_report"].(string)
	disclosures := own["disclosures"].(map[string]any)
	report := disclosures[reportID].(map[string]any)["agent_input"].(map[string]any)["report"].(map[string]any)
	rest := map[string]any{}
	for k, v := range disclosures {
		if k != reportID {
			rest[k] = v
		}
	}
	return report, mustJSONString(t, own["records"]) + mustJSONString(t, rest)
}

func TestAFloorAboveTheLimitIsRefused(t *testing.T) {
	dealFixture(t)
	bad := strings.Replace(sellerWithFloor, `"min_total_minor": 170000`, `"min_total_minor": 170000, "max_total_minor": 150000`, 1)
	_, err := invoke(t, "", "--profile", "deal", "deal", "open", "--input", writeJSON(t, bad))
	require.ErrorIs(t, err, ErrInput)
}

// A typed deal's task authority is shared whole: it carries the floor as
// bounds_commitment, and a confirmed lower floor gives a new version that
// carries the proposal's commitment.
func TestATypedTaskAuthorityCarriesTheFloorAsACommitment(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, sellerWithFloor))["deal_id"].(string)
	lowered := dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim": "1600 is ok", "min_total_minor": 160000, "allowed": ["commit"]}`))
	dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", lowered["capsule_id"].(string), "--choice", "confirm_limits", "--said", "Yes, 1600")

	_, chain := ownReportAndChain(t, id)
	assert.NotContains(t, chain, "min_total_minor")
	assert.Equal(t, 2, strings.Count(chain, `"type":"task-authority/v0"`), "the opening authority and the confirmed version")
	proposal := []string{}
	for _, part := range strings.Split(chain, `"bounds_commitment":"`)[1:] {
		proposal = append(proposal, part[:64])
	}
	// The intent note's commitment is the one the confirmed version carries.
	note, err := commitText(dealNonceOf(t, id, lowered["capsule_id"].(string), "bounds"), commercialBoundsText(160000))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, strings.Count(chain, note), 2, "the note seals it, and the confirmed task authority carries it")
	assert.NotEmpty(t, proposal)
}

// dealNonceOf is a sealed step's nonce, from the deal's own local values.
func dealNonceOf(t *testing.T, dealID, capsuleID, name string) string {
	t.Helper()
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	events, err := s.loadOther(t.Context(), dealID)
	require.NoError(t, err)
	for _, se := range events {
		if se.CapsuleID == capsuleID {
			return se.Event.Nonces[name]
		}
	}
	t.Fatalf("no step %s", capsuleID)
	return ""
}

// The profile's own rules checker, on this device, is given the opening of
// the floor in force, which it checks against the sealed commitment. A deal
// with no floor sends none.
func TestTheLocalRulesCheckerIsGivenTheFloorsOpening(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, sellerWithFloor))["deal_id"].(string)
	_, _, input := ruleInputs(t, id, `{"action":"commit","terms":{"item":"example bicycle","price_minor":175000}}`)
	o := input["commercial_bounds_opening"].(map[string]any)
	doc := o["document"].(map[string]any)
	assert.Equal(t, map[string]any{"type": "commercial-bounds/v0", "min_total_minor": float64(170000)}, doc)
	c, err := commitText(o["nonce"].(string), commercialBoundsText(170000))
	require.NoError(t, err)
	assert.Equal(t, c, o["bounds_commitment"])
	_, chain := ownReportAndChain(t, id)
	assert.Contains(t, chain, `"bounds_commitment":"`+c+`"`, "the opening is of the commitment the deal sealed")

	other := stickerDeal(t, "card", false)
	_, _, input = ruleInputs(t, other, stickerPay)
	assert.NotContains(t, input, "commercial_bounds_opening")
}

// One gate for both roles: a seller's counterparty copy may not name the floor
// as money (unless it equals an amount the copy may show anyway) nor either
// bound by name; the words say what was kept back for a seller.
func TestTheShareGateKeepsASellersFloor(t *testing.T) {
	floor, price := int64(170000), int64(190000)
	events := []sealedEvent{{Event: dealEvent{Kind: "open", Open: &dealOpen{
		Intent: dealIntent{PartyRole: dealRoleSeller, MinTotalMinor: &floor},
		Terms:  dealTerms{PriceMinor: &price, Currency: "USD"}}}}}
	err := dealCeilingGate([]byte("we will take $1700.00"), events, dealAudienceCounterparty)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the lowest price you will take")
	assert.NoError(t, dealCeilingGate([]byte("we will take $1700.00"), events, dealAudienceAdjudicator))
	assert.NoError(t, dealCeilingGate([]byte("asking $1900.00"), events, dealAudienceCounterparty), "the asked price is shown anyway")
	for _, key := range []string{"min_total_minor", "max_total_minor"} {
		assert.Error(t, dealCeilingGate([]byte(`{"`+key+`":1}`), events, dealAudienceCounterparty), key)
	}
}

// A seller's copy for the buyer withholds any limit the seller set as well
// as the floor, and says so in a seller's words.
func TestASellersShareWithholdsBothBounds(t *testing.T) {
	dealFixture(t)
	both := strings.Replace(sellerWithFloor, `"min_total_minor": 170000`, `"min_total_minor": 170000, "max_total_minor": 250000`, 1)
	id := dealRun(t, "open", "--input", writeJSON(t, both))["deal_id"].(string)
	_, shared := sharedCopy(t, id, dealAudienceCounterparty, "the buyer")
	for _, hidden := range []string{"min_total_minor", "max_total_minor", "250000", "2500.00", "1700.00"} {
		assert.NotContains(t, shared, hidden)
	}
	assert.Contains(t, shared, "the lowest price you will take, and any limit you set")
	assert.NotContains(t, shared, "your spending limit")
}

// A seller's share with the counterparty names its recipient as the buyer;
// a buyer's still names the fulfilling merchant.
func TestASellersCounterpartyIsTheBuyer(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, sellerWithFloor))["deal_id"].(string)
	share := `{"action":"share_contact","disclosing_to":"counterparty","description":"give the buyer my phone for pickup","disclosing":["phone"]}`
	sealed, given, _ := ruleInputs(t, id, share)
	assert.Equal(t, "buyer", sealed["recipient_role"])
	assert.Equal(t, "buyer", given["recipient_role"])

	other := stickerDeal(t, "card", false)
	sealed, _, _ = ruleInputs(t, other, share)
	assert.Equal(t, "fulfilling_merchant", sealed["recipient_role"])
}
