package plugin

import (
	"testing"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/action-state-group/evidencebook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sealStepsAs has this process seal deal steps as a later build will
// (capsule-cli#264): the History profile's record kind, and the deal id, or
// exchangeID when given, as the step's correlation identifier. It returns a
// function that restores this build's writer.
func sealStepsAs(t *testing.T, exchangeID string) func() {
	t.Helper()
	before := dealStepHeader
	dealStepHeader = func(dealID string, _ dealEvent) (string, evidencebook.Correlation) {
		id := dealID
		if exchangeID != "" {
			id = exchangeID
		}
		return dealStepCompleted, evidencebook.Correlation{ExchangeID: id}
	}
	restore := func() { dealStepHeader = before }
	t.Cleanup(restore)
	return restore
}

// mixedPair opens and closes one deal with the buyer on this build's writer
// and the seller on the later writer, then has each record the other's
// copy and disclose its own, with the buyer selected.
func mixedPair(t *testing.T, sellerExchangeID string) (pair reconcilePair, buyerFile, sellerResponse string) {
	t.Helper()
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, received))
	buyer := thisDealParty(t)
	restore := sealStepsAs(t, sellerExchangeID)
	dealFixture(t)
	dealRun(t, "open", "--records", "typed", "--deal-id", id, "--input", writeJSON(t, sellerTyped))
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, received))
	restore()
	seller := thisDealParty(t)
	pair = reconcilePair{id: id, buyer: buyer, seller: seller}
	pair.response = recordedCopy(t, buyer, seller, id)
	sellerResponse = recordedCopy(t, seller, buyer, id)
	party(t, seller.config)
	pair.sellerFile = discloseTo(t, id, "the buyer")
	party(t, buyer.config)
	buyerFile = discloseTo(t, id, "the seller")
	return pair, buyerFile, sellerResponse
}

// A counterparty on this build admits the half of one that seals steps
// with the History profile's record kinds and the deal as their
// correlation identifier, and that party's own reads and its own half work
// on those steps.
func TestHalvesAdmitTheHistoryProfilesStepHeaders(t *testing.T) {
	pair, buyerFile, sellerResponse := mixedPair(t, "")

	// The seller's steps carry the later headers.
	party(t, pair.seller.config)
	withSession(t, func(s *dealSession) {
		records, err := s.index.(shelfDealIndex).records(t.Context(), pair.id)
		require.NoError(t, err)
		steps := 0
		for _, r := range records {
			if r.Header.RecordType == dealReportRecordType {
				continue
			}
			steps++
			assert.Equal(t, dealStepCompleted, r.Header.RecordType)
			require.NotNil(t, r.Header.Correlation)
			assert.Equal(t, pair.id, r.Header.Correlation.ExchangeID)
		}
		require.NotZero(t, steps)
	})

	// The buyer, on this build's writer, admits the seller's half.
	party(t, pair.buyer.config)
	_, result, _, err := runTermsResult(t, pair.id, pair.response, "--openings", pair.sellerFile)
	require.NoError(t, err)
	for _, req := range []string{amountReq, currencyReq} {
		c := claimsOf(result)[req]
		assert.Equal(t, "met", c.Verdict, req)
		assert.Len(t, c.Evidence, 2, "both halves admitted")
	}

	// The seller reads its own steps of the later kind: its own half is
	// found, and it admits the buyer's half of this build's kind.
	party(t, pair.seller.config)
	_, result, _, err = runTermsResult(t, pair.id, sellerResponse, "--openings", buyerFile)
	require.NoError(t, err)
	for _, req := range []string{amountReq, currencyReq} {
		assert.Equal(t, "met", claimsOf(result)[req].Verdict, req)
	}
}

// A step whose correlation identifier names another deal is not a step of
// this deal, whatever its book. The party holding such steps finds no half
// of its own, so its counterparty file refuses (stored state conflicts);
// the counterparty's admission check 2 uses the same rule (stepOfDeal,
// TestStepHeaderPredicates).
func TestAStepCorrelatedToAnotherDealIsNoStepOfThisDeal(t *testing.T) {
	sealStepsAs(t, "deal-feedfacecafebeef")
	dealFixture(t)
	id := dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, sellerTyped))["deal_id"].(string)
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, received))
	_, err := invoke(t, "", "--profile", "deal", "disclose", "--deal", id, "--share", "counterparty", "--to", "the buyer", "--out", t.TempDir()+"/copy.json")
	require.ErrorIs(t, err, cli.ErrConflict)
}

func TestStepHeaderPredicates(t *testing.T) {
	for kind, step := range map[string]bool{
		dealStepRecordType: true, dealStepCompleted: true, dealStepFailed: true,
		dealReportRecordType: false, "evidence_result": false, "": false,
	} {
		assert.Equal(t, step, isDealStep(kind), kind)
	}
	book := cli.DealLogID("deal-a")
	for name, c := range map[string]struct {
		h    evidencebook.Header
		want bool
	}{
		"this build's step":           {evidencebook.Header{RecordType: dealStepRecordType, BookID: book}, true},
		"a later step of this deal":   {evidencebook.Header{RecordType: dealStepFailed, BookID: book, Correlation: &evidencebook.Correlation{ExchangeID: "deal-a"}}, true},
		"a correlation without an id": {evidencebook.Header{RecordType: dealStepCompleted, BookID: book, Correlation: &evidencebook.Correlation{RequestDigest: "r"}}, true},
		"another deal's correlation":  {evidencebook.Header{RecordType: dealStepCompleted, BookID: book, Correlation: &evidencebook.Correlation{ExchangeID: "deal-b"}}, false},
		"another book":                {evidencebook.Header{RecordType: dealStepRecordType, BookID: cli.DealLogID("deal-b")}, false},
		"a report":                    {evidencebook.Header{RecordType: dealReportRecordType, BookID: book}, false},
	} {
		assert.Equal(t, c.want, stepOfDeal(c.h, "deal-a"), name)
	}
}
