package plugin

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/action-state-group/evidencebook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reconcilePair is one deal two parties each closed on a book. The buyer has
// recorded the seller's copy over the evidence-request exchange, and holds
// the disclose file the seller handed it.
type reconcilePair struct {
	id            string
	buyer, seller dealParty
	// response is the buyer's record of the seller's copy.
	response string
	// sellerFile is the seller's `disclose --share counterparty` file.
	sellerFile string
}

const received = `{"status":"received"}`

// closedPair opens one deal on both sides, closes each with its own input,
// has the buyer ask for and record the seller's copy, and has the seller
// disclose its copy to the buyer. before runs as the seller after its close,
// before the copy is asked for. It returns with the buyer selected.
func closedPair(t *testing.T, buyerClose, sellerClose string, before ...func(id string)) reconcilePair {
	t.Helper()
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, buyerClose))
	buyer := thisDealParty(t)
	dealFixture(t)
	dealRun(t, "open", "--records", "typed", "--deal-id", id, "--input", writeJSON(t, sellerTyped))
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, sellerClose))
	for _, fn := range before {
		fn(id)
	}
	seller := thisDealParty(t)
	pair := reconcilePair{id: id, buyer: buyer, seller: seller}
	pair.response = recordedCopy(t, buyer, seller, id)
	party(t, seller.config)
	pair.sellerFile = discloseTo(t, id, "the buyer")
	party(t, buyer.config)
	return pair
}

// recordedCopy has asker ask for and record responder's copy of the deal,
// and returns the response record, with asker selected.
func recordedCopy(t *testing.T, asker, responder dealParty, id string) string {
	t.Helper()
	ask := askFor(t, asker, responder, id, map[string]any{"min_freshness": map[string]any{"size": 1}})
	require.Equal(t, "artifact", ask.outcome)
	var response string
	withSession(t, func(s *dealSession) {
		outcome, err := s.outcomeOf(t.Context(), ask.recordID)
		require.NoError(t, err)
		response = outcome.digest
	})
	return response
}

// discloseTo writes the selected party's `disclose --share counterparty`
// file for the deal.
func discloseTo(t *testing.T, id, to string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "disclosed.json")
	out, err := invoke(t, "", "--profile", "deal", "disclose", "--deal", id, "--share", "counterparty", "--to", to, "--out", path)
	require.NoError(t, err, out)
	return path
}

func withSession(t *testing.T, fn func(s *dealSession)) {
	t.Helper()
	s, err := openDealSession(t.Context(), dealProfile(t))
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	fn(s)
}

// reconcileResult runs the verb as the selected party, writing the Result.
func runReconcileResult(t *testing.T, id, response string, extra ...string) (map[string]any, evidencebook.DealReconcileResult, string, error) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "result.json")
	args := append([]string{"--profile", "deal", "deal", "reconcile-result", "--deal", id, "--response", response, "--out", out}, extra...)
	text, err := invoke(t, "", args...)
	if err != nil {
		return nil, evidencebook.DealReconcileResult{}, text, err
	}
	var summary map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &summary), text)
	var result evidencebook.DealReconcileResult
	require.NoError(t, json.Unmarshal(mustRead(t, out), &result))
	return summary, result, out, nil
}

// claimsOf are a Result's claims by requirement.
func claimsOf(result evidencebook.DealReconcileResult) map[string]evidencebook.ReconcileClaim {
	out := map[string]evidencebook.ReconcileClaim{}
	for _, c := range result.Claims {
		out[c.RequirementRef] = c
	}
	return out
}

const (
	amountReq   = "amount-commitment-matched"
	currencyReq = "currency-commitment-matched"
)

// #36's acceptance, end to end: two real parties close as received,
// exchange copies, and the buyer's Result has both claims met, each with
// tallies matched 1, citing no Close. No deal state is shown anywhere.
func TestReconcileResultOfTwoPartiesIsMet(t *testing.T) {
	pair := closedPair(t, received, received)
	summary, result, out, err := runReconcileResult(t, pair.id, pair.response, "--openings", pair.sellerFile)
	require.NoError(t, err)
	claims := claimsOf(result)
	for _, req := range []string{amountReq, currencyReq} {
		c := claims[req]
		assert.Equal(t, "met", c.Verdict, req)
		assert.Equal(t, evidencebook.ResultTallies{Matched: 1}, c.Reconcile.Tallies, req)
		assert.Equal(t, evidencebook.DealReconcileContractRef, c.ContractRef)
		assert.Equal(t, pair.seller.signer, c.Reconcile.Peer, "the peer is the seller's signing key")
		assert.Len(t, c.Evidence, 2, "the two half digests, nothing else")
	}
	written := string(mustRead(t, out))
	encodedSummary, err := json.Marshal(summary)
	require.NoError(t, err)
	help, err := invoke(t, "", "--profile", "deal", "deal", "reconcile-result", "--help")
	require.NoError(t, err)
	for _, text := range []string{written, string(encodedSummary), help} {
		assert.NotContains(t, text, "reconciled")
		assert.NotContains(t, text, "deal_state")
		assert.NotContains(t, text, "close_ref")
	}
	assert.Equal(t, pair.id, summary["deal_id"])
}

func TestReconcileResultWithADifferentPriceConflicts(t *testing.T) {
	pair := closedPair(t, received, `{"status":"received","delivered":{"price_minor":185000}}`)
	_, result, _, err := runReconcileResult(t, pair.id, pair.response, "--openings", pair.sellerFile)
	require.NoError(t, err)
	claims := claimsOf(result)
	assert.Equal(t, evidencebook.ResultTallies{Conflicting: 1}, claims[amountReq].Reconcile.Tallies)
	assert.Equal(t, "not_met", claims[amountReq].Verdict)
	assert.Equal(t, "met", claims[currencyReq].Verdict)
}

// A side that did not receive commits empty money terms: not evaluable,
// never two empty texts matching. A seller that did not receive discloses
// no openings at all.
func TestReconcileResultWhenOneSideDidNotReceive(t *testing.T) {
	notReceived := `{"status":"not_received","reason":"not_delivered"}`
	for name, closes := range map[string][2]string{"buyer": {notReceived, received}, "seller": {received, notReceived}} {
		t.Run(name, func(t *testing.T) {
			pair := closedPair(t, closes[0], closes[1])
			_, result, _, err := runReconcileResult(t, pair.id, pair.response, "--openings", pair.sellerFile)
			require.NoError(t, err)
			for _, c := range result.Claims {
				assert.Equal(t, evidencebook.ResultTallies{Insufficient: 1}, c.Reconcile.Tallies, c.RequirementRef)
				assert.Equal(t, "not_evaluable", c.Verdict)
			}
		})
	}
}

// editOpenings rewrites a disclose file's field_openings.
func editOpenings(t *testing.T, path string, edit func(map[string]map[string]evidencebook.Opening)) string {
	t.Helper()
	openings, err := dealOpeningsFile(mustRead(t, path))
	require.NoError(t, err)
	edit(openings)
	return writeJSONFile(t, "openings.json", map[string]any{"field_openings": openings})
}

// A tampered amount opening fails only the amount: UNRESOLVED, with the
// currency still met. A missing one is INSUFFICIENT.
func TestReconcileResultWithATamperedOrMissingOpening(t *testing.T) {
	pair := closedPair(t, received, received)
	tampered := editOpenings(t, pair.sellerFile, func(m map[string]map[string]evidencebook.Opening) {
		for d, fields := range m {
			o := fields[dealAmountField]
			o.Text = "1"
			m[d][dealAmountField] = o
		}
	})
	_, result, _, err := runReconcileResult(t, pair.id, pair.response, "--openings", tampered)
	require.NoError(t, err)
	claims := claimsOf(result)
	assert.Equal(t, evidencebook.ResultTallies{Unresolved: 1}, claims[amountReq].Reconcile.Tallies)
	assert.Equal(t, "met", claims[currencyReq].Verdict)

	missing := editOpenings(t, pair.sellerFile, func(m map[string]map[string]evidencebook.Opening) {
		for d := range m {
			delete(m[d], dealAmountField)
		}
	})
	_, result, _, err = runReconcileResult(t, pair.id, pair.response, "--openings", missing)
	require.NoError(t, err)
	claims = claimsOf(result)
	assert.Equal(t, evidencebook.ResultTallies{Insufficient: 1}, claims[amountReq].Reconcile.Tallies)
	assert.Equal(t, "met", claims[currencyReq].Verdict)
}

// Without the seller's openings there is no peer half: the buyer's lone
// half is INSUFFICIENT, never "no close".
func TestReconcileResultWithoutOpenings(t *testing.T) {
	pair := closedPair(t, received, received)
	for _, extra := range [][]string{nil, {"--openings", writeJSONFile(t, "empty.json", map[string]any{"records": []any{}})}} {
		_, result, _, err := runReconcileResult(t, pair.id, pair.response, extra...)
		require.NoError(t, err)
		for _, c := range result.Claims {
			assert.Equal(t, evidencebook.ResultTallies{Insufficient: 1}, c.Reconcile.Tallies)
			assert.Len(t, c.Evidence, 1)
		}
	}
}

// The seller's half digest is the same on both sides: the key of its own
// disclose file, the half of its own close, and the record of the copy the
// buyer recorded.
func TestBothPartiesComputeOneHalfDigest(t *testing.T) {
	pair := closedPair(t, received, received)
	openings, err := dealOpeningsFile(mustRead(t, pair.sellerFile))
	require.NoError(t, err)
	digest, err := soleDigest(openings)
	require.NoError(t, err)
	require.NotEmpty(t, digest)
	party(t, pair.seller.config)
	withSession(t, func(s *dealSession) {
		require.NoError(t, s.useDeal(t.Context(), pair.id, false))
		own, ownOpenings, err := s.ownDealHalf(t.Context(), pair.id)
		require.NoError(t, err)
		require.NotNil(t, own)
		assert.Equal(t, digest, own.digest)
		assert.Equal(t, ownOpenings, openings[digest], "the file carries the store's openings")
		// The half mapping resolves on the real record.
		var doc map[string]any
		require.NoError(t, json.Unmarshal(own.octets, &doc))
		assert.Equal(t, pair.id, doc["deal_id"])
		assert.Equal(t, pair.seller.signer, doc["key_id"])
	})
	party(t, pair.buyer.config)
	withSession(t, func(s *dealSession) {
		peer, err := s.peerCopy(t.Context(), pair.id, pair.response)
		require.NoError(t, err)
		half, ok := copyHalves(peer.bundle)[digest]
		require.True(t, ok, "the recorded copy holds the same record, with the same digest")
		assert.Equal(t, pair.seller.signer, half.book)
	})
}

// Openings travel only in the user's own counterparty share, for the final
// close only: never in a respond copy, an adjudicator's copy or the user's
// own bundle. The share's recorded digest is the file's.
func TestOpeningsTravelOnlyInACounterpartyDisclose(t *testing.T) {
	pair := closedPair(t, received, received)
	party(t, pair.seller.config)
	var b map[string]any
	require.NoError(t, json.Unmarshal(mustRead(t, pair.sellerFile), &b))
	require.Contains(t, b, "field_openings")
	assert.Len(t, b["field_openings"], 1, "the final close only")
	withSession(t, func(s *dealSession) {
		recorded, err := s.recordedCounterpartyShare(t.Context(), pair.id, digestOfFile(t, pair.sellerFile))
		require.NoError(t, err)
		assert.True(t, recorded, "receipt_sha256 is the file's SHA-256")
	})
	adjudicator, _ := sharedCopy(t, pair.id, "adjudicator", "the platform")
	assert.NotContains(t, adjudicator, "field_openings")
	own := filepath.Join(t.TempDir(), "own.json")
	_, err := invoke(t, "", "--profile", "deal", "bundle", "--deal", pair.id, "--out", own)
	require.NoError(t, err)
	assert.NotContains(t, string(mustRead(t, own)), "field_openings")

	ask := askFor(t, pair.buyer, pair.seller, pair.id, map[string]any{"min_freshness": map[string]any{"size": 1}})
	require.Equal(t, "artifact", ask.outcome)
	assert.NotContains(t, string(mustRead(t, ask.responsePath)), "field_openings", "a respond copy goes to an unchecked requester")

	// The core verifier accepts the copy that carries them.
	result, err := verifyWithDirectory(t, pair.sellerFile, "")
	require.NoError(t, err)
	assert.Equal(t, "VALID", result["verdict"])
}

// A deal that is still open discloses no openings.
func TestAnOpenDealDisclosesNoOpenings(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	assert.NotContains(t, string(mustRead(t, discloseTo(t, id, "the seller"))), "field_openings")
}

// The final close is admitted when later evidence confirms it and reports
// follow it in the copy.
func TestTheFinalCloseIsAdmittedPastLaterEvidenceAndReports(t *testing.T) {
	pair := closedPair(t, received, received, func(id string) {
		dealRun(t, "note", "--deal", id, "--kind", "evidence", "--input", writeJSON(t, `{"about": "the parcel arrived", "source": "agent"}`))
	})
	_, result, _, err := runReconcileResult(t, pair.id, pair.response, "--openings", pair.sellerFile)
	require.NoError(t, err)
	assert.Equal(t, "met", result.Claims[0].Verdict)
}

// Every step command but later evidence refuses a finally closed deal, and
// later evidence confirms the close: check 4 relies on it.
func TestAClosedDealTakesLaterEvidenceOnly(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, received))
	for _, args := range [][]string{
		{"close", "--deal", id, "--input", writeJSON(t, received)},
		{"check", "--deal", id, "--input", writeJSON(t, `{}`)},
		{"note", "--deal", id, "--kind", "message", "--input", writeJSON(t, `{"from": "user", "text": "hello"}`)},
	} {
		err := dealRunErr(t, args...)
		require.ErrorIs(t, err, cli.ErrInput, args[0])
		assert.Contains(t, cli.SafeError(err), "closed", args[0])
	}
	late := dealRun(t, "note", "--deal", id, "--kind", "evidence", "--input", writeJSON(t, `{"about": "the parcel arrived", "source": "agent"}`))
	withSession(t, func(s *dealSession) {
		require.NoError(t, s.useDeal(t.Context(), id, false))
		events, err := s.load(t.Context(), id)
		require.NoError(t, err)
		last := events[len(events)-1]
		assert.Equal(t, late["capsule_id"], last.CapsuleID)
		assert.Equal(t, events[finalClose(events)].CapsuleID, last.Event.Confirms, "later evidence confirms the close")
	})
}

// writeOpeningsFor writes a field_openings file naming one record of the
// buyer's recorded copy, with the seller's store openings of that record.
func writeOpeningsFor(t *testing.T, pair reconcilePair, recordID string) string {
	t.Helper()
	party(t, pair.seller.config)
	openings := map[string]evidencebook.Opening{}
	r := shelfRecord(t, pair.id, recordID)
	withSession(t, func(s *dealSession) {
		for _, f := range dealHalfFields {
			o, err := s.index.(shelfDealIndex).openingOf(t.Context(), r, f)
			require.NoError(t, err)
			openings[f] = o
		}
	})
	party(t, pair.buyer.config)
	var digest string
	withSession(t, func(s *dealSession) {
		peer, err := s.peerCopy(t.Context(), pair.id, pair.response)
		require.NoError(t, err)
		for d, h := range copyHalves(peer.bundle) {
			if h.recordID == recordID {
				digest = d
			}
		}
	})
	require.NotEmpty(t, digest)
	return writeJSONFile(t, "named.json", map[string]any{"field_openings": map[string]any{digest: openings}})
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

// The verb refuses a peer half that fails admission, and inputs it cannot
// reconcile.
func TestReconcileResultRefuses(t *testing.T) {
	pair := closedPair(t, received, received)
	party(t, pair.seller.config)
	records := bookRecords(t, pair.id)
	party(t, pair.buyer.config)
	var steps []string
	for _, r := range records {
		if r.Header.RecordType == dealStepRecordType {
			steps = append(steps, r.RecordID)
		}
	}
	require.GreaterOrEqual(t, len(steps), 3)
	sellerOpenings, err := dealOpeningsFile(mustRead(t, pair.sellerFile))
	require.NoError(t, err)
	two := map[string]any{}
	for d, o := range sellerOpenings {
		two[d] = o
	}
	two[strings.Repeat("a", 64)] = map[string]any{}

	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"a step before the last (check 4)": {[]string{"--openings", writeOpeningsFor(t, pair, steps[1])}, "not the counterparty's final close"},
		"a digest of no record":            {[]string{"--openings", writeJSONFile(t, "none.json", map[string]any{"field_openings": map[string]any{strings.Repeat("b", 64): map[string]any{}}})}, "no record of the copy"},
		"two named records":                {[]string{"--openings", writeJSONFile(t, "two.json", map[string]any{"field_openings": two})}, "more than one record"},
		"a period holding neither close":   {[]string{"--openings", pair.sellerFile, "--from", "2020-01-01T00:00:00Z", "--to", "2020-01-02T00:00:00Z"}, "nothing to reconcile in this period"},
		"a period that is not seconds":     {[]string{"--openings", pair.sellerFile, "--from", "2026-10-19T00:00:00.5Z"}, "--from is an RFC 3339 UTC time"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, text, err := runReconcileResult(t, pair.id, pair.response, c.args...)
			require.ErrorIs(t, err, cli.ErrInput, text)
			assert.Contains(t, cli.SafeError(err), c.want)
		})
	}
	_, _, _, err = runReconcileResult(t, pair.id, strings.Repeat("0", 64), "--openings", pair.sellerFile)
	require.ErrorIs(t, err, cli.ErrInput, "a response that was not recorded")
}

// No half at all: the buyer has no final close and the seller's openings
// name none.
func TestReconcileResultRefusesWhenNothingIsClosed(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	buyer := thisDealParty(t)
	dealFixture(t)
	dealRun(t, "open", "--records", "typed", "--deal-id", id, "--input", writeJSON(t, sellerTyped))
	seller := thisDealParty(t)
	response := recordedCopy(t, buyer, seller, id)
	_, _, _, err := runReconcileResult(t, id, response)
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Contains(t, cli.SafeError(err), "nothing to reconcile")
}

// A legacy deal has no halves.
func TestReconcileResultRefusesALegacyDeal(t *testing.T) {
	legacyDeals(t)
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, received))
	_, _, _, err := runReconcileResult(t, id, strings.Repeat("0", 64))
	require.ErrorIs(t, err, cli.ErrInput)
}

// A copy recorded as failing verification is never a half.
func TestReconcileResultRefusesACopyThatFailedVerification(t *testing.T) {
	pair := closedPair(t, received, received)
	wrong := pair.seller
	wrong.checkpoint = pair.buyer.checkpoint
	ask := askFor(t, pair.buyer, wrong, pair.id, map[string]any{"min_freshness": map[string]any{"size": 1}})
	require.Equal(t, dealOutcomeArtifactFailed, ask.outcome)
	var failed string
	withSession(t, func(s *dealSession) {
		outcome, err := s.outcomeOf(t.Context(), ask.recordID)
		require.NoError(t, err)
		failed = outcome.digest
	})
	_, _, _, err := runReconcileResult(t, pair.id, failed, "--openings", pair.sellerFile)
	require.ErrorIs(t, err, cli.ErrInput)
}

// A close signed under a key other than the responder key pinned for the
// response (the seller rotated its key, keeping the old one trusted, and
// answered under the new one) is refused (check 1).
func TestReconcileResultRefusesACloseUnderAnotherKey(t *testing.T) {
	pair := closedPair(t, received, received)
	party(t, pair.seller.config)
	p := dealProfile(t)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	seed := filepath.Join(t.TempDir(), "rotated.seed")
	require.NoError(t, os.WriteFile(seed, []byte(hex.EncodeToString(private.Seed())), 0o600))
	p.Signing = cli.Secret{File: seed}
	p.TrustedKeys = append(p.TrustedKeys, hex.EncodeToString(public))
	require.NoError(t, cli.SaveProfile(p, true))
	rotated := thisDealParty(t)
	response := recordedCopy(t, pair.buyer, rotated, pair.id)
	_, _, _, err = runReconcileResult(t, pair.id, response, "--openings", pair.sellerFile)
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Contains(t, cli.SafeError(err), "not signed under the responder key pinned")
}

// Check 5 reads only what a copy discloses, against the openings that
// verify: a disclosed record that is not a close, or a close whose outcome
// is open, is refused; a verified opening that contradicts the disclosed
// outcome is refused; a withheld payload, or an opening that does not
// verify, is no finding here.
func TestCheck5ComparesOnlyVerifiedOpenings(t *testing.T) {
	outcome := map[string]any{"x-deal-v0": map[string]any{"record_type": "outcome"}, "body": map[string]any{"status": "received", "delivered": map[string]any{"price_minor": json.Number("185000")}}}
	closeOf := func(result string) map[string]any {
		return map[string]any{"x-deal-v0": map[string]any{"record_type": "close", "refs": []any{map[string]any{"rel": "outcome", "digest": jsonDigest(t, outcome)}}}, "body": map[string]any{"outcome": result}}
	}
	amount, err := evidencebook.NewOpening("190000")
	require.NoError(t, err)
	agreeing, err := evidencebook.NewOpening("185000")
	require.NoError(t, err)
	currency, err := evidencebook.NewOpening("USD")
	require.NoError(t, err)
	commit := func(o evidencebook.Opening) string {
		c, err := o.Commitment()
		require.NoError(t, err)
		return c
	}
	half := func(o evidencebook.Opening) dealHalf {
		return dealHalf{recordID: "c", header: evidencebook.Header{CommitAlg: evidencebook.CommitAlgNonce256, FieldCommitments: map[string]string{dealAmountField: commit(o), dealCurrencyField: commit(currency)}}}
	}
	bundle := func(closeRecord map[string]any) map[string]any {
		return map[string]any{"disclosures": map[string]any{"c": map[string]any{"agent_input": closeRecord}, "o": map[string]any{"agent_input": outcome}}}
	}
	openings := func(o evidencebook.Opening) map[string]evidencebook.Opening {
		return map[string]evidencebook.Opening{dealAmountField: o, dealCurrencyField: currency}
	}
	require.ErrorIs(t, disclosedCloseHolds(bundle(closeOf("completed")), half(amount), openings(amount)), cli.ErrInput, "a verified amount the disclosed outcome contradicts")
	require.NoError(t, disclosedCloseHolds(bundle(closeOf("completed")), half(agreeing), openings(agreeing)), "a verified amount the outcome states")
	tampered := amount
	tampered.Text = "185000"
	require.NoError(t, disclosedCloseHolds(bundle(closeOf("completed")), half(amount), openings(tampered)), "an opening that does not verify is left to rule 4")
	require.NoError(t, disclosedCloseHolds(bundle(closeOf("completed")), half(amount), map[string]evidencebook.Opening{dealCurrencyField: currency}), "a missing opening is left to rule 3")
	require.ErrorIs(t, disclosedCloseHolds(bundle(closeOf("open")), half(agreeing), openings(agreeing)), cli.ErrInput, "a close whose outcome is open")
	require.ErrorIs(t, disclosedCloseHolds(map[string]any{"disclosures": map[string]any{"c": map[string]any{"agent_input": outcome}}}, half(agreeing), openings(agreeing)), cli.ErrInput, "a disclosed record that is not a close")
	require.NoError(t, disclosedCloseHolds(map[string]any{}, half(amount), openings(amount)), "a withheld payload is no finding")
}

func jsonDigest(t *testing.T, v any) string {
	t.Helper()
	d, err := canonical.JSONDigest(v)
	require.NoError(t, err)
	return d
}

// checkResult runs --check as the selected party.
func runReconcileCheck(t *testing.T, id, response, result string, extra ...string) (map[string]any, error) {
	t.Helper()
	args := append([]string{"--profile", "deal", "deal", "reconcile-result", "--deal", id, "--response", response, "--check", result}, extra...)
	text, err := invoke(t, "", args...)
	var out map[string]any
	if json.Unmarshal([]byte(text), &out) != nil {
		out = map[string]any{"text": text}
	}
	return out, err
}

// --check from both sides: the producer re-checks with its inputs, and the
// counterparty with the file it sent. "differs" and "cannot reproduce" are
// kept apart.
func TestReconcileResultCheck(t *testing.T) {
	pair := closedPair(t, received, received)
	_, _, resultPath, err := runReconcileResult(t, pair.id, pair.response, "--openings", pair.sellerFile)
	require.NoError(t, err)
	buyerFile := discloseTo(t, pair.id, "the seller")

	out, err := runReconcileCheck(t, pair.id, pair.response, resultPath, "--openings", pair.sellerFile)
	require.NoError(t, err, out)
	assert.Equal(t, reconcileCheckEqual, out["check"])

	sellerResponse := recordedCopy(t, pair.seller, pair.buyer, pair.id)
	out, err = runReconcileCheck(t, pair.id, sellerResponse, resultPath, "--openings", buyerFile, "--sent", pair.sellerFile)
	require.NoError(t, err, out)
	assert.Equal(t, reconcileCheckEqual, out["check"], "the counterparty recomputes the same Result")

	for name, args := range map[string][]string{
		"without --sent":                  {"--openings", buyerFile},
		"a --sent file no share recorded": {"--openings", buyerFile, "--sent", writeJSONFile(t, "other.json", map[string]any{"field_openings": map[string]any{}})},
		"the producer's openings missing": {"--sent", pair.sellerFile},
		"an adjudicator's copy as --sent": {"--openings", buyerFile, "--sent", adjudicatorFile(t, pair.id)},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := runReconcileCheck(t, pair.id, sellerResponse, resultPath, args...)
			require.ErrorIs(t, err, cli.ErrInput)
			assert.Equal(t, reconcileCheckCannotReproduce, out["check"])
		})
	}

	party(t, pair.buyer.config)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(mustRead(t, resultPath), &doc))
	for name, change := range map[string]func(map[string]any){
		"a tally":   func(c map[string]any) { c["reconcile"].(map[string]any)["tallies"].(map[string]any)["matched"] = 2 },
		"a verdict": func(c map[string]any) { c["verdict"] = "not_met" },
		"the peer": func(c map[string]any) {
			c["reconcile"].(map[string]any)["peer"] = strings.Repeat("c", 64)
		},
		// A period is part of the input set: one that holds neither half
		// recomputes to other tallies. (One that still holds both is the
		// recompute of these inputs over that period, and is equal.)
		"the period": func(c map[string]any) {
			c["reconcile"].(map[string]any)["period"] = map[string]any{"start": "2020-01-01T00:00:00Z", "end": "2020-01-02T00:00:00Z"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			var changed map[string]any
			raw, err := json.Marshal(doc)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &changed))
			for _, c := range changed["claims"].([]any) {
				change(c.(map[string]any))
			}
			out, err := runReconcileCheck(t, pair.id, pair.response, writeJSONFile(t, "changed.json", changed), "--openings", pair.sellerFile)
			require.ErrorIs(t, err, cli.ErrConflict)
			assert.Equal(t, reconcileCheckDiffers, out["check"])
		})
	}
}

// A Result made from openings without the amount is the recompute of that
// input set: the producer's check with it is equal, and the counterparty's
// check with the share it recorded, which carries the amount, differs and
// names that file.
func TestReconcileResultCheckIsAgainstTheInputsGiven(t *testing.T) {
	pair := closedPair(t, received, received)
	missing := editOpenings(t, pair.sellerFile, func(m map[string]map[string]evidencebook.Opening) {
		for d := range m {
			delete(m[d], dealAmountField)
		}
	})
	_, _, resultPath, err := runReconcileResult(t, pair.id, pair.response, "--openings", missing)
	require.NoError(t, err)
	out, err := runReconcileCheck(t, pair.id, pair.response, resultPath, "--openings", missing)
	require.NoError(t, err, out)
	assert.Equal(t, reconcileCheckEqual, out["check"])

	buyerFile := discloseTo(t, pair.id, "the seller")
	sellerResponse := recordedCopy(t, pair.seller, pair.buyer, pair.id)
	out, err = runReconcileCheck(t, pair.id, sellerResponse, resultPath, "--openings", buyerFile, "--sent", pair.sellerFile)
	require.ErrorIs(t, err, cli.ErrConflict)
	assert.Equal(t, reconcileCheckDiffers, out["check"])
	assert.Contains(t, out["detail"], digestOfFile(t, pair.sellerFile))
}

func adjudicatorFile(t *testing.T, id string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "adjudicator.json")
	out, err := invoke(t, "", "--profile", "deal", "disclose", "--deal", id, "--share", "adjudicator", "--to", "the platform", "--out", path)
	require.NoError(t, err, out)
	return path
}

// result build refuses a claim under exactly ec:deal-reconcile@1 before it
// opens the book, and keeps its own rules for the same claim under another
// contract_ref. result open names the claim as not recomputed.
func TestResultCommandsAndTheDealReconcileContract(t *testing.T) {
	pair := closedPair(t, received, received)
	_, _, resultPath, err := runReconcileResult(t, pair.id, pair.response, "--openings", pair.sellerFile)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(mustRead(t, resultPath), &doc))

	text, err := invoke(t, "", "result", "open", resultPath)
	require.NoError(t, err)
	assert.Contains(t, text, "not recomputed: claim reconcile-amount")
	out, err := invoke(t, "", "result", "open", resultPath, "--format", "json")
	require.NoError(t, err)
	var opened map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &opened))
	assert.Equal(t, []any{"reconcile-amount", "reconcile-currency"}, opened["not_recomputed"])

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := bookProfile(t, "a") // no store: a refusal from the book would be "no book yet"
	_, _, err = runResultBuild(t, p, doc)
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Contains(t, cli.SafeError(err), "deal reconcile-result")

	for _, c := range doc["claims"].([]any) {
		c.(map[string]any)["contract_ref"] = "ec:deal-reconcile@2"
	}
	_, _, err = runResultBuild(t, p, doc)
	require.Error(t, err)
	assert.NotContains(t, cli.SafeError(err), "deal reconcile-result", "another version gets no exception and no refusal of this kind")
}
