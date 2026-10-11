package cli

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/evidencebook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dealParty is one party's configuration and the two public keys the other
// party pins for it: the signing key its answers are signed under and the
// checkpoint key its copies are anchored under.
type dealParty struct {
	config, signer, checkpoint string
}

func thisDealParty(t *testing.T) dealParty {
	t.Helper()
	p, err := loadProfile("deal")
	require.NoError(t, err)
	signing, err := privateKey(p.Signing)
	require.NoError(t, err)
	checkpoint, err := privateKey(p.Checkpoint.Signing)
	require.NoError(t, err)
	return dealParty{
		config: os.Getenv("XDG_CONFIG_HOME"),
		signer: hex.EncodeToString(signing.Public().(ed25519.PublicKey)), checkpoint: hex.EncodeToString(checkpoint.Public().(ed25519.PublicKey)),
	}
}

func runJSON(t *testing.T, args ...string) map[string]any {
	t.Helper()
	out, err := invoke(t, "", append([]string{"--profile", "deal"}, args...)...)
	require.NoError(t, err, out)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m), out)
	return m
}

// askedCopy is one recorded request, the bytes `request --output` wrote,
// and the response recorded for it.
type askedCopy struct {
	recordID, requestPath, responsePath, outcome string
	response                                     map[string]any
}

// askFor runs the carrier once: the asker records a request for the deal's
// copy with coverage, the responder answers it, and the asker records the
// answer under the responder's pinned keys.
func askFor(t *testing.T, asker, responder dealParty, dealID string, coverage map[string]any) askedCopy {
	t.Helper()
	dir := t.TempDir()
	ask := askedCopy{requestPath: filepath.Join(dir, "request.json"), responsePath: filepath.Join(dir, "response.json")}
	party(t, asker.config)
	sent := runJSON(t, "request", "--request", writeJSONFile(t, "ask.json", map[string]any{
		"subject": map[string]any{"kind": "correlation", "value": dealID}, "coverage": coverage,
	}), "--responder", "the other party", "--output", ask.requestPath)
	ask.recordID = sent["record_id"].(string)
	assert.Equal(t, sent["request_digest"], digestOfFile(t, ask.requestPath), "the recorded digest is the digest of the bytes written")

	party(t, responder.config)
	runJSON(t, "respond", "--request", ask.requestPath, "--requester", "the asking party", "--output", ask.responsePath)

	party(t, asker.config)
	recorded := runJSON(t, "request", "--response", ask.responsePath, "--for", ask.recordID,
		"--responder-key", responder.signer, "--responder-checkpoint-key", responder.checkpoint)
	ask.outcome = recorded["outcome"].(string)
	require.NoError(t, json.Unmarshal(mustRead(t, ask.responsePath), &ask.response))
	return ask
}

// pinnedCopy fetches a party's copy once under min_freshness, then again
// pinned to the anchor that copy came under: the answer composed is the
// pinned one.
func pinnedCopy(t *testing.T, asker, responder dealParty, dealID string) (askedCopy, map[string]any) {
	t.Helper()
	fresh := askFor(t, asker, responder, dealID, map[string]any{"min_freshness": map[string]any{"size": 1}})
	require.Equal(t, "artifact", fresh.outcome)
	pin := pinOf(t, fresh.response)
	pinned := askFor(t, asker, responder, dealID, map[string]any{"expected_pin": pin})
	require.Equal(t, "artifact", pinned.outcome)
	return pinned, pin
}

func pinOf(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	root, size, ok := strings.Cut(response["artifact"].(map[string]any)["anchor"].(string), ":")
	require.True(t, ok)
	var n uint64
	require.NoError(t, json.Unmarshal([]byte(size), &n))
	return map[string]any{"root": root, "mmr_size": n}
}

func digestOfFile(t *testing.T, path string) string {
	t.Helper()
	sum := sha256.Sum256(mustRead(t, path))
	return hex.EncodeToString(sum[:])
}

func composeMembers(t *testing.T, dealID string, members ...askedCopy) (string, error) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "composed.json")
	args := []string{"--profile", "deal", "compose", "--deal", dealID, "--output", out}
	for _, m := range members {
		args = append(args, "--member", m.recordID+"="+m.responsePath)
	}
	text, err := invoke(t, "", args...)
	if err != nil {
		return text, err
	}
	return out, nil
}

func composedBlock(t *testing.T, path string) map[string]any {
	t.Helper()
	b := exactBundle(t, string(mustRead(t, path)))
	return b["extensions"].(map[string]any)["composed/v1"].(map[string]any)
}

func memberByID(t *testing.T, block map[string]any, id string) map[string]any {
	t.Helper()
	for _, raw := range block["members"].([]any) {
		if m := raw.(map[string]any); m["id"] == id {
			return m
		}
	}
	t.Fatalf("no member %s", id)
	return nil
}

// u817's acceptance: a buyer and a seller (opened under the buyer's deal id)
// ask each other for their copies over the evidence-request carrier, pinned,
// and compose from what they recorded; the composition verifies VALID, each
// member's request_digest is the digest of the bytes `request --output`
// wrote, and the seller's member engages x-deal-seller/v0 as the seller cut
// it.
func TestDealCopiesTravelOverEvidenceRequestsAndCompose(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	buyer := thisDealParty(t)
	dealFixture(t)
	dealRun(t, "open", "--records", "typed", "--deal-id", id, "--input", writeJSON(t, sellerTyped))
	dealRun(t, "check", "--deal", id, "--input", writeJSON(t,
		`{"action":"offer","amount_minor":190000,"terms":{"item":"example bicycle","quantity":1,"price_minor":190000}}`))
	seller := thisDealParty(t)

	// Both directions, each pinned; each party also asks itself, so the set
	// it composes has a real request behind its own member too.
	buyerCopyForSeller, _ := pinnedCopy(t, seller, buyer, id)
	buyerOwn, _ := pinnedCopy(t, buyer, buyer, id)
	sellerCopyForBuyer, sellerPin := pinnedCopy(t, buyer, seller, id)
	sellerOwn, _ := pinnedCopy(t, seller, seller, id)

	for name, set := range map[string]struct {
		composer dealParty
		members  []askedCopy
		seller   askedCopy
	}{
		"the buyer composes":  {buyer, []askedCopy{buyerOwn, sellerCopyForBuyer}, sellerCopyForBuyer},
		"the seller composes": {seller, []askedCopy{buyerCopyForSeller, sellerOwn}, sellerOwn},
	} {
		t.Run(name, func(t *testing.T) {
			party(t, set.composer.config)
			path, err := composeMembers(t, id, set.members...)
			require.NoError(t, err, path)
			result, err := verifyWithDirectory(t, path, "")
			require.NoError(t, err)
			assert.Equal(t, "VALID", result["verdict"], result)

			block := composedBlock(t, path)
			for _, m := range set.members {
				member := memberByID(t, block, m.recordID)
				assert.Equal(t, digestOfFile(t, m.requestPath), member["request_digest"], "the real request's digest")
				assert.Equal(t, "artifact", member["outcome"])
			}
			sellerMember := memberByID(t, block, set.seller.recordID)
			extensions := sellerMember["bundle"].(map[string]any)["extensions"].(map[string]any)
			assert.Contains(t, extensions, dealSellerExtension, "the seller's copy engages its kind as cut")
			assert.NotContains(t, block, "joins", "no join is declared that the root records cannot ground")
		})
	}

	// The deal moves on: the seller's copy at the old pin no longer exists,
	// so the pinned ask is answered with a signed refusal, never a copy at
	// another anchor.
	party(t, seller.config)
	dealRun(t, "check", "--deal", id, "--input", writeJSON(t,
		`{"action":"offer","amount_minor":185000,"terms":{"item":"example bicycle","quantity":1,"price_minor":185000}}`))
	reports := dealReportCount(t, id)
	stale := askFor(t, buyer, seller, id, map[string]any{"expected_pin": sellerPin})
	assert.Equal(t, "refusal", stale.outcome)
	assert.Equal(t, "coverage_unsatisfiable", stale.response["refusal"].(map[string]any)["reason"])
	party(t, seller.config)
	assert.Equal(t, reports, dealReportCount(t, id), "a refused pin seals nothing on the deal's log")

	// A fresh copy puts a new report at the head; the old pin still names
	// another checkpoint, and is still refused.
	askFor(t, buyer, seller, id, map[string]any{"min_freshness": map[string]any{"size": 1}})
	staleAgain := askFor(t, buyer, seller, id, map[string]any{"expected_pin": sellerPin})
	assert.Equal(t, "refusal", staleAgain.outcome)

	party(t, buyer.config)
	path, err := composeMembers(t, id, buyerOwn, stale)
	require.NoError(t, err, path)
	result, err := verifyWithDirectory(t, path, "")
	require.NoError(t, err)
	// Bundle -01: a verified refusal member is not a failed claim, so the
	// composition verifies; the refusal is what it reports. (u817 DESIGN.md
	// §4: the item's "not VALID" for this case is open with Steven.)
	assert.Equal(t, "VALID", result["verdict"])
	refusal := memberByID(t, composedBlock(t, path), stale.recordID)
	assert.Equal(t, "refusal", refusal["outcome"])
	assert.Equal(t, digestOfFile(t, stale.requestPath), refusal["request_digest"])
	composed := composedEntry(t, result)
	for _, raw := range composed["members"].([]any) {
		if m := raw.(map[string]any); m["id"] == stale.recordID {
			assert.Equal(t, map[string]any{"status": "verified", "profile": EvidenceBookRefusalProfile}, m["refusal_signature"],
				"the refusal is signed by the seller and verifies")
			assert.Equal(t, "refusal", m["outcome"])
		}
	}
	assert.Equal(t, seller.signer, refusal["observer"], "observed by the responder whose key it verified under")
}

func composedEntry(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	for _, raw := range result["extensions"].([]any) {
		if e := raw.(map[string]any); e["kind"] == "composed/v1" {
			return e["composed"].(map[string]any)
		}
	}
	t.Fatal("no composed/v1 entry")
	return nil
}

// compose takes recorded answers only: a member whose request was never
// recorded here, a request still pending, and response bytes other than the
// ones recorded are each refused.
func TestComposeTakesRecordedAnswersOnly(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	buyer := thisDealParty(t)
	own, _ := pinnedCopy(t, buyer, buyer, id)
	again := askFor(t, buyer, buyer, id, map[string]any{"min_freshness": map[string]any{"size": 1}})

	unrecorded := askedCopy{recordID: strings.Repeat("ab", 32), responsePath: own.responsePath}
	_, err := composeMembers(t, id, own, unrecorded)
	assert.ErrorContains(t, err, "has no recorded request")

	party(t, buyer.config)
	pendingPath := filepath.Join(t.TempDir(), "pending.json")
	pending := runJSON(t, "request", "--request", writeJSONFile(t, "ask.json", map[string]any{
		"subject": map[string]any{"kind": "correlation", "value": id}, "coverage": map[string]any{"min_freshness": map[string]any{"size": 1}},
	}), "--responder", "nobody yet", "--output", pendingPath)
	_, err = composeMembers(t, id, own, askedCopy{recordID: pending["record_id"].(string), responsePath: own.responsePath})
	assert.ErrorContains(t, err, "has no recorded outcome")

	swapped := again
	swapped.responsePath = own.responsePath
	_, err = composeMembers(t, id, own, swapped)
	assert.ErrorContains(t, err, "is not the response recorded for this request")

	_, err = invoke(t, "", "--profile", "deal", "compose", "--deal", id, "--output", filepath.Join(t.TempDir(), "x.json"), "--member", own.recordID+"="+own.responsePath)
	assert.ErrorContains(t, err, "at least two")

	path, err := composeMembers(t, id, own, again)
	require.NoError(t, err, path)
}

// A response is recorded only for the request it answers, under the pinned
// key: an answer to another request, or one checked under another key, is
// refused and never becomes the outcome.
func TestDealResponseIsRecordedOnlyForItsRequestUnderThePinnedKey(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	buyer := thisDealParty(t)
	first := askFor(t, buyer, buyer, id, map[string]any{"min_freshness": map[string]any{"size": 1}})

	party(t, buyer.config)
	second := runJSON(t, "request", "--request", writeJSONFile(t, "ask.json", map[string]any{
		"subject": map[string]any{"kind": "correlation", "value": id}, "coverage": map[string]any{"min_freshness": map[string]any{"size": 1}},
	}), "--responder", "me", "--output", filepath.Join(t.TempDir(), "r.json"))
	_, err := invoke(t, "", "--profile", "deal", "request", "--response", first.responsePath, "--for", second["record_id"].(string),
		"--responder-key", buyer.signer, "--responder-checkpoint-key", buyer.checkpoint)
	assert.ErrorContains(t, err, "not for this request")

	other := strings.Repeat("11", 32)
	_, err = invoke(t, "", "--profile", "deal", "request", "--response", first.responsePath, "--for", first.recordID,
		"--responder-key", other, "--responder-checkpoint-key", buyer.checkpoint)
	assert.ErrorContains(t, err, "already recorded", "the first answer is the outcome; nothing replaces it")
}

// An answer whose copy is anchored under a checkpoint key other than the
// pinned one is genuinely signed, so it is recorded, but as a failed
// artifact, never as a grant.
func TestADealCopyUnderAnotherCheckpointKeyIsRecordedFailed(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	buyer := thisDealParty(t)
	wrong := buyer
	wrong.checkpoint = buyer.signer
	got := askFor(t, buyer, wrong, id, map[string]any{"min_freshness": map[string]any{"size": 1}})
	assert.Equal(t, "artifact_failed_verification", got.outcome)
}

// respond refuses, signed, what it cannot serve: a deal this profile does
// not hold, a subject that names no deal, a request without exactly one
// coverage member, a derivation, and a field it does not define.
func TestDealRespondRefusesWhatItCannotServe(t *testing.T) {
	dealFixture(t)
	held := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	for name, c := range map[string]struct {
		request map[string]any
		reason  string
	}{
		"a deal not held": {map[string]any{"subject": map[string]any{"kind": "correlation", "value": "deal-0000000000000000"},
			"coverage": map[string]any{"min_freshness": map[string]any{"size": 1}}}, "no_such_subject"},
		"not a deal subject": {map[string]any{"subject": map[string]any{"kind": "range", "first": 1, "last": 1},
			"coverage": map[string]any{"min_freshness": map[string]any{"size": 1}}}, "no_such_subject"},
		"a held deal with no coverage": {map[string]any{"subject": map[string]any{"kind": "correlation", "value": held}, "coverage": map[string]any{}}, "coverage_unsatisfiable"},
		"a held deal with a derivation": {map[string]any{"subject": map[string]any{"kind": "correlation", "value": held},
			"coverage": map[string]any{"min_freshness": map[string]any{"size": 1}}, "derivation": "history_card/1"}, "derivation_unsupported"},
		"unknown field": {map[string]any{"subject": map[string]any{"kind": "correlation", "value": "deal-0000000000000000"},
			"coverage": map[string]any{"min_freshness": map[string]any{"size": 1}}, "extra": true}, "request_malformed"},
	} {
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "response.json")
			result := runJSON(t, "respond", "--request", writeJSONFile(t, "req.json", c.request), "--requester", "x", "--output", out)
			assert.Equal(t, "refusal", result["outcome"])
			assert.Equal(t, c.reason, result["reason"])
		})
	}
}

// dealReportCount is how many reports the profile's store has sealed for a
// deal.
func dealReportCount(t *testing.T, id string) int {
	t.Helper()
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	ids, err := s.reportIDs(t.Context(), id)
	require.NoError(t, err)
	return len(ids)
}

// dealDisclosureRecords are the disclosure records the store holds for a
// deal, in order.
func dealDisclosureRecords(t *testing.T, id string) []map[string]any {
	t.Helper()
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	rows, err := s.db.QueryContext(t.Context(), `SELECT record FROM deal_disclosures WHERE deal_id=? ORDER BY n`, id)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var out []map[string]any
	for rows.Next() {
		var raw string
		require.NoError(t, rows.Scan(&raw))
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &m))
		out = append(out, m)
	}
	return out
}

// A copy served as an answer is a share on record like any other, and its
// disclosure record names the request it answered and the exact artifact.
func TestAServedCopyIsAShareNamingItsRequest(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	buyer := thisDealParty(t)
	got := askFor(t, buyer, buyer, id, map[string]any{"min_freshness": map[string]any{"size": 1}})
	records := dealDisclosureRecords(t, id)
	require.Len(t, records, 1)
	assert.Equal(t, digestOfFile(t, got.requestPath), records[0]["request_digest"])
	assert.Equal(t, got.response["artifact"].(map[string]any)["artifact_digest"], records[0]["receipt_sha256"])
	assert.Equal(t, dealAudienceCounterparty, records[0]["audience"])
	assert.Equal(t, "the asking party", records[0]["recipient"])
}

// A deal profile asks only for a deal copy, of a deal it holds.
func TestADealProfileAsksOnlyForADealItHolds(t *testing.T) {
	dealFixture(t)
	dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))
	for name, c := range map[string]struct {
		subject map[string]any
		refusal string
	}{
		"not a deal subject": {map[string]any{"kind": "correlation", "value": "order-1"}, "asks for a deal's copy"},
		"a deal not held":    {map[string]any{"kind": "correlation", "value": "deal-0000000000000000"}, "unknown deal"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := invoke(t, "", "--profile", "deal", "request", "--request", writeJSONFile(t, "ask.json", map[string]any{
				"subject": c.subject, "coverage": map[string]any{"min_freshness": map[string]any{"size": 1}},
			}), "--responder", "x", "--output", filepath.Join(t.TempDir(), "r.json"))
			assert.ErrorContains(t, err, c.refusal)
		})
	}
}

// Nothing came back: the asker records an absence once its window closes,
// and the absence composes as an absence member with the real request
// digest, observed by the composer.
func TestADealRequestWithNoAnswerComposesAsAnAbsence(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	buyer := thisDealParty(t)
	own, _ := pinnedCopy(t, buyer, buyer, id)

	party(t, buyer.config)
	requestPath := filepath.Join(t.TempDir(), "request.json")
	sent := runJSON(t, "request", "--request", writeJSONFile(t, "ask.json", map[string]any{
		"subject": map[string]any{"kind": "correlation", "value": id}, "coverage": map[string]any{"min_freshness": map[string]any{"size": 1}},
	}), "--responder", "silent party", "--output", requestPath)
	asked := dealClock()
	_, err := invoke(t, "", "--profile", "deal", "request", "--absent-until", asked.Add(2*time.Minute).Format(time.RFC3339), "--for", sent["record_id"].(string))
	assert.ErrorContains(t, err, "pending", "a window that has not closed is not an absence")
	old := dealClock
	dealClock = func() time.Time { return asked.Add(time.Hour) }
	t.Cleanup(func() { dealClock = old })
	absent := runJSON(t, "request", "--absent-until", asked.Add(2*time.Minute).Format(time.RFC3339), "--for", sent["record_id"].(string))
	assert.Equal(t, "recorded_absence", absent["outcome"])

	out := filepath.Join(t.TempDir(), "composed.json")
	_, err = invoke(t, "", "--profile", "deal", "compose", "--deal", id, "--output", out,
		"--member", own.recordID+"="+own.responsePath, "--member", sent["record_id"].(string))
	require.NoError(t, err)
	member := memberByID(t, composedBlock(t, out), sent["record_id"].(string))
	assert.Equal(t, "absence", member["outcome"])
	assert.Equal(t, digestOfFile(t, requestPath), member["request_digest"])
	assert.Equal(t, buyer.signer, member["observer"])
	result, err := verifyWithDirectory(t, out, "")
	require.NoError(t, err)
	assert.Equal(t, "VALID", result["verdict"], result)
}

// resigned is a copy of an artifact response, edited by edit and signed
// again under this profile's own signing key: a genuine signature over an
// answer that does not hold up.
func resigned(t *testing.T, response map[string]any, edit func(a *evidencebook.ArtifactResponse, bundle map[string]any)) string {
	t.Helper()
	raw, err := json.Marshal(response["artifact"])
	require.NoError(t, err)
	var a evidencebook.ArtifactResponse
	require.NoError(t, json.Unmarshal(raw, &a))
	bundle := exactBundle(t, string(a.Artifact))
	edit(&a, bundle)
	generic, err := genericJSON(bundle)
	require.NoError(t, err)
	a.Artifact, err = canonical.JCS(generic)
	require.NoError(t, err)
	a.ArtifactDigest = bytesDigest(a.Artifact)
	p, err := loadProfile("deal")
	require.NoError(t, err)
	key, err := privateKey(p.Signing)
	require.NoError(t, err)
	body, err := a.SigningBody()
	require.NoError(t, err)
	a.Sig = hex.EncodeToString(ed25519.Sign(key, body))
	encoded, err := json.Marshal(evidencebook.Response{Artifact: &a})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "resigned.json")
	require.NoError(t, os.WriteFile(path, encoded, 0o600))
	return path
}

// A genuinely signed answer is recorded as a failed artifact, never as a
// grant, when its copy does not verify VALID or is a copy of another deal.
func TestASignedAnswerThatDoesNotHoldUpIsRecordedFailed(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	other := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	buyer := thisDealParty(t)
	served := askFor(t, buyer, buyer, id, map[string]any{"min_freshness": map[string]any{"size": 1}})
	otherCopy := askFor(t, buyer, buyer, other, map[string]any{"min_freshness": map[string]any{"size": 1}})

	ask := func() (string, string) {
		party(t, buyer.config)
		path := filepath.Join(t.TempDir(), "request.json")
		sent := runJSON(t, "request", "--request", writeJSONFile(t, "ask.json", map[string]any{
			"subject": map[string]any{"kind": "correlation", "value": id}, "coverage": map[string]any{"min_freshness": map[string]any{"size": 1}},
		}), "--responder", "me", "--output", path)
		return sent["record_id"].(string), sent["request_digest"].(string)
	}
	record := func(recordID, path string) map[string]any {
		return runJSON(t, "request", "--response", path, "--for", recordID, "--responder-key", buyer.signer, "--responder-checkpoint-key", buyer.checkpoint)
	}

	recordID, digest := ask()
	edited := resigned(t, served.response, func(a *evidencebook.ArtifactResponse, bundle map[string]any) {
		a.RequestDigest = digest
		bundle["root"] = strings.Repeat("0", 64)
	})
	assert.Equal(t, "artifact_failed_verification", record(recordID, edited)["outcome"], "a copy that does not verify VALID")

	recordID, digest = ask()
	elsewhere := resigned(t, otherCopy.response, func(a *evidencebook.ArtifactResponse, _ map[string]any) { a.RequestDigest = digest })
	assert.Equal(t, "artifact_failed_verification", record(recordID, elsewhere)["outcome"], "a copy of another deal")
}
