package deal

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"github.com/action-state-group/capsule-cli/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// terms.handover is how a sold item reaches the buyer (capsule-engine#192):
// in_person or shipped, a seller's term stated on the offer and the commit
// that rests on it, and on a sale. The seller rules read it at the commit:
// cash passes only in person.

const handoverOffer = `{"action":"offer","amount_minor":190000,"terms":{"item":"example bicycle","quantity":1,"price_minor":190000,"handover":"in_person"},"recourse":{"rail":"cash","refundable":false}}`

func handoverCommit(handover string) string {
	return `{"action":"commit","amount_minor":190000,"terms":{"item":"example bicycle","quantity":1,"price_minor":190000,"handover":"` + handover + `"},"recourse":{"rail":"cash","refundable":false}}`
}

// The check body carries the handover under terms, and the rules checker is
// given it with the commit's rail.
func TestASellersCommitStatesTheRailAndTheHandover(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	check := approved(t, id, dealRun(t, "check", "--deal", id, "--input", writeJSON(t, handoverOffer)))
	require.Equal(t, false, dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"offer","amount_minor":190000}`))["unchecked"])
	_, err := acceptOffer(t, id, check)
	require.NoError(t, err)

	checker := stubChecker(t, "rules", checkerPrints("allow", passFinding))
	pinChecker(t, map[string]any{"command": []string{checker}})
	approved(t, id, dealRun(t, "check", "--deal", id, "--input", writeJSON(t, handoverCommit("in_person"))))
	raw, err := os.ReadFile(checker + ".input")
	require.NoError(t, err)
	var input map[string]any
	require.NoError(t, json.Unmarshal(raw, &input))
	body := input["record"].(map[string]any)["agent_input"].(map[string]any)["body"].(map[string]any)
	assert.Equal(t, "commit", body["action"])
	assert.Equal(t, "in_person", body["terms"].(map[string]any)["handover"])
	assert.Equal(t, "cash", body["recourse"].(map[string]any)["rail"])

	act := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"commit","amount_minor":190000}`))
	assert.Equal(t, false, act["unchecked"], "the commit seals the accepted offer's terms, handover included")
	checkerPasses(t, id)
}

// A commit seals exactly the accepted offer's terms: a commit naming another
// handover, or none where the offer named one, is not covered by it.
func TestACommitOnAnotherHandoverIsNotTheAcceptedOffer(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	check := approved(t, id, dealRun(t, "check", "--deal", id, "--input", writeJSON(t, handoverOffer)))
	dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"offer","amount_minor":190000}`))
	_, err := acceptOffer(t, id, check)
	require.NoError(t, err)
	for name, commit := range map[string]string{
		"shipped":     handoverCommit("shipped"),
		"no handover": `{"action":"commit","amount_minor":190000,"terms":{"item":"example bicycle","quantity":1,"price_minor":190000},"recourse":{"rail":"cash","refundable":false}}`,
	} {
		approved(t, id, dealRun(t, "check", "--deal", id, "--input", writeJSON(t, commit)))
		act := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"commit"}`))
		assert.Equal(t, "changed_after_acceptance", act["rule"], name)
	}
	checkerPasses(t, id)
}

// The handover is in neither count's field list, so stating it moves no
// basis and counts as no changed field.
func TestTheHandoverIsNotAMaterialOrOfferField(t *testing.T) {
	for _, list := range [][]string{materialFields, offerFields} {
		assert.NotContains(t, list, "terms.handover")
	}
	values := dealFieldValues(dealTerms{Item: "bike", Handover: "in_person"}, nil, "")
	assert.Equal(t, map[string]interface{}{"terms.item": "bike"}, values)
	assert.Equal(t, 0, changedFields(materialFields, values, dealFieldValues(dealTerms{Item: "bike"}, nil, "")))
	assert.Equal(t, "in_person", termsBody(dealTerms{Handover: "in_person"})["handover"])
	assert.NotContains(t, termsBody(dealTerms{Item: "bike"}), "handover")
}

func TestTheHandoverIsAClosedSet(t *testing.T) {
	for _, v := range []string{"in_person", " In_Person ", "shipped"} {
		terms := dealTerms{Handover: v}
		require.NoError(t, normalizeTerms(&terms), v)
	}
	for _, v := range []string{"pickup", "mail", "in person"} {
		err := normalizeTerms(&dealTerms{Handover: v})
		require.ErrorIs(t, err, cli.ErrInput, v)
		assert.Contains(t, err.Error(), "in_person or shipped", v)
	}
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	_, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", id, "--input", writeJSON(t,
		`{"action":"offer","amount_minor":190000,"terms":{"item":"example bicycle","price_minor":190000,"handover":"drone"}}`))
	require.ErrorIs(t, err, cli.ErrInput)
}

// capsulectl refuses the handover on a buyer's deal: it is a seller's term.
func TestABuyersDealStatesNoHandover(t *testing.T) {
	dealFixture(t)
	_, err := invoke(t, "", "--profile", "deal", "deal", "open", "--input", writeJSON(t, `{"type":"purchase","channel":"marketplace",
		"intent":{"verbatim":"buy the bike","allowed":["pay","commit"]},
		"who":{"name":"Pat Seller"},"terms":{"item":"bike","price_minor":12000,"currency":"USD","handover":"in_person"},"recourse":{"rail":"cash","refundable":false}}`))
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Contains(t, err.Error(), "only where the user sells")

	id := openTypedSticker(t)
	before := len(records(t, id))
	_, err = invoke(t, "", "--profile", "deal", "deal", "check", "--deal", id, "--input", writeJSON(t,
		`{"action":"commit","amount_minor":600,"terms":{"item":"otter sticker","price_minor":600,"handover":"shipped"}}`))
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Contains(t, err.Error(), "only where the user sells")
	assert.Len(t, records(t, id), before, "a refused check seals nothing")
}

// Where the user sells, the handover goes on a check or the opening, never
// on what the user asked, a counterparty's change or a close.
func TestASellerStatesTheHandoverOnlyOnACheckOrTheOpening(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	before := len(records(t, id))
	for kind, input := range map[string]string{
		"change": `{"source":"buyer message","terms":{"handover":"shipped"}}`,
		"intent": `{"verbatim":"hand it over in person","asked":{"handover":"in_person"}}`,
	} {
		_, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", id, "--kind", kind, "--input", writeJSON(t, input))
		require.ErrorIs(t, err, cli.ErrInput, kind)
		assert.Contains(t, err.Error(), "Leave it out of other checks", kind)
	}
	_, err := invoke(t, "", "--profile", "deal", "deal", "close", "--deal", id, "--input", writeJSON(t,
		`{"status":"received","delivered":{"item":"example bicycle","handover":"in_person"}}`))
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Len(t, records(t, id), before, "nothing is sealed")

	_, err = invoke(t, "", "--profile", "deal", "deal", "open", "--records", "typed", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"Sell my bike","party_role":"seller","asked":{"item":"bike","handover":"in_person"},"allowed":["offer","commit"]},
		"who":{"name":"Example Buyer"},"terms":{"item":"bike","price_minor":12000,"currency":"USD"},"recourse":{"rail":"cash","refundable":false}}`))
	require.ErrorIs(t, err, cli.ErrInput, "what the user asked states no handover")

	opened := openTyped(t, `{"type": "purchase", "demo": true, "channel": "web",
 "intent": {"verbatim": "Sell my bike, pickup only", "party_role": "seller", "allowed": ["offer", "commit"]},
 "who": {"name": "Example Buyer", "domain": "buyer.example"},
 "terms": {"item": "bike", "quantity": 1, "price_minor": 12000, "currency": "USD", "handover": "in_person"},
 "recourse": {"rail": "cash", "refundable": false}}`)
	baseline := records(t, opened)[0]["body"].(map[string]any)
	assert.Equal(t, "in_person", baseline["terms"].(map[string]any)["handover"])
	checkerPasses(t, opened)
}

// A sale states its handover in its own terms, and a thread under it states
// it on the offer and the commit.
func TestASaleStatesItsHandover(t *testing.T) {
	dealFixture(t)
	out := dealRun(t, "sale", "new", "--input", writeJSON(t, `{"type": "purchase", "demo": true,
 "intent": {"verbatim": "Sell my example bicycle; pickup only", "asked": {"item": "example bicycle", "quantity": 1}, "allowed": ["offer", "commit"]},
 "terms": {"item": "example bicycle", "quantity": 1, "price_minor": 190000, "currency": "USD", "handover": "in_person"},
 "recourse": {"rail": "cash", "refundable": false}}`))
	sale := out["sale_id"].(string)
	recs, _ := exportSale(t, sale)
	assert.Equal(t, "in_person", recs[0]["body"].(map[string]any)["terms"].(map[string]any)["handover"])
	thread := buyerThread(t, sale, "buyer.example")

	// The thread's offer and the commit on it state the handover, and the
	// profile checker passes the thread and the sale's own log.
	offer := approved(t, thread, dealRun(t, "check", "--deal", thread, "--input", writeJSON(t, handoverOffer)))
	require.Equal(t, false, dealRun(t, "note", "--deal", thread, "--kind", "act", "--input", writeJSON(t, `{"action":"offer","amount_minor":190000}`))["unchecked"])
	_, err := acceptOffer(t, thread, offer)
	require.NoError(t, err)
	approved(t, thread, dealRun(t, "check", "--deal", thread, "--input", writeJSON(t, handoverCommit("in_person"))))
	act := dealRun(t, "note", "--deal", thread, "--kind", "act", "--input", writeJSON(t, `{"action":"commit","amount_minor":190000}`))
	assert.Equal(t, false, act["unchecked"], "the commit rests on the accepted offer, handover included")
	var stated []string
	for _, r := range records(t, thread) {
		if b, ok := r["body"].(map[string]any); ok && r["type"] == "proposed-action/v0" {
			if terms, ok := b["terms"].(map[string]any); ok {
				stated = append(stated, b["action"].(string)+" "+terms["handover"].(string))
			}
		}
	}
	assert.Equal(t, []string{"offer in_person", "commit in_person"}, stated)

	python := profilePython(t)
	if python == "" {
		t.Skip("no Python with jsonschema for the profile checker")
	}
	checkerPasses(t, thread)
	_, salePath := exportSale(t, sale)
	result, err := exec.Command(python, dealProfileDir+"/check_profile.py", salePath).CombinedOutput()
	require.NoError(t, err, string(result))
	assert.Contains(t, string(result), "ALL OK")
}

// A seller's check of any action but an offer or a commit states no
// handover: a share, a payment or a representation says nothing of how the
// item reaches the buyer.
func TestOnlyAnOfferOrACommitCheckStatesTheHandover(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	before := len(records(t, id))
	for name, check := range map[string]string{
		"share_contact": `{"action":"share_contact","disclosing_to":"counterparty","disclosing":["phone"],"terms":{"handover":"in_person"}}`,
		"pay":           `{"action":"pay","amount_minor":500,"authorized_max_minor":500,"terms":{"handover":"shipped"},"recourse":{"rail":"card","refundable":true}}`,
		"represent":     `{"action":"represent","representing":{"class":"condition","text":"a scratch on the frame"},"terms":{"handover":"in_person"}}`,
	} {
		_, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", id, "--input", writeJSON(t, check))
		require.ErrorIs(t, err, cli.ErrInput, name)
		if name != "represent" { // a represent check refuses any terms first
			assert.Contains(t, err.Error(), "Leave it out of other checks", name)
		}
	}
	_, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", id, "--input", writeJSON(t,
		`{"action":"share_contact","disclosing_to":"counterparty","disclosing":["phone"],"terms":{"handover":"in_person"}}`))
	assert.Contains(t, err.Error(), "Leave it out of other checks")
	assert.Len(t, records(t, id), before, "a refused check seals nothing")
}
