package plugin

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/action-state-group/evidencebook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// adjudication is a pair's files as an adjudicator holds them: the buyer's
// Result and each party's own adjudicator file, with the keys pinned for
// each party.
type adjudication struct {
	pair                  reconcilePair
	result                string
	buyerFile, sellerFile string
	buyerKeys, sellerKeys string
}

// adjudicate has the buyer make its Result from the seller's counterparty
// file, then each party disclose its own copy to a named adjudicator.
func adjudicate(t *testing.T, buyerClose, sellerClose string) adjudication {
	t.Helper()
	pair := closedPair(t, buyerClose, sellerClose)
	_, _, result, err := runTermsResult(t, pair.id, pair.response, "--openings", pair.sellerFile)
	require.NoError(t, err)
	a := adjudication{pair: pair, result: result}
	a.buyerFile = adjudicatorFile(t, pair.id)
	party(t, pair.seller.config)
	a.sellerFile = adjudicatorFile(t, pair.id)
	party(t, pair.buyer.config)
	a.buyerKeys = pair.buyer.signer + ":" + pair.buyer.checkpoint
	a.sellerKeys = pair.seller.signer + ":" + pair.seller.checkpoint
	return a
}

// asAdjudicator runs the file-mode check from a configuration with no deal
// profile or store.
func asAdjudicator(t *testing.T, args ...string) (map[string]any, error) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	text, err := invoke(t, "", append([]string{"deal", "terms-result"}, args...)...)
	var out map[string]any
	if json.Unmarshal([]byte(text), &out) != nil {
		out = map[string]any{"text": text}
	}
	return out, err
}

func (a adjudication) args(extra ...string) []string {
	return append([]string{"--check", a.result, "--copy", a.buyerFile, "--copy", a.sellerFile, "--party-key", a.buyerKeys, "--party-key", a.sellerKeys}, extra...)
}

// #278's acceptance: an adjudicator holding each party's named-adjudicator
// file recomputes the buyer's Result, with no deal store, and it is equal.
// The keys are paired with the files by their checkpoint key and signer,
// never by order.
func TestAnAdjudicatorRecomputesTheTermsResult(t *testing.T) {
	a := adjudicate(t, received, received)
	out, err := asAdjudicator(t, a.args()...)
	require.NoError(t, err, out)
	assert.Equal(t, termsCheckEqual, out["check"])
	swapped := []string{"--check", a.result, "--copy", a.sellerFile, "--copy", a.buyerFile, "--party-key", a.buyerKeys, "--party-key", a.sellerKeys}
	out, err = asAdjudicator(t, swapped...)
	require.NoError(t, err, out)
	assert.Equal(t, termsCheckEqual, out["check"], "flags are not paired by order")
}

// The dispute case: the parties closed on different amounts. The buyer's
// Result has amount CONFLICTING, and the adjudicator's check of it is equal.
func TestAnAdjudicatorChecksADispute(t *testing.T) {
	a := adjudicate(t, received, `{"status":"received","delivered":{"price_minor":185000}}`)
	var result evidencebook.DealReconcileResult
	require.NoError(t, json.Unmarshal(mustRead(t, a.result), &result))
	claims := claimsOf(result)
	require.Equal(t, evidencebook.ResultTallies{Conflicting: 1}, claims[amountReq].Reconcile.Tallies)
	require.Equal(t, "met", claims[currencyReq].Verdict)
	out, err := asAdjudicator(t, a.args()...)
	require.NoError(t, err, out)
	assert.Equal(t, termsCheckEqual, out["check"])
}

// An adjudicator file carries its party's openings for the final close,
// says so, and is on record; a respond copy carries none whoever asks, and
// the core verifier accepts the file.
func TestTheAdjudicatorShareCarriesItsPartysOpenings(t *testing.T) {
	pair := closedPair(t, received, received)
	path, note := discloseAs(t, pair.id, "adjudicator", "the platform")
	assert.True(t, strings.HasPrefix(note, "carried:"), note)
	openings, err := dealOpeningsFile(mustRead(t, path))
	require.NoError(t, err)
	require.Len(t, openings, 1)
	withSession(t, func(s *dealSession) {
		rows, err := s.db.QueryContext(t.Context(), `SELECT record FROM deal_disclosures WHERE deal_id=?`, pair.id)
		require.NoError(t, err)
		defer func() { require.NoError(t, rows.Close()) }()
		found := false
		for rows.Next() {
			var raw string
			require.NoError(t, rows.Scan(&raw))
			var record map[string]any
			require.NoError(t, json.Unmarshal([]byte(raw), &record))
			if record["audience"] == cli.DealAudienceAdjudicator && record["receipt_sha256"] == digestOfFile(t, path) {
				found = record["recipient"] == "the platform"
			}
		}
		require.NoError(t, rows.Err())
		assert.True(t, found, "the release is on record: recipient, audience and the exact file digest")
	})
	result, err := verifyWithDirectory(t, path, "")
	require.NoError(t, err)
	assert.Equal(t, "VALID", result["verdict"])

	ask := askFor(t, pair.buyer, pair.seller, pair.id, map[string]any{"min_freshness": map[string]any{"size": 1}})
	assert.NotContains(t, string(mustRead(t, ask.responsePath)), "field_openings", "a respond copy carries none, whoever asks")

	dealFixture(t)
	open := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	_, note = discloseAs(t, open, "adjudicator", "the platform")
	assert.Equal(t, "none: the deal has no final close yet", note)
}

// discloseAs writes the selected party's disclose file for an audience.
func discloseAs(t *testing.T, id, audience, to string) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), audience+".json")
	out, err := invoke(t, "", "--profile", "deal", "disclose", "--deal", id, "--share", audience, "--to", to, "--out", path)
	require.NoError(t, err, out)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m), out)
	note, _ := m["field_openings"].(string)
	return path, note
}

// The ceiling gate is per audience: a received close whose amount is the
// buyer's own spending limit, shown nowhere else, is kept from the
// counterparty and carried to a named adjudicator, whose copy may already
// carry that limit.
func TestTheGateIsPerAudience(t *testing.T) {
	dealFixture(t)
	open := strings.Replace(crossPartyBuyerOpen, `"max_total_minor":190000`, `"max_total_minor":200000`, 1)
	id := dealRun(t, "open", "--input", writeJSON(t, open))["deal_id"].(string)
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, `{"status":"received","delivered":{"price_minor":200000}}`))
	_, note := discloseAs(t, id, "counterparty", "the seller")
	assert.Contains(t, note, "none: the amount, written as money, is one this share keeps from its reader")
	path, note := discloseAs(t, id, "adjudicator", "the platform")
	assert.True(t, strings.HasPrefix(note, "carried:"), note)
	assert.Contains(t, string(mustRead(t, path)), "field_openings")
}

// The gate's exception, end to end: a buyer at its limit hands the seller a
// counterparty file with no openings, and the adjudicator a file with them.
// The seller's Result has one half and differs in file mode on its
// evidence, without calling the claim false; the buyer's Result is equal in
// file mode, and the seller's own --sent check of it cannot reproduce.
func TestTheGateExceptionAcrossChecks(t *testing.T) {
	limit := strings.Replace(crossPartyBuyerOpen, `"max_total_minor":190000`, `"max_total_minor":185000`, 1)
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, limit))["deal_id"].(string)
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, `{"status":"received","delivered":{"price_minor":185000}}`))
	buyer := thisDealParty(t)
	dealFixture(t)
	dealRun(t, "open", "--records", "typed", "--deal-id", id, "--input", writeJSON(t, sellerTyped))
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, `{"status":"received","delivered":{"price_minor":185000}}`))
	seller := thisDealParty(t)

	// The buyer's Result, from the seller's counterparty file.
	buyerResponse := recordedCopy(t, buyer, seller, id)
	party(t, seller.config)
	sellerCounterparty := discloseTo(t, id, "the buyer")
	sellerAdjudicator := adjudicatorFile(t, id)
	party(t, buyer.config)
	_, _, buyerResult, err := runTermsResult(t, id, buyerResponse, "--openings", sellerCounterparty)
	require.NoError(t, err)
	buyerCounterparty, note := discloseAs(t, id, "counterparty", "the seller")
	require.Contains(t, note, "none:", "kept back at the buyer's limit")
	buyerAdjudicator := adjudicatorFile(t, id)

	// The seller's Result, from the buyer's counterparty file: one half.
	sellerResponse := recordedCopy(t, seller, buyer, id)
	_, sellerResultDoc, sellerResult, err := runTermsResult(t, id, sellerResponse, "--openings", buyerCounterparty)
	require.NoError(t, err)
	require.Len(t, sellerResultDoc.Claims[0].Evidence, 1)

	keys := []string{"--party-key", buyer.signer + ":" + buyer.checkpoint, "--party-key", seller.signer + ":" + seller.checkpoint}
	files := []string{"--copy", buyerAdjudicator, "--copy", sellerAdjudicator}
	out, err := asAdjudicator(t, append(append([]string{"--check", sellerResult}, files...), keys...)...)
	require.ErrorIs(t, err, cli.ErrConflict)
	assert.Equal(t, termsCheckDiffers, out["check"])
	assert.Contains(t, out["detail"], "omits the other party's half supplied here")
	out, err = asAdjudicator(t, append(append([]string{"--check", buyerResult}, files...), keys...)...)
	require.NoError(t, err, out)
	assert.Equal(t, termsCheckEqual, out["check"])

	party(t, seller.config)
	check, err := runTermsCheck(t, id, sellerResponse, buyerResult, "--openings", buyerCounterparty, "--sent", sellerCounterparty)
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Equal(t, termsCheckCannotReproduce, check["check"])
}

// The trust boundary and the flag contract of the file mode.
func TestTheAdjudicatorsCheckRefuses(t *testing.T) {
	a := adjudicate(t, received, received)
	other := adjudicate(t, received, received)
	party(t, a.pair.buyer.config)
	wrongSigner := other.pair.buyer.signer + ":" + a.pair.buyer.checkpoint
	crossedA := a.pair.buyer.signer + ":" + a.pair.seller.checkpoint
	crossedB := a.pair.seller.signer + ":" + a.pair.buyer.checkpoint
	for name, args := range map[string][]string{
		"copies of different deals":     {"--check", a.result, "--copy", a.buyerFile, "--copy", other.sellerFile, "--party-key", a.buyerKeys, "--party-key", other.sellerKeys},
		"one party's file twice":        {"--check", a.result, "--copy", a.buyerFile, "--copy", a.buyerFile, "--party-key", a.buyerKeys, "--party-key", a.sellerKeys},
		"a wrong pinned signing key":    {"--check", a.result, "--copy", a.buyerFile, "--copy", a.sellerFile, "--party-key", wrongSigner, "--party-key", a.sellerKeys},
		"crossed signer and checkpoint": {"--check", a.result, "--copy", a.buyerFile, "--copy", a.sellerFile, "--party-key", crossedA, "--party-key", crossedB},
		"three copies":                  {"--check", a.result, "--copy", a.buyerFile, "--copy", a.sellerFile, "--copy", a.sellerFile, "--party-key", a.buyerKeys, "--party-key", a.sellerKeys},
		"two copies, one key pair":      {"--check", a.result, "--copy", a.buyerFile, "--copy", a.sellerFile, "--party-key", a.buyerKeys},
		"the same key pair twice":       {"--check", a.result, "--copy", a.buyerFile, "--copy", a.sellerFile, "--party-key", a.buyerKeys, "--party-key", a.buyerKeys},
		"without --check":               {"--copy", a.buyerFile, "--copy", a.sellerFile, "--party-key", a.buyerKeys, "--party-key", a.sellerKeys},
		"with --response":               a.args("--response", strings.Repeat("0", 64)),
		"with --deal":                   a.args("--deal", a.pair.id),
		"with --openings":               a.args("--openings", a.sellerFile),
		"with --sent":                   a.args("--sent", a.sellerFile),
		"with --out":                    a.args("--out", filepath.Join(t.TempDir(), "x.json")),
		"with --from":                   a.args("--from", "2026-01-01T00:00:00Z"),
		"with --to":                     a.args("--to", "2026-01-01T00:00:00Z"),
	} {
		t.Run(name, func(t *testing.T) {
			out, err := asAdjudicator(t, args...)
			require.ErrorIs(t, err, cli.ErrInput, out)
			assert.NotEqual(t, termsCheckCannotReproduce, out["check"], "refused before any outcome")
		})
	}
}

// The outcomes from what the files carry: one file, or a missing field, is
// cannot_reproduce; a tampered opening reproduces UNRESOLVED and differs
// against an honest Result; openings naming two records, or a digest of no
// record, are refused; a changed tally, a peer that is neither party, or an
// evidence digest that is no half, differ.
func TestTheAdjudicatorsOutcomes(t *testing.T) {
	a := adjudicate(t, received, received)
	out, err := asAdjudicator(t, "--check", a.result, "--copy", a.buyerFile, "--party-key", a.buyerKeys, "--party-key", a.sellerKeys)
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Equal(t, termsCheckCannotReproduce, out["check"], "one side cannot be checked alone")

	edited := func(path string, edit func(map[string]any)) string {
		var b map[string]any
		require.NoError(t, json.Unmarshal(mustRead(t, path), &b))
		edit(b["field_openings"].(map[string]any))
		return writeJSONFile(t, "edited.json", b)
	}
	noAmount := edited(a.sellerFile, func(m map[string]any) {
		for _, v := range m {
			delete(v.(map[string]any), dealAmountField)
		}
	})
	out, err = asAdjudicator(t, "--check", a.result, "--copy", a.buyerFile, "--copy", noAmount, "--party-key", a.buyerKeys, "--party-key", a.sellerKeys)
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Equal(t, termsCheckCannotReproduce, out["check"])

	tampered := edited(a.sellerFile, func(m map[string]any) {
		for _, v := range m {
			v.(map[string]any)[dealAmountField].(map[string]any)["text"] = "1"
		}
	})
	out, err = asAdjudicator(t, "--check", a.result, "--copy", a.buyerFile, "--copy", tampered, "--party-key", a.buyerKeys, "--party-key", a.sellerKeys)
	require.ErrorIs(t, err, cli.ErrConflict)
	assert.Equal(t, termsCheckDiffers, out["check"], "the tampered opening recomputes UNRESOLVED, never CONFLICTING")

	twoRecords := edited(a.sellerFile, func(m map[string]any) {
		for _, v := range m {
			m[strings.Repeat("e", 64)] = v
		}
	})
	_, err = asAdjudicator(t, "--check", a.result, "--copy", a.buyerFile, "--copy", twoRecords, "--party-key", a.buyerKeys, "--party-key", a.sellerKeys)
	require.ErrorIs(t, err, cli.ErrInput)
	noRecord := edited(a.sellerFile, func(m map[string]any) {
		for k, v := range m {
			delete(m, k)
			m[strings.Repeat("f", 64)] = v
		}
	})
	_, err = asAdjudicator(t, "--check", a.result, "--copy", a.buyerFile, "--copy", noRecord, "--party-key", a.buyerKeys, "--party-key", a.sellerKeys)
	require.ErrorIs(t, err, cli.ErrInput)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(mustRead(t, a.result), &doc))
	for name, change := range map[string]func(map[string]any){
		"a tally":                      func(c map[string]any) { c["reconcile"].(map[string]any)["tallies"].(map[string]any)["matched"] = 2 },
		"a peer that is neither party": func(c map[string]any) { c["reconcile"].(map[string]any)["peer"] = strings.Repeat("c", 64) },
		"an evidence digest": func(c map[string]any) {
			c["evidence"].([]any)[0].(map[string]any)["digest"] = strings.Repeat("d", 64)
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
			path := writeJSONFile(t, "changed.json", changed)
			out, err := asAdjudicator(t, "--check", path, "--copy", a.buyerFile, "--copy", a.sellerFile, "--party-key", a.buyerKeys, "--party-key", a.sellerKeys)
			require.ErrorIs(t, err, cli.ErrConflict)
			assert.Equal(t, termsCheckDiffers, out["check"])
		})
	}
}
