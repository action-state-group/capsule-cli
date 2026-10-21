package plugin

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sellerLaterFixture is a seller's sale that closes with the delivery still
// due, and then takes later evidence (Seller PRD §5.4, exit criterion 9).
// scripts/presentation-goldens/build-fixtures.sh builds presentation golden
// 9-seller-later-evidence from the same files.
const sellerLaterFixture = "testdata/deal/seller-later-evidence"

func sellerLaterFile(name string) string { return filepath.Join(sellerLaterFixture, name) }

// sellerLaterDeal is the fixture's sale up to its close: the agent's
// statements, an offer the buyer accepts, the commit, the delivery due date,
// and a close that carries it. It returns the deal, the close's capsule id
// and the due date's step.
func sellerLaterDeal(t *testing.T) (dealID, closeID string, dueStep int64) {
	t.Helper()
	setDealClock(t, "2026-09-28T10:00:00Z")
	dealID = dealRun(t, "open", "--records", "typed", "--input", sellerLaterFile("open.json"))["deal_id"].(string)
	note := func(args ...string) map[string]any {
		return dealRun(t, append([]string{"note", "--deal", dealID}, args...)...)
	}
	note("--kind", "claim", "--input", sellerLaterFile("claim-condition.json"))
	note("--kind", "claim", "--input", sellerLaterFile("claim-refund-terms.json"))
	offer := approved(t, dealID, dealRun(t, "check", "--deal", dealID, "--input", sellerLaterFile("check-offer.json")))
	require.Equal(t, false, note("--kind", "act", "--input", sellerLaterFile("act-offer.json"))["unchecked"])
	note("--kind", "acceptance", "--check", offer, "--words", "Deal, 1900 works for me", "--channel", "marketplace")
	approved(t, dealID, dealRun(t, "check", "--deal", dealID, "--input", sellerLaterFile("check-commit.json")))
	require.Equal(t, false, note("--kind", "act", "--input", sellerLaterFile("act-commit.json"))["unchecked"])
	due := note("--kind", "evidence", "--input", sellerLaterFile("deliver-by.json"))
	dueStep = int64(due["step"].(float64))
	setDealClock(t, "2026-09-29T09:00:00Z")
	closeID = dealRun(t, "close", "--deal", dealID, "--carry-open-obligations", "--input", sellerLaterFile("close.json"))["capsule_id"].(string)
	return dealID, closeID, dueStep
}

// sellerLaterEvidence seals the fixture's two later records: the carrier's
// delivery scan, which resolves the carried due date, and the buyer's refund
// request, a claim and so unverified.
func sellerLaterEvidence(t *testing.T, dealID string, dueStep int64) (delivered, refund string) {
	t.Helper()
	setDealClock(t, "2026-10-02T15:00:00Z")
	delivered = dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--input",
		withResolvesStep(t, sellerLaterFile("later-delivered.json"), dueStep))["capsule_id"].(string)
	setDealClock(t, "2026-10-05T11:30:00Z")
	refund = dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--input", sellerLaterFile("later-refund-claim.json"))["capsule_id"].(string)
	return delivered, refund
}

// withResolvesStep is the evidence input at path naming step as the
// obligation it resolves.
func withResolvesStep(t *testing.T, path string, step int64) string {
	t.Helper()
	var in map[string]any
	require.NoError(t, json.Unmarshal(mustRead(t, path), &in))
	in["resolves_step"] = step
	raw, err := json.Marshal(in)
	require.NoError(t, err)
	return writeJSON(t, string(raw))
}

// A seller's refund request after the close chains `confirms` to it, as an
// unverified claim; the carrier's delivery scan chains the same way and
// resolves the due date the close carried. Both verify, and the seller's
// Receipt shows them under the close with their tier (checked, unverified).
func TestASellersLaterEvidenceChainsToTheCloseAndRenders(t *testing.T) {
	dealFixture(t)
	dealID, closeID, dueStep := sellerLaterDeal(t)
	delivered, refund := sellerLaterEvidence(t, dealID, dueStep)

	// Each later record's Capsule chains to the close with `confirms`.
	bundle := filepath.Join(t.TempDir(), "bundle.json")
	report := dealRun(t, "report", "--deal", dealID, "--bundle", bundle)
	chains := bundleChain(t, bundle)
	for _, id := range []string{delivered, refund} {
		require.Contains(t, chains, id)
		assert.Equal(t, "confirms", chains[id]["relation"])
		assert.Equal(t, closeID, chains[id]["parent_capsule_id"])
	}
	out, err := invoke(t, "", "verify", "--bundle", bundle)
	require.NoError(t, err, out)
	assert.Contains(t, out, `"VALID"`)

	// Each record commits to the close record's digest; the scan resolves the
	// due date by its record's digest, and the refund request is a claim.
	records, export := exportRecords(t, dealID)
	byType := map[string][]map[string]any{}
	var closeRecord, dueRecord map[string]any
	for _, r := range records {
		block, ok := r["x-deal-v0"].(map[string]any)
		if !ok {
			continue // a typed record: an offer, an acceptance, an act
		}
		kind := block["record_type"].(string)
		byType[kind] = append(byType[kind], r)
		if kind == "close" {
			closeRecord = r
		}
		if _, ok := r["body"].(map[string]any)["obligation"]; ok {
			dueRecord = r
		}
	}
	require.NotNil(t, closeRecord)
	require.NotNil(t, dueRecord)
	evidence := byType["evidence"]
	require.Len(t, evidence, 3, "the due date and the two later records")
	scan, claim := evidence[1], evidence[2]
	for _, r := range []map[string]any{scan, claim} {
		assert.Equal(t, recordDigest(t, closeRecord), relDigest(r, "confirms"))
	}
	assert.Equal(t, recordDigest(t, dueRecord), scan["body"].(map[string]any)["resolves_obligation"].(map[string]any)["digest"])
	assert.Equal(t, true, scan["body"].(map[string]any)["verified"])
	assert.Equal(t, false, claim["body"].(map[string]any)["verified"], "a claim is unverified")
	assert.NotContains(t, claim["body"], "resolves_obligation", "a claim resolves nothing")
	// The refund request is about the agent's own refund-terms statement.
	var refundTerms map[string]any
	for _, r := range byType["claim"] {
		if r["body"].(map[string]any)["class"] == "refund_terms" {
			refundTerms = r
		}
	}
	require.NotNil(t, refundTerms)
	assert.Equal(t, recordDigest(t, refundTerms), relDigest(claim, "about"))
	checkProfile(t, export)

	// The due date the close carried is resolved.
	all := dealRun(t, "deadlines", "--all")["deadlines"].([]any)
	require.Len(t, all, 1)
	assert.Equal(t, "resolved", all[0].(map[string]any)["status"])

	// The seller's Receipt lists both under the close, in order, each with
	// its tier.
	life := report["lifecycle"].(map[string]any)
	assert.Equal(t, "closed", life["state"])
	later := life["later"].([]any)
	require.Len(t, later, 2)
	first, second := later[0].(map[string]any), later[1].(map[string]any)
	assert.Equal(t, delivered, first["capsule_id"])
	assert.Equal(t, refund, second["capsule_id"])
	assert.Equal(t, "confirms", second["relation"])
	assert.Contains(t, first["text"], "evidence (carrier), checked")
	assert.Contains(t, second["text"], "evidence (buyer), unverified: Returns accepted within 7 days if unridden")
	assert.NotContains(t, first["text"], "confirms it", "the relation is said once, beside the line")
	assert.Contains(t, life["text"], "2 records sealed after the close are linked to it")

	// The page carries the same report.
	page := filepath.Join(t.TempDir(), "page.html")
	dealRun(t, "report", "--deal", dealID, "--html", page)
	html := string(mustRead(t, page))
	assert.Contains(t, html, "evidence (buyer), unverified: Returns accepted within 7 days if unridden")
	assert.Contains(t, html, refund)
}

// After the close only later evidence that confirms it is taken: no message,
// no new obligation, no check, and a resolution must name an obligation.
func TestASellersClosedSaleTakesOnlyLaterEvidence(t *testing.T) {
	dealFixture(t)
	dealID, _, dueStep := sellerLaterDeal(t)
	for name, args := range map[string][]string{
		"a message":         {"--kind", "message", "--input", writeJSON(t, `{"from":"counterparty","text":"where is my refund"}`)},
		"a claim":           {"--kind", "claim", "--input", sellerLaterFile("claim-condition.json")},
		"a new due date":    {"--kind", "evidence", "--input", sellerLaterFile("deliver-by.json")},
		"a step not a date": {"--kind", "evidence", "--input", withResolvesStep(t, sellerLaterFile("later-delivered.json"), dueStep-1)},
		"no such step":      {"--kind", "evidence", "--input", withResolvesStep(t, sellerLaterFile("later-delivered.json"), dueStep+100)},
	} {
		require.ErrorIs(t, dealRunErr(t, append([]string{"note", "--deal", dealID}, args...)...), cli.ErrInput, name)
	}
	require.ErrorIs(t, dealRunErr(t, "check", "--deal", dealID, "--input", sellerLaterFile("check-commit.json")), cli.ErrInput)
	// Nothing was sealed by the refusals.
	records, _ := exportRecords(t, dealID)
	assert.Equal(t, "close", records[len(records)-1]["x-deal-v0"].(map[string]any)["record_type"])
}

// The sale is a book deal, so its close ends it on the cadence; once the
// tick holding its final checkpoint is witnessed it leaves the live set, and
// a confirming append brings it back (capsule-cli#242 note §2.4).
func TestASellersLaterEvidenceBringsTheBookDealBackIntoTheTick(t *testing.T) {
	p, _, witnessKey := rc16Store(t)
	cli.DealClock = func() time.Time { return time.Date(2026, 10, 19, 9, 28, 0, 0, time.UTC) }
	dealRun(t, "tick")
	dealID := dealRun(t, "open", "--records", "typed", "--input", sellerLaterFile("open.json"))["deal_id"].(string)
	dealRun(t, "close", "--deal", dealID, "--input", sellerLaterFile("close.json"))
	require.Contains(t, liveLogs(t, p), cli.DealLogID(dealID))
	cli.DealClock = func() time.Time { return time.Date(2026, 10, 19, 9, 40, 0, 0, time.UTC) }
	tick := dealRun(t, "tick")
	require.Equal(t, "ticked", tick["state"])
	deliver(t, p, uint64(tick["checkpoint"].(float64)), witnessKey)
	require.NotContains(t, liveLogs(t, p), cli.DealLogID(dealID), "the ended, witnessed sale leaves the tick")

	cli.DealClock = func() time.Time { return time.Date(2026, 10, 19, 9, 45, 0, 0, time.UTC) }
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--input", sellerLaterFile("later-refund-claim.json"))
	assert.Contains(t, liveLogs(t, p), cli.DealLogID(dealID), "a confirming append re-enters the live set")
}

// relDigest is the digest of the record's first ref with relation rel, or
// "" when it has none.
func relDigest(r map[string]any, rel string) string {
	refs, _ := r["x-deal-v0"].(map[string]any)["refs"].([]any)
	for _, x := range refs {
		if m := x.(map[string]any); m["rel"] == rel {
			d, _ := m["digest"].(string)
			return d
		}
	}
	return ""
}
