package plugin

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/action-state-group/evidencebook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// halvesScenario is one deal two parties each hold on a book: the buyer has
// recorded the seller's copy over the existing exchange, and peer is the
// seller's final close as the buyer's engine takes it.
type halvesScenario struct {
	id            string
	buyer, seller dealParty
	peer          dealPeerHalf
}

// closedHalves opens one deal on both sides and closes each with its own
// input, then has the buyer ask for and record the seller's copy. The
// seller's openings come from the seller's book, standing in for the copy
// that carries them from the follow-up on (note §2.9).
func closedHalves(t *testing.T, buyerClose, sellerClose string) halvesScenario {
	t.Helper()
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, buyerClose))
	buyer := thisDealParty(t)
	dealFixture(t)
	dealRun(t, "open", "--records", "typed", "--deal-id", id, "--input", writeJSON(t, sellerTyped))
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, sellerClose))
	seller := thisDealParty(t)
	peer := dealPeerHalf{DealID: id, RecordID: finalCloseRecord(t, id), Openings: map[string]evidencebook.Opening{}}
	r := shelfRecord(t, id, peer.RecordID)
	withSession(t, func(s *dealSession) {
		for _, f := range dealHalfFields {
			o, err := s.index.(shelfDealIndex).openingOf(t.Context(), r, f)
			require.NoError(t, err)
			peer.Openings[f] = o
		}
	})
	ask := askFor(t, buyer, seller, id, map[string]any{"min_freshness": map[string]any{"size": 1}})
	require.Equal(t, "artifact", ask.outcome)
	withSession(t, func(s *dealSession) {
		outcome, err := s.outcomeOf(t.Context(), ask.recordID)
		require.NoError(t, err)
		peer.ResponseRecord = outcome.digest
	})
	return halvesScenario{id: id, buyer: buyer, seller: seller, peer: peer}
}

func withSession(t *testing.T, fn func(s *dealSession)) {
	t.Helper()
	s, err := openDealSession(t.Context(), dealProfile(t))
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	fn(s)
}

// finalCloseRecord is the record id of the profile's final close of id.
func finalCloseRecord(t *testing.T, id string) string {
	t.Helper()
	var record string
	withSession(t, func(s *dealSession) {
		require.NoError(t, s.useDeal(t.Context(), id, false))
		events, err := s.load(t.Context(), id)
		require.NoError(t, err)
		i := finalClose(events)
		require.GreaterOrEqual(t, i, 0)
		record = events[i].CapsuleID
	})
	return record
}

func shelfRecord(t *testing.T, id, recordID string) evidencebook.ShelfRecord {
	t.Helper()
	for _, r := range bookRecords(t, id) {
		if r.RecordID == recordID {
			return r
		}
	}
	t.Fatalf("no record %s on %s's book", recordID, id)
	return evidencebook.ShelfRecord{}
}

// reconcileAs runs the engine as party, for the deal and peer input given.
func reconcileAs(t *testing.T, party_ dealParty, id string, peer dealPeerHalf) (evidencebook.Tallies, error) {
	t.Helper()
	party(t, party_.config)
	var tallies evidencebook.Tallies
	var err error
	withSession(t, func(s *dealSession) {
		require.NoError(t, s.useDeal(t.Context(), id, false))
		_, tallies, err = s.reconcileDealHalves(t.Context(), id, peer)
	})
	return tallies, err
}

const received = `{"status":"received"}`

// Same terms on both sides: the two halves match, with both openings
// verified on each half.
func TestHalvesClosedOnTheSameTermsMatch(t *testing.T) {
	sc := closedHalves(t, received, received)
	assert.Equal(t, "190000", sc.peer.Openings[dealAmountField].Text)
	tallies, err := reconcileAs(t, sc.buyer, sc.id, sc.peer)
	require.NoError(t, err)
	assert.Equal(t, evidencebook.Tallies{Matched: 1}, tallies)
}

// A different delivered price on one side is a conflict.
func TestHalvesWithADifferentPriceConflict(t *testing.T) {
	sc := closedHalves(t, received, `{"status":"received","delivered":{"price_minor":185000}}`)
	tallies, err := reconcileAs(t, sc.buyer, sc.id, sc.peer)
	require.NoError(t, err)
	assert.Equal(t, evidencebook.Tallies{Conflicting: 1}, tallies)
}

// Only the money terms are compared: a close that differs on the item only
// still matches.
func TestHalvesCompareTheMoneyTermsOnly(t *testing.T) {
	sc := closedHalves(t, received, `{"status":"received","delivered":{"item":"example bicycle, blue"}}`)
	tallies, err := reconcileAs(t, sc.buyer, sc.id, sc.peer)
	require.NoError(t, err)
	assert.Equal(t, evidencebook.Tallies{Matched: 1}, tallies)
}

// A side that did not receive commits empty money terms: the pair is
// insufficient, never two empty texts matching.
func TestAHalfThatWasNotReceivedIsInsufficient(t *testing.T) {
	sc := closedHalves(t, `{"status":"not_received","reason":"not_delivered"}`, `{"status":"not_received","reason":"not_delivered"}`)
	assert.Empty(t, sc.peer.Openings[dealAmountField].Text)
	tallies, err := reconcileAs(t, sc.buyer, sc.id, sc.peer)
	require.NoError(t, err)
	assert.Equal(t, evidencebook.Tallies{Insufficient: 1}, tallies)
}

// A missing opening leaves its field unopened: insufficient. A tampered one
// opens nothing and makes the half untrusted: unresolved.
func TestHalvesWithAMissingOrTamperedOpening(t *testing.T) {
	sc := closedHalves(t, received, received)
	missing := sc.peer
	missing.Openings = map[string]evidencebook.Opening{dealAmountField: sc.peer.Openings[dealAmountField]}
	tallies, err := reconcileAs(t, sc.buyer, sc.id, missing)
	require.NoError(t, err)
	assert.Equal(t, evidencebook.Tallies{Insufficient: 1}, tallies)

	tampered := sc.peer
	o := sc.peer.Openings[dealAmountField]
	o.Text = "1"
	tampered.Openings = map[string]evidencebook.Opening{dealAmountField: o, dealCurrencyField: sc.peer.Openings[dealCurrencyField]}
	tallies, err = reconcileAs(t, sc.buyer, sc.id, tampered)
	require.NoError(t, err)
	assert.Equal(t, evidencebook.Tallies{Unresolved: 1}, tallies)
}

// A record of the copy that is not the final close commits empty money
// terms: insufficient.
func TestARecordThatIsNotTheCloseIsInsufficient(t *testing.T) {
	sc := closedHalves(t, received, received)
	party(t, sc.seller.config)
	first := bookRecords(t, sc.id)[0]
	notClose := sc.peer
	notClose.RecordID = first.RecordID
	notClose.Openings = map[string]evidencebook.Opening{}
	withSession(t, func(s *dealSession) {
		for _, f := range dealHalfFields {
			o, err := s.index.(shelfDealIndex).openingOf(t.Context(), first, f)
			require.NoError(t, err)
			notClose.Openings[f] = o
		}
	})
	tallies, err := reconcileAs(t, sc.buyer, sc.id, notClose)
	require.NoError(t, err)
	assert.Equal(t, evidencebook.Tallies{Insufficient: 1}, tallies)
}

// The engine refuses an input that is not the counterparty's half: another
// deal, a record of this profile's own book, a response that was not
// recorded, and a legacy deal.
func TestTheEngineRefusesWhatIsNotTheCounterpartysHalf(t *testing.T) {
	sc := closedHalves(t, received, received)
	wrongDeal := sc.peer
	wrongDeal.DealID = "deal-0000000000000000"
	_, err := reconcileAs(t, sc.buyer, sc.id, wrongDeal)
	require.ErrorIs(t, err, cli.ErrInput)

	party(t, sc.buyer.config)
	own := sc.peer
	own.RecordID = finalCloseRecord(t, sc.id)
	_, err = reconcileAs(t, sc.buyer, sc.id, own)
	require.ErrorIs(t, err, cli.ErrInput)

	unrecorded := sc.peer
	unrecorded.ResponseRecord = "0000000000000000000000000000000000000000000000000000000000000000"
	_, err = reconcileAs(t, sc.buyer, sc.id, unrecorded)
	require.ErrorIs(t, err, cli.ErrInput)

	legacyDeals(t)
	dealFixture(t)
	legacy := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	withSession(t, func(s *dealSession) {
		require.NoError(t, s.useDeal(t.Context(), legacy, false))
		_, _, err := s.reconcileDealHalves(t.Context(), legacy, dealPeerHalf{DealID: legacy})
		require.ErrorIs(t, err, cli.ErrInput)
	})
}

// A capsule signed under a key other than the pinned responder key is not
// the counterparty's: untrusted.
func TestACapsuleUnderAnotherKeyIsNotTrusted(t *testing.T) {
	assert.False(t, signedBy(map[string]interface{}{"signature": "00", "key_id": "aa"}, "id", "bb"))
}

// The cross-check reads only what a copy discloses: a disclosed close whose
// outcome record states a delivered price other than the opened amount makes
// the half untrusted; a withheld one is no finding.
func TestTheCrossCheckReadsOnlyWhatACopyDiscloses(t *testing.T) {
	outcome := map[string]interface{}{"x-deal-v0": map[string]interface{}{"record_type": "outcome"}, "body": map[string]interface{}{"status": "received", "delivered": map[string]interface{}{"price_minor": json.Number("185000")}}}
	digest := jsonDigest(t, outcome)
	closeRecord := map[string]interface{}{"x-deal-v0": map[string]interface{}{"record_type": "close", "refs": []interface{}{map[string]interface{}{"rel": "outcome", "digest": digest}}}, "body": map[string]interface{}{"outcome": "completed"}}
	half := evidencebook.Half{Opened: map[string]string{dealAmountField: "190000", dealCurrencyField: "USD"}}
	both := map[string]interface{}{"disclosures": map[string]interface{}{"c": map[string]interface{}{"agent_input": closeRecord}, "o": map[string]interface{}{"agent_input": outcome}}}
	assert.False(t, disclosedCloseAgrees(both, "c", half))
	onlyClose := map[string]interface{}{"disclosures": map[string]interface{}{"c": map[string]interface{}{"agent_input": closeRecord}}}
	assert.True(t, disclosedCloseAgrees(onlyClose, "c", half), "a withheld outcome record is no finding")
	assert.True(t, disclosedCloseAgrees(map[string]interface{}{}, "c", half), "a withheld close is no finding")
	notClose := map[string]interface{}{"disclosures": map[string]interface{}{"c": map[string]interface{}{"agent_input": outcome}}}
	assert.False(t, disclosedCloseAgrees(notClose, "c", half))
}

func jsonDigest(t *testing.T, v any) string {
	t.Helper()
	d, err := canonical.JSONDigest(v)
	require.NoError(t, err)
	return d
}

// A real copy's capsule is trusted only under the responder key the request
// pinned: the seller's key verifies it, another key does not.
func TestARealCopysCapsuleIsTrustedUnderThePinnedKeyOnly(t *testing.T) {
	sc := closedHalves(t, received, received)
	party(t, sc.buyer.config)
	var capsule map[string]interface{}
	withSession(t, func(s *dealSession) {
		rows, err := s.exchangeRows(t.Context(), `kind=? AND record_digest=?`, dealExchangeResponse, sc.peer.ResponseRecord)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		raw, held, err := s.keptReply(t.Context(), &rows[0])
		require.NoError(t, err)
		require.True(t, held)
		var resp dealResponse
		require.NoError(t, json.Unmarshal(raw, &resp))
		bundle, err := cli.DecodeBundleJSON(resp.Artifact.Artifact)
		require.NoError(t, err)
		capsule = copyRecord(bundle, sc.peer.RecordID)
	})
	require.NotNil(t, capsule)
	assert.True(t, signedBy(capsule, sc.peer.RecordID, sc.seller.signer))
	assert.False(t, signedBy(capsule, sc.peer.RecordID, sc.buyer.signer), "another key: untrusted")
}

// Coverage is judged before trust: a tampered opening against an own close
// no checkpoint covers yet (a crash between the seal and its milestone) is
// insufficient, not unresolved.
func TestATamperedOpeningOnAnUncoveredCloseIsInsufficient(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	old := dealMilestoneStops
	t.Cleanup(func() { dealMilestoneStops = old })
	dealMilestoneStops = func() error { return assert.AnError }
	_, err := invoke(t, "", "--profile", "deal", "deal", "close", "--deal", id, "--input", writeJSON(t, received))
	require.ErrorIs(t, err, assert.AnError, "sealed, not checkpointed")
	dealMilestoneStops = old
	buyer := thisDealParty(t)
	dealFixture(t)
	dealRun(t, "open", "--records", "typed", "--deal-id", id, "--input", writeJSON(t, sellerTyped))
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, received))
	seller := thisDealParty(t)
	peer := dealPeerHalf{DealID: id, RecordID: finalCloseRecord(t, id), Openings: map[string]evidencebook.Opening{}}
	r := shelfRecord(t, id, peer.RecordID)
	withSession(t, func(s *dealSession) {
		for _, f := range dealHalfFields {
			o, err := s.index.(shelfDealIndex).openingOf(t.Context(), r, f)
			require.NoError(t, err)
			peer.Openings[f] = o
		}
	})
	o := peer.Openings[dealAmountField]
	o.Text = "1"
	peer.Openings[dealAmountField] = o
	ask := askFor(t, buyer, seller, id, map[string]any{"min_freshness": map[string]any{"size": 1}})
	require.Equal(t, "artifact", ask.outcome)
	withSession(t, func(s *dealSession) {
		outcome, err := s.outcomeOf(t.Context(), ask.recordID)
		require.NoError(t, err)
		peer.ResponseRecord = outcome.digest
	})
	tallies, err := reconcileAs(t, buyer, id, peer)
	require.NoError(t, err)
	assert.Equal(t, evidencebook.Tallies{Insufficient: 1}, tallies)
}

// A copy recorded as failing verification is never a half: the engine
// refuses it.
func TestACopyThatFailedVerificationIsRefused(t *testing.T) {
	sc := closedHalves(t, received, received)
	wrong := sc.seller
	wrong.checkpoint = sc.buyer.checkpoint
	ask := askFor(t, sc.buyer, wrong, sc.id, map[string]any{"min_freshness": map[string]any{"size": 1}})
	require.Equal(t, dealOutcomeArtifactFailed, ask.outcome)
	failed := sc.peer
	withSession(t, func(s *dealSession) {
		outcome, err := s.outcomeOf(t.Context(), ask.recordID)
		require.NoError(t, err)
		failed.ResponseRecord = outcome.digest
	})
	_, err := reconcileAs(t, sc.buyer, sc.id, failed)
	require.ErrorIs(t, err, cli.ErrInput)
}

// The cross-check runs only for a half that asserts a value: a half with
// empty openings stays trusted (and insufficient) even when its disclosed
// outcome was not received. And a close whose outcome ref names an
// unchecked act, not an outcome record, is checked on its outcome only.
func TestTheCrossCheckSkipsHalvesThatAssertNothingAndActs(t *testing.T) {
	notReceived := map[string]interface{}{"x-deal-v0": map[string]interface{}{"record_type": "outcome"}, "body": map[string]interface{}{"status": "not_received"}}
	closeOf := func(ref map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{"x-deal-v0": map[string]interface{}{"record_type": "close", "refs": []interface{}{map[string]interface{}{"rel": "outcome", "digest": jsonDigest(t, ref)}}}, "body": map[string]interface{}{"outcome": "mismatch"}}
	}
	bundle := map[string]interface{}{"disclosures": map[string]interface{}{"c": map[string]interface{}{"agent_input": closeOf(notReceived)}, "o": map[string]interface{}{"agent_input": notReceived}}}
	empty := evidencebook.Half{Trusted: true, Opened: map[string]string{dealAmountField: "", dealCurrencyField: ""}}
	assert.True(t, crossChecked(empty, bundle, "c"), "nothing asserted, nothing checked")
	asserting := evidencebook.Half{Trusted: true, Opened: map[string]string{dealAmountField: "190000", dealCurrencyField: "USD"}}
	assert.False(t, crossChecked(asserting, bundle, "c"), "an asserted value against a disclosed not-received outcome")

	act := map[string]interface{}{"x-deal-v0": map[string]interface{}{"record_type": "action"}, "body": map[string]interface{}{"action": "pay"}}
	actBundle := map[string]interface{}{"disclosures": map[string]interface{}{"c": map[string]interface{}{"agent_input": closeOf(act)}, "a": map[string]interface{}{"agent_input": act}}}
	assert.True(t, crossChecked(asserting, actBundle, "c"), "an unchecked act has no status to check")
}

// A verified copy whose selected close was signed under a key other than the
// responder key pinned for the response (the seller rotated its key, keeping
// the old one trusted, and answered under the new one) is untrusted: the
// engine finds the pair unresolved.
func TestACloseSignedUnderAnotherKeyIsUnresolved(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, received))
	buyer := thisDealParty(t)
	dealFixture(t)
	dealRun(t, "open", "--records", "typed", "--deal-id", id, "--input", writeJSON(t, sellerTyped))
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, received))
	peer := dealPeerHalf{DealID: id, RecordID: finalCloseRecord(t, id), Openings: map[string]evidencebook.Opening{}}
	r := shelfRecord(t, id, peer.RecordID)
	withSession(t, func(s *dealSession) {
		for _, f := range dealHalfFields {
			o, err := s.index.(shelfDealIndex).openingOf(t.Context(), r, f)
			require.NoError(t, err)
			peer.Openings[f] = o
		}
	})
	// The seller rotates its signing key, keeping the old one trusted.
	p := dealProfile(t)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	seed := filepath.Join(t.TempDir(), "rotated.seed")
	require.NoError(t, os.WriteFile(seed, []byte(hex.EncodeToString(private.Seed())), 0o600))
	p.Signing = cli.Secret{File: seed}
	p.TrustedKeys = append(p.TrustedKeys, hex.EncodeToString(public))
	require.NoError(t, cli.SaveProfile(p, true))
	seller := thisDealParty(t)
	ask := askFor(t, buyer, seller, id, map[string]any{"min_freshness": map[string]any{"size": 1}})
	require.Equal(t, "artifact", ask.outcome, "a valid copy, answered under the new key")
	withSession(t, func(s *dealSession) {
		outcome, err := s.outcomeOf(t.Context(), ask.recordID)
		require.NoError(t, err)
		peer.ResponseRecord = outcome.digest
	})
	tallies, err := reconcileAs(t, buyer, id, peer)
	require.NoError(t, err)
	assert.Equal(t, evidencebook.Tallies{Unresolved: 1}, tallies)
}
