package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A buyer the seller did not choose: their thread closes not_selected, with
// nothing taken or delivered, and the close is final.
func TestADeclinedThreadClosesNotSelected(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, sellerOpen))["deal_id"].(string)
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, `{"status":"not_selected"}`))
	assert.Contains(t, dealOwnBundle(t, id), `"outcome":"not_selected"`)

	_, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", id, "--kind", "message",
		"--input", writeJSON(t, `{"from": "counterparty", "text": "Is it still available?"}`))
	require.ErrorIs(t, err, ErrInput, "a not_selected close is final")
}

func TestNotSelectedNeedsNothingTakenOrDelivered(t *testing.T) {
	dealFixture(t)
	id := openJetSki(t)
	dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":20000}`))
	out, err := invoke(t, "", "--profile", "deal", "deal", "close", "--deal", id, "--input", writeJSON(t, `{"status":"not_selected"}`))
	require.ErrorIs(t, err, ErrInput, out)
	assert.Contains(t, err.Error(), "action on record")
}
