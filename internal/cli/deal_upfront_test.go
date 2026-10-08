package cli

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sealedCheckBody is the body of the deal's sealed check record (the
// snapshot of what is about to happen), x-deal-v0 or typed.
func sealedCheckBody(t *testing.T, id string) map[string]any {
	t.Helper()
	records, export := exportRecords(t, id)
	checkProfile(t, export)
	for _, r := range records {
		if typeOf(r) == typeProposedAction {
			return r["body"].(map[string]any)
		}
		if b, ok := r["x-deal-v0"].(map[string]any); ok && b["record_type"] == "check" {
			return r["body"].(map[string]any)
		}
	}
	t.Fatal("no check record")
	return nil
}

// A check whose terms state a deposit seals it again as
// upfront_amount_minor, a scalar a rules checker reads; the checker's input
// carries it, and the recourse the check states, as they were sealed. A
// check with no deposit has no upfront_amount_minor: never inferred.
func TestACheckSealsTheDepositItStatesAsTheUpfrontAmount(t *testing.T) {
	deposit := `{"action":"pay","amount_minor":454,"authorized_max_minor":454,"terms":{"item":"otter sticker","price_minor":454,"deposit_minor":200},"recourse":{"rail":"card","refundable":false}}`
	for name, typed := range map[string]bool{"x-deal-v0": false, "typed": true} {
		t.Run(name, func(t *testing.T) {
			id := stickerDeal(t, "card", typed)
			checker := stubChecker(t, "rules", checkerPrints("allow", passFinding))
			pinChecker(t, map[string]any{"command": []string{checker}})
			dealRun(t, "check", "--deal", id, "--input", writeJSON(t, deposit))
			body := sealedCheckBody(t, id)
			assert.Equal(t, float64(200), body["upfront_amount_minor"])
			assert.Equal(t, map[string]any{"rail": "card", "refundable": false}, body["recourse"])

			raw, err := os.ReadFile(checker + ".input")
			require.NoError(t, err)
			var input map[string]any
			require.NoError(t, json.Unmarshal(raw, &input))
			given := input["record"].(map[string]any)["agent_input"].(map[string]any)["body"].(map[string]any)
			assert.Equal(t, float64(200), given["upfront_amount_minor"])
			assert.Equal(t, map[string]any{"rail": "card", "refundable": false}, given["recourse"])
		})
	}
	t.Run("no deposit", func(t *testing.T) {
		id := stickerDeal(t, "card", false)
		dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 454)))
		_, has := sealedCheckBody(t, id)["upfront_amount_minor"]
		assert.False(t, has, "absent when no deposit is stated")
	})
}
