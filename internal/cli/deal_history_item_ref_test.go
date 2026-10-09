package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An act on a sale's thread reaches a later check's history with the sale's
// item reference beside it, so a rule on a second buyer's thread sees that
// the item was already accepted on the first. An act of any other deal has
// none, and no record carries it.
func TestAHistoryActOnASaleThreadCarriesTheSalesItemRef(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	item := saleItemRef(t, saleID)
	a := buyerThread(t, saleID, "buyer-a.example")
	b := buyerThread(t, saleID, "buyer-b.example")

	// The first buyer accepts the offer and the seller commits to it.
	offer := makeOffer(t, a, "190000")
	_, err := acceptOffer(t, a, offer)
	require.NoError(t, err)
	commit := commitNow(t, a, "190000")
	require.Equal(t, false, commit["unchecked"])

	// An act on a deal that is not a sale's thread.
	plain := openTyped(t, sellerTyped)
	makeOffer(t, plain, "190000")

	dealOf := map[string]string{}
	for _, id := range []string{a, plain} {
		for _, se := range chainSteps(t, id) {
			dealOf[se.CapsuleID] = id
		}
	}

	_, _, input := ruleInputs(t, b, offerInput)
	assert.Equal(t, item, input["item_ref"], "the check's own thread")
	var onA, onPlain []string
	for _, h := range input["history"].([]any) {
		entry := h.(map[string]any)
		body := entry["agent_input"].(map[string]any)["body"].(map[string]any)
		switch dealOf[entry["capsule_id"].(string)] {
		case a:
			assert.Equal(t, item, entry["item_ref"], "an act on the sale's first thread")
			onA = append(onA, body["action"].(string))
		case plain:
			assert.NotContains(t, entry, "item_ref", "an act of a deal that is not a sale's thread")
			onPlain = append(onPlain, body["action"].(string))
		default:
			t.Fatalf("an act of no deal here: %v", entry["capsule_id"])
		}
	}
	assert.ElementsMatch(t, []string{"offer", "commit"}, onA, "the first thread's offer and the commit to its acceptance")
	assert.Equal(t, []string{"offer"}, onPlain)

	// What the checker is given fits external-check-input/v0.
	raw, err := os.ReadFile(filepath.Join(dealProfileDir, "external-check-input-v0.schema.json"))
	require.NoError(t, err)
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	require.NoError(t, err)
	c := jsonschema.NewCompiler()
	require.NoError(t, c.AddResource("input.json", doc))
	compiled, err := c.Compile("input.json")
	require.NoError(t, err)
	given, err := jsonschema.UnmarshalJSON(strings.NewReader(mustJSONString(t, input)))
	require.NoError(t, err)
	assert.NoError(t, compiled.Validate(given))
	// item_ref is a history entry's only: on the checked record it is refused.
	input["record"].(map[string]any)["item_ref"] = item
	misplaced, err := jsonschema.UnmarshalJSON(strings.NewReader(mustJSONString(t, input)))
	require.NoError(t, err)
	assert.Error(t, compiled.Validate(misplaced), "the checked record carries no item_ref")
	delete(input["record"].(map[string]any), "item_ref")

	// No record, in the sale's log or any deal's, carries the reference or
	// an item_ref member: only the salted item_ref_commitment it always had.
	sale, salePath := exportSale(t, saleID)
	checkProfile(t, salePath)
	logs := [][]map[string]any{sale}
	for _, id := range []string{a, b, plain} {
		recs, export := exportRecords(t, id)
		checkProfile(t, export)
		logs = append(logs, recs)
	}
	for _, recs := range logs {
		text := mustJSONString(t, recs)
		assert.NotContains(t, text, item)
		assert.NotContains(t, text, `"item_ref":`)
	}
	checkerPasses(t, a)
}
