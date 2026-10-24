package deal

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/capsule-cli/cli"
	"github.com/action-state-group/evidencebook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sealedOf is the verb summary's sealed member.
func sealedOf(t *testing.T, summary map[string]any) map[string]any {
	t.Helper()
	sealed, ok := summary["sealed"].(map[string]any)
	require.True(t, ok, "the verb reports what it sealed: %v", summary)
	return sealed
}

// resultsRecords are the evidence_result records on a deal's results book,
// in order, as the selected party holds them.
func resultsRecords(t *testing.T, id string) []evidencebook.Record {
	t.Helper()
	var records []evidencebook.Record
	withSession(t, func(s *dealSession) {
		book, err := s.shelf.Open(t.Context(), dealResultsLogID(id))
		require.NoError(t, err)
		records, err = book.Query(t.Context(), evidencebook.Filter{RecordType: evidencebook.RecordTypeEvidenceResult})
		require.NoError(t, err)
	})
	return records
}

func jcsOf(t *testing.T, raw []byte) []byte {
	t.Helper()
	doc, err := cli.DecodeBundleJSON(raw)
	require.NoError(t, err)
	out, err := canonical.JCS(doc)
	require.NoError(t, err)
	return out
}

// A terms Result is sealed on the deal's results book, never the deal's own
// book: the record's statement is the Result's JCS, which the file holds in
// readable form, and a checkpoint covers it. With no witness configured its
// state is sealed.
func TestTermsResultIsSealedOnTheResultsBook(t *testing.T) {
	pair := closedPair(t, received, received)
	summary, _, out, err := runTermsResult(t, pair.id, pair.response, "--openings", pair.sellerFile)
	require.NoError(t, err)
	sealed := sealedOf(t, summary)
	assert.Equal(t, "deal/"+pair.id+"/results", sealed["log_id"])
	assert.Equal(t, termsSealed, sealed["state"])
	assert.Equal(t, false, sealed["retry"])
	records := resultsRecords(t, pair.id)
	require.Len(t, records, 1)
	assert.Equal(t, sealed["record_id"], records[0].RecordID)
	assert.Equal(t, evidencebook.DealReconcileContractRef, records[0].Header.SubjectRef)
	assert.Empty(t, records[0].Header.Statement, "the Result is never in the header, which every bundle carries")
	withSession(t, func(s *dealSession) {
		require.NoError(t, s.useDeal(t.Context(), pair.id, false))
		payload, err := s.verifiedPayload(t.Context(), records[0].RecordID)
		require.NoError(t, err)
		inner, err := termsPayloadResult(payload)
		require.NoError(t, err)
		innerJCS, err := canonical.JCS(inner)
		require.NoError(t, err)
		assert.Equal(t, jcsOf(t, mustRead(t, out)), innerJCS, "the record's payload holds the file's document")
		var wrapper map[string]any
		require.NoError(t, json.Unmarshal(payload, &wrapper))
		assert.Len(t, wrapper["nonce"], 64, "under a fresh nonce")
		book, err := s.shelf.Open(t.Context(), dealResultsLogID(pair.id))
		require.NoError(t, err)
		covered, err := book.CheckpointCovers(t.Context(), records[0].Seq)
		require.NoError(t, err)
		assert.True(t, covered)
	})
	// Nothing was added to the deal's own book.
	for _, r := range bookRecords(t, pair.id) {
		assert.NotEqual(t, records[0].RecordID, r.RecordID)
	}
}

// A retry, equal in every member but generated_at to the newest sealed
// Result, seals nothing new and keeps that Result's generated_at; a changed
// run is a new issuance; and A, then B, then A again seals three records.
func TestTermsResultRetriesAndNewIssuances(t *testing.T) {
	pair := closedPair(t, received, received)
	missing := editOpenings(t, pair.sellerFile, func(m map[string]map[string]evidencebook.Opening) {
		for d := range m {
			delete(m[d], dealAmountField)
		}
	})
	setDealClock(t, "2026-10-19T12:00:00Z")
	first, a1, _, err := runTermsResult(t, pair.id, pair.response, "--openings", pair.sellerFile)
	require.NoError(t, err)
	setDealClock(t, "2026-10-19T12:00:05Z")
	retry, a1again, _, err := runTermsResult(t, pair.id, pair.response, "--openings", pair.sellerFile)
	require.NoError(t, err)
	assert.Equal(t, true, sealedOf(t, retry)["retry"])
	assert.Equal(t, sealedOf(t, first)["record_id"], sealedOf(t, retry)["record_id"])
	assert.Equal(t, a1.GeneratedAt, a1again.GeneratedAt, "a retry keeps the sealed Result's generated_at")
	require.Len(t, resultsRecords(t, pair.id), 1)

	// With the clock held, A again is byte for byte the first A: only the
	// newest Result is a retry's match, so it is a third record.
	setDealClock(t, "2026-10-19T12:00:00Z")
	b, _, _, err := runTermsResult(t, pair.id, pair.response, "--openings", missing)
	require.NoError(t, err)
	assert.Equal(t, false, sealedOf(t, b)["retry"])
	again, _, _, err := runTermsResult(t, pair.id, pair.response, "--openings", pair.sellerFile)
	require.NoError(t, err)
	assert.Equal(t, false, sealedOf(t, again)["retry"], "only the newest Result is a retry's match")
	assert.NotEqual(t, sealedOf(t, first)["record_id"], sealedOf(t, again)["record_id"])
	assert.Len(t, resultsRecords(t, pair.id), 3)
}

// A deal with no own final close, reconciled on the peer's half alone, is
// sealed like any other.
func TestTermsResultSealsAPeerOnlyResult(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	buyer := thisDealParty(t)
	dealFixture(t)
	dealRun(t, "open", "--records", "typed", "--deal-id", id, "--input", writeJSON(t, sellerTyped))
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, received))
	seller := thisDealParty(t)
	response := recordedCopy(t, buyer, seller, id)
	party(t, seller.config)
	sellerFile := discloseTo(t, id, "the buyer")
	party(t, buyer.config)
	summary, result, _, err := runTermsResult(t, id, response, "--openings", sellerFile)
	require.NoError(t, err)
	assert.Equal(t, evidencebook.ResultTallies{Insufficient: 1}, result.Claims[0].Reconcile.Tallies)
	assert.NotEmpty(t, sealedOf(t, summary)["record_id"])
	assert.Len(t, resultsRecords(t, id), 1)
}

// The verb's only resolver is the run's own halves: a Result citing any
// other digest is never sealed.
func TestSealingRefusesEvidenceThatIsNotTheRunsHalves(t *testing.T) {
	pair := closedPair(t, received, received)
	_, result, _, err := runTermsResult(t, pair.id, pair.response, "--openings", pair.sellerFile)
	require.NoError(t, err)
	withSession(t, func(s *dealSession) {
		halves := map[string]bool{result.Claims[0].Evidence[0].Digest: true}
		_, err := s.sealTermsResult(t.Context(), pair.id, &result, halves)
		require.ErrorIs(t, err, cli.ErrConflict)
	})
	assert.Len(t, resultsRecords(t, pair.id), 1, "nothing more was sealed")
}

// No copy of the deal carries a results record: they are bundles of the
// deal's own book only.
func TestNoDealCopyCarriesAResultsRecord(t *testing.T) {
	pair := closedPair(t, received, received)
	summary, _, _, err := runTermsResult(t, pair.id, pair.response, "--openings", pair.sellerFile)
	require.NoError(t, err)
	recordID := sealedOf(t, summary)["record_id"].(string)
	own := filepath.Join(t.TempDir(), "own.json")
	_, err = invoke(t, "", "--profile", "deal", "bundle", "--deal", pair.id, "--out", own)
	require.NoError(t, err)
	shared := discloseTo(t, pair.id, "the seller")
	for _, path := range []string{own, shared} {
		assert.NotContains(t, string(mustRead(t, path)), recordID)
		assert.NotContains(t, string(mustRead(t, path)), dealTermsResultActionID)
	}
	sellerResponse := recordedCopy(t, pair.seller, pair.buyer, pair.id)
	withSession(t, func(s *dealSession) {
		peer, err := s.peerCopy(t.Context(), pair.id, sellerResponse)
		require.NoError(t, err)
		assert.Nil(t, copyRecord(peer.bundle, recordID), "a respond copy carries none either")
	})
}

// `deal terms-result --share counterparty` discloses exactly one sealed
// Result, passes the core verifier, and is on record with the exact bytes
// written; the other Results' payloads are withheld.
func TestTermsResultShareDisclosesOneResult(t *testing.T) {
	pair := closedPair(t, received, received)
	missing := editOpenings(t, pair.sellerFile, func(m map[string]map[string]evidencebook.Opening) {
		for d := range m {
			delete(m[d], dealAmountField)
		}
	})
	setDealClock(t, "2026-10-19T12:00:00Z")
	first, _, _, err := runTermsResult(t, pair.id, pair.response, "--openings", pair.sellerFile)
	require.NoError(t, err)
	setDealClock(t, "2026-10-19T12:00:10Z")
	_, _, _, err = runTermsResult(t, pair.id, pair.response, "--openings", missing)
	require.NoError(t, err)
	firstID := sealedOf(t, first)["record_id"].(string)

	path := filepath.Join(t.TempDir(), "result-share.json")
	out, err := invoke(t, "", "--profile", "deal", "deal", "terms-result", "--deal", pair.id, "--share", "counterparty", "--recipient", "the seller", "--record", firstID, "--out", path)
	require.NoError(t, err, out)
	var shared map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &shared))
	assert.Equal(t, firstID, shared["record_id"])
	assert.Equal(t, termsSealed, shared["state"])

	var b map[string]any
	require.NoError(t, json.Unmarshal(mustRead(t, path), &b))
	disclosures, _ := b["disclosures"].(map[string]any)
	require.Len(t, disclosures, 1, "exactly one Result's payload is disclosed")
	assert.Contains(t, disclosures, firstID)
	assert.Len(t, b["records"], 2, "both Results are present as positions")
	// The other Result appears nowhere in the bundle: not as a disclosure,
	// and not in its record's header.
	for _, r := range b["records"].([]any) {
		header := r.(map[string]any)[evidencebook.RecordHeaderMember].(map[string]any)
		assert.NotContains(t, header, "statement")
	}
	other := resultsRecords(t, pair.id)[1]
	withSession(t, func(s *dealSession) {
		require.NoError(t, s.useDeal(t.Context(), pair.id, false))
		payload, err := s.verifiedPayload(t.Context(), other.RecordID)
		require.NoError(t, err)
		assert.NotContains(t, string(mustRead(t, path)), string(payload))
		inner, err := termsPayloadResult(payload)
		require.NoError(t, err)
		innerJSON, err := json.Marshal(inner)
		require.NoError(t, err)
		var otherDoc evidencebook.DealReconcileResult
		require.NoError(t, json.Unmarshal(innerJSON, &otherDoc))
		// Nor can it be confirmed by a guess: the withheld record binds a
		// nonce-salted payload, so hashing the bare Result matches nothing.
		bare, err := canonical.JCS(inner)
		require.NoError(t, err)
		assert.NotContains(t, string(mustRead(t, path)), sha256Hex(bare), "a guessed earlier Result cannot be confirmed")
		tallies, err := json.Marshal(otherDoc.Claims[0].Reconcile.Tallies)
		require.NoError(t, err)
		assert.NotContains(t, string(mustRead(t, path)), string(tallies), "the withheld Result's tallies are not readable")
	})
	result, err := verifyWithDirectory(t, path, "")
	require.NoError(t, err)
	assert.Equal(t, "VALID", result["verdict"])
	withSession(t, func(s *dealSession) {
		recorded, err := s.recordedCounterpartyShare(t.Context(), pair.id, digestOfFile(t, path))
		require.NoError(t, err)
		assert.True(t, recorded, "the share is on record with the exact bytes written")
	})

	for _, args := range [][]string{
		{"--share", "adjudicator", "--recipient", "x", "--out", filepath.Join(t.TempDir(), "a.json")},
		{"--share", "counterparty", "--out", filepath.Join(t.TempDir(), "b.json")},
		{"--share", "counterparty", "--recipient", "x", "--out", filepath.Join(t.TempDir(), "c.json"), "--response", pair.response},
		{"--share", "counterparty", "--recipient", "x", "--out", filepath.Join(t.TempDir(), "d.json"), "--record", "no-such-record"},
	} {
		_, err := invoke(t, "", append([]string{"--profile", "deal", "deal", "terms-result", "--deal", pair.id}, args...)...)
		require.ErrorIs(t, err, cli.ErrInput, args)
	}
}

// A results book ends with its deal: settled through settleEnded, which
// maps the live results book to its deal.
func TestAResultsBookEndsWithItsDeal(t *testing.T) {
	pair := closedPair(t, received, received)
	_, _, _, err := runTermsResult(t, pair.id, pair.response, "--openings", pair.sellerFile)
	require.NoError(t, err)
	withSession(t, func(s *dealSession) {
		c, _, err := s.cadenceOf(t.Context())
		require.NoError(t, err)
		live, err := c.Live(t.Context())
		require.NoError(t, err)
		assert.Contains(t, live, dealResultsLogID(pair.id), "a sealed Result makes its book live")
		require.NoError(t, s.settleEnded(t.Context(), c))
		ended, err := s.ended(t.Context(), dealResultsLogID(pair.id))
		require.NoError(t, err)
		assert.True(t, ended, "the deal is finally closed, so its results book has ended")
	})
	id, ok := dealOfResultsBook("deal/" + pair.id + "/results")
	assert.True(t, ok)
	assert.Equal(t, pair.id, id)
	for _, bad := range []string{"deal/" + pair.id, "deal/x/results", "results", "deal/" + pair.id + "/results/x"} {
		_, ok := dealOfResultsBook(bad)
		assert.False(t, ok, bad)
	}
}

// The shelf's enumerators never show a results book as a deal.
func TestEnumeratorsSkipResultsBooks(t *testing.T) {
	pair := closedPair(t, received, received)
	_, _, _, err := runTermsResult(t, pair.id, pair.response, "--openings", pair.sellerFile)
	require.NoError(t, err)
	out, err := invoke(t, "", "--profile", "deal", "deal", "list", "--json")
	require.NoError(t, err)
	var list []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &list))
	require.Len(t, list, 1)
	assert.Equal(t, pair.id, list[0]["deal_id"])
	assert.NotContains(t, out, "/results")
	status, err := invoke(t, "", "--profile", "deal", "deal", "checkpoint", "status", "--deal", pair.id)
	require.NoError(t, err, status)
	assert.NotContains(t, status, "/results")
}

// With a witness configured, a sealed Result is pending until a witnessed
// tick covers its book's checkpoint, and then its share carries the chain.
func TestASealedResultIsWitnessedThroughTheCadence(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	endpoint, _ := countingWitness(t)
	p := cadenceFixture(t, endpoint, public, "1h", "0s", 0)
	id := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, received))
	buyer := thisDealParty(t)
	dealFixture(t)
	dealRun(t, "open", "--records", "typed", "--deal-id", id, "--input", writeJSON(t, sellerTyped))
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, received))
	seller := thisDealParty(t)
	response := recordedCopy(t, buyer, seller, id)
	party(t, seller.config)
	sellerFile := discloseTo(t, id, "the buyer")
	party(t, buyer.config)

	summary, _, _, err := runTermsResult(t, id, response, "--openings", sellerFile)
	require.NoError(t, err)
	assert.Equal(t, termsPendingWitness, sealedOf(t, summary)["state"], "sealed, not yet in a witnessed tick")

	require.Equal(t, "ticked", dealRun(t, "tick")["state"])
	deliver(t, p, cadenceSize(t, p), key)
	path := filepath.Join(t.TempDir(), "share.json")
	out, err := invoke(t, "", "--profile", "deal", "deal", "terms-result", "--deal", id, "--share", "counterparty", "--recipient", "the seller", "--out", path)
	require.NoError(t, err, out)
	var shared map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &shared))
	assert.Equal(t, termsWitnessed, shared["state"])
	result, err := verifyWithDirectory(t, path, writeDirectory(t, rawKeyRow(endpoint, public)))
	require.NoError(t, err)
	status, _ := bundleWitnesses(result)
	assert.Equal(t, "pass", status, "results book checkpoint -> tree path -> cadence entry -> witnessed cadence checkpoint")
}

// A run that seals and then fails to write its file still reports the
// record, and its retry seals nothing new.
func TestASealedResultSurvivesAFailedWrite(t *testing.T) {
	pair := closedPair(t, received, received)
	bad := filepath.Join(t.TempDir(), "result.json")
	require.NoError(t, os.WriteFile(bad, []byte("taken"), 0o600)) // --out never overwrites a file
	text, err := invoke(t, "", "--profile", "deal", "deal", "terms-result", "--deal", pair.id, "--response", pair.response, "--openings", pair.sellerFile, "--out", bad)
	require.Error(t, err)
	records := resultsRecords(t, pair.id)
	require.Len(t, records, 1, "the Result was sealed before the write failed")
	assert.Contains(t, text, records[0].RecordID, "the failure names the sealed record")
	setDealClock(t, "2026-10-19T13:00:00Z")
	retry, _, _, err := runTermsResult(t, pair.id, pair.response, "--openings", pair.sellerFile)
	require.NoError(t, err)
	assert.Equal(t, true, sealedOf(t, retry)["retry"])
	assert.Equal(t, records[0].RecordID, sealedOf(t, retry)["record_id"])
	assert.Len(t, resultsRecords(t, pair.id), 1)
}

// The results book's whole lifecycle with a witness: it is live once a
// Result is sealed, leaves the tick with its finally closed deal once a
// witnessed tick holds its checkpoint, comes back live on a new issuance
// after the deal itself has left, and leaves again once that is witnessed.
func TestAResultsBookLeavesAndReturnsWithItsDeal(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	endpoint, _ := countingWitness(t)
	p := cadenceFixture(t, endpoint, public, "1h", "0s", 0)
	id := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, received))
	buyer := thisDealParty(t)
	dealFixture(t)
	dealRun(t, "open", "--records", "typed", "--deal-id", id, "--input", writeJSON(t, sellerTyped))
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, received))
	seller := thisDealParty(t)
	response := recordedCopy(t, buyer, seller, id)
	party(t, seller.config)
	sellerFile := discloseTo(t, id, "the buyer")
	party(t, buyer.config)
	results := dealResultsLogID(id)
	witnessTick := func() {
		t.Helper()
		setDealClock(t, cli.DealClock().UTC().Add(time.Hour).Format("2006-01-02T15:04:05Z"))
		require.Equal(t, "ticked", dealRun(t, "tick")["state"])
		deliver(t, p, cadenceSize(t, p), key)
	}

	_, _, _, err = runTermsResult(t, id, response, "--openings", sellerFile)
	require.NoError(t, err)
	assert.Contains(t, liveLogs(t, p), results)
	witnessTick()
	live := liveLogs(t, p)
	assert.NotContains(t, live, cli.DealLogID(id), "the closed deal has left")
	assert.NotContains(t, live, results, "its results book left with it")

	missing := editOpenings(t, sellerFile, func(m map[string]map[string]evidencebook.Opening) {
		for d := range m {
			delete(m[d], dealAmountField)
		}
	})
	_, _, _, err = runTermsResult(t, id, response, "--openings", missing)
	require.NoError(t, err)
	live = liveLogs(t, p)
	assert.Contains(t, live, results, "a new issuance makes the results book live again")
	assert.NotContains(t, live, cli.DealLogID(id), "the deal itself stays out")
	witnessTick()
	assert.NotContains(t, liveLogs(t, p), results, "settled through its deal, it leaves again")
}

// A second Result sealed after a witnessed tick is pending: the witnessed
// tick holds the results book's earlier checkpoint (a "part" chain), and
// what is not yet witnessed waits for the next tick (scheduled).
func TestASecondResultIsPartlyWitnessed(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	endpoint, _ := countingWitness(t)
	p := cadenceFixture(t, endpoint, public, "1h", "0s", 0)
	id := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, received))
	buyer := thisDealParty(t)
	dealFixture(t)
	dealRun(t, "open", "--records", "typed", "--deal-id", id, "--input", writeJSON(t, sellerTyped))
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, received))
	seller := thisDealParty(t)
	response := recordedCopy(t, buyer, seller, id)
	party(t, seller.config)
	sellerFile := discloseTo(t, id, "the buyer")
	party(t, buyer.config)

	_, _, _, err = runTermsResult(t, id, response, "--openings", sellerFile)
	require.NoError(t, err)
	setDealClock(t, cli.DealClock().UTC().Add(time.Hour).Format("2006-01-02T15:04:05Z"))
	require.Equal(t, "ticked", dealRun(t, "tick")["state"])
	deliver(t, p, cadenceSize(t, p), key)

	missing := editOpenings(t, sellerFile, func(m map[string]map[string]evidencebook.Opening) {
		for d := range m {
			delete(m[d], dealAmountField)
		}
	})
	second, _, _, err := runTermsResult(t, id, response, "--openings", missing)
	require.NoError(t, err)
	assert.Equal(t, termsPendingWitness, sealedOf(t, second)["state"])

	path := filepath.Join(t.TempDir(), "share.json")
	out, err := invoke(t, "", "--profile", "deal", "deal", "terms-result", "--deal", id, "--share", "counterparty", "--recipient", "the seller", "--out", path)
	require.NoError(t, err, out)
	var b map[string]any
	require.NoError(t, json.Unmarshal(mustRead(t, path), &b))
	chain := b["extensions"].(map[string]any)[cli.DealCadenceExtension].(map[string]any)
	assert.Equal(t, "part", chain["extent"], "the witnessed tick holds the results book's earlier checkpoint")
	assert.Equal(t, "scheduled", chain["rest"].(map[string]any)["state"], "the rest waits for the next tick")
}

// A share never provisions a results book for a deal that has none.
func TestAShareOfNoResultLeavesNoBook(t *testing.T) {
	pair := closedPair(t, received, received)
	_, err := invoke(t, "", "--profile", "deal", "deal", "terms-result", "--deal", pair.id, "--share", "counterparty", "--recipient", "x", "--out", filepath.Join(t.TempDir(), "x.json"))
	require.ErrorIs(t, err, cli.ErrInput)
	withSession(t, func(s *dealSession) {
		books, err := s.shelf.Books(t.Context())
		require.NoError(t, err)
		assert.NotContains(t, books, dealResultsLogID(pair.id))
	})
}
