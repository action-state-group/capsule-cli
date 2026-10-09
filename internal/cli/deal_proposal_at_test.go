package cli

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sellerCommit = `{"action":"commit","amount_minor":190000,"terms":{"item":"example bicycle","quantity":1,"price_minor":190000}}`

// laterClock moves the deal clock on, so a later step's sealed at differs
// from an earlier one's.
func laterClock(t *testing.T, by time.Duration) {
	t.Helper()
	now := dealClock().Add(by)
	old := dealClock
	dealClock = func() time.Time { return now }
	t.Cleanup(func() { dealClock = old })
}

// offerRecordAt is the sealed at of the deal's latest offer record.
func offerRecordAt(t *testing.T, id string) string {
	t.Helper()
	records, _ := exportRecords(t, id)
	at := ""
	for _, r := range records {
		if b, ok := r["body"].(map[string]any); ok && typeOf(r) == typeProposedAction && b["action"] == offerAction {
			at = r["at"].(string)
		}
	}
	require.NotEmpty(t, at, "the deal has an offer record")
	return at
}

// A seller's commit check tells the rules checker when the offer it rests on
// was made: the accepted offer's sealed at, byte for byte, on the checked
// record (never the top level).
func TestASellersCommitCheckCarriesTheAcceptedOffersAt(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	check := makeOffer(t, id, "190000")
	offered := offerRecordAt(t, id)
	laterClock(t, 3*time.Hour)
	_, err := acceptOffer(t, id, check)
	require.NoError(t, err)
	_, _, input := ruleInputs(t, id, sellerCommit)
	record := input["record"].(map[string]any)
	assert.Equal(t, offered, record["proposal_at"])
	assert.NotEqual(t, dealClock().UTC().Format(time.RFC3339), record["proposal_at"], "the offer's time, not the commit's")
	assert.NotContains(t, input, "proposal_at", "on the checked record, not the top level")
}

// After a change of details the acceptance no longer holds: the commit's check
// still runs, with no proposal_at, and the commit is sealed unauthorized.
func TestNoProposalAtAfterAChangeOfDetails(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	_, err := acceptOffer(t, id, makeOffer(t, id, "190000"))
	require.NoError(t, err)
	dealRun(t, "note", "--deal", id, "--kind", "change", "--input", writeJSON(t,
		`{"source":"buyer message","who":{"name":"Example Buyer","domain":"buyer-two.example"}}`))
	_, _, input := ruleInputs(t, id, sellerCommit)
	assert.NotContains(t, input["record"], "proposal_at")
	act := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"commit","amount_minor":190000}`))
	assert.Equal(t, "changed_after_acceptance", act["rule"])
}

// A later offer supersedes the accepted one: until it is accepted, there is
// no accepted offer for the commit to rest on.
func TestNoProposalAtWhenALaterOfferIsNotAccepted(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	_, err := acceptOffer(t, id, makeOffer(t, id, "190000"))
	require.NoError(t, err)
	makeOffer(t, id, "195000")
	_, _, input := ruleInputs(t, id, sellerCommit)
	assert.NotContains(t, input["record"], "proposal_at")
}

// Only a seller's commit carries it: not an offer's own check, and nothing on
// a deal where the user buys.
func TestNoProposalAtOffACommitOrOnABuyersDeal(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	_, _, input := ruleInputs(t, id, `{"action":"offer","amount_minor":190000,"terms":{"item":"example bicycle","quantity":1,"price_minor":190000}}`)
	assert.NotContains(t, input["record"], "proposal_at")

	buyer := stickerDeal(t, "card", true)
	_, _, input = ruleInputs(t, buyer, stickerPay)
	assert.NotContains(t, input["record"], "proposal_at")
}
