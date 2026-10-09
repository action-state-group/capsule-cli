package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	listingClaim = "they have 2 jet skis for Saturday"
	saleClaim    = "a sale price ends tonight"
)

// claimDeal opens a deal with a claim at first contact (a marketplace's
// listing, so its kind is stated) and notes another (the seller's message,
// whose kind is taken as merchant).
func claimDeal(t *testing.T) string {
	t.Helper()
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"rental","channel":"marketplace",
		"intent":{"verbatim":"rent two jet skis for Saturday","max_total_minor":45000,"allowed":["pay"]},
		"who":{"name":"Coastal Jet Rentals","domain":"coastal-jet-rentals.example"},
		"terms":{"item":"jet ski rental","quantity":2,"price_minor":40000,"currency":"USD"},
		"claims":[{"text":"`+listingClaim+`","source":"listing page","source_kind":"platform"}],
		"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	dealRun(t, "note", "--deal", id, "--kind", "claim", "--input", writeJSON(t, `{"text":"`+saleClaim+`","source":"seller message"}`))
	return id
}

// claimObjects are the claims a deal's records carry: each claim record's
// body, and each of the baseline's claims.
func claimObjects(t *testing.T, records []map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range records {
		block, ok := r["x-deal-v0"].(map[string]any)
		if !ok {
			continue
		}
		body := r["body"].(map[string]any)
		switch block["record_type"] {
		case "claim":
			out = append(out, body)
		case "baseline":
			list, _ := body["claims"].([]any)
			for _, c := range list {
				out = append(out, c.(map[string]any))
			}
		}
	}
	return out
}

// A claim record carries no clear text beyond whose it is: its words and its
// source note are salted commitments.
func TestAClaimRecordCarriesNoClearText(t *testing.T) {
	id := claimDeal(t)
	records, export := exportRecords(t, id)
	checkProfile(t, export)
	claims := claimObjects(t, records)
	require.Len(t, claims, 2)
	for _, c := range claims {
		assert.ElementsMatch(t, []string{"text_commitment", "source_kind", "source_ref_commitment"}, keysOf(c))
	}
	assert.Equal(t, "platform", claims[0]["source_kind"], "stated")
	assert.Equal(t, "merchant", claims[1]["source_kind"], "the seller's own message")
	raw, err := os.ReadFile(export)
	require.NoError(t, err)
	for _, clear := range []string{listingClaim, saleClaim, "listing_page", "seller_message"} {
		assert.NotContains(t, string(raw), clear, "no claim words or source note in any record")
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Whose a claim is is stated, or taken from a source that can only mean the
// other party; never guessed between the merchant and a platform.
func TestAClaimsSourceKind(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"buy a sticker","allowed":["pay"]},"who":{"name":"Sticker Marketplace","domain":"stickers.example"},
		"terms":{"item":"otter sticker","price_minor":454,"currency":"USD"},"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	for _, ambiguous := range []string{"booking page", "checkout page", "listing page", "listing_photo", "page_snapshot"} {
		_, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", id, "--kind", "claim", "--input",
			writeJSON(t, `{"text":"on sale today","source":"`+ambiguous+`"}`))
		require.ErrorIs(t, err, ErrInput, ambiguous)
		assert.Contains(t, err.Error(), "source_kind", ambiguous)
	}
	_, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", id, "--kind", "claim", "--input",
		writeJSON(t, `{"text":"on sale today","source":"page","source_kind":"vendor"}`))
	require.ErrorIs(t, err, ErrInput, "a kind outside the set")
	for _, merchant := range []string{"counterparty", "seller_message", "merchant email"} {
		dealRun(t, "note", "--deal", id, "--kind", "claim", "--input", writeJSON(t, `{"text":"on sale today","source":"`+merchant+`"}`))
	}
	dealRun(t, "note", "--deal", id, "--kind", "claim", "--input", writeJSON(t, `{"text":"ships tomorrow","source_kind":"external"}`))
	records, export := exportRecords(t, id)
	checkProfile(t, export)
	claims := claimObjects(t, records)
	require.Len(t, claims, 4)
	for _, c := range claims[:3] {
		assert.Equal(t, "merchant", c["source_kind"])
	}
	assert.Equal(t, "external", claims[3]["source_kind"])
	assert.NotContains(t, claims[3], "source_ref_commitment", "no source note, no commitment to one")
}

// The user's own copy opens every claim; the profile's Python checker
// recomputes each opening against the commitment the sealed record carries,
// and refuses an opening that does not match.
func TestClaimOpeningsRoundTripToThePythonChecker(t *testing.T) {
	python := profilePython(t)
	if python == "" {
		t.Skip("no python3 with the profile checker's dependencies")
	}
	id := claimDeal(t)
	_, export := exportRecords(t, id)
	bundle := filepath.Join(t.TempDir(), "own.json")
	dealRun(t, "report", "--deal", id, "--bundle", bundle)
	raw, err := os.ReadFile(bundle)
	require.NoError(t, err)
	var b map[string]any
	require.NoError(t, json.Unmarshal(raw, &b))
	openings := dealReportOf(b)["claim_openings"].([]any)
	require.Len(t, openings, 2)
	write := func(v any) string {
		p := filepath.Join(t.TempDir(), "openings.json")
		raw, err := json.Marshal(v)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(p, raw, 0o600))
		return p
	}
	out, err := exec.Command(python, dealProfileDir+"/check_profile.py", "--openings="+write(openings), export).CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Contains(t, string(out), "2 claim opening(s) match")

	tampered := openings[0].(map[string]any)["text"].(map[string]any)
	tampered["text"] = "they have 3 jet skis for Saturday"
	out, err = exec.Command(python, dealProfileDir+"/check_profile.py", "--openings="+write(openings), export).CombinedOutput()
	require.Error(t, err, string(out))
	assert.Contains(t, string(out), "the opening does not match text_commitment")
}

// A counterparty's copy shows that each claim exists and whose it is, never
// its words or an opening; an adjudicator's copy opens them.
func TestSharedCopiesAndClaims(t *testing.T) {
	id := claimDeal(t)
	counterparty, raw := sharedCopy(t, id, dealAudienceCounterparty, "the shop")
	// The claim step's record is disclosed (the baseline is withheld from a
	// counterparty whole: it carries the user's spending limit).
	assert.Contains(t, raw, `"source_kind":"merchant"`, "the claim record is disclosed")
	assert.NotContains(t, raw, listingClaim)
	assert.NotContains(t, raw, saleClaim)
	assert.NotContains(t, dealReportOf(counterparty), "claim_openings")

	adjudicator, _ := sharedCopy(t, id, dealAudienceAdjudicator, "a dispute desk")
	openings := dealReportOf(adjudicator)["claim_openings"].([]any)
	require.Len(t, openings, 2)
	words := []string{}
	for _, o := range openings {
		words = append(words, o.(map[string]any)["text"].(map[string]any)["text"].(string))
	}
	assert.ElementsMatch(t, []string{listingClaim, saleClaim}, words)
}

// A verdict names the claims it found unverified by reference, not by their
// words.
func TestAVerdictNamesUnverifiedClaimsByReference(t *testing.T) {
	id := claimDeal(t)
	dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":40000,"authorized_max_minor":40000,"terms":{"item":"jet ski rental","quantity":2,"price_minor":40000},"recourse":{"rail":"card","refundable":true}}`))
	records, export := exportRecords(t, id)
	checkProfile(t, export)
	for _, r := range records {
		if r["x-deal-v0"].(map[string]any)["record_type"] != "verdict" {
			continue
		}
		body := r["body"].(map[string]any)
		assert.NotContains(t, body, "unverified")
		refs := body["unverified_claims"].([]any)
		require.Len(t, refs, 2)
		assert.Equal(t, float64(0), refs[0].(map[string]any)["index"], "the baseline's first claim")
		assert.NotContains(t, refs[1].(map[string]any), "index", "a claim's own record")
	}
}

// A step sealed before claims were committed re-derives its claim in the
// clear, unchanged.
func TestAClaimSealedBeforeReDerivesUnchanged(t *testing.T) {
	id := claimDeal(t)
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	require.NoError(t, s.useDeal(t.Context(), id, false))
	events, err := s.load(t.Context(), id)
	require.NoError(t, err)
	for i, se := range events {
		if se.Event.Kind != "claim" {
			continue
		}
		ev := se.Event
		ev.ClaimCommit = ""
		raw, _, err := encodeDealRecord(ev, events[:i], s.dkey)
		require.NoError(t, err)
		assert.True(t, strings.Contains(string(raw), `"text":"`+saleClaim+`"`))
		assert.False(t, strings.Contains(string(raw), "text_commitment"))
	}
}
