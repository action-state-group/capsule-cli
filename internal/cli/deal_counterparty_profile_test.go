package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// profileStep is a deal's counterparty_profile record and the check it
// follows.
type profileStep struct {
	payee, checkDigest, checkPayee string
	record                         map[string]any
}

// profileSteps reads every counterparty_profile record of a deal.
func profileSteps(t *testing.T, dealID string) []profileStep {
	t.Helper()
	events, records := recordsOf(t, dealID)
	var out []profileStep
	for i, se := range events {
		if se.Event.Kind != "counterparty_profile" {
			continue
		}
		block := records[i][dealProfile].(map[string]any)
		cp := block["counterparty_profile"].(map[string]any)
		require.Equal(t, dealProfileFPAlg, cp["fp_alg"])
		refs := block["refs"].([]any)
		require.Len(t, refs, 1)
		ref := refs[0].(map[string]any)
		require.Equal(t, "about", ref["rel"])
		require.Positive(t, i)
		check := events[i-1]
		require.Equal(t, "snapshot", check.Event.Kind, "the companion follows its check")
		checkBlock := records[i-1][dealProfile].(map[string]any)
		checkIDs := checkBlock["counterparty"].(map[string]any)["ids"].(map[string]any)
		out = append(out, profileStep{
			payee: cp["ids"].(map[string]any)["payee"].(string), checkDigest: check.Digest,
			checkPayee: checkIDs["payee"].(string), record: records[i],
		})
		assert.Equal(t, check.Digest, ref["digest"], "about names the check's record digest")
		assert.Empty(t, bodyOf(records[i]), "the companion carries nothing else")
	}
	return out
}

// One merchant has one profile fingerprint across a profile's deals, while
// each deal's own fingerprint of it differs.
func TestTheSamePayeeHasOneProfileFingerprintAcrossDeals(t *testing.T) {
	dealFixture(t)
	first, second := profileSteps(t, retailDeal(t)), profileSteps(t, retailDeal(t))
	require.Len(t, first, 1, "the pay check")
	require.Len(t, second, 1)
	assert.Equal(t, first[0].payee, second[0].payee, "the same merchant, the same profile value")
	assert.NotEqual(t, first[0].checkPayee, second[0].checkPayee, "each deal's own fingerprint differs")
	assert.NotEqual(t, first[0].payee, first[0].checkPayee, "never the per-deal value")
}

// Another profile, with its own store, has another value for the merchant.
func TestAnotherProfileHasAnotherProfileFingerprint(t *testing.T) {
	dealFixture(t)
	mine := profileSteps(t, retailDeal(t))
	// A second fixture: a fresh configuration and a new store of its own.
	dealFixture(t)
	theirs := profileSteps(t, retailDeal(t))
	require.Len(t, theirs, 1)
	assert.NotEqual(t, mine[0].payee, theirs[0].payee)
}

// No record holds the merchant's id in the clear, or a bare digest of it.
func TestNoPlainDigestOfThePayeeIsSealed(t *testing.T) {
	dealFixture(t)
	var open struct {
		Who struct{ Name string } `json:"who"`
	}
	require.NoError(t, json.Unmarshal(mustRead(t, filepath.Join(retailDemo, "open.json")), &open))
	require.NotEmpty(t, open.Who.Name)
	id := retailDeal(t)
	require.Len(t, profileSteps(t, id), 1)
	normalized, err := normalizeID("payee", open.Who.Name)
	require.NoError(t, err)
	var forbidden []string
	for _, v := range []string{open.Who.Name, normalized, strings.ToLower(open.Who.Name)} {
		sum := sha256.Sum256([]byte(v))
		forbidden = append(forbidden, hex.EncodeToString(sum[:]))
	}
	_, records := recordsOf(t, id)
	for _, r := range records {
		raw, err := json.Marshal(r)
		require.NoError(t, err)
		for _, f := range forbidden {
			assert.NotContains(t, string(raw), f, "no bare digest of the merchant id")
		}
		assert.NotContains(t, string(raw), open.Who.Name)
	}
}

// No shared copy, for any audience, carries the profile value: the
// companion is withheld, and the check it follows is shared as before.
func TestNoShareCarriesTheProfileFingerprint(t *testing.T) {
	dealFixture(t)
	id := retailDeal(t)
	steps := profileSteps(t, id)
	require.Len(t, steps, 1)
	events := chainSteps(t, id)
	private := dealPrivateValues(events)
	for _, audience := range []string{dealAudienceCounterparty, dealAudienceAdjudicator} {
		assert.False(t, dealRecordShareable(steps[0].record, "", audience, private), audience)
		_, raw := sharedCopy(t, id, audience, "x")
		assert.NotContains(t, raw, steps[0].payee, audience)
		assert.NotContains(t, raw, dealProfileFPAlg, audience)
	}
}

// The profile key and fingerprint for a fixed store secret: the vector an
// independent implementation recomputes.
func TestTheProfileFingerprintVector(t *testing.T) {
	secret, err := hex.DecodeString(strings.Repeat("11", 32))
	require.NoError(t, err)
	key := profileKeyFor(secret)
	fp, err := fingerprintID(key, "payee", "Example Stickers")
	require.NoError(t, err)
	assert.Equal(t, hex.EncodeToString(hmacSHA256(secret, "x-deal-v0/profile-key\x00")), hex.EncodeToString(key))
	assert.Equal(t, hex.EncodeToString(hmacSHA256(key, "x-deal-v0/fp\x00", "payee", "\x00", mustNormalize(t, "payee", "Example Stickers"))), fp)
	assert.NotEqual(t, hex.EncodeToString(dealKeyFor(secret, "deal-0000000000000000")), hex.EncodeToString(key), "never a deal's key")
}

func mustNormalize(t *testing.T, kind, raw string) string {
	t.Helper()
	n, err := normalizeID(kind, raw)
	require.NoError(t, err)
	return n
}
