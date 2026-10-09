package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dealCopies is every copy of a deal, each as written: the user's own (from
// `deal report --bundle` and from `bundle --deal`), the counterparty's and
// an adjudicator's.
func dealCopies(t *testing.T, id string) map[string][]byte {
	t.Helper()
	own := filepath.Join(t.TempDir(), "own.json")
	out, err := invoke(t, "", "--profile", "deal", "bundle", "--deal", id, "--out", own)
	require.NoError(t, err, out)
	copies := map[string][]byte{"own (deal report)": []byte(dealOwnBundle(t, id)), "own (bundle --deal)": mustRead(t, own)}
	for _, audience := range []string{dealAudienceCounterparty, dealAudienceAdjudicator} {
		_, raw := sharedCopy(t, id, audience, "the other party")
		copies[audience] = []byte(raw)
	}
	return copies
}

func engagedKinds(t *testing.T, raw []byte) ([]string, map[string]any) {
	t.Helper()
	var b map[string]any
	require.NoError(t, json.Unmarshal(raw, &b))
	exts := b["extensions"].(map[string]any)
	kinds := make([]string, 0, len(exts))
	for k := range exts {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return kinds, exts
}

// Every copy of a deal the user sells, a sale's thread included, engages
// x-deal-seller/v0, with an empty block: packaging a viewer selects on,
// carrying no value.
func TestEveryCopyOfASellersDealEngagesTheSellerKind(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	for name, id := range map[string]string{"a seller's deal": openTyped(t, sellerTyped), "a sale's thread": buyerThread(t, saleID, "buyer-a.example")} {
		for audience, raw := range dealCopies(t, id) {
			kinds, exts := engagedKinds(t, raw)
			assert.Equal(t, []string{dealCadenceExtension, dealSellerExtension, dealProfile}, kinds, "%s, %s", name, audience)
			assert.Equal(t, map[string]any{}, exts[dealSellerExtension], "%s, %s: no values", name, audience)
		}
	}
}

// A buyer's deal engages exactly what it did before: the cadence chain and
// the deal section, nothing new.
func TestNoCopyOfABuyersDealEngagesTheSellerKind(t *testing.T) {
	for name, typed := range map[string]bool{"x-deal-v0": false, "typed": true} {
		t.Run(name, func(t *testing.T) {
			id := stickerDeal(t, "card", typed)
			for audience, raw := range dealCopies(t, id) {
				kinds, _ := engagedKinds(t, raw)
				assert.Equal(t, []string{dealCadenceExtension, dealProfile}, kinds, audience)
			}
		})
	}
}

// The kind is never authority for capsulectl's own verify: a buyer's copy
// relabelled with it by hand verifies exactly as before, the kind reported
// as an extension it does not interpret, and no claim depends on it.
func TestVerifyDoesNotTreatTheSellerKindAsAuthority(t *testing.T) {
	id := stickerDeal(t, "card", true)
	_, raw := sharedCopy(t, id, dealAudienceCounterparty, "the merchant")
	plain := filepath.Join(t.TempDir(), "plain.json")
	require.NoError(t, os.WriteFile(plain, []byte(raw), 0o600))
	var b map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &b))
	b["extensions"].(map[string]any)[dealSellerExtension] = map[string]any{}
	relabelled := writeBundle(t, b)

	before, errBefore := verifyBundleOutput(t, plain)
	after, errAfter := verifyBundleOutput(t, relabelled)
	assert.Equal(t, errBefore, errAfter)
	assert.Equal(t, before["verdict"], after["verdict"])
	for _, claim := range []string{"graph_closure", "interval_coverage", "per_record_membership", "checkpoint", "producer_signatures", "witnesses"} {
		assert.Equal(t, before[claim], after[claim], claim)
	}
	var seller map[string]any
	for _, x := range after["extensions"].([]any) {
		if x.(map[string]any)["kind"] == dealSellerExtension {
			seller = x.(map[string]any)
		}
	}
	require.NotNil(t, seller)
	assert.Equal(t, "uninterpreted", seller["status"])
}
