package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sealedClaimClasses are the classes a deal's claim records carry, in order.
func sealedClaimClasses(t *testing.T, id string) []any {
	t.Helper()
	records, export := exportRecords(t, id)
	checkProfile(t, export)
	var out []any
	for _, c := range claimObjects(t, records) {
		out = append(out, c["class"])
	}
	return out
}

// A claim's class is one of a seller's representation classes: each is
// accepted and sealed as given.
func TestEachClaimClassIsAccepted(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	want := make([]any, 0, len(dealClaimClassList))
	for _, class := range dealClaimClassList {
		dealRun(t, "note", "--deal", id, "--kind", "claim", "--input",
			writeJSON(t, `{"text": "a word about the `+class+`", "source_kind": "agent", "class": "`+class+`"}`))
		want = append(want, class)
	}
	assert.Equal(t, want, sealedClaimClasses(t, id))
	checkerPasses(t, id)
}

// The set is the seller rules' representation classes, copied as they are.
func TestTheClaimClassesAreTheSellersRepresentationClasses(t *testing.T) {
	assert.Equal(t, []string{
		"price", "condition", "features", "authenticity", "availability", "delivery_date",
		"service_scope", "warranty", "refund_terms", "payment_methods", "pickup", "deadline",
		"address", "other",
	}, dealClaimClassList)
}

// An input that says delivery_promise is sealed as delivery_date: capsulectl
// never seals delivery_promise again.
func TestADeliveryPromiseIsSealedAsADeliveryDate(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	dealRun(t, "note", "--deal", id, "--kind", "claim", "--input",
		writeJSON(t, `{"text": "Ships within two days", "source_kind": "agent", "class": " Delivery_Promise "}`))
	assert.Equal(t, []any{"delivery_date"}, sealedClaimClasses(t, id))
}

func TestAnUnknownClaimClassIsRefused(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	for _, class := range []string{"guarantee", "delivery", "marketplace.listing"} {
		_, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", id, "--kind", "claim", "--input",
			writeJSON(t, `{"text": "Mint", "source_kind": "agent", "class": "`+class+`"}`))
		require.ErrorIs(t, err, ErrInput, class)
	}
	assert.Empty(t, sealedClaimClasses(t, id), "nothing sealed")
}

// A claim sealed with delivery_promise, before delivery_date replaced it,
// still re-derives, passes the profile and is opened for the buyer.
func TestAClaimSealedAsADeliveryPromiseStillVerifies(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	require.NoError(t, s.useDeal(t.Context(), id, false))
	events, err := s.load(t.Context(), id)
	require.NoError(t, err)
	_, err = s.seal(t.Context(), id, events, dealEvent{Kind: "claim", Claim: &dealClaim{
		Text: "Ships within two days", SourceKind: "agent", Class: "delivery_promise",
	}})
	require.NoError(t, err)
	require.NoError(t, s.close())

	assert.Equal(t, []any{"delivery_promise"}, sealedClaimClasses(t, id))
	checkerPasses(t, id)
	b, shared := sharedCopy(t, id, dealAudienceCounterparty, "the buyer")
	reps := dealReportOf(b)["representations"].([]any)
	require.Len(t, reps, 1)
	assert.Equal(t, "delivery_promise", reps[0].(map[string]any)["class"])
	assert.Contains(t, shared, `"class":"delivery_promise"`, "the shared copy keeps the class it was sealed with")
}
