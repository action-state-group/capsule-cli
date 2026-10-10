package cli

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var hex64Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// baselineIDs are the counterparty fingerprints the thread's opening sealed.
func baselineIDs(t *testing.T, id string) map[string]any {
	t.Helper()
	records, _ := exportRecords(t, id)
	for _, r := range records {
		if b, ok := r["x-deal-v0"].(map[string]any); ok && b["record_type"] == "baseline" {
			return b["counterparty"].(map[string]any)["ids"].(map[string]any)
		}
	}
	t.Fatal("no baseline")
	return nil
}

// A check on a sale's thread that names no one is about the buyer the thread
// was opened for: it seals that buyer as its counterparty, keyed per deal as
// a purchase check's counterparty is, and the rules checker is given it, so
// a rule keyed on the counterparty (one commitment per sale) has a target.
func TestASaleThreadCheckSealsItsBuyer(t *testing.T) {
	dealFixture(t)
	sale, _ := newSale(t)
	threads := map[string]string{"buyer-a.example": buyerThread(t, sale, "buyer-a.example"), "buyer-b.example": buyerThread(t, sale, "buyer-b.example")}
	seen := map[string]bool{}
	for domain, id := range threads {
		sealed, given, _ := ruleInputs(t, id, offerInput)
		for name, body := range map[string]map[string]any{"sealed": sealed, "given to the checker": given} {
			cp, ok := body["counterparty"].(map[string]any)
			require.True(t, ok, "%s: %s carries the buyer", domain, name)
			assert.Equal(t, typedFPAlg, cp["fp_alg"], name)
			ids := cp["ids"].(map[string]any)
			assert.Equal(t, baselineIDs(t, id), ids, "%s: the same keyed fingerprints the thread's opening sealed", name)
			for kind, fp := range ids {
				assert.Regexp(t, hex64Re, fp, kind)
			}
		}
		fp := sealed["counterparty"].(map[string]any)["ids"].(map[string]any)["domain"].(string)
		assert.False(t, seen[fp], "two buyers never share a fingerprint")
		seen[fp] = true

		// No per-profile companion: the check names no payee, so none is sealed.
		records, _ := exportRecords(t, id)
		for _, r := range records {
			b, _ := r["x-deal-v0"].(map[string]any)
			assert.NotEqual(t, "counterparty_profile", b["record_type"])
		}
	}
}

// Each shared copy of a thread carries the buyer's per-deal fingerprints, as a
// purchase check's, and never the buyer's identifiers themselves.
func TestASaleThreadsSharedCopiesCarryNoPlainBuyerID(t *testing.T) {
	dealFixture(t)
	sale, _ := newSale(t)
	id := buyerThread(t, sale, "buyer-a.example")
	sealed, _, _ := ruleInputs(t, id, offerInput)
	fp := sealed["counterparty"].(map[string]any)["ids"].(map[string]any)["domain"].(string)
	for audience, to := range map[string]string{"counterparty": "Example Buyer", "adjudicator": "Example Adjudicator"} {
		shared := filepath.Join(t.TempDir(), audience+".json")
		out, err := invoke(t, "", "--profile", "deal", "disclose", "--deal", id, "--share", audience, "--to", to, "--out", shared)
		require.NoError(t, err, out)
		text := string(mustRead(t, shared))
		assert.NotContains(t, strings.ToLower(text), "buyer-a.example", audience)
		assert.Contains(t, text, fp, "%s: the check's keyed fingerprint is shared, as a purchase check's is", audience)
	}
}

// A check sealed before the thread's buyer was sealed keeps its bytes: without
// the step's marker it re-derives with no counterparty.
func TestASaleThreadCheckSealedBeforeReDerivesUnchanged(t *testing.T) {
	dealFixture(t)
	sale, _ := newSale(t)
	id := buyerThread(t, sale, "buyer-a.example")
	dealRun(t, "check", "--deal", id, "--input", writeJSON(t, offerInput))
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	events, err := s.loadOther(t.Context(), id)
	require.NoError(t, err)
	found := false
	for i, se := range events {
		if se.Event.Kind != "snapshot" {
			continue
		}
		found = true
		require.Equal(t, dealThreadCounterpartyVersion, se.Event.ThreadCounterparty)
		_, digest, err := encodeDealRecord(se.Event, events[:i], s.dkey)
		require.NoError(t, err)
		assert.Equal(t, se.Digest, digest, "the marked check re-derives to its sealed bytes")

		before := se.Event
		before.ThreadCounterparty = ""
		payload, _, err := encodeDealRecord(before, events[:i], s.dkey)
		require.NoError(t, err)
		var record map[string]any
		require.NoError(t, json.Unmarshal(payload, &record))
		assert.NotContains(t, record["body"], "counterparty", "a check sealed before carries no counterparty")
	}
	require.True(t, found)
}
