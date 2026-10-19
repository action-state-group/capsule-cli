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

// A close whose amount equals the buyer's spending limit is a structured
// field, shared by its field's rules (Ethan's decision on capsule-cli#278):
// with the agreed price 1900.00, a limit of 2000.00 and both parties closing
// at 2000.00, each party's counterparty file carries its openings, each
// party's Result names both halves, and every check of either Result is
// equal: the seller's --sent check of the buyer's, and the adjudicator's
// file-mode check of both. The adjudicator's file carries them too, as
// before.
func TestACloseAtTheLimitReconcilesForEveryone(t *testing.T) {
	limit := strings.Replace(crossPartyBuyerOpen, `"max_total_minor":190000`, `"max_total_minor":200000`, 1)
	atLimit := `{"status":"received","delivered":{"price_minor":200000}}`
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, limit))["deal_id"].(string)
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, atLimit))
	buyer := thisDealParty(t)
	dealFixture(t)
	dealRun(t, "open", "--records", "typed", "--deal-id", id, "--input", writeJSON(t, sellerTyped))
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, atLimit))
	seller := thisDealParty(t)

	// The buyer's Result, from the seller's counterparty file.
	buyerResponse := recordedCopy(t, buyer, seller, id)
	party(t, seller.config)
	sellerCounterparty := discloseTo(t, id, "the buyer")
	sellerAdjudicator := adjudicatorFile(t, id)
	party(t, buyer.config)
	_, buyerDoc, buyerResult, err := runTermsResult(t, id, buyerResponse, "--openings", sellerCounterparty)
	require.NoError(t, err)
	require.Len(t, buyerDoc.Claims[0].Evidence, 2)
	buyerCounterparty, note := discloseAs(t, id, "counterparty", "the seller")
	require.True(t, strings.HasPrefix(note, "carried:"), "an amount equal to the limit is carried: %s", note)
	raw := string(mustRead(t, buyerCounterparty))
	assert.Contains(t, raw, "field_openings")
	assert.NotContains(t, raw, "max_total_minor", "the bound field itself is never in a counterparty copy")
	buyerAdjudicator, note := discloseAs(t, id, "adjudicator", "the platform")
	require.True(t, strings.HasPrefix(note, "carried:"), note)

	// The seller's Result, from the buyer's counterparty file: two halves.
	sellerResponse := recordedCopy(t, seller, buyer, id)
	_, sellerDoc, sellerResult, err := runTermsResult(t, id, sellerResponse, "--openings", buyerCounterparty)
	require.NoError(t, err)
	require.Len(t, sellerDoc.Claims[0].Evidence, 2)
	claims := claimsOf(sellerDoc)
	assert.Equal(t, evidencebook.ResultTallies{Matched: 1}, claims[amountReq].Reconcile.Tallies)

	check, err := runTermsCheck(t, id, sellerResponse, buyerResult, "--openings", buyerCounterparty, "--sent", sellerCounterparty)
	require.NoError(t, err, check)
	assert.Equal(t, termsCheckEqual, check["check"])

	keys := []string{"--party-key", buyer.signer + ":" + buyer.checkpoint, "--party-key", seller.signer + ":" + seller.checkpoint}
	files := []string{"--copy", buyerAdjudicator, "--copy", sellerAdjudicator}
	for _, result := range []string{buyerResult, sellerResult} {
		out, err := asAdjudicator(t, append(append([]string{"--check", result}, files...), keys...)...)
		require.NoError(t, err, out)
		assert.Equal(t, termsCheckEqual, out["check"])
	}
}

// The trust boundary and the flag contract of the file mode.
func TestTheAdjudicatorsCheckRefuses(t *testing.T) {
	a := adjudicate(t, received, received)
	other := adjudicate(t, received, received)
	party(t, a.pair.buyer.config)
	wrongSigner := other.pair.buyer.signer + ":" + a.pair.buyer.checkpoint
	crossedA := a.pair.buyer.signer + ":" + a.pair.seller.checkpoint
	crossedB := a.pair.seller.signer + ":" + a.pair.buyer.checkpoint
	var b map[string]any
	require.NoError(t, json.Unmarshal(mustRead(t, a.buyerFile), &b))
	require.True(t, flipSignature(b), "the copy has a signature to flip")
	invalid := writeJSONFile(t, "invalid.json", b)
	for name, args := range map[string][]string{
		"a copy that does not verify":   {"--check", a.result, "--copy", invalid, "--copy", a.sellerFile, "--party-key", a.buyerKeys, "--party-key", a.sellerKeys},
		"a checkpoint signed by no key": {"--check", a.result, "--copy", a.buyerFile, "--copy", a.sellerFile, "--party-key", a.buyerKeys, "--party-key", other.sellerKeys},
		"no --copy":                     {"--check", a.result, "--party-key", a.buyerKeys, "--party-key", a.sellerKeys},
		"with --share":                  a.args("--share", "counterparty"),
		"with --recipient":              a.args("--recipient", "the platform"),
		"with --record":                 a.args("--record", strings.Repeat("0", 64)),
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
	// Each guard refuses for its own reason.
	for copyPath, guard := range map[string]string{invalid: "not VALID", a.sellerFile: "signed by no --party-key's checkpoint key"} {
		_, err := asAdjudicator(t, "--check", a.result, "--copy", copyPath, "--copy", a.buyerFile, "--party-key", a.buyerKeys, "--party-key", other.sellerKeys)
		assert.ErrorContains(t, err, guard)
	}
}

// flipSignature changes one character of the first signature in a decoded
// copy, so the copy no longer verifies.
func flipSignature(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		if sig, ok := t["signature"].(string); ok && sig != "" {
			flip := byte('0')
			if sig[0] == '0' {
				flip = '1'
			}
			t["signature"] = string(flip) + sig[1:]
			return true
		}
		for _, child := range t {
			if flipSignature(child) {
				return true
			}
		}
	case []any:
		for _, child := range t {
			if flipSignature(child) {
				return true
			}
		}
	}
	return false
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
	evidenceOf := func(digest string) string {
		var changed map[string]any
		require.NoError(t, json.Unmarshal(mustRead(t, a.result), &changed))
		for _, c := range changed["claims"].([]any) {
			c.(map[string]any)["evidence"] = []any{map[string]any{"digest": digest}}
		}
		return writeJSONFile(t, "one-half.json", changed)
	}
	ownHalf, err := soleDigest(mustOpenings(t, a.buyerFile))
	require.NoError(t, err)
	peerHalf, err := soleDigest(mustOpenings(t, a.sellerFile))
	require.NoError(t, err)
	for digest, omits := range map[string]bool{ownHalf: true, peerHalf: false, strings.Repeat("d", 64): false} {
		out, err := asAdjudicator(t, "--check", evidenceOf(digest), "--copy", a.buyerFile, "--copy", a.sellerFile, "--party-key", a.buyerKeys, "--party-key", a.sellerKeys)
		require.ErrorIs(t, err, cli.ErrConflict)
		assert.Equal(t, termsCheckDiffers, out["check"])
		detail, _ := out["detail"].(string)
		assert.Equal(t, omits, strings.Contains(detail, "omits the other party's half"), "only the producer's own lone half reads as omitted: %s", detail)
		assert.Contains(t, detail, a.sellerFile, "the outcome names the files")
	}
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

// mustOpenings are the field_openings of a disclose file.
func mustOpenings(t *testing.T, path string) map[string]map[string]evidencebook.Opening {
	t.Helper()
	openings, err := dealOpeningsFile(mustRead(t, path))
	require.NoError(t, err)
	return openings
}
